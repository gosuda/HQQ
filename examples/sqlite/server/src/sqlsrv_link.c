#include "sqlsrv_link.h"

#include "hqq/hqq_listener.h"
#include "sqlsrv_port.h"

#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

/* A link is polled this often for shutdown and for the end of Serve (ms). */
#define LINK_POLL_MS 100L
/*
 * A request whose stream is not registered yet waits for the accept loop. A
 * stream that never appears is answered NO_SESSION after this many polls of 5 ms.
 */
#define LOOKUP_POLL_MS 5L
#define LOOKUP_MAX_POLLS 1000u

/* One accepted link: the Advanced link of one client and its sessions. */
typedef struct lnk {
    sqlsrv_shared *sh;
    hqq_adv *adv;
    sqlsrv_mutex mu; /* guards sessions and nsessions; lock order: lnk.mu, then session mu */
    sqlsrv_cond cv;  /* registrations, unlinks and session ends */
    sqlsrv_session *sessions;
    unsigned nsessions;
    atomic_int serve_done;
} lnk;

struct sqlsrv_listener {
    sqlsrv_shared *sh;
    hqq_acceptor *acc;
    sqlsrv_thread th;
};

static int ctx_done(const hqq_req_ctx *ctx) {
    return hqq_req_ctx_err(ctx) != HQQ_OK;
}

static void send_no_session(hqq_response *w) {
    static const char msg[] = "no session on this stream";
    (void)hqq_response_error(w, SQLSRV_STATUS_PROTO(SQLSRV_PE_NO_SESSION), msg, sizeof msg - 1);
}

static void lnk_free(lnk *l) {
    sqlsrv_cond_destroy(&l->cv);
    sqlsrv_mutex_destroy(&l->mu);
    free(l);
}

/* The end of a link that was counted in live_links. */
static void lnk_done(lnk *l) {
    sqlsrv_shared *sh = l->sh;
    lnk_free(l);
    sqlsrv_mutex_lock(&sh->mu);
    sh->live_links--;
    sqlsrv_cond_broadcast(&sh->cv);
    sqlsrv_mutex_unlock(&sh->mu);
}

/* Session of a stream, referenced for the caller. Waits a while for an accept that is still in flight. */
static sqlsrv_session *lnk_lookup(lnk *l, uint64_t id, hqq_req_ctx *ctx) {
    unsigned polls = 0;
    sqlsrv_session *s;

    sqlsrv_mutex_lock(&l->mu);
    for (;;) {
        for (s = l->sessions; s != NULL; s = s->next) {
            if (s->id == id) {
                sqlsrv_sess_ref(s);
                sqlsrv_mutex_unlock(&l->mu);
                return s;
            }
        }
        if (ctx_done(ctx) || polls >= LOOKUP_MAX_POLLS) {
            sqlsrv_mutex_unlock(&l->mu);
            return NULL;
        }
        polls++;
        sqlsrv_cond_timedwait_ms(&l->cv, &l->mu, LOOKUP_POLL_MS);
    }
}

/* The handler of every request of the link (PROTOCOL.md section 2: stream 0 has no session). */
static void lnk_serve_request(void *user, hqq_req_ctx *ctx, hqq_response *w, const hqq_request *req) {
    lnk *l = user;
    uint64_t sid = hqq_request_stream_id(req);
    sqlsrv_session *s;

    if (sid == 0) {
        send_no_session(w);
        return;
    }
    s = lnk_lookup(l, sid, ctx);
    if (s == NULL) {
        if (!ctx_done(ctx)) {
            send_no_session(w);
        }
        return;
    }
    sqlsrv_sess_request(s, ctx, w, hqq_request_method(req), hqq_request_data(req), hqq_request_len(req));
    sqlsrv_sess_unref(s);
}

/* Ends a session: stop and join its worker, close its stream, and wait until no handler uses it. */
static void lnk_retire(lnk *l, sqlsrv_session *s) {
    sqlsrv_session **link;

    sqlsrv_sess_stop(s);
    (void)hqq_stream_close(s->stream);

    sqlsrv_mutex_lock(&l->mu);
    link = &l->sessions;
    while (*link != NULL && *link != s) {
        link = &(*link)->next;
    }
    if (*link == s) {
        *link = s->next;
    }
    sqlsrv_mutex_unlock(&l->mu);

    sqlsrv_sess_wait_refs(s);

    /* After this decrement the link may be freed by its owner: l is not used again. */
    sqlsrv_mutex_lock(&l->mu);
    l->nsessions--;
    sqlsrv_cond_broadcast(&l->cv);
    sqlsrv_mutex_unlock(&l->mu);

    sqlsrv_sess_free(s);
}

