/* Uninitialized-memory regression test of the Advanced link.
 *
 * Every allocation of the link goes through a poisoning allocator that fills the
 * block with the repeating 4-byte pattern 01 00 00 00: each int field reads 1 and
 * each uint64 field reads 0x0000000100000001 (about 4.3 s of monotonic time, in
 * the past on any machine). A structure whose fields are only partly assigned
 * after allocation then misbehaves deterministically instead of by luck.
 *
 * The bug this guards against: a fresh request context was returned by
 * malloc without being cleared, so a request that carried no budget had a garbage
 * has_deadline / deadline_mono; the deadline timer marked it expired, the worker
 * skipped it ("no handler, no response") and the caller waited forever.
 *
 * Many threads call a serving link with a wait that has no timeout, so no budget
 * is sent with the requests (a wait with a timeout would send one, and the
 * deadline would then be assigned), enough to exhaust the request-context pool so
 * that fresh contexts are allocated. The handler is slower than the deadline
 * timer's period, so a context with a garbage deadline is seen by the timer while
 * its request is live. Every call must get its echo. A watchdog cancels the calls
 * after a generous bound so that a regression fails instead of hanging. */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "hqq/hqq_advanced.h"
#include "hqq_test.h"
#include "hqq_testutil.h"

#define UN_COUNT 8u
#define UN_SIZE 64u
#define UN_THREADS 32
#define UN_CALLS 50
#define UN_HANDLER_MS 6u /* longer than the deadline timer's period (2 ms) */
#define UN_WATCHDOG_MS 10000u

/* Poisoning allocator that counts live blocks. */
static _Atomic long live_blocks;
static _Atomic long total_blocks;

static void *poison_alloc(void *ctx, size_t n) {
    static const unsigned char pat[4] = {1, 0, 0, 0};
    unsigned char *p;
    size_t i;
    (void)ctx;
    p = (unsigned char *)malloc(n == 0 ? 1 : n);
    if (p == NULL) {
        return NULL;
    }
    for (i = 0; i < n; i++) {
        p[i] = pat[i & 3u];
    }
    atomic_fetch_add_explicit(&live_blocks, 1, memory_order_seq_cst);
    atomic_fetch_add_explicit(&total_blocks, 1, memory_order_seq_cst);
    return p;
}

static void poison_free(void *ctx, void *p, size_t n) {
    (void)ctx;
    (void)n;
    if (p != NULL) {
        atomic_fetch_sub_explicit(&live_blocks, 1, memory_order_seq_cst);
    }
    free(p);
}

static const hqq_allocator poisoning = {NULL, poison_alloc, poison_free};

typedef struct neg_arg {
    hqq_adv *a;
    hqq_status st;
} neg_arg;

static void neg_secondary(void *arg) {
    neg_arg *n = (neg_arg *)arg;
    hqq_wait w = {3000000000LL, NULL};
    n->st = hqq_adv_negotiate(n->a, HQQ_FEATURES_ALL, &w);
}

/* Echoes the payload of every request, slowly. */
static void echo_handler(void *user, hqq_req_ctx *ctx, hqq_response *w, const hqq_request *req) {
    (void)user;
    (void)ctx;
    hqq_test_sleep_ms(UN_HANDLER_MS);
    (void)hqq_response_write(w, hqq_request_data(req), hqq_request_len(req));
}

static void serve_thread(void *arg) {
    (void)hqq_adv_serve((hqq_adv *)arg, echo_handler, NULL, NULL);
}

typedef struct caller {
    hqq_adv *a;
    const hqq_cancel *cancel;
    _Atomic int *finished; /* callers that have returned */
    unsigned id;
    int ok;               /* every call echoed */
    int done;             /* calls completed */
    hqq_status first_bad; /* status of the first failed call */
} caller;

static void caller_main(void *arg) {
    caller *c = (caller *)arg;
    unsigned char out[80];
    int i;
    c->ok = 1;
    c->first_bad = HQQ_OK;
    for (i = 0; i < UN_CALLS; i++) {
        /* No timeout: the request carries no budget. Only the watchdog's cancel ends a stuck call. */
        hqq_wait w = {-1, c->cancel};
        hqq_msg *resp = NULL;
        size_t n = 16u + (size_t)((c->id + (unsigned)i) % 60u); /* 16..75: one or two frames */
        hqq_status st;
        size_t k;
        for (k = 0; k < n; k++) {
            out[k] = (unsigned char)(c->id * 31u + (unsigned)i * 7u + (unsigned)k);
        }
        st = hqq_stream_call(hqq_adv_stream0(c->a), 4, out, n, &resp, &w);
        if (st != HQQ_OK || resp == NULL || hqq_msg_len(resp) != n || memcmp(hqq_msg_data(resp), out, n) != 0) {
            c->ok = 0;
            c->first_bad = st;
            if (resp != NULL) {
                hqq_msg_release(resp);
            }
            break;
        }
        hqq_msg_release(resp);
        c->done++;
    }
    atomic_fetch_add(c->finished, 1);
}

