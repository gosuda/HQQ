/*
 * GenMC harness (b): the wake handshake of the real ring core.
 *
 *   waiter (consumer)                          waker (producer)
 *   peek: seq at pos not published             claim pos, write packet
 *   hqq_ring_park(NOT_EMPTY, pos, pos+1):      hqq_ring_publish(pos):
 *     W1 waiters += 1      (SEQ_CST)             K1 seq = pos+1  (SEQ_CST)
 *     W2 v = word          (SEQ_CST)             K2 n = waiters  (SEQ_CST)
 *     W3 re-check seq      (SEQ_CST)             K3 word += 1    if n != 0
 *        blocked -> wait(word, v)
 *     W4 waiters -= 1
 *   peek again; read the packet once it is published
 *
 * The waiter and the waker form a store-buffering pair (seq vs waiters), so
 * the four accesses must be SEQ_CST. Invariant checked in every execution:
 * if the waiter went to sleep on word == v, the waker bumped the word after
 * that read (word != v at the end). Otherwise the waiter would sleep forever:
 * a lost wake-up.
 *
 * Variants (after GenMC's `--`):
 *   (none)                       must PASS
 *   -DHQQ_VERIFY_WEAKEN_ACQREL   the four handshake accesses (and K1..K3 of
 *                                publish) weakened to acquire/release: must FAIL
 *                                (store-buffering outcome: lost wake-up)
 */
#include "hqq_model.h"

#include <pthread.h>

#define PARK_TIMEOUT_NS 5000000000ull /* host build: the hook gives up after 5 s */

static hqq_ring ring;
static _Alignas(64) unsigned char mem[HQQ_RING_HEADER_SIZE + 2 * HQQ_RING_SLOT_STRIDE];

static void *waiter(void *arg) {
    uint64_t pos = 0;
    uint64_t v = 0;
    int ready;
    (void)arg;
    ready = hqq_ring_peek_consumer(&ring, &pos);
    if (!ready) {
        hqq_ring_park(&ring, HQQ_RING_NOT_EMPTY, pos, pos + 1, PARK_TIMEOUT_NS, NULL, NULL);
        ready = hqq_ring_peek_consumer(&ring, &pos);
    }
    if (ready) {
        memcpy(&v, hqq_ring_slot(&ring, pos)->packet, sizeof v);
        assert(v == 42);
        hqq_ring_release(&ring, pos);
    }
    return NULL;
}

static void *waker(void *arg) {
    uint64_t pos = 0;
    uint64_t v = 42;
    int ok;
    (void)arg;
    ok = hqq_ring_try_claim_producer(&ring, &pos); /* 2 slots, empty: cannot fail */
    assert(ok && pos == 0);
    memcpy(hqq_ring_slot(&ring, pos)->packet, &v, sizeof v);
    hqq_ring_publish(&ring, pos);
    HQV_PEER_DONE(); /* no further wake can come from this waker */
    return NULL;
}

static void run_once(void) {
    pthread_t w, p;
    hqv_init_ring(&ring, mem, 2);
    hqv_park.parked = 0;
    hqv_park.expect = 0;
    hqv_park.word = NULL;
    if (pthread_create(&w, NULL, waiter, NULL) != 0) abort();
    if (pthread_create(&p, NULL, waker, NULL) != 0) abort();
    if (pthread_join(w, NULL) != 0) abort();
    if (pthread_join(p, NULL) != 0) abort();
    if (hqv_park.parked) {
        /* A kernel wait(word, expect) would never return if the word still
         * held the value it slept on: the waker must have bumped it. */
        assert(atomic_load_explicit(hqv_park.word, memory_order_acquire) != hqv_park.expect);
    }
}

HQV_MAIN(run_once)
