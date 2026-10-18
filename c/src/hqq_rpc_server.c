/*
 * hqq_rpc_server.c - server of the hqq-bindgen C runtime: the listener, the bootstrap over a
 * short-lived control link (BINDGEN.md 4.11), one thread per session, shutdown (5.2), and the
 * process entry point hqqrpc_main (9.5).
 *
 * Threads: the caller of hqqrpc_server_run is the acceptor. Each accepted control link and each
 * session has a thread of its own; all of them are tracked in one list so that run can wait for
 * them and join them. Every wait on a ring or on the listener carries the server's cancel token
 * and a bounded timeout, so no thread sleeps through a stop (5.2).
 *
 * Portable C11. Threads, mutexes, events, clocks and random bytes come from hqq_platform.h;
 * POSIX headers appear only inside #if !defined(_WIN32) blocks (hqqrpc_main).
 */
#if defined(_WIN32)
#  ifndef WIN32_LEAN_AND_MEAN
#    define WIN32_LEAN_AND_MEAN 1
#  endif
#  ifndef NOMINMAX
#    define NOMINMAX 1
#  endif
#  ifndef _WIN32_WINNT
#    define _WIN32_WINNT 0x0A00
#  endif
#elif defined(__linux__)
#  ifndef _GNU_SOURCE
#    define _GNU_SOURCE 1
#  endif
#elif defined(__APPLE__)
#  ifndef _DARWIN_C_SOURCE
#    define _DARWIN_C_SOURCE 1
#  endif
#else
#  ifndef _POSIX_C_SOURCE
#    define _POSIX_C_SOURCE 200809L
#  endif
#endif

#include <errno.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#if defined(_WIN32)
#  include <fcntl.h>
#  include <io.h>
#  include <signal.h>
#  include <windows.h>
#  ifndef SIGBREAK
#    define SIGBREAK 21
#  endif
#else
#  include <pthread.h>
#  include <signal.h>
#  include <unistd.h>
#endif

#define HQQ_BUILDING 1
#include "hqq/hqq_lifeline.h"
#include "hqq_rpc_internal.h"

/* ---- Server structures --------------------------------------------------------------------- */

typedef struct hqqrpc_thr {
    struct hqqrpc_thr *next;
    hqqrpc_server *srv;
    hqq_plat_thread *h;
    void (*fn)(void *);
    void *arg;
    int done;
    int detached;                 /* left running after a shutdown timeout; never joined */
} hqqrpc_thr;

typedef struct hqqrpc_ctl {
    struct hqqrpc_ctl *next;
    hqqrpc_server *srv;
    hqq_adv *adv;
    uint64_t id;
    hqq_cancel cancel;
    _Atomic int opened;
    int released;                 /* no longer counted against max_control_links (guarded by srv->mu) */
} hqqrpc_ctl;

enum {
    SRV_CREATED = 0,
    SRV_RUNNING,
    SRV_DONE,    /* run returned HQQ_OK: no thread is left */
    SRV_FAILED   /* run returned an error: threads may still use the server */
};

struct hqqrpc_server {
    const hqqrpc_iface *iface;
    hqqrpc_cfg cfg;
    hqqrpc_server_options opts;
    hqq_allocator alloc_copy;
    const hqq_allocator *alloc;   /* NULL: malloc */
    hqq_options link_opts;        /* options of session links */
    char *listener;
    hqq_acceptor *acc;
    hqq_cancel cancel;
    _Atomic int stop;
    hqq_plat_mutex *exec_mu;      /* the execution lock of serialize(session) */
    hqq_plat_mutex *mu;           /* guards everything below */
    hqq_plat_event *evt;          /* signalled whenever a thread ends */
    hqqrpc_thr *thrs;
    unsigned thr_active;
    hqqrpc_ctl *ctls;
    unsigned ctl_count;
    uint64_t ctl_seq;
    hqqrpc_session *sess_list;
    uint32_t sessions;
    int state;
};

static void srv_log(hqqrpc_server *srv, int level, const char *fmt, ...) {
    char buf[256];
    va_list ap;

    if (srv->opts.on_log == NULL) {
        return;
    }
    va_start(ap, fmt);
    (void)vsnprintf(buf, sizeof buf, fmt, ap);
    va_end(ap);
    srv->opts.on_log(srv->opts.user, level, buf);
}

/* ---- Options ------------------------------------------------------------------------------- */

void hqqrpc_server_options_init(hqqrpc_server_options *opts) {
    if (opts == NULL) {
        return;
    }
    memset(opts, 0, sizeof *opts);
    opts->struct_size = sizeof *opts;
    hqq_options_init(&opts->link);
    opts->link.peer_timeout_ns = HQQRPC_DEF_PEER_TIMEOUT_NS;
    hqq_acceptor_options_init(&opts->acceptor);
}

/* ---- Thread tracking ----------------------------------------------------------------------- */

static void thr_entry(void *p) {
    hqqrpc_thr *t = (hqqrpc_thr *)p;
    hqqrpc_server *srv = t->srv;

    t->fn(t->arg);
    hqq_plat_mutex_lock(srv->mu);
    t->done = 1;
    srv->thr_active--;
    hqq_plat_mutex_unlock(srv->mu);
    hqq_plat_event_signal(srv->evt);
}

/* Starts fn(arg) on a tracked thread. 0 on success. */
static int srv_spawn(hqqrpc_server *srv, void (*fn)(void *), void *arg) {
    hqqrpc_thr *t = (hqqrpc_thr *)hqq_alloc(srv->alloc, sizeof *t);
    int os_err = 0;

    if (t == NULL) {
        return -1;
    }
    memset(t, 0, sizeof *t);
    t->srv = srv;
    t->fn = fn;
    t->arg = arg;
    /* The list lock is held across the start so that the thread cannot finish before t->h is set. */
    hqq_plat_mutex_lock(srv->mu);
    t->h = hqq_plat_thread_start(srv->alloc, thr_entry, t, &os_err);
    if (t->h == NULL) {
        hqq_plat_mutex_unlock(srv->mu);
        hqq_free(srv->alloc, t, sizeof *t);
        return -1;
    }
    t->next = srv->thrs;
    srv->thrs = t;
    srv->thr_active++;
    hqq_plat_mutex_unlock(srv->mu);
    return 0;
}