static void check_calls_without_budget_under_poison(void) {
    hqq_adv_options o;
    unsigned char *mem;
    size_t size;
    hqq_adv *p = NULL;
    hqq_adv *s = NULL;
    neg_arg na;
    hqq_test_thread *nt;
    hqq_test_thread *st_thread;
    hqq_test_thread *th[UN_THREADS];
    caller callers[UN_THREADS];
    hqq_wait nw = {3000000000LL, NULL};
    hqq_status st;
    hqq_cancel watchdog;
    _Atomic int finished;
    unsigned waited_ms = 0;
    int i;

    hqq_adv_options_init(&o);
    o.link.heartbeat_interval_ns = 20 * 1000000;
    o.link.peer_timeout_ns = 2000 * 1000000;
    o.link.allocator = &poisoning;
    size = hqq_standard_size(UN_COUNT, UN_SIZE);
    mem = (unsigned char *)hqq_test_pages(size);
    HQQ_REQUIRE(hqq_adv_open_memory(mem, size, UN_COUNT, UN_SIZE, &o, &p, NULL) == HQQ_OK);
    HQQ_REQUIRE(hqq_adv_open_memory(mem, size, UN_COUNT, UN_SIZE, &o, &s, NULL) == HQQ_OK);

    na.a = s;
    na.st = HQQ_OK;
    nt = hqq_test_thread_start(neg_secondary, &na);
    st = hqq_adv_negotiate(p, HQQ_FEATURES_ALL, &nw);
    hqq_test_thread_join(nt);
    HQQ_CHECK_STATUS(st, HQQ_OK);
    HQQ_CHECK_STATUS(na.st, HQQ_OK);

    hqq_cancel_init(&watchdog);
    atomic_init(&finished, 0);
    st_thread = hqq_test_thread_start(serve_thread, s);
    for (i = 0; i < UN_THREADS; i++) {
        memset(&callers[i], 0, sizeof callers[i]);
        callers[i].a = p;
        callers[i].cancel = &watchdog;
        callers[i].finished = &finished;
        callers[i].id = (unsigned)i;
        th[i] = hqq_test_thread_start(caller_main, &callers[i]);
    }
    while (atomic_load(&finished) < UN_THREADS && waited_ms < UN_WATCHDOG_MS) {
        hqq_test_sleep_ms(10);
        waited_ms += 10;
    }
    if (atomic_load(&finished) < UN_THREADS) {
        fprintf(stderr, "calls still pending after %u ms: canceling\n", waited_ms);
        hqq_cancel_signal(&watchdog);
    }
    for (i = 0; i < UN_THREADS; i++) {
        hqq_test_thread_join(th[i]);
    }
    {
        int failed = 0, total_done = 0;
        for (i = 0; i < UN_THREADS; i++) {
            total_done += callers[i].done;
            if (!callers[i].ok || callers[i].done != UN_CALLS) {
                if (failed++ == 0) {
                    fprintf(stderr, "caller %d: %d of %d calls done, first failure: %s\n", i, callers[i].done,
                            UN_CALLS, hqq_strerror(callers[i].first_bad));
                }
            }
        }
        if (failed != 0) {
            fprintf(stderr, "%d of %d callers failed (%d of %d calls done)\n", failed, UN_THREADS, total_done,
                    UN_THREADS * UN_CALLS);
        }
        HQQ_CHECK_EQ_INT(failed, 0);
        HQQ_CHECK_EQ_INT(total_done, UN_THREADS * UN_CALLS);
    }

    HQQ_CHECK_STATUS(hqq_adv_close(s), HQQ_OK);
    hqq_test_thread_join(st_thread);
    HQQ_CHECK_STATUS(hqq_adv_close(p), HQQ_OK);
    hqq_test_pages_free(mem, size);

    /* The allocator was used, and everything it handed out came back. */
    HQQ_CHECK(atomic_load(&total_blocks) > 0);
    HQQ_CHECK_EQ_INT(atomic_load(&live_blocks), 0);
}

int main(void) {
    check_calls_without_budget_under_poison();
    return HQQ_TEST_RESULT("test_adv_uninit");
}
