/* Advanced link on caller-owned memory, both endpoints in one process: negotiation
 * (versions, features, rejection), chunked messages of several sizes, streams
 * (open, accept, parity, close), calls with error responses, zero-wait calls,
 * Serve, and steady-state allocation (no allocator calls after warm-up for send,
 * receive, call and serve). */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "hqq/hqq_advanced.h"
#include "hqq_test.h"
#include "hqq_testutil.h"

#define ADV_COUNT 8u
#define ADV_SIZE 64u

typedef struct adv_pair {
    unsigned char *mem;
    size_t size;
    hqq_adv *p;
    hqq_adv *s;
} adv_pair;

/* Counting allocator hook: every allocation of the link's control and data plane. */
static _Atomic long hook_allocs;
static void *hook_alloc(void *ctx, size_t n) {
    (void)ctx;
    atomic_fetch_add_explicit(&hook_allocs, 1, memory_order_seq_cst);
    return malloc(n == 0 ? 1 : n);
}
static void hook_free(void *ctx, void *p, size_t n) {
    (void)ctx;
    (void)n;
    free(p);
}
static const hqq_allocator counting = {NULL, hook_alloc, hook_free};

static void pair_open_with(adv_pair *pr, uint64_t count, uint64_t size, const hqq_allocator *al) {
    hqq_adv_options o;
    hqq_adv_options_init(&o);
    o.link.heartbeat_interval_ns = 20 * 1000000;
    o.link.peer_timeout_ns = 2000 * 1000000;
    o.link.allocator = al;
    pr->size = hqq_standard_size(count, size);
    pr->mem = (unsigned char *)hqq_test_pages(pr->size);
    HQQ_REQUIRE(hqq_adv_open_memory(pr->mem, pr->size, count, size, &o, &pr->p, NULL) == HQQ_OK);
    HQQ_REQUIRE(hqq_adv_open_memory(pr->mem, pr->size, count, size, &o, &pr->s, NULL) == HQQ_OK);
}

static void pair_open(adv_pair *pr, uint64_t count, uint64_t size) {
    pair_open_with(pr, count, size, NULL);
}

static void pair_close(adv_pair *pr) {
    if (pr->s != NULL) {
        (void)hqq_adv_close(pr->s);
    }
    if (pr->p != NULL) {
        (void)hqq_adv_close(pr->p);
    }
    hqq_test_pages_free(pr->mem, pr->size);
    memset(pr, 0, sizeof *pr);
}

/* The secondary's side of a negotiation, on its own thread. */
typedef struct neg_arg {
    hqq_adv *a;
    uint64_t features;
    hqq_status st;
} neg_arg;

static void neg_secondary(void *arg) {
    neg_arg *n = (neg_arg *)arg;
    hqq_wait w = {3000000000LL, NULL};
    n->st = hqq_adv_negotiate(n->a, n->features, &w);
}

static void negotiate_pair(adv_pair *pr, uint64_t features_p, uint64_t features_s) {
    neg_arg n;
    hqq_test_thread *t;
    hqq_status st;
    n.a = pr->s;
    n.features = features_s;
    n.st = HQQ_OK;
    t = hqq_test_thread_start(neg_secondary, &n);
    {
        hqq_wait w = {3000000000LL, NULL};
        st = hqq_adv_negotiate(pr->p, features_p, &w);
    }
    hqq_test_thread_join(t);
    HQQ_CHECK_STATUS(st, HQQ_OK);
    HQQ_CHECK_STATUS(n.st, HQQ_OK);
}

/* Echo server: method 2 answers an error response, others echo the payload. */
static void echo_handler(void *user, hqq_req_ctx *ctx, hqq_response *w, const hqq_request *req) {
    (void)user;
    (void)ctx;
    if (hqq_request_method(req) == 2) {
        (void)hqq_response_error(w, 42, "bad", 3);
        return;
    }
    (void)hqq_response_write(w, hqq_request_data(req), hqq_request_len(req));
}

static void serve_thread(void *arg) {
    hqq_adv *a = (hqq_adv *)arg;
    (void)hqq_adv_serve(a, echo_handler, NULL, NULL);
}

static void fill(unsigned char *buf, size_t n, unsigned seed) {
    size_t i;
    for (i = 0; i < n; i++) {
        buf[i] = (unsigned char)(seed * 131u + i * 7u);
    }
}

static void check_negotiation(void) {
    adv_pair pr;
    pair_open(&pr, ADV_COUNT, ADV_SIZE);
    /* Messages need a negotiated link. */
    HQQ_CHECK_STATUS(hqq_stream_send(hqq_adv_stream0(pr.p), "x", 1, NULL), HQQ_ERR_NOT_NEGOTIATED);
    negotiate_pair(&pr, 0, 0);
    HQQ_CHECK(hqq_adv_protocol_state(pr.p) == HQQ_PROTO_NEGOTIATED);
    HQQ_CHECK(hqq_adv_negotiated_version(pr.p) == HQQ_PROTOCOL_1_1);
    HQQ_CHECK(hqq_adv_negotiated_features(pr.p) == HQQ_FEATURES_ALL);
    HQQ_CHECK(hqq_adv_negotiated_features(pr.s) == HQQ_FEATURES_ALL);
    HQQ_CHECK_STATUS(hqq_adv_negotiate(pr.p, 0, NULL), HQQ_ERR_ALREADY_NEGOTIATED);
    pair_close(&pr);
}

