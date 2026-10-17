/* hqq_adv_serve: where a handler runs and what that must not break.
 *
 * Serve takes the model of the Go Serve. While no handler runs, the dispatcher thread of the link
 * runs the next request itself (no thread hand-off: a Go client measured 33 us per call when every
 * request went through a worker thread, 1 us inline). A handler that holds the receive path longer
 * than the link's inline limit is escalated: another thread takes over receiving, so cancels,
 * responses and further requests are still processed, and the handler's thread ends after it.
 *
 * Each scenario builds a negotiated pair on caller-owned memory and drives the secondary, which
 * serves, from the primary. The verdicts come from the link's internal Serve counters, the handlers'
 * own observations and call results; no scenario sleeps to synchronize. The one-sided "has not
 * returned yet" checks wait a few milliseconds and can only miss a bug, never fail a correct build.
 * Every wait is bounded by a watchdog. The counting allocator checks that no block of either link
 * (thread handles of retired dispatcher threads included) is left after close.
 */
#include <stdatomic.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "hqq/hqq_advanced.h"
#include "hqq_adv_internal.h"
#include "hqq_test.h"
#include "hqq_testutil.h"

#define COUNT 8u
#define SIZE 64u
#define WATCHDOG_MS 10000u
#define WATCHDOG_NS (INT64_C(10) * 1000 * 1000 * 1000)
#define NO_ESCALATION_NS (UINT64_C(60) * 1000 * 1000 * 1000) /* an inline limit no test reaches */

static _Atomic long live_blocks;

static void *count_alloc(void *ctx, size_t n) {
    void *p = malloc(n == 0 ? 1 : n);

    (void)ctx;
    if (p != NULL) {
        atomic_fetch_add(&live_blocks, 1);
    }
    return p;
}

static void count_free(void *ctx, void *p, size_t n) {
    (void)ctx;
    (void)n;
    if (p != NULL) {
        atomic_fetch_sub(&live_blocks, 1);
    }
    free(p);
}

static const hqq_allocator counting = {NULL, count_alloc, count_free};

/* ---- Fixture ---------------------------------------------------------------------------- */

typedef struct pair {
    unsigned char *mem;
    size_t size;
    hqq_adv *p; /* the caller */
    hqq_adv *s; /* the server */
} pair;

typedef struct neg_arg {
    hqq_adv *a;
    hqq_status st;
} neg_arg;

static void neg_secondary(void *arg) {
    neg_arg *n = (neg_arg *)arg;
    hqq_wait w = {3000000000LL, NULL};

    n->st = hqq_adv_negotiate(n->a, 0, &w);
}

/* A negotiated pair whose server raises its inline limit to `limit_ns` (0 keeps the default). */
static void pair_open(pair *pr, uint64_t limit_ns) {
    hqq_adv_options o;
    neg_arg n;
    hqq_test_thread *t;
    hqq_wait w = {3000000000LL, NULL};

    atomic_store(&live_blocks, 0);
    memset(pr, 0, sizeof *pr);
    hqq_adv_options_init(&o);
    o.link.heartbeat_interval_ns = 20 * 1000000;
    o.link.peer_timeout_ns = 2000 * 1000000;
    o.link.allocator = &counting;
    pr->size = hqq_standard_size(COUNT, SIZE);
    pr->mem = (unsigned char *)hqq_test_pages(pr->size);
    HQQ_REQUIRE(hqq_adv_open_memory(pr->mem, pr->size, COUNT, SIZE, &o, &pr->p, NULL) == HQQ_OK);
    HQQ_REQUIRE(hqq_adv_open_memory(pr->mem, pr->size, COUNT, SIZE, &o, &pr->s, NULL) == HQQ_OK);
    n.a = pr->s;
    n.st = HQQ_OK;
    t = hqq_test_thread_start(neg_secondary, &n);
    HQQ_REQUIRE(hqq_adv_negotiate(pr->p, 0, &w) == HQQ_OK);
    hqq_test_thread_join(t);
    HQQ_REQUIRE(n.st == HQQ_OK);
    if (limit_ns != 0) {
        pr->s->inline_limit_ns = limit_ns; /* before Serve starts its timer thread */
    }
}

