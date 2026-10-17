/* hqq_plat_mutex_*: mutual exclusion, hand-off, destroy and allocator behavior. */
#include <stdatomic.h>
#include <string.h>

#include "hqq_platform.h"
#include "hqq_test.h"
#include "hqq_testutil.h"

#define THREADS 4
#define ROUNDS 50000

/* A deliberately non-atomic counter: only the mutex keeps the increments whole. TSan reports a
 * data race here if lock and unlock do not order the accesses. */
typedef struct contend {
    hqq_plat_mutex *m;
    long counter;
    long inside; /* threads currently in the critical section */
    long overlap; /* times a thread saw another one inside */
} contend;

static void contend_worker(void *arg) {
    contend *c = (contend *)arg;
    int i;
    for (i = 0; i < ROUNDS; i++) {
        hqq_plat_mutex_lock(c->m);
        c->inside++;
        if (c->inside != 1) {
            c->overlap++;
        }
        c->counter++;
        c->inside--;
        hqq_plat_mutex_unlock(c->m);
    }
}

static void test_contention(void) {
    contend c;
    hqq_test_thread *t[THREADS];
    int i;

    memset(&c, 0, sizeof c);
    c.m = hqq_plat_mutex_create(NULL);
    HQQ_REQUIRE(c.m != NULL);
    for (i = 0; i < THREADS; i++) {
        t[i] = hqq_test_thread_start(contend_worker, &c);
    }
    for (i = 0; i < THREADS; i++) {
        hqq_test_thread_join(t[i]);
    }
    HQQ_CHECK_EQ_INT(c.counter, (long)THREADS * ROUNDS);
    HQQ_CHECK_EQ_INT(c.overlap, 0);
    hqq_plat_mutex_destroy(NULL, c.m);
}

/* A second thread must stay blocked while the first holds the lock and proceed right after the
 * unlock. */
typedef struct handoff {
    hqq_plat_mutex *m;
    _Atomic int started;
    _Atomic int acquired;
} handoff;

static void handoff_worker(void *arg) {
    handoff *h = (handoff *)arg;
    atomic_store(&h->started, 1);
    hqq_plat_mutex_lock(h->m);
    atomic_store(&h->acquired, 1);
    hqq_plat_mutex_unlock(h->m);
}

static void test_handoff(void) {
    handoff h;
    hqq_test_thread *t;
    int i;

    h.m = hqq_plat_mutex_create(NULL);
    HQQ_REQUIRE(h.m != NULL);
    atomic_init(&h.started, 0);
    atomic_init(&h.acquired, 0);

    hqq_plat_mutex_lock(h.m);
    t = hqq_test_thread_start(handoff_worker, &h);
    for (i = 0; i < 1000 && atomic_load(&h.started) == 0; i++) {
        hqq_test_sleep_ms(1);
    }
    HQQ_CHECK(atomic_load(&h.started) == 1);
    hqq_test_sleep_ms(50); /* the worker must still be parked on the lock */
    HQQ_CHECK_EQ_INT(atomic_load(&h.acquired), 0);
    hqq_plat_mutex_unlock(h.m);
    hqq_test_thread_join(t);
    HQQ_CHECK_EQ_INT(atomic_load(&h.acquired), 1);

    /* The mutex is reusable after the hand-off and can be destroyed once unlocked. */
    hqq_plat_mutex_lock(h.m);
    hqq_plat_mutex_unlock(h.m);
    hqq_plat_mutex_destroy(NULL, h.m);
}

/* Allocation accounting: create takes one block, destroy returns exactly that block, a failing
 * allocator makes create return NULL. */
typedef struct acct {
    size_t allocs;
    size_t frees;
    size_t last_alloc;
    size_t last_free;
    void *last_ptr;
    int fail;
} acct;

static void *acct_alloc(void *ctx, size_t size) {
    acct *a = (acct *)ctx;
    if (a->fail) {
        return NULL;
    }
    a->allocs++;
    a->last_alloc = size;
    a->last_ptr = malloc(size);
    return a->last_ptr;
}

static void acct_free(void *ctx, void *ptr, size_t size) {
    acct *a = (acct *)ctx;
    a->frees++;
    a->last_free = size;
    HQQ_CHECK(ptr == a->last_ptr);
    free(ptr);
}

static void test_allocator(void) {
    acct a;
    hqq_allocator al;
    hqq_plat_mutex *m;

    memset(&a, 0, sizeof a);
    al.ctx = &a;
    al.alloc = acct_alloc;
    al.free = acct_free;

    m = hqq_plat_mutex_create(&al);
    HQQ_REQUIRE(m != NULL);
    HQQ_CHECK_EQ_INT(a.allocs, 1);
    HQQ_CHECK_EQ_INT(a.frees, 0);
    hqq_plat_mutex_lock(m);
    hqq_plat_mutex_unlock(m);
    hqq_plat_mutex_destroy(&al, m);
    HQQ_CHECK_EQ_INT(a.frees, 1);
    HQQ_CHECK_EQ_INT(a.last_free, a.last_alloc);

    a.fail = 1;
    HQQ_CHECK(hqq_plat_mutex_create(&al) == NULL);
    HQQ_CHECK_EQ_INT(a.allocs, 1);

    hqq_plat_mutex_destroy(&al, NULL); /* no-op */
    HQQ_CHECK_EQ_INT(a.frees, 1);
}

/* Many create/lock/unlock/destroy cycles (leaks and double frees show up under ASan). */
static void test_cycles(void) {
    int i;
    for (i = 0; i < 1000; i++) {
        hqq_plat_mutex *m = hqq_plat_mutex_create(NULL);
        HQQ_REQUIRE(m != NULL);
        hqq_plat_mutex_lock(m);
        hqq_plat_mutex_unlock(m);
        hqq_plat_mutex_lock(m);
        hqq_plat_mutex_unlock(m);
        hqq_plat_mutex_destroy(NULL, m);
    }
}

int main(void) {
    test_contention();
    test_handoff();
    test_allocator();
    test_cycles();
    return HQQ_TEST_RESULT("platform_mutex");
}