/* Joins and frees the threads that have ended (all of them when `all` is set; the caller then
 * knows that thr_active is 0). */
static void srv_reap(hqqrpc_server *srv) {
    hqqrpc_thr *ended = NULL;
    hqqrpc_thr **pp;

    hqq_plat_mutex_lock(srv->mu);
    pp = &srv->thrs;
    while (*pp != NULL) {
        hqqrpc_thr *t = *pp;

        if (t->done) {
            *pp = t->next;
            t->next = ended;
            ended = t;
        } else {
            pp = &t->next;
        }
    }
    hqq_plat_mutex_unlock(srv->mu);
    while (ended != NULL) {
        hqqrpc_thr *next = ended->next;

        hqq_plat_thread_join(srv->alloc, ended->h);
        hqq_free(srv->alloc, ended, sizeof *ended);
        ended = next;
    }
}

/* ---- Stop ------------------------------------------------------------------------------------ */

void hqqrpc_server_stop(hqqrpc_server *srv) {
    hqqrpc_ctl *c;

    if (srv == NULL) {
        return;
    }
    atomic_store(&srv->stop, 1);
    hqq_cancel_signal(&srv->cancel);
    hqq_plat_mutex_lock(srv->mu);
    for (c = srv->ctls; c != NULL; c = c->next) {
        hqq_cancel_signal(&c->cancel);
    }
    hqq_plat_mutex_unlock(srv->mu);
}

/* A valid HELLO arrived on a session that came from control link ctl_id: the control link has
 * done its job (4.11). */
void hqqrpc_server_session_started(hqqrpc_server *srv, uint64_t ctl_id) {
    hqqrpc_ctl *c;

    if (ctl_id == 0) {
        return;
    }
    hqq_plat_mutex_lock(srv->mu);
    for (c = srv->ctls; c != NULL; c = c->next) {
        if (c->id == ctl_id) {
            hqq_cancel_signal(&c->cancel);
            /* Its job is done: stop counting it now; the serve loop notices the cancel at its next
             * park slice, and the thread ends later. */
            if (!c->released) {
                c->released = 1;
                srv->ctl_count--;
            }
            break;
        }
    }
    hqq_plat_mutex_unlock(srv->mu);
}

/* ---- Session thread ------------------------------------------------------------------------- */

static void session_loop(hqqrpc_session *s) {
    hqqrpc_server *srv = s->srv;

    for (;;) {
        hqq_read_reservation rr;
        hqq_wait w;
        hqq_status st;

        w.timeout_ns = HQQRPC_POLL_NS;
        w.cancel = &srv->cancel;
        st = hqq_read_reserve(s->link, &rr, &w);
        if (st == HQQ_OK) {
            if (hqqrpc_session_rx_slot(s, &rr)) {
                return;
            }
            continue;
        }
        if (st == HQQ_ERR_TIMEOUT) {
            uint64_t now = hqq_plat_mono_ns();

            if (atomic_load(&srv->stop)) {
                hqqrpc_session_set_end(s, HQQRPC_END_STOP);
            } else if (s->state == HQQRPC_ST_HELLO && now - s->start_ns > (uint64_t)s->cfg.open_timeout_ns) {
                hqqrpc_log(s, HQQRPC_LOG_WARN, "no HELLO within the open timeout");
                hqqrpc_session_set_end(s, HQQRPC_END_HANDSHAKE);
            } else if (s->cfg.idle_timeout_ns != 0 && s->state == HQQRPC_ST_ACTIVE &&
                       now - s->last_req_ns > (uint64_t)s->cfg.idle_timeout_ns) {
                hqqrpc_session_set_end(s, HQQRPC_END_IDLE);
            }
        } else if (st == HQQ_ERR_CANCELED) {
            hqqrpc_session_set_end(s, HQQRPC_END_STOP);
        } else if (st == HQQ_ERR_INVALID_SIZE || st == HQQ_ERR_UNEXPECTED_PACKET) {
            /* The link dropped a frame: the one-response-per-request pairing is broken (5.4). */
            hqqrpc_log(s, HQQRPC_LOG_WARN, "protocol violation: %s", hqq_strerror(st));
            hqqrpc_session_set_end(s, HQQRPC_END_PROTOCOL);
        } else {
            hqqrpc_session_set_end(s, HQQRPC_END_PEER);
        }
        if (s->end != HQQRPC_END_NONE) {
            return;
        }
    }
}

static void session_main(void *arg) {
    hqqrpc_session *s = (hqqrpc_session *)arg;
    hqqrpc_server *srv = s->srv;
    hqqrpc_session **pp;

    hqqrpc_session_start_callback(s);
    session_loop(s);
    hqqrpc_session_finish(s);
    hqq_plat_mutex_lock(srv->mu);
    for (pp = &srv->sess_list; *pp != NULL; pp = &(*pp)->srv_next) {
        if (*pp == s) {
            *pp = s->srv_next;
            s->srv_next = NULL;
            break;
        }
    }
    while (s->cancel_refs > 0) {
        hqq_plat_mutex_unlock(srv->mu);
        hqq_plat_sleep_ns(1000000u);
        hqq_plat_mutex_lock(srv->mu);
    }
    hqq_plat_mutex_unlock(srv->mu);
    hqqrpc_session_destroy(s); /* closes the link: the creator removes the name if it is still there */
    hqq_plat_mutex_lock(srv->mu);
    srv->sessions--;
    hqq_plat_mutex_unlock(srv->mu);
}

/* ---- Bootstrap (4.11) ------------------------------------------------------------------------ */

static void open_error(hqq_response *w, unsigned code, const char *text) {
    size_t n = strlen(text);

    if (n > HQQRPC_OPEN_MSG_MAX) {
        n = HQQRPC_OPEN_MSG_MAX;
    }
    (void)hqq_response_error(w, HQQRPC_OPEN_STATUS_BASE + code, text, n);
}

