/*
 * interop_adv.c - Advanced and acceptor scenarios of the C interop helper
 * (X-A01 .. X-A10, conformance/INTEROP_CLI.md).
 *
 * Each scenario mirrors its Go reference in cmd/hqq-interop/adv_*.go: same
 * variants, params, steps, roles, expectations and check names. Only the
 * public Advanced and listener API is used (hqq_advanced.h, hqq_listener.h).
 * Raw frames (bad-major and OpError answers, OpCancel, OpConnCreate, aborted
 * chunks) go through the raw producer of hqq_interop.c (ihx_raw_*). The Go
 * goroutines are POSIX threads (Windows: _beginthreadex).
 */
#if defined(__linux__)
#  define _GNU_SOURCE 1
#elif defined(__APPLE__)
#  define _DARWIN_C_SOURCE 1
#endif

#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#if defined(_WIN32)
#  include <process.h>
#  include <windows.h>
#else
#  include <pthread.h>
#  include <time.h>
#endif

#include "hqq/hqq_advanced.h"
#include "hqq/hqq_listener.h"
#include "interop_internal.h"

/* ---- Constants ------------------------------------------------------------ */

#define ADV_NS_PER_MS INT64_C(1000000)
#define ADV_NS_PER_S INT64_C(1000000000)
#define ADV_MS(n) (INT64_C(n) * ADV_NS_PER_MS)
#define ADV_S(n) (INT64_C(n) * ADV_NS_PER_S)

#define ADV_PERM 0600u
#define ADV_RPC_CALLS 16

/* Method selectors (adv_setup.go). */
#define ADV_M_SLEEP_ECHO UINT64_C(1)
#define ADV_M_FAIL UINT64_C(2)
#define ADV_M_SLOW UINT64_C(3)
#define ADV_M_ECHO UINT64_C(4)
#define ADV_M_BIG UINT64_C(5)
#define ADV_M_SINK UINT64_C(6)
#define ADV_M_BLOCK UINT64_C(7)
#define ADV_M_META UINT64_C(8)

/* Wire vocabulary (internal/protocol, PROTOCOL_1_1 draft section 3 and the flag table). */
#define ADV_OP_ERROR UINT64_C(0)
#define ADV_OP_CONN_CREATE UINT64_C(1)
#define ADV_OP_CONN_COPY UINT64_C(4)
#define ADV_OP_PROTO_ACK UINT64_C(6)
#define ADV_OP_CANCEL UINT64_C(7)
#define ADV_FLAG_BEGIN UINT64_C(0x01)
#define ADV_FLAG_ABORT UINT64_C(0x04)

/* Feature sets of the scenarios (Go advNoCancelFeatures, advNoMetaFeatures, advLateFeatures). */
#define ADV_FEATURES_NO_CANCEL (HQQ_FEATURE_MESSAGES | HQQ_FEATURE_REQUEST_RESPONSE | HQQ_FEATURE_METADATA)
#define ADV_FEATURES_NO_META (HQQ_FEATURE_MESSAGES | HQQ_FEATURE_REQUEST_RESPONSE | HQQ_FEATURE_CANCEL)
#define ADV_FEATURES_LATE (HQQ_FEATURE_MESSAGES | HQQ_FEATURE_REQUEST_RESPONSE)

static const char adv_sink_text[] = "sink ran although its request was refused";

/* ---- Threads, locks and events (the Go reference uses goroutines) ---------- */

#if defined(_WIN32)
typedef unsigned(__stdcall *adv_thread_fn)(void *);
#  define ADV_THREAD_FN(name) static unsigned __stdcall name(void *arg)
#  define ADV_THREAD_EXIT 0u
typedef struct adv_thread {
    HANDLE h;
} adv_thread;
typedef struct adv_lock {
    CRITICAL_SECTION cs;
    CONDITION_VARIABLE cv;
} adv_lock;
#else
typedef void *(*adv_thread_fn)(void *);
#  define ADV_THREAD_FN(name) static void *name(void *arg)
#  define ADV_THREAD_EXIT NULL
typedef struct adv_thread {
    pthread_t t;
    int started;
} adv_thread;
typedef struct adv_lock {
    pthread_mutex_t m;
    pthread_cond_t c;
} adv_lock;
#endif

static int thread_start(adv_thread *th, adv_thread_fn fn, void *arg) {
#if defined(_WIN32)
    uintptr_t h = _beginthreadex(NULL, 0, fn, arg, 0, NULL);
    th->h = (HANDLE)h;
    return h == 0 ? -1 : 0;
#else
    if (pthread_create(&th->t, NULL, fn, arg) != 0) {
        return -1;
    }
    th->started = 1;
    return 0;
#endif
}

static void thread_join(adv_thread *th) {
#if defined(_WIN32)
    if (th->h != NULL) {
        WaitForSingleObject(th->h, INFINITE);
        CloseHandle(th->h);
        th->h = NULL;
    }
#else
    if (th->started) {
        (void)pthread_join(th->t, NULL);
        th->started = 0;
    }
#endif
}

static int spawn(ihx_env *e, adv_thread *th, adv_thread_fn fn, void *arg) {
    if (thread_start(th, fn, arg) != 0) {
        return ihx_fail(e, "Unknown", "cannot start a helper thread");
    }
    return IHX_OK;
}

static void lock_init(adv_lock *l) {
#if defined(_WIN32)
    InitializeCriticalSection(&l->cs);
    InitializeConditionVariable(&l->cv);
#else
    (void)pthread_mutex_init(&l->m, NULL);
    (void)pthread_cond_init(&l->c, NULL);
#endif
}

static void lock_fini(adv_lock *l) {
#if defined(_WIN32)
    DeleteCriticalSection(&l->cs);
#else
    (void)pthread_cond_destroy(&l->c);
    (void)pthread_mutex_destroy(&l->m);
#endif
}

static void lock_acquire(adv_lock *l) {
#if defined(_WIN32)
    EnterCriticalSection(&l->cs);
#else
    (void)pthread_mutex_lock(&l->m);
#endif
}

static void lock_release(adv_lock *l) {
#if defined(_WIN32)
    LeaveCriticalSection(&l->cs);
#else
    (void)pthread_mutex_unlock(&l->m);
#endif
}

static void lock_notify(adv_lock *l) {
#if defined(_WIN32)
    WakeAllConditionVariable(&l->cv);
#else
    (void)pthread_cond_broadcast(&l->c);
#endif
}

/* Lock held: waits for a notification or at most ns nanoseconds. */
static void lock_wait(adv_lock *l, int64_t ns) {
#if defined(_WIN32)
    DWORD ms = (DWORD)(ns / ADV_NS_PER_MS);
    if (ms == 0) {
        ms = 1;
    }
    (void)SleepConditionVariableCS(&l->cv, &l->cs, ms);
#else
    struct timespec now;
    struct timespec until;
    int64_t nsec;
    (void)clock_gettime(CLOCK_REALTIME, &now);
    nsec = (int64_t)now.tv_nsec + ns % ADV_NS_PER_S;
    until.tv_sec = now.tv_sec + (time_t)(ns / ADV_NS_PER_S + nsec / ADV_NS_PER_S);
    until.tv_nsec = (long)(nsec % ADV_NS_PER_S);
    (void)pthread_cond_timedwait(&l->c, &l->m, &until);
#endif
}

/* One-shot event with an int value (a Go channel with one send). */
typedef struct adv_ev {
    adv_lock lk;
    int set;
    int value;
} adv_ev;

static void ev_init(adv_ev *ev) {
    memset(ev, 0, sizeof *ev);
    lock_init(&ev->lk);
}

static void ev_fini(adv_ev *ev) {
    lock_fini(&ev->lk);
}

static void ev_set(adv_ev *ev, int value) {
    lock_acquire(&ev->lk);
    ev->set = 1;
    ev->value = value;
    lock_notify(&ev->lk);
    lock_release(&ev->lk);
}

static int ev_is_set(adv_ev *ev) {
    int s;
    lock_acquire(&ev->lk);
    s = ev->set;
    lock_release(&ev->lk);
    return s;
}

/* Waits at most timeout_ns. Returns 1 when set (*value gets its value when value != NULL). */
static int ev_wait(adv_ev *ev, int64_t timeout_ns, int *value) {
    uint64_t until = ihx_now_ns() + (uint64_t)timeout_ns;
    int s;
    lock_acquire(&ev->lk);
    while (!ev->set) {
        uint64_t now = ihx_now_ns();
        if (now >= until) {
            break;
        }
        lock_wait(&ev->lk, (int64_t)(until - now));
    }
    s = ev->set;
    if (s && value != NULL) {
        *value = ev->value;
    }
    lock_release(&ev->lk);
    return s;
}

/* ---- Waits, options, handles ------------------------------------------------ */

/* Blocking bound of a call: the rest of the process deadline (Go e.ctx). */
static hqq_wait wait_left(const ihx_env *e) {
    hqq_wait w;
    int64_t left = ihx_remaining_ns(e);
    w.timeout_ns = left > 0 ? left : 0;
    w.cancel = NULL;
    return w;
}

static hqq_wait wait_ns(int64_t ns) {
    hqq_wait w;
    w.timeout_ns = ns;
    w.cancel = NULL;
    return w;
}

static uint64_t cfg_size(const ihx_config *c) {
    return c->size != 0 ? c->size : c->buffer_size;
}

/* MaxMessageSize in force (advMaxMessage). */
static uint64_t adv_max_message(const ihx_config *c) {
    return c->max_message != 0 ? c->max_message : HQQ_ADV_DEFAULT_MAX_MESSAGE_SIZE;
}

/* Go advOptions: the AdvancedOptions of this process. */
static void adv_options_fill(const ihx_config *c, hqq_adv_options *o) {
    hqq_adv_options_init(o);
    hqq_options_init(&o->link);
    o->max_message_size = c->max_message;
    o->max_reassembly_bytes = c->max_reassembly;
    o->queue_size = c->queue_size;
    o->link.wait_policy = c->spin ? HQQ_WAIT_SPIN : HQQ_WAIT_ADAPTIVE;
    o->link.heartbeat_interval_ns = c->heartbeat_ns;
    o->link.peer_timeout_ns = c->peer_timeout_ns;
    o->link.attach_timeout_ns = c->attach_timeout_ns;
}

static void close_link(hqq_adv **link) {
    if (*link != NULL) {
        (void)hqq_adv_close(*link);
        *link = NULL;
    }
}

static hqq_adv_stats adv_stats(const hqq_adv *link) {
    hqq_adv_stats s;
    hqq_adv_stats_get(link, &s);
    return s;
}

/* Records an error response (Go ResponseError) in meta.status of the result. */
static void note_response(ihx_env *e, hqq_status rc, const hqq_msg *m) {
    if (rc == HQQ_ERR_RESPONSE && m != NULL) {
        e->meta_status_set = 1;
        e->meta_status = hqq_msg_status(m);
    }
}

/* True when a call returned an error response; *status gets its status. */
static int resp_status(hqq_status rc, const hqq_msg *m, uint64_t *status) {
    if (rc != HQQ_ERR_RESPONSE || m == NULL) {
        return 0;
    }
    *status = hqq_msg_status(m);
    return 1;
}

/* The message carries exactly the n bytes of s. */
static int msg_is(const hqq_msg *m, const void *s, size_t n) {
    return hqq_msg_len(m) == n && (n == 0 || memcmp(hqq_msg_data(m), s, n) == 0);
}

static uint8_t *make_payload(uint64_t seed, uint64_t seq, size_t n) {
    uint8_t *p = malloc(n != 0 ? n : 1);
    if (p != NULL) {
        ihx_payload(seed, seq, p, n);
    }
    return p;
}

