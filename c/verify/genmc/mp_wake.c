/*
 * GenMC proof-of-concept: slot publication (message passing) combined with
 * the Dekker-style wake handshake of the HQQ ring.
 *
 *   producer                              consumer
 *   --------                              --------
 *   data = 42            (plain write)    waiters += 1               [W]
 *   seq  = 1             [P]              s = seq                    [R]
 *   w = waiters          [V]              if (s == 1) read data      (plain)
 *   if (w == 0) no_wake = 1               else would_park = 1
 *
 * "would_park && no_wake" is a lost wake-up: the consumer decided to sleep
 * and the producer decided nobody needs waking. The two threads form a
 * store-buffering (Dekker) pair (seq vs waiters), so seq_cst is REQUIRED on
 * the four accesses; release/acquire alone is not enough. Separately the
 * plain read of `data` needs the publication store [P] to be a release.
 *
 * Variant (select with -DVARIANT_xxx after the GenMC `--`):
 *   VARIANT_SEQCST    all of [P] [V] [W] [R] are seq_cst            -> must PASS
 *   VARIANT_FENCE     release/acquire + atomic_thread_fence(seq_cst) -> must PASS
 *   VARIANT_ACQREL    release/acquire only (no seq_cst)              -> must FAIL (lost wake-up)
 *   VARIANT_RELAXED_PUB  [P] relaxed (fences as in FENCE)           -> must FAIL (publication race on `data`)
 */
#include <assert.h>
#include <pthread.h>
#include <stdatomic.h>
#include <stdlib.h>

#if defined(VARIANT_SEQCST)
#  define ORD_PUB     memory_order_seq_cst /* [P] store seq       */
#  define ORD_WAKER   memory_order_seq_cst /* [V] load  waiters   */
#  define ORD_WAITER  memory_order_seq_cst /* [W] RMW   waiters   */
#  define ORD_CHECK   memory_order_seq_cst /* [R] load  seq       */
#  define FENCE_BETWEEN() ((void)0)
#elif defined(VARIANT_FENCE)
#  define ORD_PUB     memory_order_release
#  define ORD_WAKER   memory_order_relaxed
#  define ORD_WAITER  memory_order_relaxed
#  define ORD_CHECK   memory_order_acquire
#  define FENCE_BETWEEN() atomic_thread_fence(memory_order_seq_cst)
#elif defined(VARIANT_ACQREL)
#  define ORD_PUB     memory_order_release
#  define ORD_WAKER   memory_order_acquire
#  define ORD_WAITER  memory_order_acq_rel
#  define ORD_CHECK   memory_order_acquire
#  define FENCE_BETWEEN() ((void)0)
#elif defined(VARIANT_RELAXED_PUB)
/* The handshake is fine (seq_cst fences), but the publication store is not a
 * release: the consumer can see seq == 1 and race with the write of `data`. */
#  define ORD_PUB     memory_order_relaxed
#  define ORD_WAKER   memory_order_relaxed
#  define ORD_WAITER  memory_order_relaxed
#  define ORD_CHECK   memory_order_acquire
#  define FENCE_BETWEEN() atomic_thread_fence(memory_order_seq_cst)
#else
#  error "define one of VARIANT_SEQCST, VARIANT_FENCE, VARIANT_ACQREL, VARIANT_RELAXED_PUB"
#endif

static int data;                /* payload: plain, owned by the slot protocol */
static atomic_int seq;          /* slot sequence word (0 = empty, 1 = published) */
static atomic_int waiters;      /* parked-consumer count                        */

static int no_wake;             /* producer decided not to wake                 */
static int would_park;          /* consumer decided to park                     */

static void *producer(void *unused) {
    (void)unused;
    data = 42;
    atomic_store_explicit(&seq, 1, ORD_PUB);
    FENCE_BETWEEN();
    if (atomic_load_explicit(&waiters, ORD_WAKER) == 0) {
        no_wake = 1;
    }
    return NULL;
}

static void *consumer(void *unused) {
    (void)unused;
    atomic_fetch_add_explicit(&waiters, 1, ORD_WAITER);
    FENCE_BETWEEN();
    if (atomic_load_explicit(&seq, ORD_CHECK) == 1) {
        assert(data == 42);
    } else {
        would_park = 1;
    }
    return NULL;
}

int main(void) {
    pthread_t p, c;
    if (pthread_create(&p, NULL, producer, NULL) != 0) abort();
    if (pthread_create(&c, NULL, consumer, NULL) != 0) abort();
    if (pthread_join(p, NULL) != 0) abort();
    if (pthread_join(c, NULL) != 0) abort();
    assert(!(would_park && no_wake)); /* no lost wake-up */
    return 0;
}