/* name = first 8 bytes of the listener name (outside [A-Za-z0-9_-] replaced by '_'), ".s", then
 * 16 hexadecimal digits from the cryptographic random source. At most 26 bytes. */
static int session_name(const char *listener, char name[32]) {
    static const char hex[] = "0123456789abcdef";
    uint8_t rnd[8];
    size_t i = 0;
    size_t n = 0;

    if (hqq_plat_random(rnd, sizeof rnd) != 0) {
        return -1;
    }
    for (; i < 8 && listener[i] != '\0'; i++) {
        char c = listener[i];
        int ok = (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-';

        name[n++] = ok ? c : '_';
    }
    name[n++] = '.';
    name[n++] = 's';
    for (i = 0; i < sizeof rnd; i++) {
        name[n++] = hex[rnd[i] >> 4];
        name[n++] = hex[rnd[i] & 15u];
    }
    name[n] = '\0';
    return 0;
}

static int is_pow2(uint32_t v) {
    return v != 0 && (v & (v - 1u)) == 0;
}

static void ctl_handler(void *user, hqq_req_ctx *rctx, hqq_response *w, const hqq_request *req) {
    hqqrpc_ctl *ctl = (hqqrpc_ctl *)user;
    hqqrpc_server *srv = ctl->srv;
    const uint8_t *d = (const uint8_t *)hqq_request_data(req);
    size_t n = hqq_request_len(req);
    uint64_t method = hqq_request_method(req);
    uint32_t B;
    uint32_t S;
    int expected = 0;
    char name[32];
    uint8_t reply[12 + 32];
    size_t name_len;
    hqq_link *link = NULL;
    hqqrpc_session *s;
    hqq_status st = HQQ_OK;
    int attempt;

    (void)rctx;
    if (method == HQQRPC_CANCEL_METHOD) {
        uint32_t req_id;
        uint16_t cname_len;
        char cname[32];
        hqqrpc_session *target = NULL;

        if (n < 17 || d == NULL || memcmp(d, "HQBC", 4) != 0) {
            open_error(w, HQQRPC_OPEN_E_INTERNAL, "bad cancel request");
            return;
        }
        if (hqqrpc_le16(d + 4) != HQQRPC_PROTO_MAJOR) {
            open_error(w, HQQRPC_OPEN_E_VERSION, "unsupported protocol version");
            return;
        }
        req_id = hqqrpc_le32(d + 8);
        cname_len = hqqrpc_le16(d + 12);
        if (cname_len < 1 || cname_len >= sizeof cname || n != 16u + cname_len || hqqrpc_le16(d + 14) != 0) {
            open_error(w, HQQRPC_OPEN_E_INTERNAL, "bad cancel request");
            return;
        }
        if (!atomic_compare_exchange_strong(&ctl->opened, &expected, 1)) {
            open_error(w, HQQRPC_OPEN_E_INTERNAL, "control link already used");
            return;
        }
        hqqrpc_server_session_started(srv, ctl->id);
        memcpy(cname, d + 16, cname_len);
        cname[cname_len] = '\0';
        hqq_plat_mutex_lock(srv->mu);
        for (s = srv->sess_list; s != NULL; s = s->srv_next) {
            if (strcmp(s->sess_name, cname) == 0) {
                s->cancel_refs++;
                target = s;
                break;
            }
        }
        hqq_plat_mutex_unlock(srv->mu);
        if (target != NULL) {
            hqqrpc_session_cancel(target, req_id);
            hqq_plat_mutex_lock(srv->mu);
            target->cancel_refs--;
            hqq_plat_mutex_unlock(srv->mu);
        }
        (void)hqq_response_write(w, NULL, 0);
        return;
    }
    if (method != HQQRPC_OPEN_METHOD) {
        open_error(w, HQQRPC_OPEN_E_INTERNAL, "unknown method");
        return;
    }
    if (n != 16 || d == NULL || memcmp(d, "HQBO", 4) != 0) {
        open_error(w, HQQRPC_OPEN_E_INTERNAL, "bad open request");
        return;
    }
    if (hqqrpc_le16(d + 4) != HQQRPC_PROTO_MAJOR) {
        open_error(w, HQQRPC_OPEN_E_VERSION, "unsupported protocol version");
        return;
    }
    B = hqqrpc_le32(d + 8);
    S = hqqrpc_le32(d + 12);
    if (B == 0) {
        B = srv->cfg.buffer_count;
    }
    if (S == 0) {
        S = srv->cfg.buffer_size;
    }
    if (!is_pow2(B) || B < HQQ_MIN_BUFFER_COUNT || B > srv->cfg.max_buffer_count || (S % 64u) != 0 ||
        S < HQQRPC_MIN_BUFFER_SIZE || S > srv->cfg.max_buffer_size ||
        2u * (uint64_t)B * S > srv->cfg.max_segment_bytes) {
        open_error(w, HQQRPC_OPEN_E_GEOMETRY, "unsupported geometry");
        return;
    }
    if (atomic_load(&srv->stop)) {
        open_error(w, HQQRPC_OPEN_E_INTERNAL, "shutting down");
        return;
    }
    if (!atomic_compare_exchange_strong(&ctl->opened, &expected, 1)) {
        open_error(w, HQQRPC_OPEN_E_INTERNAL, "control link already used");
        return;
    }
    hqq_plat_mutex_lock(srv->mu);
    if (srv->sessions >= srv->cfg.max_sessions) {
        hqq_plat_mutex_unlock(srv->mu);
        atomic_store(&ctl->opened, 0);
        open_error(w, HQQRPC_OPEN_E_BUSY, "too many sessions");
        return;
    }
    srv->sessions++;
    hqq_plat_mutex_unlock(srv->mu);

    /* The name has 64 random bits; a live segment of that name is retried with a new one. */
    for (attempt = 0; attempt < 8; attempt++) {
        if (session_name(srv->listener, name) != 0) {
            st = HQQ_ERR_SYSTEM;
            break;
        }
        st = hqq_standard_create(name, B, S, srv->cfg.perm, &srv->link_opts, &link, NULL);
        if (st != HQQ_ERR_LINK_BUSY) {
            break;
        }
    }
    s = NULL;
    if (st == HQQ_OK) {
        s = hqqrpc_session_create(srv->iface, &srv->cfg, &srv->opts, srv, link, srv->alloc, S, B);
        if (s == NULL) {
            st = HQQ_ERR_NO_MEMORY;
            (void)hqq_close(link);
        }
    }
    if (st == HQQ_OK) {
        s->cancel = &srv->cancel;
        s->exec_mu = srv->exec_mu;
        s->ctl_id = ctl->id;
        memcpy(s->sess_name, name, strlen(name) + 1u);
        hqq_plat_mutex_lock(srv->mu);
        s->srv_next = srv->sess_list;
        srv->sess_list = s;
        hqq_plat_mutex_unlock(srv->mu);
        if (srv_spawn(srv, session_main, s) != 0) {
            hqqrpc_session **pp;

            hqq_plat_mutex_lock(srv->mu);
            for (pp = &srv->sess_list; *pp != NULL; pp = &(*pp)->srv_next) {
                if (*pp == s) {
                    *pp = s->srv_next;
                    s->srv_next = NULL;
                    break;
                }
            }
            hqq_plat_mutex_unlock(srv->mu);
            hqqrpc_session_destroy(s); /* closes the link */
            st = HQQ_ERR_SYSTEM;
        }
    }
    if (st != HQQ_OK) {
        hqq_plat_mutex_lock(srv->mu);
        srv->sessions--;
        hqq_plat_mutex_unlock(srv->mu);
        atomic_store(&ctl->opened, 0);
        srv_log(srv, HQQRPC_LOG_ERROR, "cannot create a session: %s", hqq_strerror(st));
        open_error(w, HQQRPC_OPEN_E_INTERNAL, "cannot create the session");
        return;
    }
    name_len = strlen(name);
    hqqrpc_put_le32(reply, B);
    hqqrpc_put_le32(reply + 4, S);
    hqqrpc_put_le16(reply + 8, (uint16_t)name_len);
    hqqrpc_put_le16(reply + 10, 0);
    memcpy(reply + 12, name, name_len);
    (void)hqq_response_write(w, reply, 12 + name_len);
}

static void ctl_main(void *arg) {
    hqqrpc_ctl *ctl = (hqqrpc_ctl *)arg;
    hqqrpc_server *srv = ctl->srv;
    hqq_wait w;
    hqqrpc_ctl **pp;

    w.timeout_ns = srv->cfg.open_timeout_ns;
    w.cancel = &ctl->cancel;
    (void)hqq_adv_serve(ctl->adv, ctl_handler, ctl, &w);
    hqq_plat_mutex_lock(srv->mu);
    for (pp = &srv->ctls; *pp != NULL; pp = &(*pp)->next) {
        if (*pp == ctl) {
            *pp = ctl->next;
            break;
        }
    }
    if (!ctl->released) {
        ctl->released = 1;
        srv->ctl_count--;
    }
    hqq_plat_mutex_unlock(srv->mu);
    (void)hqq_adv_close(ctl->adv);
    hqq_free(srv->alloc, ctl, sizeof *ctl);
}

/* An accepted control link: unlink it at once (the client created the segment and would leak it),
 * apply max_control_links, and serve OPEN on a thread of its own. */
static void on_accepted(hqqrpc_server *srv, hqq_adv *adv) {
    hqqrpc_ctl *ctl;
    hqqrpc_ctl **pp;

    (void)hqq_adv_unlink(adv);
    ctl = (hqqrpc_ctl *)hqq_alloc(srv->alloc, sizeof *ctl);
    if (ctl == NULL) {
        (void)hqq_adv_close(adv);
        return;
    }
    memset(ctl, 0, sizeof *ctl);
    ctl->srv = srv;
    ctl->adv = adv;
    hqq_cancel_init(&ctl->cancel);
    atomic_init(&ctl->opened, 0);
    hqq_plat_mutex_lock(srv->mu);
    if (srv->ctl_count >= srv->cfg.max_control_links) {
        hqqrpc_ctl *c;

        /* A link whose client has closed it (a refused OPEN, a client that left) is not served any
         * more, but its thread only notices at the next park slice of hqq_adv_serve. Count only the
         * links whose peer is still there. The adv handle stays valid while the link is listed:
         * ctl_main closes it after removing it from the list, under this lock. */
        for (c = srv->ctls; c != NULL; c = c->next) {
            if (!c->released && hqq_adv_peer_state(c->adv) != HQQ_PEER_CONNECTED) {
                c->released = 1;
                srv->ctl_count--;
            }
        }
    }
    if (srv->ctl_count >= srv->cfg.max_control_links) {
        hqq_plat_mutex_unlock(srv->mu);
        hqq_free(srv->alloc, ctl, sizeof *ctl);
        (void)hqq_adv_close(adv);
        srv_log(srv, HQQRPC_LOG_WARN, "control link refused: max_control_links reached");
        return;
    }
    ctl->id = ++srv->ctl_seq;
    ctl->next = srv->ctls;
    srv->ctls = ctl;
    srv->ctl_count++;
    hqq_plat_mutex_unlock(srv->mu);
    if (atomic_load(&srv->stop)) {
        hqq_cancel_signal(&ctl->cancel);
    }
    if (srv_spawn(srv, ctl_main, ctl) != 0) {
        hqq_plat_mutex_lock(srv->mu);
        for (pp = &srv->ctls; *pp != NULL; pp = &(*pp)->next) {
            if (*pp == ctl) {
                *pp = ctl->next;
                break;
            }
        }
        srv->ctl_count--;
        hqq_plat_mutex_unlock(srv->mu);
        hqq_free(srv->alloc, ctl, sizeof *ctl);
        (void)hqq_adv_close(adv);
    }
}

/* ---- Create, run, destroy ------------------------------------------------------------------- */

static void srv_free(hqqrpc_server *srv) {
    hqq_allocator a = srv->alloc_copy;
    const hqq_allocator *ap = srv->alloc != NULL ? &a : NULL;

    hqq_plat_mutex_destroy(ap, srv->exec_mu);
    hqq_plat_mutex_destroy(ap, srv->mu);
    hqq_plat_event_destroy(ap, srv->evt);
    if (srv->listener != NULL) {
        hqq_free(ap, srv->listener, strlen(srv->listener) + 1u);
    }
    hqq_free(ap, srv, sizeof *srv);
}

hqq_status hqqrpc_server_create(const hqqrpc_iface *iface, const char *listener_name,
                                const hqqrpc_server_options *opts, hqqrpc_server **out) {
    hqqrpc_server_options def;
    hqqrpc_cfg cfg;
    hqqrpc_server *srv;
    hqq_acceptor_options aopts;
    const hqq_allocator *user_alloc;
    size_t name_len;
    hqq_status st;

    if (out == NULL) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    *out = NULL;
    if (hqqrpc_iface_check(iface) != HQQ_OK || listener_name == NULL || listener_name[0] == '\0') {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    if (opts == NULL) {
        hqqrpc_server_options_init(&def);
        opts = &def;
    }
    st = hqqrpc_cfg_resolve(opts, &cfg);
    if (st != HQQ_OK) {
        return st;
    }
    user_alloc = opts->link.allocator;
    srv = (hqqrpc_server *)hqq_alloc(user_alloc, sizeof *srv);
    if (srv == NULL) {
        return HQQ_ERR_NO_MEMORY;
    }
    memset(srv, 0, sizeof *srv);
    if (user_alloc != NULL) {
        srv->alloc_copy = *user_alloc;
        srv->alloc = &srv->alloc_copy;
    }
    srv->iface = iface;
    srv->cfg = cfg;
    srv->opts = *opts;
    if (srv->opts.link.struct_size == 0) { /* a zero-filled options struct: the defaults of hqqrpc_server_options_init */
        hqq_options_init(&srv->opts.link);
        srv->opts.link.peer_timeout_ns = HQQRPC_DEF_PEER_TIMEOUT_NS;
    }
    srv->opts.link.allocator = srv->alloc;
    srv->link_opts = srv->opts.link;
    atomic_init(&srv->stop, 0);
    hqq_cancel_init(&srv->cancel);

    name_len = strlen(listener_name);
    srv->listener = (char *)hqq_alloc(srv->alloc, name_len + 1u);
    srv->mu = hqq_plat_mutex_create(srv->alloc);
    srv->evt = hqq_plat_event_create(srv->alloc);
    if (iface->serialize == HQQRPC_SERIALIZE_SESSION) {
        srv->exec_mu = hqq_plat_mutex_create(srv->alloc);
    }
    if (srv->listener == NULL || srv->mu == NULL || srv->evt == NULL ||
        (iface->serialize == HQQRPC_SERIALIZE_SESSION && srv->exec_mu == NULL)) {
        srv_free(srv);
        return HQQ_ERR_NO_MEMORY;
    }
    memcpy(srv->listener, listener_name, name_len + 1u);

    aopts = srv->opts.acceptor;
    if (aopts.struct_size == 0) {
        hqq_acceptor_options_init(&aopts);
    }
    aopts.perm = cfg.perm;
    st = hqq_acceptor_create(listener_name, &aopts, &srv->acc, NULL);
    if (st != HQQ_OK) {
        srv_free(srv);
        return st;
    }
    *out = srv;
    return HQQ_OK;
}

hqq_status hqqrpc_server_run(hqqrpc_server *srv) {
    hqq_status fatal = HQQ_OK;
    unsigned errors = 0;
    uint64_t deadline;
    int all_done;

    if (srv == NULL) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    hqq_plat_mutex_lock(srv->mu);
    if (srv->state != SRV_CREATED) {
        hqq_plat_mutex_unlock(srv->mu);
        return HQQ_ERR_INVALID_OP;
    }
    srv->state = SRV_RUNNING;
    hqq_plat_mutex_unlock(srv->mu);
    srv_log(srv, HQQRPC_LOG_INFO, "serving %s on %s (serialize %s, %u sessions at most)",
            srv->iface->service != NULL ? srv->iface->service : "?", srv->listener,
            srv->iface->serialize == HQQRPC_SERIALIZE_NONE ? "none" : "session", (unsigned)srv->cfg.max_sessions);

    while (!atomic_load(&srv->stop)) {
        hqq_adv *adv = NULL;
        hqq_wait w;
        hqq_status st;

        w.timeout_ns = HQQRPC_POLL_NS;
        w.cancel = &srv->cancel;
        st = hqq_acceptor_accept(srv->acc, &adv, &w);
        if (st == HQQ_OK) {
            errors = 0;
            on_accepted(srv, adv);
        } else if (st == HQQ_ERR_TIMEOUT) {
            errors = 0;
            srv_reap(srv);
        } else if (st == HQQ_ERR_CANCELED) {
            break;
        } else {
            srv_log(srv, HQQRPC_LOG_ERROR, "accept failed: %s", hqq_strerror(st));
            if (++errors >= 100u) {
                fatal = st;
                break;
            }
            hqq_plat_sleep_ns(50u * 1000000u);
        }
    }

    hqqrpc_server_stop(srv);
    (void)hqq_acceptor_close(srv->acc); /* frees the acceptor; its name is removed */
    srv->acc = NULL;

    /* Wait for every control and session thread; what is stuck in the library stays detached. */
    deadline = hqq_plat_mono_ns() + (uint64_t)srv->cfg.shutdown_timeout_ns;
    all_done = 0;
    for (;;) {
        unsigned active;
        uint64_t now;

        hqq_plat_event_reset(srv->evt);
        hqq_plat_mutex_lock(srv->mu);
        active = srv->thr_active;
        hqq_plat_mutex_unlock(srv->mu);
        if (active == 0) {
            all_done = 1;
            break;
        }
        now = hqq_plat_mono_ns();
        if (now >= deadline) {
            break;
        }
        (void)hqq_plat_event_wait(srv->evt, deadline - now < 50u * 1000000u ? deadline - now : 50u * 1000000u);
        srv_reap(srv);
    }
    if (!all_done) {
        unsigned stuck = 0;

        srv_reap(srv);
        hqq_plat_mutex_lock(srv->mu);
        {
            hqqrpc_thr *t;

            /* What is stuck in the library cannot be cancelled: detach it (its record stays alive
             * for the thread to mark itself done; destroy refuses, so nothing else is freed). */
            for (t = srv->thrs; t != NULL; t = t->next) {
                if (!t->done && !t->detached) {
                    hqq_plat_thread_detach(t->h);
                    t->detached = 1;
                    stuck++;
                }
            }
        }
        srv->state = SRV_FAILED;
        hqq_plat_mutex_unlock(srv->mu);
        srv_log(srv, HQQRPC_LOG_ERROR, "shutdown timeout: %u threads still running, left detached", stuck);
        return HQQ_ERR_TIMEOUT;
    }
    srv_reap(srv);
    hqq_plat_mutex_lock(srv->mu);
    srv->state = fatal == HQQ_OK ? SRV_DONE : SRV_FAILED;
    hqq_plat_mutex_unlock(srv->mu);
    return fatal;
}

hqq_status hqqrpc_server_destroy(hqqrpc_server *srv) {
    int state;

    if (srv == NULL) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    hqq_plat_mutex_lock(srv->mu);
    state = srv->state;
    hqq_plat_mutex_unlock(srv->mu);
    if (state == SRV_RUNNING || state == SRV_FAILED) {
        return HQQ_ERR_INVALID_OP;
    }
    if (srv->acc != NULL) {
        (void)hqq_acceptor_close(srv->acc);
        srv->acc = NULL;
    }
    srv_free(srv);
    return HQQ_OK;
}

/* ---- hqqrpc_main ---------------------------------------------------------------------------- */

typedef struct main_ctx {
    hqqrpc_server *srv;
#if defined(_WIN32)
    HANDLE done_event;
#else
    sigset_t set;
#endif
    _Atomic int done;      /* run returned: the signal thread only has to exit */
    _Atomic int signo;     /* the signal that stopped the server, 0 if none */
    _Atomic int parent;    /* hqq_lifeline_reason that fired, 0 if none */
} main_ctx;

static void on_parent_gone(void *ctx, hqq_lifeline_reason reason) {
    main_ctx *m = (main_ctx *)ctx;

    atomic_store(&m->parent, (int)reason);
    hqqrpc_server_stop(m->srv);
}

#if defined(_WIN32)

static main_ctx *_Atomic g_win_main_ctx;

static BOOL WINAPI on_console_ctrl(DWORD type) {
    main_ctx *m = atomic_load(&g_win_main_ctx);
    int sig = 0;

    if (m == NULL) {
        return FALSE;
    }
    switch (type) {
    case CTRL_C_EVENT:
        sig = SIGINT;
        break;
    case CTRL_BREAK_EVENT:
        sig = SIGBREAK;
        break;
    case CTRL_CLOSE_EVENT:
    case CTRL_SHUTDOWN_EVENT:
        sig = SIGTERM;
        break;
    default:
        return FALSE;
    }
    if (!atomic_load(&m->done)) {
        int expected = 0;
        (void)atomic_compare_exchange_strong(&m->signo, &expected, sig);
        hqqrpc_server_stop(m->srv);
    }
    if ((type == CTRL_CLOSE_EVENT || type == CTRL_SHUTDOWN_EVENT) && m->done_event != NULL) {
        (void)WaitForSingleObject(m->done_event, 4000u);
    }
    return TRUE;
}

static int stdin_is_terminal(void) {
    DWORD mode = 0;
    HANDLE h = GetStdHandle(STD_INPUT_HANDLE);
    return h != NULL && h != INVALID_HANDLE_VALUE && GetConsoleMode(h, &mode) != 0 ? 1 : 0;
}

#else

static void signal_thread(void *arg) {
    main_ctx *m = (main_ctx *)arg;
    int sig = 0;

    while (sigwait(&m->set, &sig) != 0) {
        /* retry on a spurious error */
    }
    if (!atomic_load(&m->done)) {
        atomic_store(&m->signo, sig);
        hqqrpc_server_stop(m->srv);
    }
}

static int stdin_is_terminal(void) {
    return isatty(STDIN_FILENO);
}

#endif

static void stderr_log(void *user, int level, const char *msg) {
    static const char *const names[] = {"error", "warning", "info"};

    (void)user;
    fprintf(stderr, "hqqrpc: %s: %s\n", level >= 0 && level <= 2 ? names[level] : "log", msg);
}

static void usage(FILE *f, const char *prog) {
    fprintf(f,
            "usage: %s <listener-name> [options]\n"
            "  --perm OCTAL              permission of the listener and session segments (default 0600)\n"
            "  --max-sessions N          concurrent sessions (default 64)\n"
            "  --max-handles N           handle table size per session, at most 65535 (default 4096)\n"
            "  --max-in-flight N         pipelined requests granted per session (default: the buffer count)\n"
            "  --max-message BYTES       largest message body, at most 1073741824 (default 16777216)\n"
            "  --buffer-count N          default session buffer count, a power of two (default 64)\n"
            "  --buffer-size BYTES       default session buffer size, a multiple of 64 (default 16384)\n"
            "  --heartbeat-ms N          link heartbeat interval\n"
            "  --peer-timeout-ms N       link peer timeout (default 2000)\n"
            "  --open-timeout-ms N       bootstrap and HELLO timeout (default 5000)\n"
            "  --shutdown-timeout-ms N   wait for sessions at shutdown (default 5000)\n"
            "  --write-stall-ms N        tear a session down when a response makes no progress (default 10000)\n"
            "  --idle-timeout-ms N       end a session without requests for this long (default: never)\n"
            "  --max-control-links N     concurrent control links (default 16)\n"
            "  --max-segment-bytes N     limit for 2 * buffer count * buffer size (default 67108864)\n"
            "  --copy-request            copy single-frame request bodies before decoding\n"
            "  --spin                    busy-wait on the rings (lowest latency)\n"
            "  --parent-stdin[=force]    shut down gracefully when standard input reaches end-of-file\n"
            "  --lifeline-grace-ms N     forced exit if that shutdown takes longer (default 5000; 0: never)\n"
            "Prints \"ready <listener-name>\" on stdout once the listener exists.\n",
            prog);
}

static int parse_num(const char *s, unsigned long long min, unsigned long long max, int base,
                     unsigned long long *out) {
    char *end = NULL;
    unsigned long long v;

    if (s == NULL || *s == '\0' || *s == '-' || *s == '+') {
        return -1;
    }
    errno = 0;
    v = strtoull(s, &end, base);
    if (errno != 0 || end == s || *end != '\0' || v < min || v > max) {
        return -1;
    }
    *out = v;
    return 0;
}

static int bad_arg(const char *prog, const char *flag, const char *value) {
    fprintf(stderr, "%s: invalid value for %s: %s\n", prog, flag, value != NULL ? value : "(missing)");
    return 2;
}

int hqqrpc_main(const hqqrpc_iface *iface, const hqqrpc_server_options *defaults, int argc, char **argv) {
    const char *prog = argc > 0 && argv != NULL && argv[0] != NULL ? argv[0] : "hqqrpc";
    hqqrpc_server_options o;
    hqqrpc_server *srv = NULL;
    const char *name = NULL;
    main_ctx m;
    hqq_lifeline *ll = NULL;
#if !defined(_WIN32)
    hqq_plat_thread *sigthr = NULL;
    int os_err = 0;
#endif
    uint64_t grace_ms = 5000;
    int lifeline_mode = 0; /* 0: off, 1: --parent-stdin, 2: --parent-stdin=force */
    int i;
    int signo;
    int parent;
    hqq_status st;

#if defined(_WIN32)
    if (_fileno(stdin) >= 0) {
        (void)_setmode(_fileno(stdin), _O_BINARY);
    }
    if (_fileno(stdout) >= 0) {
        (void)_setmode(_fileno(stdout), _O_BINARY);
    }
    if (_fileno(stderr) >= 0) {
        (void)_setmode(_fileno(stderr), _O_BINARY);
    }
#endif

    if (iface == NULL) {
        fprintf(stderr, "%s: no interface\n", prog);
        return 1;
    }
    if (defaults != NULL) {
        if (defaults->struct_size < sizeof o) {
            fprintf(stderr, "%s: server options are too small\n", prog);
            return 1;
        }
        o = *defaults;
    } else {
        hqqrpc_server_options_init(&o);
    }
    if (o.on_log == NULL) {
        o.on_log = stderr_log;
    }

#define HQQRPC_NUM_FLAG(flag, lo, hi, base, stmt)                    \
    else if (strcmp(a, flag) == 0 && has_next) {                     \
        if (parse_num(argv[++i], (lo), (hi), (base), &v) != 0) {     \
            return bad_arg(prog, flag, argv[i]);                     \
        }                                                            \
        stmt;                                                        \
    }

    for (i = 1; i < argc; i++) {
        const char *a = argv[i];
        unsigned long long v = 0;
        int has_next = i + 1 < argc;

        if (strcmp(a, "--help") == 0 || strcmp(a, "-h") == 0) {
            usage(stdout, prog);
            return 0;
        } else if (strcmp(a, "--version") == 0) {
            printf("%s (hqqrpc protocol %u.%u)\n", iface->service != NULL ? iface->service : "hqqrpc",
                   HQQRPC_PROTO_MAJOR, HQQRPC_PROTO_MINOR);
            return 0;
        }
        HQQRPC_NUM_FLAG("--perm", 0, 0777, 8, o.perm = (unsigned)v)
        HQQRPC_NUM_FLAG("--max-sessions", 1, 1u << 20, 10, o.max_sessions = (uint32_t)v)
        HQQRPC_NUM_FLAG("--max-handles", 1, HQQRPC_MAX_HANDLES, 10, o.max_handles = (uint32_t)v)
        HQQRPC_NUM_FLAG("--max-in-flight", 0, HQQRPC_MAX_BUFFER_COUNT, 10, o.max_in_flight = (uint32_t)v)
        HQQRPC_NUM_FLAG("--max-message", HQQRPC_MIN_MAX_MESSAGE, HQQRPC_HARD_MAX_MESSAGE, 10,
                        o.max_message = (uint32_t)v)
        HQQRPC_NUM_FLAG("--buffer-count", 2, HQQRPC_MAX_BUFFER_COUNT, 10, o.default_buffer_count = (uint32_t)v)
        HQQRPC_NUM_FLAG("--buffer-size", HQQRPC_MIN_BUFFER_SIZE, HQQRPC_MAX_BUFFER_SIZE, 10,
                        o.default_buffer_size = (uint32_t)v)
        HQQRPC_NUM_FLAG("--heartbeat-ms", 1, 60000, 10,
                        o.link.heartbeat_interval_ns = (int64_t)v * 1000000;
                        o.acceptor.link.link.heartbeat_interval_ns = (int64_t)v * 1000000)
        HQQRPC_NUM_FLAG("--peer-timeout-ms", 1, 3600000, 10,
                        o.link.peer_timeout_ns = (int64_t)v * 1000000;
                        o.acceptor.link.link.peer_timeout_ns = (int64_t)v * 1000000)
        HQQRPC_NUM_FLAG("--open-timeout-ms", 1, 3600000, 10, o.open_timeout_ns = (int64_t)v * 1000000)
        HQQRPC_NUM_FLAG("--shutdown-timeout-ms", 0, 3600000, 10, o.shutdown_timeout_ns = (int64_t)v * 1000000)
        HQQRPC_NUM_FLAG("--write-stall-ms", 1, 3600000, 10, o.write_stall_timeout_ns = (int64_t)v * 1000000)
        HQQRPC_NUM_FLAG("--idle-timeout-ms", 0, 86400000, 10, o.idle_timeout_ns = (int64_t)v * 1000000)
        HQQRPC_NUM_FLAG("--max-control-links", 1, 1u << 20, 10, o.max_control_links = (uint32_t)v)
        HQQRPC_NUM_FLAG("--max-segment-bytes", 1, UINT64_C(1) << 40, 10, o.max_segment_bytes = (uint64_t)v)
        HQQRPC_NUM_FLAG("--lifeline-grace-ms", 0, 3600000, 10, grace_ms = (uint64_t)v)
        else if (strcmp(a, "--copy-request") == 0) {
            o.copy_request = 1;
        } else if (strcmp(a, "--spin") == 0) {
            o.link.wait_policy = HQQ_WAIT_SPIN;
            o.acceptor.link.link.wait_policy = HQQ_WAIT_SPIN;
        } else if (strcmp(a, "--parent-stdin") == 0 || strcmp(a, "--exit-on-stdin-eof") == 0) {
            lifeline_mode = 1;
        } else if (strcmp(a, "--parent-stdin=force") == 0 || strcmp(a, "--exit-on-stdin-eof=force") == 0) {
            lifeline_mode = 2;
        } else if (a[0] == '-') {
            fprintf(stderr, "%s: unknown or incomplete option: %s\n", prog, a);
            usage(stderr, prog);
            return 2;
        } else if (name == NULL) {
            name = a;
        } else {
            fprintf(stderr, "%s: unexpected argument: %s\n", prog, a);
            return 2;
        }
    }
#undef HQQRPC_NUM_FLAG
    if (name == NULL || name[0] == '\0') {
        usage(stderr, prog);
        return 2;
    }

    memset(&m, 0, sizeof m);
    atomic_init(&m.done, 0);
    atomic_init(&m.signo, 0);
    atomic_init(&m.parent, 0);
#if defined(_WIN32)
    m.done_event = CreateEventW(NULL, TRUE, FALSE, NULL);
    if (m.done_event == NULL) {
        fprintf(stderr, "%s: cannot create the shutdown event\n", prog);
        return 1;
    }
#else
    /* Shutdown signals are taken by one thread with sigwait(): block them before any thread exists. */
    (void)signal(SIGPIPE, SIG_IGN);
    sigemptyset(&m.set);
    sigaddset(&m.set, SIGINT);
    sigaddset(&m.set, SIGTERM);
    if (pthread_sigmask(SIG_BLOCK, &m.set, NULL) != 0) {
        fprintf(stderr, "%s: cannot block the shutdown signals\n", prog);
        return 1;
    }
#endif

    st = hqqrpc_server_create(iface, name, &o, &srv);
    if (st != HQQ_OK) {
        fprintf(stderr, "%s: cannot create the server on %s: %s\n", prog, name, hqq_strerror(st));
#if defined(_WIN32)
        (void)CloseHandle(m.done_event);
#endif
        return 1;
    }
    m.srv = srv;
    printf("ready %s\n", name);
    fflush(stdout);

    if (lifeline_mode != 0) {
        hqq_lifeline_options lo;

        if (lifeline_mode == 1 && stdin_is_terminal()) {
            fprintf(stderr, "%s: --parent-stdin: standard input is a terminal, not watched "
                            "(use --parent-stdin=force to watch it)\n", prog);
        }
        hqq_lifeline_options_init(&lo);
        lo.callback = on_parent_gone;
        lo.ctx = &m;
        lo.force = lifeline_mode == 2;
        lo.grace_ms = grace_ms;
        if (hqq_lifeline_start(&lo, &ll) != HQQ_OK) {
            fprintf(stderr, "%s: cannot watch standard input\n", prog);
            (void)hqqrpc_server_destroy(srv);
#if defined(_WIN32)
            (void)CloseHandle(m.done_event);
#endif
            return 1;
        }
    }
#if defined(_WIN32)
    atomic_store(&g_win_main_ctx, &m);
    (void)SetConsoleCtrlHandler(on_console_ctrl, TRUE);
#else
    sigthr = hqq_plat_thread_start(NULL, signal_thread, &m, &os_err);
    if (sigthr == NULL) {
        fprintf(stderr, "%s: cannot start the signal thread (%d)\n", prog, os_err);
        hqq_lifeline_stop(ll);
        (void)hqqrpc_server_destroy(srv);
        return 1;
    }
#endif

    st = hqqrpc_server_run(srv);

    atomic_store(&m.done, 1);
#if defined(_WIN32)
    (void)SetConsoleCtrlHandler(on_console_ctrl, FALSE);
    atomic_store(&g_win_main_ctx, NULL);
#else
    if (atomic_load(&m.signo) == 0) {
        (void)kill(getpid(), SIGTERM); /* wakes the signal thread; the signal stays blocked elsewhere */
    }
    hqq_plat_thread_join(NULL, sigthr);
#endif
    signo = atomic_load(&m.signo);
    parent = atomic_load(&m.parent);
    if (parent != 0) {
        fprintf(stderr, "%s: parent lifeline (%s): shutting down; shut down\n", prog,
                parent == (int)HQQ_LIFELINE_REASON_EOF ? "standard input closed" : "standard input error");
    } else if (signo != 0) {
        fprintf(stderr, "%s: signal %d: shutting down; shut down\n", prog, signo);
    }
    hqq_lifeline_stop(ll); /* joins the lifeline thread: no callback runs after this */
    if (st != HQQ_OK) {
        fprintf(stderr, "%s: server run failed: %s; not freeing the server, the process must exit\n", prog,
                hqq_strerror(st));
#if defined(_WIN32)
        (void)SetEvent(m.done_event);
        (void)CloseHandle(m.done_event);
#endif
        return 1;
    }
    (void)hqqrpc_server_destroy(srv);
#if defined(_WIN32)
    (void)SetEvent(m.done_event);
    (void)CloseHandle(m.done_event);
#endif
    return 0;
}