/* Go advEstablish: the creator creates the named link, both roles meet at barrier "linked". */
static int adv_establish(ihx_env *e, hqq_adv **out) {
    const ihx_config *c = &e->cfg;
    hqq_adv_options o;
    hqq_open_info info;
    hqq_status st;
    int rc;
    adv_options_fill(c, &o);
    hqq_open_info_init(&info);
    *out = NULL;
    if (c->role == 0) {
        st = hqq_adv_create(c->name, c->buffer_count, c->buffer_size, ADV_PERM, &o, out, &info);
        if (st != HQQ_OK) {
            return ihx_fail_status(e, st, "create advanced link %s", c->name);
        }
        rc = ihx_barrier(e, "linked");
        if (rc != IHX_OK) {
            close_link(out);
        }
        return rc;
    }
    rc = ihx_barrier(e, "linked");
    if (rc != IHX_OK) {
        return rc;
    }
    st = hqq_adv_open(c->name, c->buffer_count, c->buffer_size, &o, out, &info);
    if (st != HQQ_OK) {
        return ihx_fail_status(e, st, "open advanced link %s", c->name);
    }
    return IHX_OK;
}

static int negotiate(ihx_env *e, hqq_adv *link, uint64_t features) {
    hqq_wait w = wait_left(e);
    hqq_status st = hqq_adv_negotiate(link, features, &w);
    if (st != HQQ_OK) {
        return ihx_fail_status(e, st, "negotiate");
    }
    return IHX_OK;
}

/* Sends Payload(seed, seq, n) as one message on s (Go Send). */
static int send_payload(ihx_env *e, hqq_stream *s, uint64_t seed, uint64_t seq, uint64_t n) {
    uint8_t *p = make_payload(seed, seq, (size_t)n);
    hqq_wait w = wait_left(e);
    hqq_status st;
    if (p == NULL) {
        return ihx_fail(e, "Unknown", "out of memory");
    }
    st = hqq_stream_send(s, p, (size_t)n, &w);
    free(p);
    if (st != HQQ_OK) {
        return ihx_fail_status(e, st, "send %llu bytes (message %llu)", (unsigned long long)n,
                               (unsigned long long)seq);
    }
    return IHX_OK;
}

/* Go advRecvCheck / advStreamRecvCheck: receives one message and checks its size and payload. */
static int recv_check(ihx_env *e, hqq_stream *s, uint64_t seed, uint64_t seq, uint64_t size, int on_stream) {
    hqq_msg *m = NULL;
    hqq_wait w = wait_left(e);
    hqq_status st = hqq_stream_recv(s, &m, &w);
    char name[IHX_NAME_MAX];
    size_t len;
    const uint8_t *d;
    int ok;
    if (st != HQQ_OK) {
        return ihx_fail_status(e, st, "receive message %llu", (unsigned long long)seq);
    }
    len = hqq_msg_len(m);
    d = hqq_msg_data(m);
    ok = (uint64_t)len == size && ihx_payload_verify(seed, seq, d != NULL ? d : (const uint8_t *)"", len) == 0;
    snprintf(name, sizeof name, "%smessage %llu has %llu bytes and is intact", on_stream ? "stream " : "",
             (unsigned long long)seq, (unsigned long long)size);
    ihx_check(e, name, ok, "len=%llu", (unsigned long long)len);
    hqq_msg_release(m);
    return IHX_OK;
}

/* ---- Serve loops ------------------------------------------------------------- */

typedef struct adv_serve {
    adv_thread th;
    hqq_adv *link;
    hqq_handler_fn fn;
    void *user;
    hqq_status st;
} adv_serve;

ADV_THREAD_FN(serve_main) {
    adv_serve *s = (adv_serve *)arg;
    s->st = hqq_adv_serve(s->link, s->fn, s->user, NULL);
    return ADV_THREAD_EXIT;
}

/* Go advServeAsync: Serve runs until the link closes (joined by serve_join after close). */
static int serve_start(ihx_env *e, adv_serve *s, hqq_adv *link, hqq_handler_fn fn, void *user) {
    s->link = link;
    s->fn = fn;
    s->user = user;
    s->st = HQQ_OK;
    return spawn(e, &s->th, serve_main, s);
}

static hqq_status serve_join(adv_serve *s) {
    thread_join(&s->th);
    return s->st;
}

/* ---- Handlers ----------------------------------------------------------------- */

static void reply_echo(hqq_response *w, const hqq_request *req) {
    (void)hqq_response_write(w, hqq_request_data(req), hqq_request_len(req));
}

/* Default mux of the scenarios: echo; any other method is the status-6 refusal. */
static void h_echo(void *user, hqq_req_ctx *ctx, hqq_response *w, const hqq_request *req) {
    (void)user;
    (void)ctx;
    if (hqq_request_method(req) == ADV_M_ECHO) {
        reply_echo(w, req);
        return;
    }
    (void)hqq_response_error(w, HQQ_ERRCODE_INVALID_OP, NULL, 0);
}

static uint32_t be32_get(const uint8_t *p) {
    return ((uint32_t)p[0] << 24) | ((uint32_t)p[1] << 16) | ((uint32_t)p[2] << 8) | (uint32_t)p[3];
}

static void be32_put(uint8_t *p, uint32_t v) {
    p[0] = (uint8_t)(v >> 24);
    p[1] = (uint8_t)(v >> 16);
    p[2] = (uint8_t)(v >> 8);
    p[3] = (uint8_t)v;
}

/* X-A04 mux: sleep-echo, error response, slow answer, echo. */
static void h_rpc(void *user, hqq_req_ctx *ctx, hqq_response *w, const hqq_request *req) {
    (void)user;
    switch (hqq_request_method(req)) {
    case ADV_M_SLEEP_ECHO: {
        const uint8_t *p = hqq_request_data(req);
        int64_t k = ADV_RPC_CALLS;
        if (hqq_request_len(req) >= 4) {
            k -= (int64_t)be32_get(p);
        }
        if (k < 0) {
            k = 0;
        }
        /* Inverse sleep: later ids finish first. */
        (void)hqq_req_ctx_wait_done(ctx, k * ADV_MS(5));
        reply_echo(w, req);
        return;
    }
    case ADV_M_FAIL:
        (void)hqq_response_error(w, 42, "boom payload", 12);
        return;
    case ADV_M_SLOW:
        (void)hqq_req_ctx_wait_done(ctx, ADV_MS(300));
        (void)hqq_response_write(w, "late", 4);
        return;
    default:
        h_echo(NULL, ctx, w, req);
        return;
    }
}

/* X-A07 blocking handler: its outcome is reported on `blocked`. */
typedef struct a07_state {
    adv_ev blocked;
    int64_t block_ns;
} a07_state;

static void h_a07(void *user, hqq_req_ctx *ctx, hqq_response *w, const hqq_request *req) {
    a07_state *st = (a07_state *)user;
    int canceled;
    if (hqq_request_method(req) != ADV_M_BLOCK) {
        h_echo(NULL, ctx, w, req);
        return;
    }
    if (hqq_req_ctx_wait_done(ctx, st->block_ns) == HQQ_OK) {
        canceled = hqq_req_ctx_err(ctx) == HQQ_ERR_CANCELED;
    } else {
        canceled = hqq_req_ctx_err(ctx) != HQQ_OK;
    }
    ev_set(&st->blocked, canceled);
}

/* X-A09 responder: A blocks until released, B is echoed. */
typedef struct a09_state {
    adv_ev started;
    adv_ev release;
} a09_state;

static void h_a09(void *user, hqq_req_ctx *ctx, hqq_response *w, const hqq_request *req) {
    a09_state *st = (a09_state *)user;
    if (hqq_request_method(req) != ADV_M_BLOCK) {
        h_echo(NULL, ctx, w, req);
        return;
    }
    ev_set(&st->started, 1);
    (void)ev_wait(&st->release, ADV_S(10), NULL);
    (void)hqq_response_write(w, "A-done", 6);
}

/* X-A08 recorder: what the handler saw of each request. */
typedef struct a08_seen {
    uint64_t caller;
    uint8_t trace[16];
    int has_dl;
    int64_t remaining;
} a08_seen;

typedef struct a08_state {
    adv_lock lk;
    a08_seen seen[8];
    size_t n;
} a08_state;

static void h_a08(void *user, hqq_req_ctx *ctx, hqq_response *w, const hqq_request *req) {
    a08_state *st = (a08_state *)user;
    a08_seen rec;
    uint64_t mono = 0;
    if (hqq_request_method(req) != ADV_M_META) {
        h_echo(NULL, ctx, w, req);
        return;
    }
    memset(&rec, 0, sizeof rec);
    rec.caller = hqq_request_caller(req);
    (void)hqq_request_trace(req, rec.trace);
    if (hqq_req_ctx_deadline(ctx, &mono)) {
        rec.has_dl = 1;
        rec.remaining = (int64_t)(mono - ihx_now_ns());
    }
    lock_acquire(&st->lk);
    if (st->n < sizeof st->seen / sizeof st->seen[0]) {
        st->seen[st->n++] = rec;
    }
    lock_release(&st->lk);
    reply_echo(w, req);
}

/* X-A03 handler: a big response, the refused-request sink, echo. */
typedef struct a03_state {
    uint64_t seed;
    uint64_t big;
} a03_state;

static void h_a03(void *user, hqq_req_ctx *ctx, hqq_response *w, const hqq_request *req) {
    a03_state *st = (a03_state *)user;
    switch (hqq_request_method(req)) {
    case ADV_M_BIG: {
        uint8_t *p = make_payload(st->seed, 99, (size_t)st->big);
        if (p != NULL) {
            (void)hqq_response_write(w, p, (size_t)st->big);
            free(p);
        }
        return;
    }
    case ADV_M_SINK:
        (void)hqq_response_write(w, adv_sink_text, sizeof adv_sink_text - 1);
        return;
    default:
        h_echo(NULL, ctx, w, req);
        return;
    }
}

/* ---- X-A01 negotiation ---------------------------------------------------------- */

static int a01_negotiate(ihx_env *e, hqq_adv *link, uint64_t *want) {
    const char *v = e->cfg.variant;
    int creator = e->cfg.role == 0;
    hqq_wait w = wait_left(e);
    hqq_status st;
    if (strcmp(v, "all-features") == 0) {
        *want = HQQ_FEATURES_ALL;
        st = hqq_adv_negotiate(link, 0, &w);
    } else if (strcmp(v, "opener-initiates") == 0) {
        *want = HQQ_FEATURES_ALL;
        st = creator ? hqq_adv_negotiate(link, HQQ_FEATURES_ALL, &w)
                     : hqq_adv_negotiate_protocol(link, HQQ_PROTOCOL_1_1, HQQ_FEATURES_ALL, &w);
    } else if (strcmp(v, "simultaneous") == 0) {
        *want = HQQ_FEATURES_ALL;
        st = hqq_adv_negotiate_protocol(link, HQQ_PROTOCOL_1_1, HQQ_FEATURES_ALL, &w);
    } else if (strcmp(v, "messages-only") == 0) {
        *want = HQQ_FEATURE_MESSAGES;
        st = hqq_adv_negotiate(link, HQQ_FEATURE_MESSAGES, &w);
    } else if (strcmp(v, "request-response-only") == 0) {
        *want = HQQ_FEATURE_REQUEST_RESPONSE;
        st = hqq_adv_negotiate(link, HQQ_FEATURE_REQUEST_RESPONSE, &w);
    } else if (strcmp(v, "none") == 0) {
        /* The creator requests the empty set explicitly; the opener answers. */
        *want = 0;
        st = creator ? hqq_adv_negotiate_protocol(link, HQQ_PROTOCOL_1_1, 0, &w)
                     : hqq_adv_negotiate(link, 0, &w);
    } else {
        return ihx_fail(e, "CheckFailed", "unknown variant %s", v);
    }
    if (st != HQQ_OK) {
        return ihx_fail_status(e, st, "negotiate (%s)", v);
    }
    return IHX_OK;
}