static void pair_close(pair *pr) {
    if (pr->s != NULL) {
        HQQ_CHECK_STATUS(hqq_adv_close(pr->s), HQQ_OK);
    }
    HQQ_CHECK_STATUS(hqq_adv_close(pr->p), HQQ_OK);
    hqq_test_pages_free(pr->mem, pr->size);
    HQQ_CHECK_EQ_INT(atomic_load(&live_blocks), 0);
}

typedef struct counters {
    uint64_t inlined, pooled, escalated, workers, esc_busy;
    int retired_listed;
} counters;

static counters counters_of(hqq_adv *a) {
    counters c;

    hqq_spin_lock(&a->lock);
    c.inlined = a->sv_inline;
    c.pooled = a->sv_pooled;
    c.escalated = a->sv_escalated;
    c.workers = a->workers_total;
    c.esc_busy = a->esc_busy;
    c.retired_listed = a->retired != NULL;
    hqq_spin_unlock(&a->lock);
    return c;
}

/* Polls until *flag is non-zero; 0 on watchdog expiry. */
static int wait_flag(_Atomic int *flag) {
    unsigned ms;

    for (ms = 0; ms < WATCHDOG_MS; ms++) {
        if (atomic_load(flag) != 0) {
            return 1;
        }
        hqq_test_sleep_ms(1);
    }
    return atomic_load(flag) != 0;
}

static int wait_escalated(hqq_adv *a, uint64_t want) {
    unsigned ms;

    for (ms = 0; ms < WATCHDOG_MS; ms++) {
        if (counters_of(a).escalated >= want) {
            return 1;
        }
        hqq_test_sleep_ms(1);
    }
    return counters_of(a).escalated >= want;
}

/* ---- Threads: Serve and callers --------------------------------------------------------- */

typedef struct serve_run {
    hqq_adv *a;
    hqq_handler_fn fn;
    void *user;
    hqq_wait w;
    hqq_status st;
    _Atomic int done;
    hqq_test_thread *t;
} serve_run;

static void serve_main(void *arg) {
    serve_run *r = (serve_run *)arg;

    r->st = hqq_adv_serve(r->a, r->fn, r->user, &r->w);
    atomic_store(&r->done, 1);
}

static int is_serving(hqq_adv *a) {
    int serving;

    hqq_spin_lock(&a->lock);
    serving = a->serving;
    hqq_spin_unlock(&a->lock);
    return serving;
}

/* Starts Serve on its own thread and returns once it is serving: a request that arrived earlier
 * would be queued for a worker instead of running inline. */
static void serve_start(serve_run *r, hqq_adv *a, hqq_handler_fn fn, void *user, const hqq_cancel *cancel) {
    unsigned ms;

    r->a = a;
    r->fn = fn;
    r->user = user;
    r->w.timeout_ns = -1;
    r->w.cancel = cancel;
    r->st = HQQ_OK;
    atomic_store(&r->done, 0);
    r->t = hqq_test_thread_start(serve_main, r);
    for (ms = 0; ms < WATCHDOG_MS && !is_serving(a); ms++) {
        hqq_test_sleep_ms(1);
    }
    HQQ_REQUIRE(is_serving(a));
}

typedef struct call_run {
    hqq_adv *a;
    uint64_t method;
    int64_t timeout_ns;
    const hqq_cancel *cancel;
    hqq_status st;
    uint64_t resp_len;
    hqq_test_thread *t;
} call_run;

static void call_main(void *arg) {
    call_run *c = (call_run *)arg;
    hqq_msg *resp = NULL;
    hqq_wait w;

    w.timeout_ns = c->timeout_ns;
    w.cancel = c->cancel;
    c->st = hqq_stream_call(hqq_adv_stream0(c->a), c->method, "payload", 7, &resp, &w);
    if (resp != NULL) {
        c->resp_len = hqq_msg_len(resp);
        hqq_msg_release(resp);
    }
}

