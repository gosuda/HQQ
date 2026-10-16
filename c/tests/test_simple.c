/* Wait policies: simple mode (spin, yield, sleep backoff, never a kernel wait), spin
 * and adaptive. Each policy must deliver every message, honour deadlines and
 * cancellation, and wake blocked peers promptly from a thread of this process. */
#include <string.h>

#include "hqq_internal.h"
#include "hqq_test.h"
#include "hqq_testutil.h"

#define SM_COUNT 4u
#define SM_SIZE 32u

typedef struct sm_arg {
    hqq_link *link;
    unsigned count;
    _Atomic int ok;
} sm_arg;

static void sm_consumer(void *p) {
    sm_arg *a = (sm_arg *)p;
    unsigned char buf[SM_SIZE];
    unsigned i;
    int good = 1;
    for (i = 0; i < a->count; i++) {
        size_t len = 0;
        if (hqq_read(a->link, buf, sizeof buf, &len, NULL) != HQQ_OK || len != 4 || buf[0] != (unsigned char)i ||
            buf[1] != 0xEE) {
            good = 0;
        }
    }
    atomic_store(&a->ok, good);
}

static void sm_producer(void *p) {
    sm_arg *a = (sm_arg *)p;
    unsigned i;
    int good = 1;
    for (i = 0; i < a->count; i++) {
        unsigned char msg[4] = {(unsigned char)i, 0xEE, 0, 0};
        if (hqq_write(a->link, msg, sizeof msg, NULL) != HQQ_OK) {
            good = 0;
        }
    }
    atomic_store(&a->ok, good);
}

/* Blocked consumer on an empty ring, a late producer: the message arrives. */
static void check_policy(hqq_wait_policy policy, const char *tag) {
    hqq_options o;
    size_t size = hqq_standard_size(SM_COUNT, SM_SIZE);
    unsigned char *mem = (unsigned char *)hqq_test_pages(size);
    hqq_link *p = NULL, *s = NULL;
    unsigned char buf[SM_SIZE];
    size_t len = 0;
    hqq_wait w;
    uint64_t t0;
    hqq_test_thread *th;
    sm_arg a;
    unsigned i;

    hqq_options_init(&o);
    o.wait_policy = policy;
    o.heartbeat_interval_ns = 20 * (int64_t)HQQ_NS_PER_MS;
    o.peer_timeout_ns = 2 * (int64_t)HQQ_NS_PER_SEC;
    HQQ_REQUIRE(hqq_standard_open_memory(mem, size, SM_COUNT, SM_SIZE, &o, &p, NULL) == HQQ_OK);
    HQQ_REQUIRE(hqq_standard_open_memory(mem, size, SM_COUNT, SM_SIZE, &o, &s, NULL) == HQQ_OK);

    /* Deadline on an empty ring. */
    w.timeout_ns = 40 * (int64_t)HQQ_NS_PER_MS;
    w.cancel = NULL;
    t0 = hqq_plat_mono_ns();
    HQQ_CHECK_STATUS(hqq_read(s, buf, sizeof buf, &len, &w), HQQ_ERR_TIMEOUT);
    HQQ_CHECK(hqq_plat_mono_ns() - t0 >= 30 * HQQ_NS_PER_MS);
    HQQ_CHECK(hqq_plat_mono_ns() - t0 < 2 * HQQ_NS_PER_SEC);

    /* Blocked reader, delayed writer. */
    a.link = s;
    a.count = 1;
    atomic_store(&a.ok, 0);
    th = hqq_test_thread_start(sm_consumer, &a);
    hqq_test_sleep_ms(60);
    {
        unsigned char msg[4] = {0, 0xEE, 0, 0};
        HQQ_CHECK_STATUS(hqq_write(p, msg, sizeof msg, NULL), HQQ_OK);
    }
    hqq_test_thread_join(th);
    HQQ_CHECK_EQ_INT(atomic_load(&a.ok), 1);

    /* Blocked writer on a full ring, delayed reader: nothing is lost, order holds. */
    a.link = p;
    a.count = 3 * SM_COUNT;
    atomic_store(&a.ok, 0);
    th = hqq_test_thread_start(sm_producer, &a);
    hqq_test_sleep_ms(40);
    for (i = 0; i < 3 * SM_COUNT; i++) {
        size_t n = 0;
        HQQ_CHECK_STATUS(hqq_read(s, buf, sizeof buf, &n, NULL), HQQ_OK);
        HQQ_CHECK_EQ_U64(n, 4);
        HQQ_CHECK(buf[0] == (unsigned char)i && buf[1] == 0xEE);
    }
    hqq_test_thread_join(th);
    HQQ_CHECK_EQ_INT(atomic_load(&a.ok), 1);

    HQQ_CHECK_STATUS(hqq_close(s), HQQ_OK);
    HQQ_CHECK_STATUS(hqq_close(p), HQQ_OK);
    hqq_test_pages_free(mem, size);
    (void)tag;
}

/* Simple mode: a link that never parks still delivers, and the waker duty keeps working. */
static void check_simple_round_trips(void) {
    check_policy(HQQ_WAIT_SIMPLE, "simple");
}

static void check_spin_round_trips(void) {
    check_policy(HQQ_WAIT_SPIN, "spin");
}

static void check_adaptive_round_trips(void) {
    check_policy(HQQ_WAIT_ADAPTIVE, "adaptive");
}

/* Cancellation is noticed promptly by a blocked simple-mode reader. */
typedef struct sm_cancel {
    hqq_cancel *token;
} sm_cancel;

static void sm_cancel_main(void *p) {
    hqq_test_sleep_ms(30);
    hqq_cancel_signal(((sm_cancel *)p)->token);
}

static void check_simple_cancel(void) {
    hqq_options o;
    size_t size = hqq_standard_size(SM_COUNT, SM_SIZE);
    unsigned char *mem = (unsigned char *)hqq_test_pages(size);
    hqq_link *p = NULL, *s = NULL;
    unsigned char buf[SM_SIZE];
    size_t len = 0;
    hqq_wait w;
    hqq_cancel token;
    sm_cancel sc;
    hqq_test_thread *th;
    uint64_t t0;

    hqq_options_init(&o);
    o.wait_policy = HQQ_WAIT_SIMPLE;
    HQQ_REQUIRE(hqq_standard_open_memory(mem, size, SM_COUNT, SM_SIZE, &o, &p, NULL) == HQQ_OK);
    HQQ_REQUIRE(hqq_standard_open_memory(mem, size, SM_COUNT, SM_SIZE, &o, &s, NULL) == HQQ_OK);
    hqq_cancel_init(&token);
    sc.token = &token;
    th = hqq_test_thread_start(sm_cancel_main, &sc);
    w.timeout_ns = -1;
    w.cancel = &token;
    t0 = hqq_plat_mono_ns();
    HQQ_CHECK_STATUS(hqq_read(s, buf, sizeof buf, &len, &w), HQQ_ERR_CANCELED);
    HQQ_CHECK(hqq_plat_mono_ns() - t0 < 2 * HQQ_NS_PER_SEC);
    hqq_test_thread_join(th);

    HQQ_CHECK_STATUS(hqq_close(s), HQQ_OK);
    HQQ_CHECK_STATUS(hqq_close(p), HQQ_OK);
    hqq_test_pages_free(mem, size);
}

int main(void) {
    check_simple_round_trips();
    check_spin_round_trips();
    check_adaptive_round_trips();
    check_simple_cancel();
    return HQQ_TEST_RESULT("test_simple");
}