/* Go advNegoExchange: Send needs FeatureMessages, Call and Serve need FeatureRequestResponse. */
static int a01_exchange(ihx_env *e, hqq_adv *link, uint64_t want, adv_serve *sv) {
    hqq_stream *s = hqq_adv_stream0(link);
    hqq_wait w = wait_left(e);
    uint64_t seed = e->cfg.seed;
    uint64_t size = cfg_size(&e->cfg);
    int rc = IHX_OK;
    if (e->cfg.role == 0) {
        hqq_msg *m = NULL;
        hqq_status st;
        if ((want & HQQ_FEATURE_MESSAGES) != 0) {
            uint8_t *p = make_payload(seed, 0, (size_t)size);
            if (p == NULL) {
                return ihx_fail(e, "Unknown", "out of memory");
            }
            st = hqq_stream_send(s, p, (size_t)size, &w);
            free(p);
            ihx_check_status(e, "send with FeatureMessages", st, HQQ_OK);
        } else {
            st = hqq_stream_send(s, "x", 1, &w);
            ihx_check_status(e, "send without FeatureMessages is ErrNotNegotiated", st, HQQ_ERR_NOT_NEGOTIATED);
        }
        st = hqq_stream_call(s, ADV_M_ECHO, "ping", 4, &m, &w);
        if ((want & HQQ_FEATURE_REQUEST_RESPONSE) != 0) {
            ihx_check(e, "call with FeatureRequestResponse is echoed", st == HQQ_OK && msg_is(m, "ping", 4), "%s",
                      hqq_strerror(st));
        } else {
            ihx_check_status(e, "call without FeatureRequestResponse is ErrNotNegotiated", st,
                             HQQ_ERR_NOT_NEGOTIATED);
        }
        hqq_msg_release(m);
        return IHX_OK;
    }
    if ((want & HQQ_FEATURE_MESSAGES) != 0) {
        rc = recv_check(e, s, seed, 0, size, 0);
        if (rc != IHX_OK) {
            return rc;
        }
    }
    if ((want & HQQ_FEATURE_REQUEST_RESPONSE) != 0) {
        rc = serve_start(e, sv, link, h_echo, NULL);
    }
    return rc;
}

/* Go advNegotiationRejected: the opener answers with a frame the API cannot produce. */
static int a01_rejected(ihx_env *e) {
    hqq_adv *link = NULL;
    int rc = adv_establish(e, &link);
    if (rc != IHX_OK) {
        return rc;
    }
    if (e->cfg.role == 1) {
        ihx_raw *raw = NULL;
        uint64_t words[8] = {0};
        rc = ihx_raw_open(e, &raw);
        if (rc == IHX_OK) {
            if (strcmp(e->cfg.variant, "bad-major") == 0) {
                /* Version 2.0 in operand 0: major 2 is not accepted. */
                words[0] = ADV_OP_PROTO_ACK;
                words[1] = 0x0200;
                words[2] = HQQ_FEATURES_ALL;
            } else {
                words[0] = ADV_OP_ERROR;
                words[2] = HQQ_ERRCODE_INVALID_OP;
            }
            rc = ihx_raw_put(e, raw, words, NULL, 0);
            if (rc == IHX_OK) {
                rc = ihx_barrier(e, "end");
            }
            ihx_raw_close(raw);
        }
        close_link(&link);
        return rc;
    }
    {
        hqq_wait w = wait_left(e);
        hqq_status st = hqq_adv_negotiate(link, 0, &w);
        hqq_status st2;
        ihx_check_status(e, "negotiation rejected by the peer's answer", st, HQQ_ERR_NEGOTIATION_REJECTED);
        ihx_check(e, "protocol state is Failed", hqq_adv_protocol_state(link) == HQQ_PROTO_FAILED, "state %d",
                  (int)hqq_adv_protocol_state(link));
        st2 = hqq_adv_negotiate(link, 0, &w);
        ihx_check_status(e, "Failed is terminal: next negotiation is ErrNegotiationFailed", st2,
                         HQQ_ERR_NEGOTIATION_FAILED);
    }
    rc = ihx_barrier(e, "end");
    close_link(&link);
    return rc;
}

static int run_a01(ihx_env *e) {
    const char *v = e->cfg.variant;
    hqq_adv *link = NULL;
    adv_serve sv;
    uint64_t want = 0;
    int rc;
    if (strcmp(v, "bad-major") == 0 || strcmp(v, "op-error") == 0) {
        return a01_rejected(e);
    }
    memset(&sv, 0, sizeof sv);
    rc = adv_establish(e, &link);
    if (rc != IHX_OK) {
        return rc;
    }
    rc = a01_negotiate(e, link, &want);
    if (rc != IHX_OK) {
        goto out;
    }
    ihx_check(e, "negotiated version is 1.1", hqq_adv_negotiated_version(link) == HQQ_PROTOCOL_1_1,
              "version %u", (unsigned)hqq_adv_negotiated_version(link));
    ihx_check(e, "negotiated features match the request", hqq_adv_negotiated_features(link) == want,
              "got %#llx want %#llx", (unsigned long long)hqq_adv_negotiated_features(link),
              (unsigned long long)want);
    ihx_check(e, "protocol state is Negotiated", hqq_adv_protocol_state(link) == HQQ_PROTO_NEGOTIATED,
              "state %d", (int)hqq_adv_protocol_state(link));
    rc = a01_exchange(e, link, want, &sv);
    if (rc != IHX_OK) {
        goto out;
    }
    rc = ihx_barrier(e, "end");
out:
    close_link(&link);
    (void)serve_join(&sv);
    return rc;
}

/* ---- X-A02 message sizes ---------------------------------------------------------- */

/* Writes the first chunk of a 3 x bufferSize+7 message and its abort frame (raw). */
static int a02_send_aborted_chunk(ihx_env *e) {
    const uint64_t msg_id = UINT64_C(1) << 40;
    uint64_t bs = e->cfg.buffer_size;
    uint64_t total = 3 * bs + 7;
    uint64_t first[8] = {ADV_OP_CONN_COPY, 0, msg_id, total, 0, bs, ADV_FLAG_BEGIN, 0};
    uint64_t abort_w[8] = {ADV_OP_CONN_COPY, 0, msg_id, 0, 0, 0, ADV_FLAG_ABORT, 0};
    ihx_raw *raw = NULL;
    uint8_t *p;
    int rc = ihx_raw_open(e, &raw);
    if (rc != IHX_OK) {
        return rc;
    }
    p = make_payload(e->cfg.seed, UINT64_C(1) << 20, (size_t)bs);
    if (p == NULL) {
        ihx_raw_close(raw);
        return ihx_fail(e, "Unknown", "out of memory");
    }
    rc = ihx_raw_put(e, raw, first, p, (size_t)bs);
    free(p);
    if (rc == IHX_OK) {
        rc = ihx_raw_put(e, raw, abort_w, NULL, 0);
    }
    ihx_raw_close(raw);
    return rc;
}

static int run_a02(ihx_env *e) {
    hqq_adv *link = NULL;
    hqq_stream *s;
    uint64_t bs = e->cfg.buffer_size;
    uint64_t max_size = adv_max_message(&e->cfg);
    uint64_t sizes_full[6] = {0, 1, bs, bs + 1, 3 * bs + 7, max_size};
    uint64_t sizes_abort[1] = {3 * bs + 7};
    const uint64_t *sizes = sizes_full;
    size_t n = 6;
    int abort_variant = strcmp(e->cfg.variant, "sender-abort-after-chunk-1") == 0;
    uint64_t rounds = e->cfg.long_sizes ? 20 : 1;
    uint64_t seed = e->cfg.seed;
    uint64_t seq = 0;
    uint64_t r;
    size_t i;
    int rc;
    if (abort_variant) {
        sizes = sizes_abort;
        n = 1;
    }
    rc = adv_establish(e, &link);
    if (rc != IHX_OK) {
        return rc;
    }
    s = hqq_adv_stream0(link);
    rc = negotiate(e, link, 0);
    if (rc != IHX_OK) {
        goto out;
    }
    if (e->cfg.role == 0) {
        if (abort_variant) {
            rc = a02_send_aborted_chunk(e);
            if (rc != IHX_OK) {
                goto out;
            }
        }
        for (r = 0; r < rounds && rc == IHX_OK; r++) {
            for (i = 0; i < n && rc == IHX_OK; i++) {
                rc = send_payload(e, s, seed, seq, sizes[i]);
                seq++;
            }
        }
        if (rc != IHX_OK) {
            goto out;
        }
        e->sent = seq;
        {
            uint8_t *big = calloc((size_t)max_size + 1, 1);
            hqq_wait w = wait_left(e);
            hqq_status st;
            if (big == NULL) {
                rc = ihx_fail(e, "Unknown", "out of memory");
                goto out;
            }
            st = hqq_stream_send(s, big, (size_t)max_size + 1, &w);
            free(big);
            ihx_check_status(e, "MaxMessageSize+1 is refused locally", st, HQQ_ERR_BUFFER_OVERFLOW);
        }
        /* Nothing of the refused message was sent: the sentinel takes its sequence number. */
        rc = send_payload(e, s, seed, seq, 1);
        if (rc != IHX_OK) {
            goto out;
        }
        e->sent = seq + 1;
    } else {
        for (r = 0; r < rounds && rc == IHX_OK; r++) {
            for (i = 0; i < n && rc == IHX_OK; i++) {
                rc = recv_check(e, s, seed, seq, sizes[i], 0);
                seq++;
            }
        }
        if (rc != IHX_OK) {
            goto out;
        }
        rc = recv_check(e, s, seed, seq, 1, 0);
        if (rc != IHX_OK) {
            goto out;
        }
        e->received = seq + 1;
    }
    rc = ihx_barrier(e, "end");
out:
    close_link(&link);
    return rc;
}

/* ---- X-A03 reassembly limit -------------------------------------------------------- */

static int run_a03(ihx_env *e) {
    hqq_adv *link = NULL;
    adv_serve sv;
    a03_state hs;
    hqq_stream *s;
    uint64_t limit = e->cfg.max_reassembly;
    uint64_t big;
    uint64_t small;
    uint64_t seed = e->cfg.seed;
    int rc;
    memset(&sv, 0, sizeof sv);
    if (limit == 0) {
        return ihx_fail(e, "CheckFailed", "X-A03 needs max_reassembly (params)");
    }
    big = limit + limit / 2;
    small = limit / 2;
    rc = adv_establish(e, &link);
    if (rc != IHX_OK) {
        return rc;
    }
    s = hqq_adv_stream0(link);
    rc = negotiate(e, link, 0);
    if (rc != IHX_OK) {
        goto out;
    }
    if (e->cfg.role == 0) {
        hqq_msg *m = NULL;
        hqq_wait w = wait_left(e);
        hqq_status st;
        uint64_t status = 0;
        uint8_t *p;
        uint64_t i;
        /* Data messages refused by the opener's limit: dropped there. */
        for (i = 0; i < 3 && rc == IHX_OK; i++) {
            rc = send_payload(e, s, seed, i, big);
        }
        if (rc != IHX_OK) {
            goto out;
        }
        /* Accepted: it fits the limit on its own. */
        rc = send_payload(e, s, seed, 3, small);
        if (rc != IHX_OK) {
            goto out;
        }
        /* Request refused by the opener's limit: an error response, status 7. */
        p = make_payload(seed, 4, (size_t)big);
        if (p == NULL) {
            rc = ihx_fail(e, "Unknown", "out of memory");
            goto out;
        }
        st = hqq_stream_call(s, ADV_M_SINK, p, (size_t)big, &m, &w);
        free(p);
        ihx_check(e, "refused request is answered with status 7 (ErrCodeBufferOverflow)",
                  resp_status(st, m, &status) && status == HQQ_ERRCODE_BUFFER_OVERFLOW, "%s", hqq_strerror(st));
        hqq_msg_release(m);

        m = NULL;
        st = hqq_stream_call(s, ADV_M_ECHO, "ping", 4, &m, &w);
        ihx_check(e, "request within the limit is answered", st == HQQ_OK && msg_is(m, "ping", 4), "%s",
                  hqq_strerror(st));
        hqq_msg_release(m);

        /* Response refused by the creator's own limit: the call fails. */
        m = NULL;
        st = hqq_stream_call(s, ADV_M_BIG, "big", 3, &m, &w);
        ihx_check_status(e, "refused response fails the waiting call with ErrReassemblyLimit", st,
                         HQQ_ERR_REASSEMBLY_LIMIT);
        hqq_msg_release(m);

        m = NULL;
        st = hqq_stream_call(s, ADV_M_ECHO, "again", 5, &m, &w);
        ihx_check(e, "link survives the refused response", st == HQQ_OK && msg_is(m, "again", 5), "%s",
                  hqq_strerror(st));
        hqq_msg_release(m);

        {
            hqq_adv_stats stt = adv_stats(link);
            ihx_check(e, "refused response is counted in DroppedMessages", stt.dropped_messages >= 1,
                      "dropped %llu", (unsigned long long)stt.dropped_messages);
        }
        e->sent = 5;
    } else {
        a03_state *hsp = &hs;
        hsp->seed = seed;
        hsp->big = big;
        rc = serve_start(e, &sv, link, h_a03, hsp);
        if (rc != IHX_OK) {
            goto out;
        }
        /* The three refused data messages never surface; the accepted one is first. */
        rc = recv_check(e, s, seed, 3, small, 0);
        if (rc != IHX_OK) {
            goto out;
        }
        e->received = 1;
        rc = ihx_barrier(e, "end");
        if (rc != IHX_OK) {
            goto out;
        }
        {
            hqq_adv_stats stt = adv_stats(link);
            /* Three refused data messages and one refused request. */
            ihx_check(e, "refused data and request are counted in DroppedMessages", stt.dropped_messages >= 4,
                      "dropped %llu", (unsigned long long)stt.dropped_messages);
        }
        goto out;
    }
    rc = ihx_barrier(e, "end");
out:
    close_link(&link);
    (void)serve_join(&sv);
    return rc;
}