/* A call that gives up when `cancel` is signalled (NULL: when the watchdog expires). */
static void call_start(call_run *c, hqq_adv *a, uint64_t method, const hqq_cancel *cancel) {
    c->a = a;
    c->method = method;
    c->timeout_ns = cancel != NULL ? -1 : WATCHDOG_NS;
    c->cancel = cancel;
    c->st = HQQ_OK;
    c->resp_len = 0;
    c->t = hqq_test_thread_start(call_main, c);
}

/* A sequential call that must return the payload. */
static void call_echo(hqq_adv *a, uint64_t method) {
    call_run c;

    call_start(&c, a, method, NULL);
    hqq_test_thread_join(c.t);
    HQQ_CHECK_STATUS(c.st, HQQ_OK);
    HQQ_CHECK_EQ_U64(c.resp_len, 7);
}

/* ---- Handlers ---------------------------------------------------------------------------- */

typedef struct scene {
    _Atomic int entered;   /* handlers started */
    _Atomic int gate;      /* the test, or a later handler, opens it */
    _Atomic int returned;  /* handlers returned */
    _Atomic int ctx_err;   /* hqq_req_ctx_err seen after the context ended */
    _Atomic int inside;    /* handlers inside the barrier */
    hqq_adv *self;         /* the link the handlers run on, for M_NEST */
    _Atomic int nested;    /* status of the call M_NEST made on the link, plus one */
} scene;

enum { M_ECHO = 1, M_BLOCK = 2, M_OPEN = 3, M_CTX = 4, M_BARRIER = 5, M_NEST = 6 };

static void echo(hqq_response *w, const hqq_request *req) {
    (void)hqq_response_write(w, hqq_request_data(req), hqq_request_len(req));
}

static void gate_wait(scene *sc) {
    unsigned ms;

    for (ms = 0; ms < WATCHDOG_MS && atomic_load(&sc->gate) == 0; ms++) {
        hqq_test_sleep_ms(1);
    }
}

static void scene_handler(void *user, hqq_req_ctx *ctx, hqq_response *w, const hqq_request *req) {
    scene *sc = (scene *)user;

    switch (hqq_request_method(req)) {
    case M_BLOCK: /* blocks until the gate opens */
        atomic_fetch_add(&sc->entered, 1);
        gate_wait(sc);
        break;
    case M_OPEN: /* opens the gate: only a request received after M_BLOCK started can run it */
        atomic_fetch_add(&sc->entered, 1);
        atomic_store(&sc->gate, 1);
        break;
    case M_CTX: /* blocks until the request context ends, then until the gate opens */
        atomic_fetch_add(&sc->entered, 1);
        (void)hqq_req_ctx_wait_done(ctx, WATCHDOG_NS);
        atomic_store(&sc->ctx_err, (int)hqq_req_ctx_err(ctx));
        gate_wait(sc);
        break;
    case M_BARRIER: /* returns when COUNT/2 handlers are inside at once */
        atomic_fetch_add(&sc->entered, 1);
        atomic_fetch_add(&sc->inside, 1);
        {
            unsigned ms;

            for (ms = 0; ms < WATCHDOG_MS && atomic_load(&sc->inside) < (int)(COUNT / 2); ms++) {
                hqq_test_sleep_ms(1);
            }
        }
        break;
    case M_NEST: /* calls the peer on the same link and waits for the answer */
        atomic_fetch_add(&sc->entered, 1);
        {
            hqq_msg *resp = NULL;
            hqq_wait bounded = {WATCHDOG_NS, NULL};
            hqq_status st = hqq_stream_call(hqq_adv_stream0(sc->self), M_ECHO, "nested", 6, &resp, &bounded);

            atomic_store(&sc->nested, (int)st + 1);
            hqq_msg_release(resp);
        }
        break;
    default:
        break;
    }
    atomic_fetch_add(&sc->returned, 1);
    echo(w, req);
}

/* ---- Scenarios --------------------------------------------------------------------------- */

