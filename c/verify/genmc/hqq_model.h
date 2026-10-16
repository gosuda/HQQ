/*
 * hqq_model.h - shared scaffolding of the GenMC harnesses under c/verify/genmc/.
 *
 * The harnesses include the REAL ring core, c/src/hqq_ring.h (not a copy),
 * and drive it through a small fixed scenario. Each harness builds in two modes:
 *
 *   GenMC (default). main() runs the scenario once and GenMC explores every
 *     RC11 execution. Blocking is __VERIFIER_assume() (HQV_AWAIT). The kernel
 *     hooks hqq_ring_ops are model stubs: a waiter that goes to sleep is
 *     recorded and returns at once; the harness then checks the recorded state.
 *     A kernel wait(word, expect) blocks forever iff the word still equals
 *     expect when the waiter gets to sleep and nobody changes it afterwards,
 *     so "word != expect at the end" is exactly "no lost wake-up".
 *
 *   Host (-DHQQ_VERIFY_HOST, POSIX threads). main() runs the scenario N times
 *     (argv[1], default 2000) with real threads. HQV_AWAIT spins with
 *     sched_yield(). The hooks are a mutex and condition variable; a waiter
 *     still asleep on an unchanged word after 5 s is a lost wake-up and aborts.
 *
 * Weakened variants (GenMC only; they must FAIL). Define at most one of:
 *   -DHQQ_VERIFY_WEAKEN_ACQREL   every seq_cst access of the ring core becomes
 *                                the strongest acquire/release order valid for
 *                                its operation (load acquire, store release,
 *                                RMW acq_rel, CAS acq_rel / acquire on failure);
 *   -DHQQ_VERIFY_WEAKEN_RELAXED  every seq_cst access becomes relaxed.
 * Acquire/release/relaxed accesses written in the header are left alone, and
 * the harness code never uses seq_cst, so only the ring core's seq_cst
 * accesses (publication K1, notify K2/K3, park W1/W2/W3, guarded CAS) change.
 * The rebinding goes through the clang __c11_atomic builtins, so weakened
 * builds need clang.
 */
#ifndef HQQ_VERIFY_MODEL_H
#define HQQ_VERIFY_MODEL_H

#undef NDEBUG /* the checks are assert()s: keep them in every build */

#ifdef HQQ_VERIFY_HOST
#  ifndef _POSIX_C_SOURCE
#    define _POSIX_C_SOURCE 200809L
#  endif
#endif

#include <assert.h>
#include <stdatomic.h>
#include <stddef.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#ifdef HQQ_VERIFY_HOST
#  include <errno.h>
#  include <pthread.h>
#  include <sched.h>
#  include <stdio.h>
#  include <time.h>
#endif

#if defined(HQQ_VERIFY_WEAKEN_ACQREL) && defined(HQQ_VERIFY_WEAKEN_RELAXED)
#  error "define at most one of HQQ_VERIFY_WEAKEN_ACQREL and HQQ_VERIFY_WEAKEN_RELAXED"
#endif

#if defined(HQQ_VERIFY_WEAKEN_ACQREL) || defined(HQQ_VERIFY_WEAKEN_RELAXED)
#  if !defined(__clang__)
#    error "the weakened variants rebind the clang __c11_atomic builtins: build them with clang"
#  endif
#  define HQQ_V_PICK(mo, weak) (((mo) == memory_order_seq_cst) ? (weak) : (mo))
#  if defined(HQQ_VERIFY_WEAKEN_ACQREL)
#    define HQQ_V_LOAD(mo)     HQQ_V_PICK((mo), memory_order_acquire)
#    define HQQ_V_STORE(mo)    HQQ_V_PICK((mo), memory_order_release)
#    define HQQ_V_RMW(mo)      HQQ_V_PICK((mo), memory_order_acq_rel)
#    define HQQ_V_CAS_OK(mo)   HQQ_V_PICK((mo), memory_order_acq_rel)
#    define HQQ_V_CAS_FAIL(mo) HQQ_V_PICK((mo), memory_order_acquire)
#  else
#    define HQQ_V_LOAD(mo)     HQQ_V_PICK((mo), memory_order_relaxed)
#    define HQQ_V_STORE(mo)    HQQ_V_PICK((mo), memory_order_relaxed)
#    define HQQ_V_RMW(mo)      HQQ_V_PICK((mo), memory_order_relaxed)
#    define HQQ_V_CAS_OK(mo)   HQQ_V_PICK((mo), memory_order_relaxed)
#    define HQQ_V_CAS_FAIL(mo) HQQ_V_PICK((mo), memory_order_relaxed)
#  endif
#  undef atomic_load_explicit
#  define atomic_load_explicit(obj, mo) __c11_atomic_load((obj), HQQ_V_LOAD(mo))
#  undef atomic_store_explicit
#  define atomic_store_explicit(obj, val, mo) __c11_atomic_store((obj), (val), HQQ_V_STORE(mo))
#  undef atomic_fetch_add_explicit
#  define atomic_fetch_add_explicit(obj, val, mo) __c11_atomic_fetch_add((obj), (val), HQQ_V_RMW(mo))
#  undef atomic_fetch_sub_explicit
#  define atomic_fetch_sub_explicit(obj, val, mo) __c11_atomic_fetch_sub((obj), (val), HQQ_V_RMW(mo))
#  undef atomic_compare_exchange_strong_explicit
#  define atomic_compare_exchange_strong_explicit(obj, exp, des, ok, fail) \
       __c11_atomic_compare_exchange_strong((obj), (exp), (des), HQQ_V_CAS_OK(ok), HQQ_V_CAS_FAIL(fail))
