/* Stream requests that arrive before ListenStreams (SPEC 10.3): held for the grace
 * period, admitted in arrival order by ListenStreams, refused when the grace period
 * ends or the bound is reached, refused at once with a negative grace, and released
 * by close and by a failing link. The held requests are observed through the link
 * internals (hqq_adv_internal.h), so this test is linked INTERNAL. */
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "hqq/hqq_advanced.h"
#include "hqq_adv_internal.h"
#include "hqq_test.h"
#include "hqq_testutil.h"

#define LN_COUNT 8u
#define LN_SIZE 64u
#define LN_SEC 1000000000LL

typedef struct ln_pair {
    unsigned char *mem;
    size_t size;
    hqq_adv *p;
    hqq_adv *s;
} ln_pair;

/* The primary (creator, even ids) and the secondary (the listening side). The
 * secondary holds requests with the given queue size and grace (negative: refuse). */
static void ln_open(ln_pair *pr, uint64_t queue_size, int64_t grace_ns) {
    hqq_adv_options po;
    hqq_adv_options so;
    hqq_adv_options_init(&po);
    po.link.heartbeat_interval_ns = 20 * 1000000;
    po.link.peer_timeout_ns = 2000 * 1000000;
    so = po;
    so.queue_size = queue_size;
    so.stream_listen_grace_ns = grace_ns;
    pr->size = hqq_standard_size(LN_COUNT, LN_SIZE);
    pr->mem = (unsigned char *)hqq_test_pages(pr->size);
    HQQ_REQUIRE(hqq_adv_open_memory(pr->mem, pr->size, LN_COUNT, LN_SIZE, &po, &pr->p, NULL) == HQQ_OK);
    HQQ_REQUIRE(hqq_adv_open_memory(pr->mem, pr->size, LN_COUNT, LN_SIZE, &so, &pr->s, NULL) == HQQ_OK);
}

static void ln_close(ln_pair *pr) {
    if (pr->s != NULL) {
        (void)hqq_adv_close(pr->s);
    }
    if (pr->p != NULL) {
        (void)hqq_adv_close(pr->p);
    }
    hqq_test_pages_free(pr->mem, pr->size);
    memset(pr, 0, sizeof *pr);
}

typedef struct ln_neg {
    hqq_adv *a;
    hqq_status st;
} ln_neg;

static void ln_neg_thread(void *arg) {
    ln_neg *n = (ln_neg *)arg;
    hqq_wait w = {3 * LN_SEC, NULL};
    n->st = hqq_adv_negotiate(n->a, HQQ_FEATURES_ALL, &w);
}

static void ln_negotiate(ln_pair *pr) {
    ln_neg n;
    hqq_test_thread *t;
    hqq_wait w = {3 * LN_SEC, NULL};
    hqq_status st;
    n.a = pr->s;
    n.st = HQQ_OK;
    t = hqq_test_thread_start(ln_neg_thread, &n);
    st = hqq_adv_negotiate(pr->p, HQQ_FEATURES_ALL, &w);
    hqq_test_thread_join(t);
    HQQ_CHECK_STATUS(st, HQQ_OK);
    HQQ_CHECK_STATUS(n.st, HQQ_OK);
}

/* One OpenStream on its own thread: the request is held until the test listens. */
typedef struct ln_open_arg {
    hqq_adv *a;
    hqq_wait w;
    hqq_stream *out;
    hqq_status st;
} ln_open_arg;

static void ln_open_thread(void *arg) {
    ln_open_arg *o = (ln_open_arg *)arg;
    o->st = hqq_adv_open_stream(o->a, &o->out, &o->w);
}

static void ln_open_start(ln_open_arg *o, hqq_test_thread **t, hqq_adv *a) {
    memset(o, 0, sizeof *o);
    o->a = a;
    o->w.timeout_ns = 10 * LN_SEC;
    o->w.cancel = NULL;
    o->st = HQQ_OK;
    *t = hqq_test_thread_start(ln_open_thread, o);
}

/* Number of requests the link holds, read under its lock. */
static uint64_t ln_held(hqq_adv *a) {
    uint64_t n;
    hqq_spin_lock(&a->lock);
    n = a->held_count;
    hqq_spin_unlock(&a->lock);
    return n;
}