/* Waits for the stream to end (by the client, by the link failing, or by a close of ours). */
static void *watch_main(void *arg) {
    sqlsrv_session *s = arg;
    lnk *l = s->owner;
    hqq_msg *m = NULL;

    /* Nothing is sent on a session stream, so Recv returns only at the end; a message is a violation. */
    (void)hqq_stream_recv(s->stream, &m, NULL);
    hqq_msg_release(m);
    lnk_retire(l, s);
    return NULL;
}

static void lnk_add_session(lnk *l, hqq_stream *st) {
    sqlsrv_session *s = sqlsrv_sess_new(l->sh, st, hqq_stream_id(st), l);
    sqlsrv_thread th;

    if (s == NULL) {
        (void)hqq_stream_close(st);
        return;
    }
    sqlsrv_mutex_lock(&l->mu);
    s->next = l->sessions;
    l->sessions = s;
    l->nsessions++;
    sqlsrv_cond_broadcast(&l->cv);
    sqlsrv_mutex_unlock(&l->mu);

    if (sqlsrv_thread_create(&th, watch_main, s) != 0) {
        lnk_retire(l, s);
        return;
    }
    sqlsrv_thread_detach(th);
}

/* Closes every stream of the link. Under the mutex, so no session is freed meanwhile. */
static void lnk_close_streams(lnk *l) {
    sqlsrv_session *s;
    sqlsrv_mutex_lock(&l->mu);
    for (s = l->sessions; s != NULL; s = s->next) {
        (void)hqq_stream_close(s->stream);
    }
    sqlsrv_mutex_unlock(&l->mu);
}

static void lnk_wait_empty(lnk *l) {
    sqlsrv_mutex_lock(&l->mu);
    while (l->nsessions > 0) {
        sqlsrv_cond_wait(&l->cv, &l->mu);
    }
    sqlsrv_mutex_unlock(&l->mu);
}

/*
 * The stream loop of a link: listens, accepts streams (each one a session),
 * and at the end drains the link: graceful shutdown when the server is
 * stopping, then the streams, the sessions, and the link itself.
 */
static void *link_accept_main(void *arg) {
    lnk *l = arg;
    hqq_status st;

    for (;;) {
        hqq_stream *ns = NULL;
        hqq_wait w;
        w.timeout_ns = LINK_POLL_MS * 1000000L;
        w.cancel = NULL;
        st = hqq_adv_accept_stream(l->adv, &ns, &w);
        if (st == HQQ_OK) {
            lnk_add_session(l, ns);
            continue;
        }
        if (st == HQQ_ERR_TIMEOUT && !atomic_load(&l->sh->shutting) && !atomic_load(&l->serve_done)) {
            continue;
        }
        break;
    }
    if (atomic_load(&l->sh->shutting)) {
        /* Refuses new requests and waits for the running ones (PROTOCOL.md section 1, shutdown). */
        hqq_wait gw;
        gw.timeout_ns = l->sh->shutdown_timeout_ns;
        gw.cancel = NULL;
        (void)hqq_adv_shutdown(l->adv, &gw);
    }
    lnk_close_streams(l);
    lnk_wait_empty(l);
    (void)hqq_adv_close(l->adv);
    return NULL;
}

/* One link: serves requests on this thread and runs the stream loop on another. */
static void *link_main(void *arg) {
    lnk *l = arg;
    sqlsrv_thread acc;

    if (sqlsrv_thread_create(&acc, link_accept_main, l) != 0) {
        fprintf(stderr, "hqq-sqlite-server: cannot start a link thread\n");
        (void)hqq_adv_close(l->adv);
        lnk_done(l);
        return NULL;
    }
    (void)hqq_adv_serve(l->adv, lnk_serve_request, l, NULL);
    atomic_store(&l->serve_done, 1);
    sqlsrv_thread_join(acc);
    lnk_done(l);
    return NULL;
}