static void check_messages(void) {
    adv_pair pr;
    unsigned char *buf = malloc(5000);
    size_t sizes[] = {0, 1, 63, 64, 65, 300, 4000};
    size_t k;
    pair_open(&pr, ADV_COUNT, ADV_SIZE);
    negotiate_pair(&pr, HQQ_FEATURES_ALL, HQQ_FEATURES_ALL);
    for (k = 0; k < sizeof sizes / sizeof sizes[0]; k++) {
        hqq_msg *m = NULL;
        size_t n = sizes[k];
        fill(buf, n, (unsigned)k + 1u);
        HQQ_CHECK_STATUS(hqq_stream_send(hqq_adv_stream0(pr.p), buf, n, NULL), HQQ_OK);
        HQQ_CHECK_STATUS(hqq_stream_recv(hqq_adv_stream0(pr.s), &m, NULL), HQQ_OK);
        if (m != NULL) {
            HQQ_CHECK(hqq_msg_len(m) == n);
            HQQ_CHECK(n == 0 || memcmp(hqq_msg_data(m), buf, n) == 0);
            hqq_msg_release(m);
        }
    }
    free(buf);
    pair_close(&pr);
}

static void check_streams(void) {
    adv_pair pr;
    hqq_stream *ps = NULL;
    hqq_stream *ss = NULL;
    hqq_msg *m = NULL;
    pair_open(&pr, ADV_COUNT, ADV_SIZE);
    negotiate_pair(&pr, HQQ_FEATURES_ALL, HQQ_FEATURES_ALL);
    /* Not listening: the request is held for the grace period (2 s by default), then refused with ErrConnNotFound. */
    HQQ_CHECK_STATUS(hqq_adv_open_stream(pr.p, &ps, NULL), HQQ_ERR_CONN_NOT_FOUND);
    HQQ_CHECK_STATUS(hqq_adv_listen_streams(pr.s), HQQ_OK);
    HQQ_CHECK_STATUS(hqq_adv_listen_streams(pr.s), HQQ_ERR_ALREADY_LISTENING);
    HQQ_CHECK_STATUS(hqq_adv_open_stream(pr.p, &ps, NULL), HQQ_OK);
    /* Primary allocates even ids; the refused open above also consumed one. */
    HQQ_CHECK(hqq_stream_id(ps) != 0 && hqq_stream_id(ps) % 2 == 0);
    HQQ_CHECK_STATUS(hqq_adv_accept_stream(pr.s, &ss, NULL), HQQ_OK);
    HQQ_CHECK(hqq_stream_id(ss) == hqq_stream_id(ps));
    HQQ_CHECK_STATUS(hqq_stream_send(ps, "hello", 5, NULL), HQQ_OK);
    HQQ_CHECK_STATUS(hqq_stream_recv(ss, &m, NULL), HQQ_OK);
    HQQ_CHECK(m != NULL && hqq_msg_len(m) == 5 && memcmp(hqq_msg_data(m), "hello", 5) == 0);
    hqq_msg_release(m);
    m = NULL;
    /* Close: the local side is closed; the peer drains and then reports the peer close. */
    HQQ_CHECK_STATUS(hqq_stream_close(ps), HQQ_OK);
    HQQ_CHECK_STATUS(hqq_stream_close(ps), HQQ_ERR_CLOSED);
    HQQ_CHECK_STATUS(hqq_stream_close(hqq_adv_stream0(pr.p)), HQQ_ERR_INVALID_OP);
    HQQ_CHECK_STATUS(hqq_stream_recv(ss, &m, NULL), HQQ_ERR_PEER_CLOSED);
    /* The secondary opens odd ids. */
    {
        hqq_stream *ps2 = NULL;
        hqq_stream *pp2 = NULL;
        HQQ_CHECK_STATUS(hqq_adv_listen_streams(pr.p), HQQ_OK);
        HQQ_CHECK_STATUS(hqq_adv_open_stream(pr.s, &ps2, NULL), HQQ_OK);
        HQQ_CHECK(hqq_stream_id(ps2) == 1);
        HQQ_CHECK_STATUS(hqq_adv_accept_stream(pr.p, &pp2, NULL), HQQ_OK);
        HQQ_CHECK(hqq_stream_id(pp2) == 1);
        HQQ_CHECK_STATUS(hqq_stream_close(ps2), HQQ_OK);
    }
    /* A blocked accept times out. */
    {
        hqq_wait w = {20 * 1000000LL, NULL};
        HQQ_CHECK_STATUS(hqq_adv_accept_stream(pr.p, &ss, &w), HQQ_ERR_TIMEOUT);
    }
    pair_close(&pr);
}