/* Waits until the link holds n requests (polls its state; no fixed delay as sync). */
static void ln_wait_held(hqq_adv *a, uint64_t n) {
    int i;
    for (i = 0; i < 5000 && ln_held(a) != n; i++) {
        hqq_plat_sleep_ns(1000000);
    }
    HQQ_CHECK(ln_held(a) == n);
}

static hqq_wait ln_wait(uint64_t ns) {
    hqq_wait w;
    w.timeout_ns = (int64_t)ns;
    w.cancel = NULL;
    return w;
}

/* A request held until ListenStreams is accepted by it, with the next ID. */
static void check_held_until_listen(void) {
    ln_pair pr;
    ln_open_arg o;
    hqq_test_thread *t;
    hqq_stream *ss = NULL;
    hqq_wait w = ln_wait(10 * LN_SEC);

    ln_open(&pr, LN_COUNT, 60 * LN_SEC);
    ln_negotiate(&pr);
    ln_open_start(&o, &t, pr.p);
    ln_wait_held(pr.s, 1);
    HQQ_CHECK_STATUS(hqq_adv_listen_streams(pr.s), HQQ_OK);
    hqq_test_thread_join(t);
    HQQ_CHECK_STATUS(o.st, HQQ_OK);
    HQQ_CHECK(o.out != NULL && hqq_stream_id(o.out) == 2);
    HQQ_CHECK_STATUS(hqq_adv_accept_stream(pr.s, &ss, &w), HQQ_OK);
    HQQ_CHECK(ss != NULL && hqq_stream_id(ss) == 2);
    HQQ_CHECK(ln_held(pr.s) == 0);
    ln_close(&pr);
}

/* The grace period ends before ListenStreams: the request is refused, and its ID
 * is not reused. */
static void check_refused_after_grace(void) {
    ln_pair pr;
    hqq_stream *st = NULL;
    hqq_stream *ss = NULL;
    hqq_wait w = ln_wait(10 * LN_SEC);

    ln_open(&pr, LN_COUNT, 50 * 1000000LL);
    ln_negotiate(&pr);
    HQQ_CHECK_STATUS(hqq_adv_open_stream(pr.p, &st, &w), HQQ_ERR_CONN_NOT_FOUND);
    HQQ_CHECK(st == NULL);
    HQQ_CHECK(ln_held(pr.s) == 0);
    HQQ_CHECK_STATUS(hqq_adv_listen_streams(pr.s), HQQ_OK);
    HQQ_CHECK_STATUS(hqq_adv_open_stream(pr.p, &st, &w), HQQ_OK);
    HQQ_CHECK(hqq_stream_id(st) == 4);
    HQQ_CHECK_STATUS(hqq_adv_accept_stream(pr.s, &ss, &w), HQQ_OK);
    HQQ_CHECK(hqq_stream_id(ss) == 4);
    ln_close(&pr);
}

/* With queue size 2 the third request finds the bound reached and is refused at
 * once, while the first two are still held; they are then accepted in order. */
static void check_bound_refused_at_once(void) {
    ln_pair pr;
    ln_open_arg o1;
    ln_open_arg o2;
    hqq_test_thread *t1;
    hqq_test_thread *t2;
    hqq_stream *st3 = NULL;
    hqq_stream *ss = NULL;
    hqq_wait w = ln_wait(10 * LN_SEC);
    int i;

    ln_open(&pr, 2, 60 * LN_SEC);
    ln_negotiate(&pr);
    ln_open_start(&o1, &t1, pr.p);
    ln_wait_held(pr.s, 1);
    ln_open_start(&o2, &t2, pr.p);
    ln_wait_held(pr.s, 2);
    HQQ_CHECK_STATUS(hqq_adv_open_stream(pr.p, &st3, &w), HQQ_ERR_CONN_NOT_FOUND);
    HQQ_CHECK(ln_held(pr.s) == 2);
    HQQ_CHECK_STATUS(hqq_adv_listen_streams(pr.s), HQQ_OK);
    hqq_test_thread_join(t1);
    hqq_test_thread_join(t2);
    HQQ_CHECK_STATUS(o1.st, HQQ_OK);
    HQQ_CHECK_STATUS(o2.st, HQQ_OK);
    HQQ_CHECK(hqq_stream_id(o1.out) == 2 && hqq_stream_id(o2.out) == 4);
    for (i = 0; i < 2; i++) {
        HQQ_CHECK_STATUS(hqq_adv_accept_stream(pr.s, &ss, &w), HQQ_OK);
        HQQ_CHECK(hqq_stream_id(ss) == (i == 0 ? 2u : 4u));
    }
    ln_close(&pr);
}

