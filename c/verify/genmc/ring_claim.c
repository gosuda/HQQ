/*
 * GenMC harness (d): two producers racing for the producer cursor (MPMC claim).
 *
 * Two producer threads each claim one position with hqq_ring_try_claim_producer()
 * (a CAS race on the cursor w), write their payload into the claimed slot and
 * publish. A 2-slot ring holds both, so no producer ever waits. After the join
 * the main thread (the only consumer) takes both messages back in position
 * order. Checked in every RC11 execution:
 *   - the two claims are distinct positions {0, 1} (no double claim);
 *   - each payload arrives exactly once, from the position its producer got
 *     (the plain packet writes are ordered by the publish/claim pairs);
 *   - the cursors end at 2.
 *
 * No negative variant: the producer cursor CAS is an RMW and stays atomic under
 * acquire/release; the weakened-order negatives live in ring_mp.c and
 * ring_wake.c.
 */
#include "hqq_model.h"

#include <pthread.h>

static hqq_ring ring;
static _Alignas(64) unsigned char mem[HQQ_RING_HEADER_SIZE + 2 * HQQ_RING_SLOT_STRIDE];
static uint64_t got_pos[2]; /* written by producer i only, read after the join */

static void *producer(void *arg) {
    uint64_t i = (uint64_t)(uintptr_t)arg;
    uint64_t pos = 0;
    uint64_t v = 1 + i;
    HQV_AWAIT(hqq_ring_try_claim_producer(&ring, &pos));
    memcpy(hqq_ring_slot(&ring, pos)->packet, &v, sizeof v);
    got_pos[i] = pos;
    hqq_ring_publish(&ring, pos);
    return NULL;
}

static void run_once(void) {
    pthread_t a, b;
    uint64_t k;
    hqv_init_ring(&ring, mem, 2);
    got_pos[0] = got_pos[1] = UINT64_MAX;
    if (pthread_create(&a, NULL, producer, (void *)(uintptr_t)0) != 0) abort();
    if (pthread_create(&b, NULL, producer, (void *)(uintptr_t)1) != 0) abort();
    if (pthread_join(a, NULL) != 0) abort();
    if (pthread_join(b, NULL) != 0) abort();

    assert(got_pos[0] < 2 && got_pos[1] < 2 && got_pos[0] != got_pos[1]);
    for (k = 0; k < 2; k++) {
        uint64_t pos = 0;
        uint64_t v = 0;
        uint64_t j;
        int ok = hqq_ring_try_claim_consumer(&ring, &pos);
        assert(ok && pos == k);
        memcpy(&v, hqq_ring_slot(&ring, pos)->packet, sizeof v);
        j = (got_pos[0] == pos) ? 0 : 1;
        assert(got_pos[j] == pos);
        assert(v == 1 + j);
        hqq_ring_release(&ring, pos);
    }
    assert(atomic_load_explicit(&ring.h->w, memory_order_acquire) == 2);
    assert(atomic_load_explicit(&ring.h->r, memory_order_acquire) == 2);
}

HQV_MAIN(run_once)