/* Sequential requests run on the dispatcher thread: no worker is ever started and no request is
 * queued. A Serve that hands every request to a worker thread fails the counters (and costs a
 * thread wake-up, tens of microseconds, per call). */
static void sequential_requests_run_inline(void) {
    pair pr;
    serve_run sv;
    hqq_cancel stop;
    scene sc;
    counters c;
    int i;

    pair_open(&pr, NO_ESCALATION_NS);
    memset(&sc, 0, sizeof sc);
    hqq_cancel_init(&stop);
    serve_start(&sv, pr.s, scene_handler, &sc, &stop);
    for (i = 0; i < 50; i++) {
        call_echo(pr.p, M_ECHO);
    }
    c = counters_of(pr.s);
    HQQ_CHECK_EQ_U64(c.inlined, 50);
    HQQ_CHECK_EQ_U64(c.pooled, 0);
    HQQ_CHECK_EQ_U64(c.escalated, 0);
    HQQ_CHECK_EQ_U64(c.workers, 0);
    hqq_cancel_signal(&stop);
    hqq_test_thread_join(sv.t);
    HQQ_CHECK_STATUS(sv.st, HQQ_ERR_CANCELED);
    pair_close(&pr);
}

/* A handler that waits for a request that is received later must not deadlock Serve: the watchdog
 * moves the receive role to another thread, the later request runs on a worker and opens the gate.
 * Afterwards the link is back to inline execution (the switch lasts one episode). */
static void handler_waiting_for_later_request_escalates(void) {
    pair pr;
    serve_run sv;
    hqq_cancel stop;
    scene sc;
    call_run blocked, opener;
    counters before, after;
    int i;

    pair_open(&pr, 0);
    memset(&sc, 0, sizeof sc);
    hqq_cancel_init(&stop);
    serve_start(&sv, pr.s, scene_handler, &sc, &stop);

    call_start(&blocked, pr.p, M_BLOCK, NULL);
    HQQ_REQUIRE(wait_flag(&sc.entered)); /* the handler runs, inline, on the dispatcher thread */
    call_start(&opener, pr.p, M_OPEN, NULL);
    hqq_test_thread_join(opener.t);
    hqq_test_thread_join(blocked.t);
    HQQ_CHECK_STATUS(opener.st, HQQ_OK);
    HQQ_CHECK_STATUS(blocked.st, HQQ_OK);
    HQQ_CHECK_EQ_INT(atomic_load(&sc.returned), 2);

    before = counters_of(pr.s);
    HQQ_CHECK(before.escalated >= 1);
    HQQ_CHECK_EQ_U64(before.esc_busy, 0);
    for (i = 0; i < 20; i++) {
        call_echo(pr.p, M_ECHO);
    }
    after = counters_of(pr.s);
    HQQ_CHECK_EQ_U64(after.inlined - before.inlined, 20);
    HQQ_CHECK_EQ_U64(after.pooled, before.pooled);

    hqq_cancel_signal(&stop);
    hqq_test_thread_join(sv.t);
    HQQ_CHECK_STATUS(sv.st, HQQ_ERR_CANCELED);
    HQQ_CHECK(!counters_of(pr.s).retired_listed);
    pair_close(&pr);
}

/* A handler that calls the peer on the same link and waits for the answer completes: the answer is
 * received by the replacement dispatcher thread. Both ends serve. */
static void handler_calling_peer_completes(void) {
    pair pr;
    serve_run server, client;
    hqq_cancel stop_server, stop_client;
    scene sc_server, sc_client;

    pair_open(&pr, 0);
    memset(&sc_server, 0, sizeof sc_server);
    memset(&sc_client, 0, sizeof sc_client);
    sc_server.self = pr.s;
    sc_client.self = pr.p;
    hqq_cancel_init(&stop_server);
    hqq_cancel_init(&stop_client);
    serve_start(&server, pr.s, scene_handler, &sc_server, &stop_server);
    serve_start(&client, pr.p, scene_handler, &sc_client, &stop_client);

    call_echo(pr.p, M_NEST);
    HQQ_CHECK_EQ_INT(atomic_load(&sc_server.nested), (int)HQQ_OK + 1);
    HQQ_CHECK_EQ_INT(atomic_load(&sc_client.returned), 1); /* the peer's handler answered the nested call */
    HQQ_CHECK(counters_of(pr.s).escalated >= 1);

    hqq_cancel_signal(&stop_server);
    hqq_cancel_signal(&stop_client);
    hqq_test_thread_join(server.t);
    hqq_test_thread_join(client.t);
    pair_close(&pr);
}