/* ---- X-A04 request/response ---------------------------------------------------------- */

typedef struct rpc_shared {
    adv_lock lk;
    int order[ADV_RPC_CALLS];
    int norder;
} rpc_shared;

typedef struct rpc_worker {
    adv_thread th;
    hqq_stream *s;
    hqq_wait w;
    int id;
    rpc_shared *sh;
    hqq_status st;
    int bad;
} rpc_worker;

ADV_THREAD_FN(rpc_worker_main) {
    rpc_worker *k = (rpc_worker *)arg;
    uint8_t p[4];
    hqq_msg *m = NULL;
    be32_put(p, (uint32_t)k->id);
    k->st = hqq_stream_call(k->s, ADV_M_SLEEP_ECHO, p, sizeof p, &m, &k->w);
    if (k->st == HQQ_OK) {
        k->bad = !msg_is(m, p, sizeof p);
    }
    hqq_msg_release(m);
    lock_acquire(&k->sh->lk);
    k->sh->order[k->sh->norder++] = k->id;
    lock_release(&k->sh->lk);
    return ADV_THREAD_EXIT;
}

/* Creator side of X-A04: concurrent calls, error payload, timeout, abandoned response, unknown method. */
static int a04_creator(ihx_env *e, hqq_adv *link) {
    hqq_stream *s = hqq_adv_stream0(link);
    rpc_shared sh;
    rpc_worker wk[ADV_RPC_CALLS];
    int i;
    int started = 0;
    int rc = IHX_OK;
    hqq_status first = HQQ_OK;
    int bad = 0;
    int sorted = 1;
    hqq_msg *m = NULL;
    hqq_wait w = wait_left(e);
    hqq_status st;
    hqq_adv_stats stt;
    uint64_t t0;

    memset(&sh, 0, sizeof sh);
    memset(wk, 0, sizeof wk);
    lock_init(&sh.lk);
    for (i = 0; i < ADV_RPC_CALLS; i++) {
        wk[i].s = s;
        wk[i].w = w;
        wk[i].id = i;
        wk[i].sh = &sh;
        rc = spawn(e, &wk[i].th, rpc_worker_main, &wk[i]);
        if (rc != IHX_OK) {
            break;
        }
        started++;
    }
    for (i = 0; i < started; i++) {
        thread_join(&wk[i].th);
    }
    lock_fini(&sh.lk);
    if (rc != IHX_OK) {
        return rc;
    }
    for (i = 0; i < ADV_RPC_CALLS; i++) {
        if (wk[i].st != HQQ_OK) {
            if (first == HQQ_OK) {
                first = wk[i].st;
            }
        } else if (wk[i].bad) {
            bad = 1;
        }
    }
    ihx_check(e, "all concurrent calls are answered with their own payload", first == HQQ_OK && !bad, "%s",
              first != HQQ_OK ? hqq_strerror(first) : (bad ? "echo mismatch" : ""));
    for (i = 1; i < sh.norder; i++) {
        if (sh.order[i] < sh.order[i - 1]) {
            sorted = 0;
        }
    }
    ihx_check(e, "responses complete out of order", !sorted, "%s", "");
    e->sent = ADV_RPC_CALLS;

    /* Error response with a payload. */
    st = hqq_stream_call(s, ADV_M_FAIL, "", 0, &m, &w);
    {
        uint64_t status = 0;
        ihx_check(e, "error response carries status 42 and its payload",
                  resp_status(st, m, &status) && status == 42 && msg_is(m, "boom payload", 12), "%s",
                  hqq_strerror(st));
    }
    hqq_msg_release(m);

    /* Cancelled call (50 ms): its late response is discarded later. */
    m = NULL;
    {
        hqq_wait cw = wait_ns(ADV_MS(50));
        st = hqq_stream_call(s, ADV_M_SLOW, "slow", 4, &m, &cw);
    }
    ihx_check_status(e, "cancelled call returns its context error", st, HQQ_ERR_TIMEOUT);
    hqq_msg_release(m);

    /* Receiving routes the late response; pump calls until it is discarded. */
    t0 = ihx_now_ns();
    for (;;) {
        stt = adv_stats(link);
        if (stt.abandoned_responses != 0 || ihx_now_ns() - t0 >= (uint64_t)ADV_S(3)) {
            break;
        }
        m = NULL;
        st = hqq_stream_call(s, ADV_M_ECHO, "pump", 4, &m, &w);
        note_response(e, st, m);
        hqq_msg_release(m);
        if (st != HQQ_OK) {
            return ihx_fail_status(e, st, "pump call");
        }
    }
    stt = adv_stats(link);
    ihx_check(e, "late response of the cancelled call is discarded (AbandonedResponses)",
              stt.abandoned_responses >= 1, "abandoned %llu", (unsigned long long)stt.abandoned_responses);

    /* Unknown method: the default error response (status 6). */
    m = NULL;
    st = hqq_stream_call(s, 99, "", 0, &m, &w);
    {
        uint64_t status = 0;
        ihx_check(e, "unknown method is answered with status 6 (ErrCodeInvalidOp)",
                  resp_status(st, m, &status) && status == HQQ_ERRCODE_INVALID_OP, "%s", hqq_strerror(st));
    }
    hqq_msg_release(m);
    return IHX_OK;
}

static int run_a04(ihx_env *e) {
    hqq_adv *link = NULL;
    adv_serve sv;
    int rc;
    memset(&sv, 0, sizeof sv);
    rc = adv_establish(e, &link);
    if (rc != IHX_OK) {
        return rc;
    }
    rc = negotiate(e, link, ADV_FEATURES_LATE);
    if (rc != IHX_OK) {
        goto out;
    }
    if (e->cfg.role == 1) {
        rc = serve_start(e, &sv, link, h_rpc, NULL);
        if (rc == IHX_OK) {
            rc = ihx_barrier(e, "end");
        }
        goto out;
    }
    rc = a04_creator(e, link);
    if (rc == IHX_OK) {
        rc = ihx_barrier(e, "end");
    }
out:
    close_link(&link);
    (void)serve_join(&sv);
    return rc;
}

/* ---- X-A05 streams ----------------------------------------------------------------- */

static int a05_basic_creator(ihx_env *e, hqq_adv *link) {
    hqq_stream *s0 = hqq_adv_stream0(link);
    hqq_stream *st = NULL;
    hqq_stream *st2 = NULL;
    hqq_stream *refused = NULL;
    hqq_wait w = wait_left(e);
    hqq_status rc;
    uint64_t seed = e->cfg.seed;
    uint64_t size = cfg_size(&e->cfg);
    int ret;

    ret = ihx_barrier(e, "p1");
    if (ret != IHX_OK) {
        return ret;
    }
    rc = hqq_adv_open_stream(link, &refused, &w);
    ihx_check_status(e, "OpenStream refused while the peer does not listen is ErrConnNotFound", rc,
                     HQQ_ERR_CONN_NOT_FOUND);
    if (rc == HQQ_OK) {
        (void)hqq_stream_close(refused);
    }
    ret = ihx_barrier(e, "p2");
    if (ret != IHX_OK) {
        return ret;
    }
    ret = ihx_barrier(e, "p3");
    if (ret != IHX_OK) {
        return ret;
    }

    rc = hqq_adv_open_stream(link, &st, &w);
    if (rc != HQQ_OK) {
        return ihx_fail_status(e, rc, "OpenStream");
    }
    ihx_check(e, "first stream has id 4 (the refused open consumed 2)", hqq_stream_id(st) == UINT64_C(4),
              "%llu", (unsigned long long)hqq_stream_id(st));
    for (uint64_t i = 0; i < 2; i++) {
        ret = send_payload(e, st, seed, i, size);
        if (ret != IHX_OK) {
            return ret;
        }
    }
    {
        hqq_msg *reply = NULL;
        rc = hqq_stream_recv(st, &reply, &w);
        if (rc != HQQ_OK) {
            return ihx_fail_status(e, rc, "receive the reply");
        }
        ihx_check(e, "reply from the peer's stream is intact",
                  ihx_payload_verify(seed + 1, 0, hqq_msg_data(reply), hqq_msg_len(reply)) == 0, "%s", "");
        hqq_msg_release(reply);
    }
    ihx_check_status(e, "stream Close", hqq_stream_close(st), HQQ_OK);
    ihx_check_status(e, "second Close is ErrClosed", hqq_stream_close(st), HQQ_ERR_CLOSED);
    {
        hqq_msg *m = NULL;
        rc = hqq_stream_recv(st, &m, &w);
        ihx_check_status(e, "Recv on a closed stream is ErrClosed", rc, HQQ_ERR_CLOSED);
        hqq_msg_release(m);
    }
    ihx_check_status(e, "stream 0 cannot be closed", hqq_stream_close(s0), HQQ_ERR_INVALID_OP);

    rc = hqq_adv_open_stream(link, &st2, &w);
    if (rc != HQQ_OK) {
        return ihx_fail_status(e, rc, "OpenStream (second)");
    }
    ihx_check(e, "second stream has id 6 (ids are not reused)", hqq_stream_id(st2) == UINT64_C(6), "%llu",
              (unsigned long long)hqq_stream_id(st2));
    ret = send_payload(e, st2, seed, 2, size);
    if (ret != IHX_OK) {
        return ret;
    }
    ihx_check_status(e, "stream 4 Close", hqq_stream_close(st2), HQQ_OK);
    e->sent = 3;
    return ihx_barrier(e, "end");
}