static void check_calls(void) {
    adv_pair pr;
    hqq_test_thread *t;
    hqq_msg *resp = NULL;
    unsigned char *big = malloc(5000);
    hqq_status st;
    pair_open(&pr, ADV_COUNT, ADV_SIZE);
    negotiate_pair(&pr, HQQ_FEATURES_ALL, HQQ_FEATURES_ALL);
    t = hqq_test_thread_start(serve_thread, pr.s);
    fill(big, 5000, 9);
    st = hqq_stream_call(hqq_adv_stream0(pr.p), 4, big, 5000, &resp, NULL);
    HQQ_CHECK_STATUS(st, HQQ_OK);
    if (resp != NULL) {
        HQQ_CHECK(hqq_msg_len(resp) == 5000 && memcmp(hqq_msg_data(resp), big, 5000) == 0);
        hqq_msg_release(resp);
        resp = NULL;
    }
    st = hqq_stream_call(hqq_adv_stream0(pr.p), 2, "abc", 3, &resp, NULL);
    HQQ_CHECK_STATUS(st, HQQ_ERR_RESPONSE);
    if (resp != NULL) {
        HQQ_CHECK(hqq_msg_status(resp) == 42);
        HQQ_CHECK(hqq_msg_len(resp) == 3 && memcmp(hqq_msg_data(resp), "bad", 3) == 0);
        hqq_msg_release(resp);
        resp = NULL;
    }
    /* A zero wait sends nothing. */
    {
        hqq_wait w = {0, NULL};
        HQQ_CHECK_STATUS(hqq_stream_call(hqq_adv_stream0(pr.p), 4, "x", 1, &resp, &w), HQQ_ERR_TIMEOUT);
    }
    HQQ_CHECK_STATUS(hqq_adv_close(pr.s), HQQ_OK);
    pr.s = NULL;
    hqq_test_thread_join(t);
    free(big);
    pair_close(&pr);
}

/* After warm-up, sending, receiving, calling and serving must not allocate. */
static void check_steady_state(void) {
    adv_pair pr;
    hqq_test_thread *t;
    hqq_msg *m = NULL;
    unsigned char *buf = malloc(600);
    unsigned attempt, round;
    long before = 0, after = 1;
    pair_open_with(&pr, ADV_COUNT, ADV_SIZE, &counting);
    negotiate_pair(&pr, HQQ_FEATURES_ALL, HQQ_FEATURES_ALL);
    t = hqq_test_thread_start(serve_thread, pr.s);
    fill(buf, 600, 3);
    /* Warm-up: message pools, parked-waiter nodes and the deadline timer are created. */
    {
        hqq_wait w = {1000000LL, NULL};
        HQQ_CHECK_STATUS(hqq_stream_recv(hqq_adv_stream0(pr.s), &m, &w), HQQ_ERR_TIMEOUT);
    }
    for (round = 0; round < 20; round++) {
        hqq_msg *resp = NULL;
        HQQ_CHECK_STATUS(hqq_stream_send(hqq_adv_stream0(pr.p), buf, 300, NULL), HQQ_OK);
        HQQ_CHECK_STATUS(hqq_stream_recv(hqq_adv_stream0(pr.s), &m, NULL), HQQ_OK);
        if (m != NULL) {
            hqq_msg_release(m);
            m = NULL;
        }
        HQQ_CHECK_STATUS(hqq_stream_call(hqq_adv_stream0(pr.p), 4, buf, 300, &resp, NULL), HQQ_OK);
        if (resp != NULL) {
            hqq_msg_release(resp);
        }
    }
    /* A 200 us scheduler pause while an inline handler runs escalates the dispatcher thread,
     * which allocates once for the replacement thread; a per-call leak allocates in every batch. */
    for (attempt = 0; attempt < 5; attempt++) {
        before = atomic_load(&hook_allocs);
        for (round = 0; round < 200; round++) {
            hqq_msg *resp = NULL;
            HQQ_CHECK_STATUS(hqq_stream_send(hqq_adv_stream0(pr.p), buf, 300, NULL), HQQ_OK);
            HQQ_CHECK_STATUS(hqq_stream_recv(hqq_adv_stream0(pr.s), &m, NULL), HQQ_OK);
            if (m != NULL) {
                hqq_msg_release(m);
                m = NULL;
            }
            HQQ_CHECK_STATUS(hqq_stream_call(hqq_adv_stream0(pr.p), 4, buf, 300, &resp, NULL), HQQ_OK);
            if (resp != NULL) {
                hqq_msg_release(resp);
            }
        }
        after = atomic_load(&hook_allocs);
        if (after == before) {
            break;
        }
    }
    HQQ_CHECK(after == before);
    HQQ_CHECK_STATUS(hqq_adv_close(pr.s), HQQ_OK);
    pr.s = NULL;
    hqq_test_thread_join(t);
    free(buf);
    pair_close(&pr);
}

int main(void) {
    check_negotiation();
    check_messages();
    check_streams();
    check_calls();
    check_steady_state();
    return HQQ_TEST_RESULT("test_adv");
}