/* A negative grace refuses at once: nothing is held and the refusal does not wait
 * for the (default) grace period. */
static void check_negative_grace(void) {
    ln_pair pr;
    hqq_stream *st = NULL;
    hqq_wait w = ln_wait(10 * LN_SEC);
    uint64_t t0;

    ln_open(&pr, LN_COUNT, -1);
    ln_negotiate(&pr);
    t0 = hqq_plat_mono_ns();
    HQQ_CHECK_STATUS(hqq_adv_open_stream(pr.p, &st, &w), HQQ_ERR_CONN_NOT_FOUND);
    HQQ_CHECK(hqq_plat_mono_ns() - t0 < LN_SEC);
    HQQ_CHECK(ln_held(pr.s) == 0);
    ln_close(&pr);
}

/* Four requests held one after another are admitted in that order. */
static void check_arrival_order(void) {
    enum { N = 4 };
    ln_pair pr;
    ln_open_arg o[N];
    hqq_test_thread *t[N];
    hqq_stream *ss = NULL;
    hqq_wait w = ln_wait(10 * LN_SEC);
    int i;

    ln_open(&pr, LN_COUNT, 60 * LN_SEC);
    ln_negotiate(&pr);
    for (i = 0; i < N; i++) {
        ln_open_start(&o[i], &t[i], pr.p);
        ln_wait_held(pr.s, (uint64_t)i + 1);
    }
    HQQ_CHECK_STATUS(hqq_adv_listen_streams(pr.s), HQQ_OK);
    for (i = 0; i < N; i++) {
        hqq_test_thread_join(t[i]);
        HQQ_CHECK_STATUS(o[i].st, HQQ_OK);
        HQQ_CHECK(hqq_stream_id(o[i].out) == (uint64_t)(2 * (i + 1)));
    }
    for (i = 0; i < N; i++) {
        HQQ_CHECK_STATUS(hqq_adv_accept_stream(pr.s, &ss, &w), HQQ_OK);
        HQQ_CHECK(hqq_stream_id(ss) == (uint64_t)(2 * (i + 1)));
    }
    ln_close(&pr);
}

/* Close discards the held requests: the link closes, and the requester's call ends
 * without a stream. */
static void check_close_releases(void) {
    ln_pair pr;
    ln_open_arg o;
    hqq_test_thread *t;

    ln_open(&pr, LN_COUNT, 60 * LN_SEC);
    ln_negotiate(&pr);
    ln_open_start(&o, &t, pr.p);
    ln_wait_held(pr.s, 1);
    HQQ_CHECK_STATUS(hqq_adv_close(pr.s), HQQ_OK);
    pr.s = NULL;
    hqq_test_thread_join(t);
    HQQ_CHECK(o.st != HQQ_OK);
    ln_close(&pr);
}

/* A failing link (here: the creator closed) releases its held requests without
 * waiting for the grace period, which is one minute. */
static void check_link_failure_releases(void) {
    ln_pair pr;
    ln_open_arg o;
    hqq_test_thread *t;
    int i;
    int failed = 0;

    ln_open(&pr, LN_COUNT, 60 * LN_SEC);
    ln_negotiate(&pr);
    ln_open_start(&o, &t, pr.p);
    ln_wait_held(pr.s, 1);
    HQQ_CHECK_STATUS(hqq_adv_close(pr.p), HQQ_OK);
    pr.p = NULL;
    for (i = 0; i < 5000 && !failed; i++) {
        hqq_spin_lock(&pr.s->lock);
        failed = pr.s->rx_failed;
        hqq_spin_unlock(&pr.s->lock);
        hqq_plat_sleep_ns(1000000);
    }
    HQQ_CHECK(failed);
    HQQ_CHECK(ln_held(pr.s) == 0);
    hqq_test_thread_join(t);
    ln_close(&pr);
}

int main(void) {
    check_held_until_listen();
    check_refused_after_grace();
    check_bound_refused_at_once();
    check_negative_grace();
    check_arrival_order();
    check_close_releases();
    check_link_failure_releases();
    return HQQ_TEST_RESULT("test_adv_listen");
}