static int a05_basic_opener(ihx_env *e, hqq_adv *link) {
    hqq_stream *st = NULL;
    hqq_stream *st2 = NULL;
    hqq_wait w = wait_left(e);
    hqq_wait shortw = wait_ns(ADV_S(1));
    hqq_status rc;
    uint64_t seed = e->cfg.seed;
    uint64_t size = cfg_size(&e->cfg);
    int ret;

    rc = hqq_adv_accept_stream(link, &st, &shortw);
    ihx_check_status(e, "AcceptStream without ListenStreams is ErrNotListening", rc, HQQ_ERR_NOT_LISTENING);
    if (rc == HQQ_OK) {
        (void)hqq_stream_close(st);
    }
    ret = ihx_barrier(e, "p1");
    if (ret != IHX_OK) {
        return ret;
    }
    ret = ihx_barrier(e, "p2");
    if (ret != IHX_OK) {
        return ret;
    }
    rc = hqq_adv_listen_streams(link);
    if (rc != HQQ_OK) {
        return ihx_fail_status(e, rc, "ListenStreams");
    }
    ret = ihx_barrier(e, "p3");
    if (ret != IHX_OK) {
        return ret;
    }

    rc = hqq_adv_accept_stream(link, &st, &w);
    if (rc != HQQ_OK) {
        return ihx_fail_status(e, rc, "AcceptStream");
    }
    ihx_check(e, "first accepted stream has id 4 (the refused open consumed 2)", hqq_stream_id(st) == UINT64_C(4),
              "%llu", (unsigned long long)hqq_stream_id(st));
    for (uint64_t i = 0; i < 2; i++) {
        ret = recv_check(e, st, seed, i, size, 1);
        if (ret != IHX_OK) {
            return ret;
        }
    }
    ret = send_payload(e, st, seed + 1, 0, 10);
    if (ret != IHX_OK) {
        return ret;
    }
    {
        hqq_msg *m = NULL;
        rc = hqq_stream_recv(st, &m, &w);
        ihx_check_status(e, "peer close is seen after the queued data as ErrPeerClosed", rc, HQQ_ERR_PEER_CLOSED);
        hqq_msg_release(m);
    }

    rc = hqq_adv_accept_stream(link, &st2, &w);
    if (rc != HQQ_OK) {
        return ihx_fail_status(e, rc, "AcceptStream (second)");
    }
    ihx_check(e, "second accepted stream has id 6 (ids are not reused)", hqq_stream_id(st2) == UINT64_C(6),
              "%llu", (unsigned long long)hqq_stream_id(st2));
    ret = recv_check(e, st2, seed, 2, size, 1);
    if (ret != IHX_OK) {
        return ret;
    }
    e->received = 3;
    return ihx_barrier(e, "end");
}

/* held: the creator's OpenStream reaches the opener before it listens; the opener holds
 * it (SPEC 10.3) and accepts it once ListenStreams runs. */
static int a05_held_creator(ihx_env *e, hqq_adv *link) {
    hqq_stream *st = NULL;
    hqq_wait w = wait_left(e);
    hqq_status rc;
    uint64_t seed = e->cfg.seed;
    uint64_t size = cfg_size(&e->cfg);
    int ret;

    ret = ihx_barrier(e, "p1");
    if (ret != IHX_OK) {
        return ret;
    }
    rc = hqq_adv_open_stream(link, &st, &w);
    if (rc != HQQ_OK) {
        return ihx_fail_status(e, rc, "OpenStream (held)");
    }
    ihx_check(e, "held stream has id 2", hqq_stream_id(st) == UINT64_C(2), "%llu",
              (unsigned long long)hqq_stream_id(st));
    ret = send_payload(e, st, seed, 0, size);
    if (ret != IHX_OK) {
        return ret;
    }
    {
        hqq_msg *reply = NULL;
        rc = hqq_stream_recv(st, &reply, &w);
        if (rc != HQQ_OK) {
            return ihx_fail_status(e, rc, "receive the reply");
        }
        ihx_check(e, "reply from the peer's stream is intact",
                  ihx_payload_verify(seed + 1, 0, hqq_msg_data(reply), hqq_msg_len(reply)) == 0, "%s", "");
        hqq_msg_release(reply);
    }
    ihx_check_status(e, "stream Close", hqq_stream_close(st), HQQ_OK);
    e->sent = 1;
    return ihx_barrier(e, "end");
}

static int a05_held_opener(ihx_env *e, hqq_adv *link) {
    hqq_stream *st = NULL;
    hqq_wait w = wait_left(e);
    hqq_wait shortw = wait_ns(ADV_S(1));
    hqq_status rc;
    uint64_t seed = e->cfg.seed;
    uint64_t size = cfg_size(&e->cfg);
    int ret;

    rc = hqq_adv_accept_stream(link, &st, &shortw);
    ihx_check_status(e, "AcceptStream without ListenStreams is ErrNotListening", rc, HQQ_ERR_NOT_LISTENING);
    ret = ihx_barrier(e, "p1");
    if (ret != IHX_OK) {
        return ret;
    }
    /* Gives the creator's request time to arrive while the link does not listen yet;
     * the checks below hold whether or not it did. */
    ihx_sleep_ns(e, UINT64_C(300) * 1000000u);
    rc = hqq_adv_listen_streams(link);
    if (rc != HQQ_OK) {
        return ihx_fail_status(e, rc, "ListenStreams");
    }
    rc = hqq_adv_accept_stream(link, &st, &w);
    if (rc != HQQ_OK) {
        return ihx_fail_status(e, rc, "AcceptStream");
    }
    ihx_check(e, "held stream is accepted with id 2", hqq_stream_id(st) == UINT64_C(2), "%llu",
              (unsigned long long)hqq_stream_id(st));
    ret = recv_check(e, st, seed, 0, size, 1);
    if (ret != IHX_OK) {
        return ret;
    }
    ret = send_payload(e, st, seed + 1, 0, 10);
    if (ret != IHX_OK) {
        return ret;
    }
    {
        hqq_msg *m = NULL;
        rc = hqq_stream_recv(st, &m, &w);
        ihx_check_status(e, "peer close is seen after the queued data as ErrPeerClosed", rc, HQQ_ERR_PEER_CLOSED);
        hqq_msg_release(m);
    }
    e->received = 1;
    return ihx_barrier(e, "end");
}

/* bad-parity and duplicate: the Primary sends a raw OpConnCreate that the Secondary refuses. */
static int a05_invalid(ihx_env *e, hqq_adv *link) {
    int bad_parity = strcmp(e->cfg.variant, "bad-parity") == 0;
    hqq_wait w = wait_left(e);
    hqq_status st;
    int rc;
    if (e->cfg.role == 1) {
        hqq_stream *s = NULL;
        st = hqq_adv_listen_streams(link);
        if (st != HQQ_OK) {
            return ihx_fail_status(e, st, "ListenStreams");
        }
        rc = ihx_barrier(e, "p1");
        if (rc != IHX_OK) {
            return rc;
        }
        if (bad_parity) {
            hqq_wait sw = wait_ns(ADV_MS(300));
            st = hqq_adv_accept_stream(link, &s, &sw);
            ihx_check_status(e, "refused stream is not accepted", st, HQQ_ERR_TIMEOUT);
            if (st == HQQ_OK) {
                (void)hqq_stream_close(s);
            }
        } else {
            st = hqq_adv_accept_stream(link, &s, &w);
            if (st != HQQ_OK) {
                return ihx_fail_status(e, st, "AcceptStream");
            }
            ihx_check(e, "first stream has id 2", hqq_stream_id(s) == UINT64_C(2), "%llu",
                      (unsigned long long)hqq_stream_id(s));
        }
        return ihx_barrier(e, "end");
    }

    rc = ihx_barrier(e, "p1");
    if (rc != IHX_OK) {
        return rc;
    }
    {
        ihx_raw *raw = NULL;
        uint64_t words[8] = {0};
        hqq_stream *s = NULL;
        rc = ihx_raw_open(e, &raw);
        if (rc != IHX_OK) {
            return rc;
        }
        if (bad_parity) {
            /* Odd ids belong to the Secondary: the Primary must not send one. */
            words[0] = ADV_OP_CONN_CREATE;
            words[1] = 3;
            rc = ihx_raw_put(e, raw, words, NULL, 0);
        } else {
            st = hqq_adv_open_stream(link, &s, &w);
            if (st != HQQ_OK) {
                rc = ihx_fail_status(e, st, "OpenStream");
            } else {
                ihx_check(e, "first stream has id 2", hqq_stream_id(s) == UINT64_C(2), "%llu",
                          (unsigned long long)hqq_stream_id(s));
                /* Id 2 is registered at the peer: the duplicate is refused. */
                words[0] = ADV_OP_CONN_CREATE;
                words[1] = hqq_stream_id(s);
                rc = ihx_raw_put(e, raw, words, NULL, 0);
            }
        }
        if (rc == IHX_OK) {
            /* The refusal is an OpError that matches no pending OpenStream: the receive path fails. */
            hqq_msg *m = NULL;
            st = hqq_stream_recv(hqq_adv_stream0(link), &m, &w);
            ihx_check_status(e, "refusal with ErrCodeInvalidOp fails the sender's receive path", st,
                             HQQ_ERR_INVALID_OP);
            hqq_msg_release(m);
            rc = ihx_barrier(e, "end");
        }
        ihx_raw_close(raw);
    }
    return rc;
}

static int run_a05(ihx_env *e) {
    hqq_adv *link = NULL;
    int rc;
    const char *v = e->cfg.variant;
    if (strcmp(v, "basic") != 0 && strcmp(v, "held") != 0 && strcmp(v, "bad-parity") != 0 &&
        strcmp(v, "duplicate") != 0) {
        return ihx_fail(e, "CheckFailed", "unknown variant %s", v);
    }
    rc = adv_establish(e, &link);
    if (rc != IHX_OK) {
        return rc;
    }
    rc = negotiate(e, link, 0);
    if (rc == IHX_OK) {
        if (strcmp(v, "basic") == 0) {
            rc = e->cfg.role == 0 ? a05_basic_creator(e, link) : a05_basic_opener(e, link);
        } else if (strcmp(v, "held") == 0) {
            rc = e->cfg.role == 0 ? a05_held_creator(e, link) : a05_held_opener(e, link);
        } else {
            rc = a05_invalid(e, link);
        }
    }
    close_link(&link);
    return rc;
}

/* ---- X-A06 flood ------------------------------------------------------------------- */

typedef struct flood_send {
    adv_thread th;
    hqq_stream *s;
    uint64_t seed;
    uint64_t n;
    uint64_t size;
    hqq_wait w;
    hqq_status st;
    uint64_t at;
} flood_send;

/* The sender of the flood: every message in order, no waiting on the receive path. */
ADV_THREAD_FN(flood_send_main) {
    flood_send *f = (flood_send *)arg;
    uint8_t *p = malloc(f->size != 0 ? (size_t)f->size : 1);
    uint64_t i;
    f->st = HQQ_OK;
    if (p == NULL) {
        f->st = HQQ_ERR_NO_MEMORY;
        return ADV_THREAD_EXIT;
    }
    for (i = 0; i < f->n; i++) {
        ihx_payload(f->seed, i, p, (size_t)f->size);
        f->st = hqq_stream_send(f->s, p, (size_t)f->size, &f->w);
        if (f->st != HQQ_OK) {
            f->at = i;
            break;
        }
    }
    free(p);
    return ADV_THREAD_EXIT;
}

