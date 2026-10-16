/* Smoke implementation: version query and an atomics self-test. */
#define HQQ_BUILDING 1
#include "hqq/hqq.h"

#include <stdatomic.h>
#include <stdint.h>

/*
 * Atomics placed in shared memory must be lock-free and address-free: a
 * lock-based fallback keeps its lock in process-local state. Refuse to
 * build where the guarantee is missing.
 */
#if ATOMIC_INT_LOCK_FREE != 2 || ATOMIC_LLONG_LOCK_FREE != 2
#  error "hqq requires always-lock-free 32-bit and 64-bit C11 atomics"
#endif

const char *hqq_version_string(void) { return HQQ_VERSION_STRING; }

unsigned hqq_version_number(void) { return (unsigned)HQQ_VERSION_NUMBER; }

int hqq_selftest_atomics(void) {
    _Atomic uint32_t a32;
    _Atomic uint64_t a64;
    uint64_t expected;

    atomic_init(&a32, 1u);
    atomic_init(&a64, UINT64_C(0x100000000));

    if (!atomic_is_lock_free(&a32) || !atomic_is_lock_free(&a64)) {
        return 1;
    }
    if (atomic_fetch_add_explicit(&a32, 1u, memory_order_seq_cst) != 1u ||
        atomic_load_explicit(&a32, memory_order_seq_cst) != 2u) {
        return 2;
    }
    if (atomic_fetch_add_explicit(&a64, 1u, memory_order_seq_cst) != UINT64_C(0x100000000) ||
        atomic_load_explicit(&a64, memory_order_seq_cst) != UINT64_C(0x100000001)) {
        return 3;
    }
    expected = UINT64_C(0x100000001);
    if (!atomic_compare_exchange_strong_explicit(&a64, &expected, UINT64_C(7),
                                                 memory_order_seq_cst,
                                                 memory_order_seq_cst) ||
        atomic_load_explicit(&a64, memory_order_seq_cst) != UINT64_C(7)) {
        return 4;
    }
    atomic_thread_fence(memory_order_seq_cst);
    return 0;
}