/* A cancel that arrives while the handler runs inline reaches it: the handler holds the dispatcher
 * thread, so the cancel is only received after the escalation. */
static void cancel_reaches_inline_handler(void) {
    pair pr;
    serve_run sv;
    hqq_cancel stop, abandon;
    scene sc;
    call_run c;
    hqq_adv_stats st;

    pair_open(&pr, 0);
    memset(&sc, 0, sizeof sc);
    hqq_cancel_init(&stop);
    hqq_cancel_init(&abandon);
    serve_start(&sv, pr.s, scene_handler, &sc, &stop);

    call_start(&c, pr.p, M_CTX, &abandon);
    HQQ_REQUIRE(wait_flag(&sc.entered));
    hqq_cancel_signal(&abandon); /* the caller gives up: OpCancel goes to the server */
    hqq_test_thread_join(c.t);
    HQQ_CHECK_STATUS(c.st, HQQ_ERR_CANCELED);
    atomic_store(&sc.gate, 1);
    HQQ_CHECK(wait_flag(&sc.returned));
    HQQ_CHECK_EQ_INT(atomic_load(&sc.ctx_err), HQQ_ERR_CANCELED);
    hqq_adv_stats_get(pr.s, &st);
    HQQ_CHECK_EQ_U64(st.canceled_requests, 1);

    hqq_cancel_signal(&stop);
    hqq_test_thread_join(sv.t);
    pair_close(&pr);
}

/* The budget of an inline request still ends its context (the timer thread, not the dispatcher,
 * watches deadlines). */
static void deadline_ends_inline_handler(void) {
    pair pr;
    serve_run sv;
    hqq_cancel stop;
    scene sc;
    call_run c;

    pair_open(&pr, NO_ESCALATION_NS);
    memset(&sc, 0, sizeof sc);
    hqq_cancel_init(&stop);
    serve_start(&sv, pr.s, scene_handler, &sc, &stop);

    /* A 30 ms wait: the remaining time travels to the server as the request's budget. */
    c.a = pr.p;
    c.method = M_CTX;
    c.timeout_ns = 30 * 1000000;
    c.cancel = NULL;
    c.st = HQQ_OK;
    c.resp_len = 0;
    c.t = hqq_test_thread_start(call_main, &c);
    hqq_test_thread_join(c.t);
    atomic_store(&sc.gate, 1); /* the handler may return once its context ended */
    HQQ_CHECK_STATUS(c.st, HQQ_ERR_TIMEOUT);
    HQQ_CHECK(wait_flag(&sc.returned));
    HQQ_CHECK_EQ_INT(atomic_load(&sc.ctx_err), HQQ_ERR_TIMEOUT);

    hqq_cancel_signal(&stop);
    hqq_test_thread_join(sv.t);
    pair_close(&pr);
}

/* Serve does not return while a handler still runs, inline or escalated: the handler's `user` data
 * must stay valid until Serve returns. */