static int run_a06(ihx_env *e) {
    hqq_adv *link = NULL;
    flood_send f;
    hqq_stream *s;
    uint64_t n = e->cfg.messages;
    uint64_t size = cfg_size(&e->cfg);
    uint64_t send_seed = e->cfg.seed + (e->cfg.role == 0 ? 0 : 1);
    uint64_t recv_seed = e->cfg.seed + (e->cfg.role == 0 ? 1 : 0);
    uint64_t i;
    uint64_t bad = 0;
    char first_bad[IHX_DETAIL_MAX];
    int rc;

    memset(&f, 0, sizeof f);
    first_bad[0] = '\0';
    if (e->cfg.long_sizes) {
        n *= 10;
    }
    rc = adv_establish(e, &link);
    if (rc != IHX_OK) {
        return rc;
    }
    s = hqq_adv_stream0(link);
    rc = negotiate(e, link, HQQ_FEATURES_ALL);
    if (rc != IHX_OK) {
        goto out;
    }
    f.s = s;
    f.seed = send_seed;
    f.n = n;
    f.size = size;
    f.w = wait_left(e);
    rc = spawn(e, &f.th, flood_send_main, &f);
    if (rc != IHX_OK) {
        goto out;
    }
    for (i = 0; i < n; i++) {
        hqq_msg *m = NULL;
        hqq_wait w = wait_left(e);
        hqq_status st;
        size_t len;
        int ok;
        ihx_sleep_ns(e, 1000000); /* slow consumer: one message per 1 ms */
        st = hqq_stream_recv(s, &m, &w);
        if (st != HQQ_OK) {
            rc = ihx_fail_status(e, st, "recv %llu", (unsigned long long)i);
            goto out;
        }
        len = hqq_msg_len(m);
        ok = (uint64_t)len == size &&
             ihx_payload_verify(recv_seed, i, hqq_msg_data(m) != NULL ? hqq_msg_data(m) : (const uint8_t *)"", len) == 0;
        if (!ok) {
            if (bad == 0) {
                snprintf(first_bad, sizeof first_bad, "message %llu: len=%llu", (unsigned long long)i,
                         (unsigned long long)len);
            }
            bad++;
        }
        hqq_msg_release(m);
    }
    {
        char name[IHX_NAME_MAX];
        snprintf(name, sizeof name, "all %llu messages from the peer arrived intact", (unsigned long long)n);
        ihx_check(e, name, bad == 0, "%s", first_bad);
    }
    thread_join(&f.th);
    if (f.st != HQQ_OK) {
        rc = ihx_fail_status(e, f.st, "send %llu", (unsigned long long)f.at);
        goto out;
    }
    e->sent = n;
    e->received = n;
    rc = ihx_barrier(e, "end");
out:
    close_link(&link);
    thread_join(&f.th);
    return rc;
}

/* ---- X-A07 cancel ------------------------------------------------------------------ */

typedef struct cancel_timer {
    adv_thread th;
    adv_ev stop;
    hqq_cancel *tok;
    int64_t delay;
} cancel_timer;

/* Signals the token after `delay` unless the call ended first. */
ADV_THREAD_FN(cancel_timer_main) {
    cancel_timer *t = (cancel_timer *)arg;
    if (!ev_wait(&t->stop, t->delay, NULL)) {
        hqq_cancel_signal(t->tok);
    }
    return ADV_THREAD_EXIT;
}

static int a07_cancel_creator(ihx_env *e, hqq_adv *link) {
    hqq_stream *s0 = hqq_adv_stream0(link);
    hqq_cancel tok;
    cancel_timer ct;
    hqq_wait cw;
    hqq_msg *m = NULL;
    hqq_status st;
    int rc;
    memset(&ct, 0, sizeof ct);
    ev_init(&ct.stop);
    hqq_cancel_init(&tok);
    ct.tok = &tok;
    ct.delay = ADV_MS(200);
    rc = spawn(e, &ct.th, cancel_timer_main, &ct);
    if (rc != IHX_OK) {
        ev_fini(&ct.stop);
        return rc;
    }
    /* No deadline on the call: a deadline would send a time budget (1.1 FeatureMetadata). */
    cw = wait_left(e);
    cw.cancel = &tok;
    st = hqq_stream_call(s0, ADV_M_BLOCK, "block", 5, &m, &cw);
    ev_set(&ct.stop, 0);
    thread_join(&ct.th);
    ev_fini(&ct.stop);
    ihx_check_status(e, "cancelled call ends with context.Canceled", st, HQQ_ERR_CANCELED);
    hqq_msg_release(m);

    /* An OpCancel whose key names no request is ignored. */
    {
        ihx_raw *raw = NULL;
        uint64_t words[8] = {ADV_OP_CANCEL, 0, 999999, 0, 0, 0, 0, 0};
        rc = ihx_raw_open(e, &raw);
        if (rc != IHX_OK) {
            return rc;
        }
        rc = ihx_raw_put(e, raw, words, NULL, 0);
        ihx_raw_close(raw);
        if (rc != IHX_OK) {
            return rc;
        }
    }
    m = NULL;
    cw = wait_left(e); /* a fresh wait: the token of the cancelled call is signalled */
    st = hqq_stream_call(s0, ADV_M_ECHO, "after", 5, &m, &cw);
    ihx_check(e, "link keeps serving calls after an ignored OpCancel", st == HQQ_OK && msg_is(m, "after", 5), "%s",
              hqq_strerror(st));
    hqq_msg_release(m);
    return ihx_barrier(e, "end");
}

static int a07_cancel_opener(ihx_env *e, hqq_adv *link) {
    a07_state st;
    adv_serve sv;
    int canceled = 0;
    int rc;
    memset(&sv, 0, sizeof sv);
    ev_init(&st.blocked);
    st.block_ns = ADV_S(3);
    rc = serve_start(e, &sv, link, h_a07, &st);
    if (rc == IHX_OK) {
        rc = ihx_barrier(e, "end");
    }
    if (rc == IHX_OK) {
        if (ev_wait(&st.blocked, ADV_S(5), &canceled)) {
            ihx_check(e, "handler context is canceled by the peer's OpCancel", canceled, "%s", "");
        } else {
            ihx_check(e, "handler context is canceled by the peer's OpCancel", 0, "%s", "handler did not finish");
        }
        {
            hqq_adv_stats stt = adv_stats(link);
            ihx_check(e, "responder counts the canceled request", stt.canceled_requests >= 1, "%llu",
                      (unsigned long long)stt.canceled_requests);
        }
    }
    (void)serve_join(&sv);
    ev_fini(&st.blocked);
    return rc;
}

static int a07_nocancel_creator(ihx_env *e, hqq_adv *link) {
    hqq_stream *s0 = hqq_adv_stream0(link);
    hqq_wait cw = wait_ns(ADV_MS(200));
    hqq_msg *m = NULL;
    hqq_status st;
    int rc;
    st = hqq_stream_call(s0, ADV_M_BLOCK, "block", 5, &m, &cw);
    hqq_msg_release(m);
    ihx_check_status(e, "cancelled call ends with DeadlineExceeded", st, HQQ_ERR_TIMEOUT);
    rc = ihx_barrier(e, "cancelled");
    if (rc != IHX_OK) {
        return rc;
    }
    {
        hqq_wait w = wait_left(e);
        hqq_msg *rm = NULL;
        st = hqq_stream_recv(s0, &rm, &w);
        ihx_check_status(e, "OpCancel without FeatureCancel fails the receive path with ErrUnexpectedPacket", st,
                         HQQ_ERR_UNEXPECTED_PACKET);
        hqq_msg_release(rm);
    }
    return ihx_barrier(e, "end");
}

static int a07_nocancel_opener(ihx_env *e, hqq_adv *link) {
    a07_state st;
    adv_serve sv;
    int canceled = 1;
    int rc;
    memset(&sv, 0, sizeof sv);
    ev_init(&st.blocked);
    st.block_ns = ADV_MS(800);
    rc = serve_start(e, &sv, link, h_a07, &st);
    if (rc == IHX_OK) {
        rc = ihx_barrier(e, "cancelled");
    }
    if (rc == IHX_OK) {
        ihx_raw *raw = NULL;
        uint64_t words[8] = {ADV_OP_CANCEL, 0, 1, 0, 0, 0, 0, 0};
        rc = ihx_raw_open(e, &raw);
        if (rc == IHX_OK) {
            rc = ihx_raw_put(e, raw, words, NULL, 0);
            ihx_raw_close(raw);
        }
    }
    if (rc == IHX_OK) {
        if (ev_wait(&st.blocked, ADV_S(5), &canceled)) {
            ihx_check(e, "no OpCancel was sent: the handler context stays alive", !canceled, "%s", "");
        } else {
            ihx_check(e, "no OpCancel was sent: the handler context stays alive", 0, "%s", "handler did not finish");
        }
        rc = ihx_barrier(e, "end");
    }
    (void)serve_join(&sv);
    ev_fini(&st.blocked);
    return rc;
}

static int run_a07(ihx_env *e) {
    hqq_adv *link = NULL;
    int nocancel = strcmp(e->cfg.variant, "nocancel") == 0;
    int rc;
    rc = adv_establish(e, &link);
    if (rc != IHX_OK) {
        return rc;
    }
    rc = negotiate(e, link, nocancel ? ADV_FEATURES_NO_CANCEL : HQQ_FEATURES_ALL);
    if (rc == IHX_OK) {
        if (nocancel) {
            rc = e->cfg.role == 0 ? a07_nocancel_creator(e, link) : a07_nocancel_opener(e, link);
        } else {
            rc = e->cfg.role == 0 ? a07_cancel_creator(e, link) : a07_cancel_opener(e, link);
        }
    }
    close_link(&link);
    return rc;
}

/* ---- X-A08 request metadata --------------------------------------------------------- */

static void adv_trace(uint8_t t[16]) {
    int i;
    for (i = 0; i < 16; i++) {
        t[i] = (uint8_t)(i + 1);
    }
}

static int is_zero16(const uint8_t *p) {
    int i;
    for (i = 0; i < 16; i++) {
        if (p[i] != 0) {
            return 0;
        }
    }
    return 1;
}

static int run_a08(ihx_env *e) {
    hqq_adv *link = NULL;
    int metadata = strcmp(e->cfg.variant, "metadata") == 0;
    int rc;
    rc = adv_establish(e, &link);
    if (rc != IHX_OK) {
        return rc;
    }
    rc = negotiate(e, link, metadata ? HQQ_FEATURES_ALL : ADV_FEATURES_NO_META);
    if (rc != IHX_OK) {
        goto out;
    }
    if (e->cfg.role == 1) {
        a08_state st;
        adv_serve sv;
        a08_seen seen[8];
        size_t n;
        memset(&sv, 0, sizeof sv);
        memset(&st, 0, sizeof st);
        lock_init(&st.lk);
        /* Serve without the process deadline: a handler context carries only the protocol's budget. */
        rc = serve_start(e, &sv, link, h_a08, &st);
        if (rc == IHX_OK) {
            rc = ihx_barrier(e, "end");
        }
        if (rc == IHX_OK) {
            lock_acquire(&st.lk);
            n = st.n;
            memcpy(seen, st.seen, sizeof seen);
            lock_release(&st.lk);
            if (metadata) {
                ihx_check(e, "responder saw 2 requests (the expired call was not sent)", n == 2, "%llu",
                          (unsigned long long)n);
                if (n == 2) {
                    uint8_t want_trace[16];
                    adv_trace(want_trace);
                    ihx_check(e, "caller ID and trace ID arrive",
                              seen[0].caller == 42 && memcmp(seen[0].trace, want_trace, 16) == 0, "%llu",
                              (unsigned long long)seen[0].caller);
                    ihx_check(e, "request without options has caller 0", seen[1].caller == 0, "%llu",
                              (unsigned long long)seen[1].caller);
                    ihx_check(e, "deadline reaches the handler within the budget",
                              seen[1].has_dl && seen[1].remaining > ADV_S(1) && seen[1].remaining <= ADV_S(2),
                              "has=%d remaining=%lld ns", seen[1].has_dl, (long long)seen[1].remaining);
                }
            } else {
                ihx_check(e, "responder saw 2 requests", n == 2, "%llu", (unsigned long long)n);
                if (n == 2) {
                    ihx_check(e, "options are ignored without FeatureMetadata: caller 0, trace zero",
                              seen[0].caller == 0 && is_zero16(seen[0].trace), "%llu",
                              (unsigned long long)seen[0].caller);
                    ihx_check(e, "no deadline reaches the handler without FeatureMetadata", !seen[1].has_dl, "%d",
                              seen[1].has_dl);
                }
            }
        }
        (void)serve_join(&sv);
        lock_fini(&st.lk);
        goto out;
    }

    {
        hqq_stream *s0 = hqq_adv_stream0(link);
        hqq_wait w = wait_left(e);
        hqq_msg *m = NULL;
        hqq_status st;
        hqq_call_options opts;
        hqq_call_options_init(&opts);
        opts.caller = 42;
        adv_trace(opts.trace);
        st = hqq_stream_call_with(s0, ADV_M_META, "a", 1, &opts, &m, &w);
        hqq_msg_release(m);
        if (st != HQQ_OK) {
            rc = ihx_fail_status(e, st, "CallWith (caller and trace)");
            goto out;
        }
        {
            hqq_wait dw = wait_ns(ADV_S(2));
            m = NULL;
            st = hqq_stream_call(s0, ADV_M_META, "b", 1, &m, &dw);
            hqq_msg_release(m);
            if (st != HQQ_OK) {
                rc = ihx_fail_status(e, st, "CallWith (deadline)");
                goto out;
            }
        }
        if (metadata) {
            uint64_t before = adv_stats(link).requests;
            hqq_wait zero = wait_ns(0);
            uint64_t after;
            m = NULL;
            st = hqq_stream_call(s0, ADV_M_META, "c", 1, &m, &zero);
            hqq_msg_release(m);
            ihx_check_status(e, "call with an expired deadline fails locally with DeadlineExceeded", st,
                             HQQ_ERR_TIMEOUT);
            after = adv_stats(link).requests;
            ihx_check(e, "the expired call sent no request", after == before, "requests %llu -> %llu",
                      (unsigned long long)before, (unsigned long long)after);
        }
        rc = ihx_barrier(e, "end");
    }
out:
    close_link(&link);
    return rc;
}