static void link_start(sqlsrv_shared *sh, hqq_adv *adv) {
    lnk *l = calloc(1, sizeof *l);
    sqlsrv_thread th;

    if (l == NULL) {
        (void)hqq_adv_close(adv);
        return;
    }
    l->sh = sh;
    l->adv = adv;
    atomic_init(&l->serve_done, 0);
    if (sqlsrv_mutex_init(&l->mu) != 0) {
        free(l);
        (void)hqq_adv_close(adv);
        return;
    }
    if (sqlsrv_cond_init(&l->cv) != 0) {
        sqlsrv_mutex_destroy(&l->mu);
        free(l);
        (void)hqq_adv_close(adv);
        return;
    }
    /* Listening before the link is served: a stream open that arrives earlier is refused (ConnNotFound). */
    if (hqq_adv_listen_streams(adv) != HQQ_OK) {
        fprintf(stderr, "hqq-sqlite-server: ListenStreams failed on an accepted link\n");
        (void)hqq_adv_close(adv);
        lnk_free(l);
        return;
    }
    sqlsrv_mutex_lock(&sh->mu);
    sh->live_links++;
    sqlsrv_mutex_unlock(&sh->mu);
    if (sqlsrv_thread_create(&th, link_main, l) != 0) {
        (void)hqq_adv_close(adv);
        lnk_done(l);
        return;
    }
    sqlsrv_thread_detach(th);
}

static void *acceptor_main(void *arg) {
    sqlsrv_listener *ls = arg;

    for (;;) {
        hqq_adv *adv = NULL;
        hqq_status st = hqq_acceptor_accept(ls->acc, &adv, NULL);
        if (st == HQQ_OK) {
            link_start(ls->sh, adv);
            continue;
        }
        if (st == HQQ_ERR_ACCEPTOR_CLOSED || atomic_load(&ls->sh->shutting)) {
            break;
        }
        fprintf(stderr, "hqq-sqlite-server: accept: %s\n", hqq_strerror(st));
        /* A failed accept is retried after a pause; a persistent failure must not spin. */
        sqlsrv_sleep_ms(50);
    }
    return NULL;
}

int sqlsrv_listener_start(sqlsrv_shared *sh, const char *name, const sqlsrv_listen_options *o,
                          sqlsrv_listener **out, char *err, size_t errcap) {
    hqq_acceptor_options ao;
    hqq_open_info info;
    hqq_acceptor *acc = NULL;
    hqq_status st;
    sqlsrv_listener *ls;

    hqq_acceptor_options_init(&ao);
    hqq_options_init(&ao.link.link);
    ao.perm = o->perm;
    /* Both endpoints must allow 16 MiB messages (PROTOCOL.md section 2). */
    ao.link.max_message_size = SQLSRV_MAX_MESSAGE_BYTES;
    ao.link.link.heartbeat_interval_ns = o->heartbeat_ns;
    ao.link.link.peer_timeout_ns = o->peer_timeout_ns;
    hqq_open_info_init(&info);
    st = hqq_acceptor_create(name, &ao, &acc, &info);
    if (st != HQQ_OK) {
        (void)snprintf(err, errcap, "cannot create listener '%s': %s", name, hqq_strerror(st));
        return -1;
    }
    ls = calloc(1, sizeof *ls);
    if (ls == NULL) {
        (void)hqq_acceptor_close(acc);
        (void)snprintf(err, errcap, "out of memory");
        return -1;
    }
    ls->sh = sh;
    ls->acc = acc;
    if (sqlsrv_thread_create(&ls->th, acceptor_main, ls) != 0) {
        (void)hqq_acceptor_close(acc);
        free(ls);
        (void)snprintf(err, errcap, "cannot start the acceptor thread");
        return -1;
    }
    *out = ls;
    return 0;
}

void sqlsrv_listener_stop(sqlsrv_listener *ls) {
    atomic_store(&ls->sh->shutting, 1);
    (void)hqq_acceptor_close(ls->acc);
    sqlsrv_thread_join(ls->th);
    free(ls);
}

void sqlsrv_shared_wait_links(sqlsrv_shared *sh) {
    sqlsrv_mutex_lock(&sh->mu);
    while (sh->live_links > 0) {
        sqlsrv_cond_wait(&sh->cv, &sh->mu);
    }
    sqlsrv_mutex_unlock(&sh->mu);
}
