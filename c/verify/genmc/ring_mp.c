/*
 * GenMC harness (a): payload message passing through the real ring core.
 *
 * One producer thread and one consumer thread use the MPMC protocol of
 * c/src/hqq_ring.h as an SPSC channel over a 2-slot ring. Four messages go
 * through, so messages 2 and 3 reuse slots 0 and 1: the wraparound.
 *
 *   producer: claim pos, write packet (plain), publish (seq store, SEQ_CST)
 *   consumer: claim pos (seq load, ACQUIRE), read packet (plain), release
 *
 * Checked in every RC11 execution: the consumer gets message i at position i
 * (FIFO, no loss, no duplicate), the payload is the one published (a missing
 * release/acquire pair shows up as a data race on the plain packet bytes), and
 * the ring ends with both cursors at 4.
 *
 * Variants (after GenMC's `--`):
 *   (none)                         must PASS
 *   -DHQQ_VERIFY_WEAKEN_RELAXED    publication store relaxed: must FAIL (race)
 */
#include "hqq_model.h"

#include <pthread.h>

#define MSGS 4u /* 2 slots: messages 2 and 3 wrap around onto slots 0 and 1 */

static hqq_ring ring;
static _Alignas(64) unsigned char mem[HQQ_RING_HEADER_SIZE + 2 * HQQ_RING_SLOT_STRIDE];

static void *producer(void *arg) {
    uint64_t i;
    (void)arg;
    for (i = 0; i < MSGS; i++) {
        uint64_t pos = 0;
        uint64_t v = 100 + i;
        HQV_AWAIT(hqq_ring_try_claim_producer(&ring, &pos));
        assert(pos == i);
        memcpy(hqq_ring_slot(&ring, pos)->packet, &v, sizeof v);
        hqq_ring_publish(&ring, pos);
    }
    return NULL;
}

static void *consumer(void *arg) {
    uint64_t i;
    (void)arg;
    for (i = 0; i < MSGS; i++) {
        uint64_t pos = 0;
        uint64_t v = 0;
        HQV_AWAIT(hqq_ring_try_claim_consumer(&ring, &pos));
        assert(pos == i);
        memcpy(&v, hqq_ring_slot(&ring, pos)->packet, sizeof v);
        assert(v == 100 + i);
        hqq_ring_release(&ring, pos);
    }
    return NULL;
}

static void run_once(void) {
    pthread_t p, c;
    hqv_init_ring(&ring, mem, 2);
    if (pthread_create(&p, NULL, producer, NULL) != 0) abort();
    if (pthread_create(&c, NULL, consumer, NULL) != 0) abort();
    if (pthread_join(p, NULL) != 0) abort();
    if (pthread_join(c, NULL) != 0) abort();
    assert(atomic_load_explicit(&ring.h->w, memory_order_acquire) == MSGS);
    assert(atomic_load_explicit(&ring.h->r, memory_order_acquire) == MSGS);
}

HQV_MAIN(run_once)