/* ---- X-A09 graceful shutdown --------------------------------------------------------- */

typedef struct shutdown_job {
    adv_thread th;
    hqq_adv *link;
    adv_ev done;
    hqq_status rc;
} shutdown_job;

ADV_THREAD_FN(shutdown_main) {
    shutdown_job *j = (shutdown_job *)arg;
    hqq_wait w = wait_ns(ADV_S(10));
    j->rc = hqq_adv_shutdown(j->link, &w);
    ev_set(&j->done, (int)j->rc);
    return ADV_THREAD_EXIT;
}

typedef struct a09_call {
    adv_thread th;
    hqq_stream *s;
    hqq_wait w;
    hqq_status rc;
    char body[32];
} a09_call;

/* Requester's A call: blocks on the responder until released. */
ADV_THREAD_FN(a09_call_main) {
    a09_call *a = (a09_call *)arg;
    hqq_msg *m = NULL;
    a->rc = hqq_stream_call(a->s, ADV_M_BLOCK, "A", 1, &m, &a->w);
    if (a->rc == HQQ_OK) {
        size_t len = hqq_msg_len(m);
        if (len > sizeof a->body - 1) {
            len = sizeof a->body - 1;
        }
        if (len != 0) {
            memcpy(a->body, hqq_msg_data(m), len);
        }
        a->body[len] = '\0';
    }
    hqq_msg_release(m);
    return ADV_THREAD_EXIT;
}

static int a09_responder(ihx_env *e, hqq_adv *link, adv_serve *sv, a09_state *st) {
    hqq_stream *s0 = hqq_adv_stream0(link);
    shutdown_job job;
    hqq_status serve_st;
    hqq_msg *m = NULL;
    hqq_wait wl;
    int early;
    int rc;
    memset(&job, 0, sizeof job);
    job.link = link;
    ev_init(&job.done);

    rc = serve_start(e, sv, link, h_a09, st);
    if (rc != IHX_OK) {
        goto out;
    }
    if (!ev_wait(&st->started, wait_left(e).timeout_ns, NULL)) {
        rc = ihx_fail(e, "ErrTimeout", "A did not start");
        goto out;
    }
    rc = spawn(e, &job.th, shutdown_main, &job);
    if (rc != IHX_OK) {
        goto out;
    }
    rc = ihx_barrier(e, "refused");
    if (rc != IHX_OK) {
        goto out;
    }
    early = ev_is_set(&job.done);
    ihx_check(e, "Shutdown waits for the in-flight request", !early, "%s", early ? "returned early" : "");
    ev_set(&st->release, 1);
    if (!ev_wait(&job.done, wait_left(e).timeout_ns, NULL)) {
        rc = ihx_fail(e, "ErrTimeout", "Shutdown did not return");
        goto out;
    }
    thread_join(&job.th);
    ihx_check_status(e, "Shutdown returns nil after the in-flight request ended", job.rc, HQQ_OK);
    serve_st = serve_join(sv);
    ihx_check_status(e, "Serve returns nil after shutdown", serve_st, HQQ_OK);
    {
        hqq_adv_stats stt = adv_stats(link);
        ihx_check(e, "refused requests are counted", stt.refused_requests >= 1, "%llu",
                  (unsigned long long)stt.refused_requests);
    }

    /* The link stays open: data flows both ways. */
    wl = wait_left(e);
    {
        hqq_status rs = hqq_stream_recv(s0, &m, &wl);
        if (rs != HQQ_OK) {
            rc = ihx_fail_status(e, rs, "receive data after shutdown");
            goto out;
        }
    }
    ihx_check(e, "data from the peer after shutdown", msg_is(m, "after-shutdown", 14), "%s", "");
    hqq_msg_release(m);
    {
        hqq_status st2 = hqq_stream_send(s0, "reply", 5, &wl);
        if (st2 != HQQ_OK) {
            rc = ihx_fail_status(e, st2, "send reply");
            goto out;
        }
    }
    rc = ihx_barrier(e, "end");
out:
    ev_set(&st->release, 1);
    thread_join(&job.th);
    ev_fini(&job.done);
    return rc;
}

static int a09_requester(ihx_env *e, hqq_adv *link) {
    hqq_stream *s0 = hqq_adv_stream0(link);
    a09_call a;
    int refused = 0;
    int fatal = 0;
    hqq_status last = HQQ_OK;
    uint64_t deadline;
    int rc;
    memset(&a, 0, sizeof a);
    a.s = s0;
    a.w = wait_left(e);
    rc = spawn(e, &a.th, a09_call_main, &a);
    if (rc != IHX_OK) {
        return rc;
    }
    deadline = ihx_now_ns() + (uint64_t)ADV_S(10);
    while (!refused && ihx_now_ns() < deadline) {
        hqq_msg *m = NULL;
        hqq_wait wb = wait_left(e);
        hqq_status st = hqq_stream_call(s0, ADV_M_ECHO, "B", 1, &m, &wb);
        uint64_t status = 0;
        if (st == HQQ_OK) {
            hqq_msg_release(m); /* answered before shutdown started */
            continue;
        }
        last = st;
        refused = resp_status(st, m, &status) && status == HQQ_ERRCODE_SHUTTING_DOWN;
        if (!refused) {
            note_response(e, st, m);
        }
        hqq_msg_release(m);
        if (!refused) {
            rc = ihx_fail_status(e, st, "request before the refusal");
            fatal = 1;
            break;
        }
    }
    if (fatal) {
        thread_join(&a.th);
        return rc;
    }
    ihx_check(e, "request after shutdown started is refused with status 8 (ErrShuttingDown)", refused, "%s",
              hqq_strerror(last));
    rc = ihx_barrier(e, "refused");
    if (rc != IHX_OK) {
        thread_join(&a.th);
        return rc;
    }
    thread_join(&a.th);
    ihx_check(e, "in-flight request completes normally", a.rc == HQQ_OK && strcmp(a.body, "A-done") == 0, "%s",
              hqq_strerror(a.rc));

    {
        hqq_wait ws = wait_left(e);
        hqq_msg *m = NULL;
        hqq_status ss = hqq_stream_send(s0, "after-shutdown", 14, &ws);
        if (ss != HQQ_OK) {
            return ihx_fail_status(e, ss, "send after-shutdown");
        }
        ss = hqq_stream_recv(s0, &m, &ws);
        if (ss != HQQ_OK) {
            return ihx_fail_status(e, ss, "receive reply");
        }
        ihx_check(e, "data from the responder after shutdown", msg_is(m, "reply", 5), "%s", "");
        hqq_msg_release(m);
    }
    return ihx_barrier(e, "end");
}

static int run_a09(ihx_env *e) {
    hqq_adv *link = NULL;
    adv_serve sv;
    a09_state st;
    int rc;
    memset(&sv, 0, sizeof sv);
    ev_init(&st.started);
    ev_init(&st.release);
    rc = adv_establish(e, &link);
    if (rc != IHX_OK) {
        ev_fini(&st.started);
        ev_fini(&st.release);
        return rc;
    }
    rc = negotiate(e, link, 0);
    if (rc == IHX_OK) {
        rc = e->cfg.role == 1 ? a09_requester(e, link) : a09_responder(e, link, &sv, &st);
    }
    close_link(&link);
    (void)serve_join(&sv);
    ev_fini(&st.started);
    ev_fini(&st.release);
    return rc;
}

/* ---- X-A10 acceptor ------------------------------------------------------------------ */

typedef struct pending_accept {
    adv_thread th;
    hqq_acceptor *acc;
    hqq_wait w;
    adv_ev done;
    hqq_adv *out;
    hqq_status rc;
} pending_accept;

ADV_THREAD_FN(pending_accept_main) {
    pending_accept *pa = (pending_accept *)arg;
    pa->rc = hqq_acceptor_accept(pa->acc, &pa->out, &pa->w);
    ev_set(&pa->done, (int)pa->rc);
    return ADV_THREAD_EXIT;
}

/* Creator: the acceptor and one server per accepted link. */
static int a10_acceptor(ihx_env *e) {
    hqq_acceptor_options ao;
    hqq_acceptor *acc = NULL;
    hqq_acceptor *second = NULL;
    hqq_open_info info;
    hqq_open_info info2;
    hqq_adv *links[3] = {NULL, NULL, NULL};
    adv_serve sv[3];
    pending_accept pa;
    hqq_status st;
    int acc_closed = 0;
    int rc = IHX_OK;
    int i;
    memset(sv, 0, sizeof sv);
    memset(&pa, 0, sizeof pa);
    ev_init(&pa.done);
    hqq_acceptor_options_init(&ao);
    adv_options_fill(&e->cfg, &ao.link);
    hqq_open_info_init(&info);
    hqq_open_info_init(&info2);

    st = hqq_acceptor_create(e->cfg.name, &ao, &acc, &info);
    if (st != HQQ_OK) {
        rc = ihx_fail_status(e, st, "create acceptor %s", e->cfg.name);
        goto out;
    }
    rc = ihx_barrier(e, "listening");
    if (rc != IHX_OK) {
        goto out;
    }

    st = hqq_acceptor_create(e->cfg.name, &ao, &second, &info2);
    ihx_check_status(e, "second acceptor on the same name is ErrListenerBusy", st, HQQ_ERR_LISTENER_BUSY);
    if (st == HQQ_OK) {
        (void)hqq_acceptor_close(second);
    }

    for (i = 0; i < 3; i++) {
        hqq_wait w = wait_left(e);
        st = hqq_acceptor_accept(acc, &links[i], &w);
        if (st != HQQ_OK) {
            rc = ihx_fail_status(e, st, "accept %d", i);
            goto out;
        }
        rc = serve_start(e, &sv[i], links[i], h_echo, NULL);
        if (rc != IHX_OK) {
            goto out;
        }
    }
    e->received = 3;
    rc = ihx_barrier(e, "clients-done");
    if (rc != IHX_OK) {
        goto out;
    }

    pa.acc = acc;
    pa.w = wait_left(e);
    rc = spawn(e, &pa.th, pending_accept_main, &pa);
    if (rc != IHX_OK) {
        goto out;
    }
    st = hqq_acceptor_close(acc);
    acc_closed = 1;
    if (st != HQQ_OK) {
        rc = ihx_fail_status(e, st, "close acceptor");
        goto out;
    }
    if (ev_wait(&pa.done, ADV_S(5), NULL)) {
        ihx_check_status(e, "pending Accept fails with ErrAcceptorClosed after Close", pa.rc,
                         HQQ_ERR_ACCEPTOR_CLOSED);
    } else {
        ihx_check(e, "pending Accept fails with ErrAcceptorClosed after Close", 0, "%s", "Accept did not return");
    }
    thread_join(&pa.th);
    if (pa.out != NULL) {
        (void)hqq_adv_close(pa.out);
    }
    rc = ihx_barrier(e, "end");
out:
    for (i = 0; i < 3; i++) {
        close_link(&links[i]);
    }
    for (i = 0; i < 3; i++) {
        (void)serve_join(&sv[i]);
    }
    if (acc != NULL && !acc_closed) {
        (void)hqq_acceptor_close(acc);
    }
    thread_join(&pa.th);
    ev_fini(&pa.done);
    return rc;
}