#  undef atomic_compare_exchange_weak_explicit
#  define atomic_compare_exchange_weak_explicit(obj, exp, des, ok, fail) \
       __c11_atomic_compare_exchange_weak((obj), (exp), (des), HQQ_V_CAS_OK(ok), HQQ_V_CAS_FAIL(fail))
#endif

/* The real ring core. Included after the weakening macros, which must
 * precede every use of the atomics it contains. */
#include "../../src/hqq_ring.h"

/* ---- Blocking: the verification-only hooks ------------------------------- */

#ifdef HQQ_VERIFY_HOST
#  define HQV_AWAIT(cond) do { while (!(cond)) { sched_yield(); } } while (0)
#else
void __VERIFIER_assume(int);
#  define HQV_AWAIT(cond) __VERIFIER_assume(!!(cond))
#endif

/* The last wait() of the waiter: GenMC records it, the host run only checks. */
static struct {
    int parked;              /* the waiter decided to sleep (wait() entered)  */
    uint32_t expect;         /* the word value it went to sleep on            */
    _Atomic uint32_t *word;  /* the wake word                                 */
} hqv_park;

#ifdef HQQ_VERIFY_HOST
static pthread_mutex_t hqv_mu = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t hqv_cv = PTHREAD_COND_INITIALIZER;
#endif

static void hqv_wait(void *ctx, int side, _Atomic uint32_t *word, uint32_t expect, uint64_t timeout_ns) {
    (void)ctx;
    (void)side;
    (void)timeout_ns;
    hqv_park.parked = 1;
    hqv_park.expect = expect;
    hqv_park.word = word;
#ifdef HQQ_VERIFY_HOST
    pthread_mutex_lock(&hqv_mu);
    while (atomic_load_explicit(word, memory_order_acquire) == expect) {
        struct timespec deadline;
        deadline.tv_sec = time(NULL) + 5;
        deadline.tv_nsec = 0;
        if (pthread_cond_timedwait(&hqv_cv, &hqv_mu, &deadline) == ETIMEDOUT &&
            atomic_load_explicit(word, memory_order_acquire) == expect) {
            fprintf(stderr, "lost wake-up: still asleep on word == %u after 5 s\n", (unsigned)expect);
            abort();
        }
    }
    pthread_mutex_unlock(&hqv_mu);
#endif
}

static void hqv_wake(void *ctx, int side, _Atomic uint32_t *word, uint32_t waiters) {
    (void)ctx;
    (void)side;
    (void)word;
    (void)waiters;
#ifdef HQQ_VERIFY_HOST
    pthread_mutex_lock(&hqv_mu);
    pthread_cond_broadcast(&hqv_cv);
    pthread_mutex_unlock(&hqv_mu);
#endif
}

static const hqq_ring_ops hqv_ops = {hqv_wait, hqv_wake};

/* ---- Setup and entry point ----------------------------------------------- */

/* Formats `mem` with `slots` slots and attaches `r` through the real attach
 * path, with the hooks installed (the wake word is bumped on every notify). */
static void hqv_init_ring(hqq_ring *r, unsigned char *mem, uint64_t slots) {
    int rc = hqq_ring_format(mem, slots, NULL, NULL);
    assert(rc == 0);
    rc = hqq_ring_attach(r, mem);
    assert(rc == HQQ_RING_OK);
    r->ops = &hqv_ops;
    r->ops_ctx = NULL;
}

/* HQV_MAIN(body): GenMC runs body() once; the host build runs it argv[1] times. */
#ifdef HQQ_VERIFY_HOST
#  define HQV_MAIN(body)                                                          \
     int main(int argc, char **argv) {                                            \
         long i, n = (argc > 1) ? strtol(argv[1], NULL, 10) : 2000;               \
         for (i = 0; i < n; i++) {                                                \
             body();                                                              \
         }                                                                        \
         printf("ok: %ld iterations\n", n);                                       \
         return 0;                                                                \
     }
#else
#  define HQV_MAIN(body)                                                          \
     int main(void) {                                                             \
         body();                                                                  \
         return 0;                                                                \
     }
#endif

#endif /* HQQ_VERIFY_MODEL_H */