static void serve_end_waits_for_running_handler(uint64_t limit_ns) {
    pair pr;
    serve_run sv;
    hqq_cancel stop;
    scene sc;
    call_run c;
    unsigned ms;

    pair_open(&pr, limit_ns);
    memset(&sc, 0, sizeof sc);
    hqq_cancel_init(&stop);
    serve_start(&sv, pr.s, scene_handler, &sc, &stop);

    call_start(&c, pr.p, M_CTX, NULL);
    HQQ_REQUIRE(wait_flag(&sc.entered));
    hqq_cancel_signal(&stop); /* Serve ends: the context of the request ends, the handler waits for the gate */
    HQQ_REQUIRE(wait_flag(&sc.ctx_err));
    for (ms = 0; ms < 50; ms++) {
        HQQ_CHECK(atomic_load(&sv.done) == 0);
        hqq_test_sleep_ms(1);
    }
    atomic_store(&sc.gate, 1);
    hqq_test_thread_join(sv.t);
    HQQ_CHECK_EQ_INT(atomic_load(&sc.returned), 1);
    HQQ_CHECK_STATUS(sv.st, HQQ_ERR_CANCELED);
    hqq_test_thread_join(c.t);
    HQQ_CHECK(!counters_of(pr.s).retired_listed);
    pair_close(&pr);
}

/* Shutdown waits for the request an inline handler is running, and Serve ends cleanly after it. */
static void shutdown_waits_for_inline_handler(void) {
    pair pr;
    serve_run sv;
    scene sc;
    call_run c;
    hqq_wait brief = {50 * 1000000LL, NULL};
    hqq_wait bounded = {WATCHDOG_NS, NULL};

    pair_open(&pr, NO_ESCALATION_NS);
    memset(&sc, 0, sizeof sc);
    serve_start(&sv, pr.s, scene_handler, &sc, NULL);

    call_start(&c, pr.p, M_BLOCK, NULL);
    HQQ_REQUIRE(wait_flag(&sc.entered));
    HQQ_CHECK_STATUS(hqq_adv_shutdown(pr.s, &brief), HQQ_ERR_TIMEOUT); /* the request is in flight */
    atomic_store(&sc.gate, 1);
    HQQ_CHECK_STATUS(hqq_adv_shutdown(pr.s, &bounded), HQQ_OK);
    hqq_test_thread_join(c.t);
    HQQ_CHECK_STATUS(c.st, HQQ_OK);
    hqq_test_thread_join(sv.t);
    HQQ_CHECK_STATUS(sv.st, HQQ_OK);
    pair_close(&pr);
}

/* Handlers run concurrently: COUNT/2 callers meet inside their handlers (the first runs inline until
 * it is escalated, the others on workers). */
static void handlers_run_concurrently(void) {
    pair pr;
    serve_run sv;
    hqq_cancel stop;
    scene sc;
    call_run cs[COUNT / 2];
    unsigned i;

    pair_open(&pr, 0);
    memset(&sc, 0, sizeof sc);
    hqq_cancel_init(&stop);
    serve_start(&sv, pr.s, scene_handler, &sc, &stop);
    for (i = 0; i < COUNT / 2; i++) {
        call_start(&cs[i], pr.p, M_BARRIER, NULL);
    }
    for (i = 0; i < COUNT / 2; i++) {
        hqq_test_thread_join(cs[i].t);
        HQQ_CHECK_STATUS(cs[i].st, HQQ_OK);
    }
    HQQ_CHECK_EQ_INT(atomic_load(&sc.inside), (int)(COUNT / 2));
    hqq_cancel_signal(&stop);
    hqq_test_thread_join(sv.t);
    pair_close(&pr);
}

/* Repeated escalations: each ends in a retired dispatcher thread that is joined (the counting
 * allocator sees a leaked thread handle), and the receive path keeps working. */
static void repeated_escalations_are_joined(void) {
    pair pr;
    serve_run sv;
    hqq_cancel stop;
    scene sc;
    int round;

    pair_open(&pr, 0);
    memset(&sc, 0, sizeof sc);
    hqq_cancel_init(&stop);
    serve_start(&sv, pr.s, scene_handler, &sc, &stop);
    for (round = 1; round <= 5; round++) {
        call_run c;

        atomic_store(&sc.gate, 0);
        call_start(&c, pr.p, M_BLOCK, NULL);
        HQQ_REQUIRE(wait_escalated(pr.s, (uint64_t)round));
        atomic_store(&sc.gate, 1);
        hqq_test_thread_join(c.t);
        HQQ_CHECK_STATUS(c.st, HQQ_OK);
        call_echo(pr.p, M_ECHO); /* the new dispatcher thread receives and answers */
    }
    hqq_cancel_signal(&stop);
    hqq_test_thread_join(sv.t);
    HQQ_CHECK(!counters_of(pr.s).retired_listed);
    HQQ_CHECK_EQ_U64(counters_of(pr.s).esc_busy, 0);
    pair_close(&pr);
}