/* Client: connects three times, one echoed request per link. */
static int a10_client(ihx_env *e) {
    hqq_connect_options co;
    int i;
    int rc = ihx_barrier(e, "listening");
    if (rc != IHX_OK) {
        return rc;
    }
    hqq_connect_options_init(&co);
    co.buffer_count = e->cfg.buffer_count;
    co.buffer_size = e->cfg.buffer_size;
    adv_options_fill(&e->cfg, &co.link);
    for (i = 0; i < 3; i++) {
        hqq_adv *l = NULL;
        hqq_wait w = wait_left(e);
        hqq_status st = hqq_connect(e->cfg.name, &co, &w, &l);
        char payload[32];
        char name[IHX_NAME_MAX];
        size_t n;
        hqq_msg *m = NULL;
        if (st != HQQ_OK) {
            return ihx_fail_status(e, st, "connect %d", i);
        }
        n = (size_t)snprintf(payload, sizeof payload, "client %d", i);
        st = hqq_stream_call(hqq_adv_stream0(l), ADV_M_ECHO, payload, n, &m, &w);
        if (st != HQQ_OK) {
            note_response(e, st, m);
            hqq_msg_release(m);
            close_link(&l);
            return ihx_fail_status(e, st, "call %d", i);
        }
        snprintf(name, sizeof name, "client %d: the accepted link answers its request", i);
        ihx_check(e, name, msg_is(m, payload, n), "%s", "");
        hqq_msg_release(m);
        close_link(&l);
    }
    e->sent = 3;
    rc = ihx_barrier(e, "clients-done");
    if (rc != IHX_OK) {
        return rc;
    }
    return ihx_barrier(e, "end");
}

static int run_a10(ihx_env *e) {
    return e->cfg.role == 0 ? a10_acceptor(e) : a10_client(e);
}

/* ---- Catalogue ----------------------------------------------------------------------- */

#define ADV_STEP_BOTH_OK {2, {"creator", "opener"}, {"ok", "ok"}, {NULL, NULL}}
#define ADV_VARIANT(nm, bc, bs, sz, msgs, maxm, maxr, qs) \
    {nm, bc, bs, sz, msgs, 0, 0, 0, 0, maxm, maxr, qs, 1, {ADV_STEP_BOTH_OK}}

static const ihx_variant a01_variants[] = {
    ADV_VARIANT("all-features", 4, 256, 0, 0, 0, 0, 0),
    ADV_VARIANT("opener-initiates", 4, 256, 0, 0, 0, 0, 0),
    ADV_VARIANT("simultaneous", 4, 256, 0, 0, 0, 0, 0),
    ADV_VARIANT("messages-only", 4, 256, 0, 0, 0, 0, 0),
    ADV_VARIANT("request-response-only", 4, 256, 0, 0, 0, 0, 0),
    ADV_VARIANT("none", 4, 256, 0, 0, 0, 0, 0),
    ADV_VARIANT("bad-major", 4, 256, 0, 0, 0, 0, 0),
    ADV_VARIANT("op-error", 4, 256, 0, 0, 0, 0, 0),
};

static const ihx_variant a02_variants[] = {
    ADV_VARIANT("sizes", 4, 256, 0, 0, 65536, 0, 0),
    ADV_VARIANT("sender-abort-after-chunk-1", 4, 256, 0, 0, 65536, 0, 0),
};

static const ihx_variant a03_variants[] = {
    ADV_VARIANT("reassembly-limit", 4, 256, 0, 0, 65536, 8192, 0),
};

static const ihx_variant a04_variants[] = {
    ADV_VARIANT("default", 4, 256, 0, 0, 0, 0, 0),
};

static const ihx_variant a05_variants[] = {
    ADV_VARIANT("basic", 4, 256, 0, 0, 0, 0, 0),
    ADV_VARIANT("bad-parity", 4, 256, 0, 0, 0, 0, 0),
    ADV_VARIANT("duplicate", 4, 256, 0, 0, 0, 0, 0),
    ADV_VARIANT("held", 4, 256, 0, 0, 0, 0, 0),
};

static const ihx_variant a06_variants[] = {
    ADV_VARIANT("flood", 2, 256, 0, 100, 0, 0, 0),
    ADV_VARIANT("flood-chunked", 2, 256, 775, 100, 0, 0, 0),
};

static const ihx_variant a07_variants[] = {
    ADV_VARIANT("cancel", 4, 256, 0, 0, 0, 0, 0),
    ADV_VARIANT("nocancel", 4, 256, 0, 0, 0, 0, 0),
};

static const ihx_variant a08_variants[] = {
    ADV_VARIANT("metadata", 4, 256, 0, 0, 0, 0, 0),
    ADV_VARIANT("without-metadata", 4, 256, 0, 0, 0, 0, 0),
};

static const ihx_variant a09_variants[] = {
    ADV_VARIANT("default", 4, 256, 0, 0, 0, 0, 0),
};

static const ihx_variant a10_variants[] = {
    ADV_VARIANT("default", 4, 256, 0, 0, 0, 0, 0),
};

const ihx_scenario ihx_adv_scenarios[] = {
    {"X-A01", "negotiation (X-15, SPEC 10.1): creator-initiated, opener-initiated, simultaneous requests, feature subsets (messages only, request-response only, none), major 2 ack and OpError from the opener, Failed is terminal. Checks: negotiated version 1.1 and feature set, ErrNotNegotiated for the features not negotiated, ErrNegotiationRejected, ErrNegotiationFailed. Raw frames: opener only (bad-major, op-error). Payload stream: creator seed, 1 message of params size",
     "advanced", "any", 30, 8, a01_variants, run_a01},
    {"X-A02", "message sizes (X-16, SPEC 10.4-10.5): sizes 0, 1, bufferSize, bufferSize+1, 3 x bufferSize+7 and MaxMessageSize (params max_message 65536, buffer_size 256) arrive intact in order; MaxMessageSize+1 is refused locally with ErrBufferOverflow and nothing is sent (a 1-byte sentinel arrives next). sender-abort-after-chunk-1: a raw first chunk and an abort frame are dropped, the next message arrives intact. Payload stream: creator seed. --long repeats the size list 20 times",
     "advanced", "any", 30, 2, a02_variants, run_a02},
    {"X-A03", "reassembly limit (X-17, SPEC 10.5): max_reassembly 8192 with max_message 65536, buffer_size 256. Creator sends three data messages of 1.5 x limit (refused, dropped) and one of limit/2 (accepted). A request of 1.5 x limit is answered with status 7 (ErrCodeBufferOverflow, ResponseError.Status). A response of 1.5 x limit fails the creator's call with ErrReassemblyLimit. The link survives. Checks: the first delivered data message is the accepted one, DroppedMessages counts the refusals. Payload stream: creator seed",
     "advanced", "any", 30, 1, a03_variants, run_a03},
    {"X-A04", "request/response (X-18, SPEC 10.6): 16 concurrent calls whose handlers sleep inversely to the id (every answer is echoed, completion order is not the issue order); an error response with status 42 and payload; a call cancelled after 50 ms whose late response is discarded (AbandonedResponses >= 1); an unknown method answered with status 6 (ErrCodeInvalidOp). Features: messages and request-response (1.0 set, so late responses are sent)",
     "advanced", "any", 30, 1, a04_variants, run_a04},
    {"X-A05", "streams (X-19, SPEC 10.2-10.3): basic: OpenStream refused while not listening (ErrConnNotFound, once the peer's grace period ends), then ids 2 and 4 (Primary parity, never reused), data both ways, peer sees ErrPeerClosed after the queued data, stream 0 not closable (ErrInvalidOp), second Close ErrClosed. held: an OpenStream that reaches the opener before it listens is held there and accepted by its ListenStreams (id 2). bad-parity: a raw OpConnCreate with an odd id is refused with ErrCodeInvalidOp (the creator's receive fails with ErrInvalidOp, nothing is accepted). duplicate: a raw OpConnCreate of an existing id is refused with ErrCodeInvalidOp. Payload stream: creator seed (2 messages on stream 2, 1 on stream 4), opener's reply seed + 1",
     "advanced", "any", 30, 4, a05_variants, run_a05},
    {"X-A06", "flood (X-20, SPEC 10.3): both endpoints send `messages` messages to each other on a 2-slot ring while each consumes one message per 1 ms. Both deliver every message intact (sequence and CRC) and finish; no deadlock: the receive path never waits for the tx ring. Variants: flood (size = bufferSize), flood-chunked (size 3 x bufferSize + 7). --long multiplies messages by 10. Payload stream: creator seed for the creator's messages, seed + 1 for the opener's",
     "advanced", "any", 30, 2, a06_variants, run_a06},
    {"X-A07", "cancel (1.1 FeatureCancel, PROTOCOL_1_1 section 2 and 4.2): cancel variant: a call without a deadline is cancelled after 200 ms and ends with context.Canceled; the responder's handler sees context.Canceled and CanceledRequests >= 1; an OpCancel for an unknown key (raw) is ignored and the link keeps serving calls. nocancel variant (FeatureCancel not negotiated): the cancelled call sends no OpCancel (the handler's context stays alive for 800 ms); a raw OpCancel from the peer fails the requester's receive path with ErrUnexpectedPacket. Raw frames used: creator (cancel), opener (nocancel)",
     "advanced", "any", 30, 2, a07_variants, run_a07},
    {"X-A08", "request metadata (1.1 FeatureMetadata, PROTOCOL_1_1 section 3): metadata variant: CallWith caller 42 and a trace ID reach Request.Caller and Request.Trace; a 2 s deadline reaches the handler's context as a deadline within (1 s, 2 s]; a call whose deadline already passed fails locally with DeadlineExceeded and sends nothing (responder handles 2 requests, requester Stats().Requests unchanged). without-metadata variant (FeatureMetadata not negotiated): the options are ignored (Caller 0, Trace zero) and no deadline reaches the handler",
     "advanced", "any", 30, 2, a08_variants, run_a08},
    {"X-A09", "graceful shutdown (1.1 section 6): the responder (creator) runs a request A that blocks until released. The requester (opener) calls B until it is refused with status 8 (ErrShuttingDown). Checks: Shutdown does not return while A is in flight; after release A completes normally (\"A-done\"), Shutdown returns nil, Serve returns nil, RefusedRequests >= 1; the link stays usable: data flows both ways after shutdown",
     "advanced", "any", 30, 1, a09_variants, run_a09},
    {"X-A10", "acceptor (LISTENER_DRAFT, Listen/Dial/Connect): the creator runs NewAcceptor under the name; a second NewAcceptor on the same name fails with ErrListenerBusy; 3 clients (opener) Connect one after the other, each link answers one request (echo) and is closed; the pending Accept of a fourth link fails with ErrAcceptorClosed after Close. Creator is the acceptor (Secondary of every accepted link), opener the client (Primary)",
     "acceptor", "any", 30, 1, a10_variants, run_a10},
    {NULL, NULL, NULL, NULL, 0, 0, NULL, NULL}
};
