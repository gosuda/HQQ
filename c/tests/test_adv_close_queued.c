/* Regression test: hqq_adv_close released a queued, unserved request twice.
 *
 * A request that reaches an Advanced link is owned by two things at once: the request queue
 * (reqq) and its request context (c->req = m, m->rc = c). hqq_adv_close drained the queue and put
 * the message back, then walked the contexts and put c->req again. The second put pushed the same
 * message on the pool's free list a second time (a one-element cycle), and the pool drain that
 * follows freed it and read its next pointer: a heap-use-after-free, or a double free with a
 * different allocator. A link that is served is not affected: a worker takes the message off the
 * queue and clears the context. Only a request that nobody serves is. The hqqrpc server hit it
 * when it closed a control link it had accepted but never served.
 *
 * The state is built directly, with no timing in the verdict: a negotiated pair, callers on the
 * primary side whose requests reach the secondary, which never calls hqq_adv_serve; the test waits
 * (bounded by a watchdog, not by a sleep) until the secondary holds every request, then closes it.
 * Run it under AddressSanitizer: before the fix the close is a hard failure there. The counting
 * allocator additionally checks that every block of both links is returned exactly once.
 */
#include <stdatomic.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "hqq/hqq_advanced.h"
#include "hqq_adv_internal.h"
#include "hqq_test.h"
#include "hqq_testutil.h"

#define Q_COUNT 8u
#define Q_SIZE 64u
#define Q_CALLERS 3
#define Q_WATCHDOG_MS 10000u

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

typedef struct neg_arg {
    hqq_adv *a;
    hqq_status st;
} neg_arg;

static void neg_secondary(void *arg) {
    neg_arg *n = (neg_arg *)arg;
    hqq_wait w = {3000000000LL, NULL};

    n->st = hqq_adv_negotiate(n->a, 0, &w);
}

typedef struct caller {
    hqq_adv *a;
    hqq_status st;
} caller;

/* A call that nobody answers: it ends when the serving side is closed. */
static void caller_main(void *arg) {
    caller *c = (caller *)arg;
    hqq_msg *resp = NULL;
    hqq_wait w = {30000000000LL, NULL};

    c->st = hqq_stream_call(hqq_adv_stream0(c->a), 4, "queued", 6, &resp, &w);
    if (resp != NULL) {
        hqq_msg_release(resp);
    }
}

/* Requests the secondary holds (queued or in a context). */
static uint64_t held_requests(hqq_adv *a) {
    uint64_t n;

    hqq_spin_lock(&a->lock);
    n = a->inflight;
    hqq_spin_unlock(&a->lock);
    return n;
}

static void check_close_with_unserved_requests(unsigned callers) {
    hqq_adv_options o;
    size_t size;
    unsigned char *mem;
    hqq_adv *p = NULL;
    hqq_adv *s = NULL;
    neg_arg n;
    hqq_test_thread *nt;
    caller cs[Q_CALLERS];
    hqq_test_thread *ct[Q_CALLERS];
    unsigned i;
    uint64_t waited = 0;

    atomic_store(&live_blocks, 0);
    hqq_adv_options_init(&o);
    o.link.heartbeat_interval_ns = 20 * 1000000;
    o.link.peer_timeout_ns = 2000 * 1000000;
    o.link.allocator = &counting;
    size = hqq_standard_size(Q_COUNT, Q_SIZE);
    mem = (unsigned char *)hqq_test_pages(size);
    HQQ_REQUIRE(hqq_adv_open_memory(mem, size, Q_COUNT, Q_SIZE, &o, &p, NULL) == HQQ_OK);
    HQQ_REQUIRE(hqq_adv_open_memory(mem, size, Q_COUNT, Q_SIZE, &o, &s, NULL) == HQQ_OK);

    n.a = s;
    n.st = HQQ_OK;
    nt = hqq_test_thread_start(neg_secondary, &n);
    {
        hqq_wait w = {3000000000LL, NULL};

        HQQ_CHECK_STATUS(hqq_adv_negotiate(p, 0, &w), HQQ_OK);
    }
    hqq_test_thread_join(nt);
    HQQ_CHECK_STATUS(n.st, HQQ_OK);

    for (i = 0; i < callers; i++) {
        cs[i].a = p;
        cs[i].st = HQQ_OK;
        ct[i] = hqq_test_thread_start(caller_main, &cs[i]);
    }
    /* The secondary never serves; wait until it holds every request. */
    while (held_requests(s) != callers && waited < Q_WATCHDOG_MS) {
        hqq_test_sleep_ms(1);
        waited++;
    }
    HQQ_REQUIRE(held_requests(s) == callers);

    HQQ_CHECK_STATUS(hqq_adv_close(s), HQQ_OK); /* the double release happened here */
    for (i = 0; i < callers; i++) {
        hqq_test_thread_join(ct[i]);
        HQQ_CHECK(cs[i].st != HQQ_OK); /* nobody answered */
    }
    HQQ_CHECK_STATUS(hqq_adv_close(p), HQQ_OK);
    hqq_test_pages_free(mem, size);
    HQQ_CHECK_EQ_INT(atomic_load(&live_blocks), 0);
}

int main(void) {
    check_close_with_unserved_requests(1);
    check_close_with_unserved_requests(Q_CALLERS);
    return HQQ_TEST_RESULT("test_adv_close_queued");
}
