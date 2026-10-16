/*
 * GenMC harness (c): the reservation completion guard of the real ring core.
 *
 * The main thread claims one producer reservation (position `reservation`)
 * and writes its payload. Two completer threads, each holding its own copy of
 * the ring handle (as a process-local copy would), both try to complete that
 * same reservation with hqq_ring_publish_guarded(). The guard is a CAS of the
 * slot sequence pos -> pos+1, so in every execution exactly one completion
 * succeeds; the stale copy fails and changes nothing. After the join the
 * payload is consumed once, from the reserved position.
 *
 * Variants (after GenMC's `--`):
 *   (none)                 must PASS
 *   -DHQQ_VERIFY_UNGUARDED negative: the completers call hqq_ring_publish() (no
 *                          CAS), so both complete the reservation: must FAIL
 *                          (two completions)
 */
#include "hqq_model.h"

#include <pthread.h>

static hqq_ring ring;
static _Alignas(64) unsigned char mem[HQQ_RING_HEADER_SIZE + 2 * HQQ_RING_SLOT_STRIDE];
static uint64_t reservation;
static atomic_uint completions;

static void *completer(void *arg) {
    hqq_ring copy = ring; /* this completer's own (possibly stale) handle */
    (void)arg;
#ifdef HQQ_VERIFY_UNGUARDED
    hqq_ring_publish(&copy, reservation);
    atomic_fetch_add_explicit(&completions, 1, memory_order_relaxed);
#else
    if (hqq_ring_publish_guarded(&copy, reservation)) {
        atomic_fetch_add_explicit(&completions, 1, memory_order_relaxed);
    }
#endif
    return NULL;
}

static void run_once(void) {
    pthread_t a, b;
    uint64_t v = 7;
    uint64_t got = 0;
    uint64_t pos = 0;
    int ok;

    hqv_init_ring(&ring, mem, 2);
    atomic_store_explicit(&completions, 0, memory_order_relaxed);
    ok = hqq_ring_try_claim_producer(&ring, &reservation);
    assert(ok && reservation == 0);
    memcpy(hqq_ring_slot(&ring, reservation)->packet, &v, sizeof v);

    if (pthread_create(&a, NULL, completer, NULL) != 0) abort();
    if (pthread_create(&b, NULL, completer, NULL) != 0) abort();
    if (pthread_join(a, NULL) != 0) abort();
    if (pthread_join(b, NULL) != 0) abort();

    assert(atomic_load_explicit(&completions, memory_order_acquire) == 1);
    ok = hqq_ring_try_claim_consumer(&ring, &pos);
    assert(ok && pos == reservation);
    memcpy(&got, hqq_ring_slot(&ring, pos)->packet, sizeof got);
    assert(got == v);
    hqq_ring_release(&ring, pos);
}

HQV_MAIN(run_once)