/* Closing the link while a handler runs, inline or on a retired dispatcher thread, cancels its
 * context and returns; Serve returns HQQ_ERR_CLOSED. */
static void close_ends_running_handler(uint64_t limit_ns, int wait_for_escalation) {
    pair pr;
    serve_run sv;
    scene sc;
    call_run c;

    pair_open(&pr, limit_ns);
    memset(&sc, 0, sizeof sc);
    serve_start(&sv, pr.s, scene_handler, &sc, NULL);
    call_start(&c, pr.p, M_CTX, NULL);
    HQQ_REQUIRE(wait_flag(&sc.entered));
    if (wait_for_escalation) {
        HQQ_REQUIRE(wait_escalated(pr.s, 1));
    }
    atomic_store(&sc.gate, 1);
    HQQ_CHECK_STATUS(hqq_adv_close(pr.s), HQQ_OK);
    pr.s = NULL;
    hqq_test_thread_join(sv.t);
    HQQ_CHECK_STATUS(sv.st, HQQ_ERR_CLOSED);
    HQQ_CHECK_EQ_INT(atomic_load(&sc.ctx_err), HQQ_ERR_CANCELED);
    hqq_test_thread_join(c.t);
    pair_close(&pr);
}

/* A request that arrives while nobody serves is queued; a later Serve answers it from a worker, and
 * a Serve that ended leaves no inline state behind for the next one. */
static void serve_restarts_and_answers_queued_request(void) {
    pair pr;
    serve_run sv;
    hqq_cancel stop1, stop2;
    scene sc;
    call_run waiting;
    counters c;

    pair_open(&pr, NO_ESCALATION_NS);
    memset(&sc, 0, sizeof sc);
    hqq_cancel_init(&stop1);
    hqq_cancel_init(&stop2);
    serve_start(&sv, pr.s, scene_handler, &sc, &stop1);
    call_echo(pr.p, M_ECHO);
    hqq_cancel_signal(&stop1);
    hqq_test_thread_join(sv.t);
    HQQ_CHECK_STATUS(sv.st, HQQ_ERR_CANCELED);

    call_start(&waiting, pr.p, M_ECHO, NULL);
    {
        unsigned ms;

        for (ms = 0; ms < WATCHDOG_MS && counters_of(pr.s).pooled == 0; ms++) {
            hqq_test_sleep_ms(1);
        }
    }
    serve_start(&sv, pr.s, scene_handler, &sc, &stop2);
    hqq_test_thread_join(waiting.t);
    HQQ_CHECK_STATUS(waiting.st, HQQ_OK);
    call_echo(pr.p, M_ECHO);
    c = counters_of(pr.s);
    HQQ_CHECK_EQ_U64(c.inlined, 2);
    HQQ_CHECK_EQ_U64(c.pooled, 1);
    hqq_cancel_signal(&stop2);
    hqq_test_thread_join(sv.t);
    HQQ_CHECK_STATUS(sv.st, HQQ_ERR_CANCELED);
    pair_close(&pr);
}

int main(void) {
    sequential_requests_run_inline();
    handler_waiting_for_later_request_escalates();
    handler_calling_peer_completes();
    cancel_reaches_inline_handler();
    deadline_ends_inline_handler();
    serve_end_waits_for_running_handler(NO_ESCALATION_NS);
    serve_end_waits_for_running_handler(0);
    shutdown_waits_for_inline_handler();
    handlers_run_concurrently();
    repeated_escalations_are_joined();
    close_ends_running_handler(NO_ESCALATION_NS, 0);
    close_ends_running_handler(0, 1);
    serve_restarts_and_answers_queued_request();
    return HQQ_TEST_RESULT("test_adv_serve");
}
