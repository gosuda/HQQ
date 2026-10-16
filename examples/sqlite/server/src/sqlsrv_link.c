#define _POSIX_C_SOURCE 200809L

#include "sqlsrv_link.h"

#include "hqq/hqq_listener.h"

#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

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
    pthread_mutex_t mu; /* guards sessions and nsessions; lock order: lnk.mu, then session mu */
    pthread_cond_t cv;  /* registrations, unlinks and session ends */
    sqlsrv_session *sessions;
    unsigned nsessions;
    atomic_int serve_done;
} lnk;

struct sqlsrv_listener {
    sqlsrv_shared *sh;
    hqq_acceptor *acc;
    pthread_t th;
};

static void timed_wait_link(pthread_cond_t *cv, pthread_mutex_t *mu, long ms) {
    struct timespec ts;
    clock_gettime(CLOCK_REALTIME, &ts);
    ts.tv_nsec += ms * 1000000L;
    if (ts.tv_nsec >= 1000000000L) {
        ts.tv_sec += 1;
        ts.tv_nsec -= 1000000000L;
    }
    (void)pthread_cond_timedwait(cv, mu, &ts);
}

static int ctx_done(const hqq_req_ctx *ctx) {
    return hqq_req_ctx_err(ctx) != HQQ_OK;
}

static void send_no_session(hqq_response *w) {
    static const char msg[] = "no session on this stream";
    (void)hqq_response_error(w, SQLSRV_STATUS_PROTO(SQLSRV_PE_NO_SESSION), msg, sizeof msg - 1);
}

static void lnk_free(lnk *l) {
    pthread_cond_destroy(&l->cv);
    pthread_mutex_destroy(&l->mu);
    free(l);
}

/* The end of a link that was counted in live_links. */
static void lnk_done(lnk *l) {
    sqlsrv_shared *sh = l->sh;
    lnk_free(l);
    pthread_mutex_lock(&sh->mu);
    sh->live_links--;
    pthread_cond_broadcast(&sh->cv);
    pthread_mutex_unlock(&sh->mu);
}

/* Session of a stream, referenced for the caller. Waits a while for an accept that is still in flight. */
static sqlsrv_session *lnk_lookup(lnk *l, uint64_t id, hqq_req_ctx *ctx) {
    unsigned polls = 0;
    sqlsrv_session *s;

    pthread_mutex_lock(&l->mu);
    for (;;) {
        for (s = l->sessions; s != NULL; s = s->next) {
            if (s->id == id) {
                sqlsrv_sess_ref(s);
                pthread_mutex_unlock(&l->mu);
                return s;
            }
        }
        if (ctx_done(ctx) || polls >= LOOKUP_MAX_POLLS) {
            pthread_mutex_unlock(&l->mu);
            return NULL;
        }
        polls++;
        timed_wait_link(&l->cv, &l->mu, LOOKUP_POLL_MS);
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

    pthread_mutex_lock(&l->mu);
    link = &l->sessions;
    while (*link != NULL && *link != s) {
        link = &(*link)->next;
    }
    if (*link == s) {
        *link = s->next;
    }
    pthread_mutex_unlock(&l->mu);

    sqlsrv_sess_wait_refs(s);

    /* After this decrement the link may be freed by its owner: l is not used again. */
    pthread_mutex_lock(&l->mu);
    l->nsessions--;
    pthread_cond_broadcast(&l->cv);
    pthread_mutex_unlock(&l->mu);

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
    pthread_t th;

    if (s == NULL) {
        (void)hqq_stream_close(st);
        return;
    }
    pthread_mutex_lock(&l->mu);
    s->next = l->sessions;
    l->sessions = s;
    l->nsessions++;
    pthread_cond_broadcast(&l->cv);
    pthread_mutex_unlock(&l->mu);

    if (pthread_create(&th, NULL, watch_main, s) != 0) {
        lnk_retire(l, s);
        return;
    }
    (void)pthread_detach(th);
}

/* Closes every stream of the link. Under the mutex, so no session is freed meanwhile. */
static void lnk_close_streams(lnk *l) {
    sqlsrv_session *s;
    pthread_mutex_lock(&l->mu);
    for (s = l->sessions; s != NULL; s = s->next) {
        (void)hqq_stream_close(s->stream);
    }
    pthread_mutex_unlock(&l->mu);
}

static void lnk_wait_empty(lnk *l) {
    pthread_mutex_lock(&l->mu);
    while (l->nsessions > 0) {
        pthread_cond_wait(&l->cv, &l->mu);
    }
    pthread_mutex_unlock(&l->mu);
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
    pthread_t acc;

    if (pthread_create(&acc, NULL, link_accept_main, l) != 0) {
        fprintf(stderr, "hqq-sqlite-server: cannot start a link thread\n");
        (void)hqq_adv_close(l->adv);
        lnk_done(l);
        return NULL;
    }
    (void)hqq_adv_serve(l->adv, lnk_serve_request, l, NULL);
    atomic_store(&l->serve_done, 1);
    (void)pthread_join(acc, NULL);
    lnk_done(l);
    return NULL;
}

static void link_start(sqlsrv_shared *sh, hqq_adv *adv) {
    lnk *l = calloc(1, sizeof *l);
    pthread_t th;

    if (l == NULL) {
        (void)hqq_adv_close(adv);
        return;
    }
    l->sh = sh;
    l->adv = adv;
    atomic_init(&l->serve_done, 0);
    if (pthread_mutex_init(&l->mu, NULL) != 0) {
        free(l);
        (void)hqq_adv_close(adv);
        return;
    }
    if (pthread_cond_init(&l->cv, NULL) != 0) {
        pthread_mutex_destroy(&l->mu);
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
    pthread_mutex_lock(&sh->mu);
    sh->live_links++;
    pthread_mutex_unlock(&sh->mu);
    if (pthread_create(&th, NULL, link_main, l) != 0) {
        (void)hqq_adv_close(adv);
        lnk_done(l);
        return;
    }
    (void)pthread_detach(th);
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
        struct timespec pause = {0, 50 * 1000000L};
        (void)nanosleep(&pause, NULL);
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
    if (pthread_create(&ls->th, NULL, acceptor_main, ls) != 0) {
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
    (void)pthread_join(ls->th, NULL);
    free(ls);
}

void sqlsrv_shared_wait_links(sqlsrv_shared *sh) {
    pthread_mutex_lock(&sh->mu);
    while (sh->live_links > 0) {
        pthread_cond_wait(&sh->cv, &sh->mu);
    }
    pthread_mutex_unlock(&sh->mu);
}
