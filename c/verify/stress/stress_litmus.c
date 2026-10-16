/*
 * Bounded stress litmus harness for the C11 memory orders HQQ relies on.
 *
 * Each test runs two threads for N iterations (or until a time budget is
 * spent). Every iteration starts with a spin barrier so that both threads
 * enter the racy window together; the outcome of each iteration is then
 * classified.
 *
 *   SB    store buffering  (the Dekker core of the wake handshake)
 *   MP    message passing  (publish / consume of a slot)
 *
 * For every test there is a "strong" variant that MUST never show the
 * forbidden outcome (seq_cst for SB; release/acquire for MP) and a "weak"
 * control (relaxed, or release/acquire for SB) that is ALLOWED to show it.
 * The control proves that the harness is sensitive: on weakly ordered
 * hardware (arm64) or with store buffers (x86-64 SB) it normally reports
 * weak outcomes, whereas a mis-implemented seq_cst would make the strong
 * variants fail. A control that never reports a weak outcome is not an
 * error (the hardware may be too strong or too slow to hit the window);
 * pass --require-control to make it one.
 *
 * Exit status: 0 all strong variants clean, 1 a strong variant exhibited a
 * forbidden outcome, 2 usage error or (with --require-control) a control
 * that never showed a weak outcome.
 */
#define _POSIX_C_SOURCE 200809L

#include "hqq/hqq.h"

#include <pthread.h>
#include <sched.h>
#include <stdatomic.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#define CACHE_LINE 128

#if defined(__SANITIZE_THREAD__)
#  define HQQ_TSAN 1
#elif defined(__has_feature)
#  if __has_feature(thread_sanitizer)
#    define HQQ_TSAN 1
#  endif
#endif

typedef enum { ORD_RELAXED, ORD_ACQREL, ORD_SEQCST } order_kind;

typedef struct {
    const char *name;
    int is_sb;           /* 1: store buffering, 0: message passing */
    order_kind order;
    int must_be_clean;   /* 1: strong variant, forbidden outcome is a failure */
} test_case;

/* One cache-line-aligned cell per iteration slot so the tests do not false-share. */
typedef struct {
    _Alignas(CACHE_LINE) _Atomic uint32_t x;
    _Alignas(CACHE_LINE) _Atomic uint32_t y;
    _Alignas(CACHE_LINE) uint32_t r0;
    _Alignas(CACHE_LINE) uint32_t r1;
} cell;

typedef struct {
    _Alignas(CACHE_LINE) _Atomic uint64_t arrived;
    _Alignas(CACHE_LINE) _Atomic int stop;
    cell *cells;
    uint64_t iterations;
    const test_case *tc;
    double deadline;     /* monotonic seconds; 0 = none */
    uint64_t done[2];    /* iterations completed by each thread (own writes only) */
} shared_state;

typedef struct {
    shared_state *st;
    int id;
} thread_arg;

static double now_seconds(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (double)ts.tv_sec + (double)ts.tv_nsec * 1e-9;
}

/* Two-party spin barrier for round i (0-based). Returns 0 when the run is over. */
static int barrier_wait(shared_state *st, uint64_t round) {
    uint64_t target = 2 * (round + 1);
    unsigned spins = 0;
    atomic_fetch_add_explicit(&st->arrived, 1, memory_order_seq_cst);
    while (atomic_load_explicit(&st->arrived, memory_order_seq_cst) < target) {
        if (++spins > 4096) { /* tolerate a single-CPU machine */
            sched_yield();
            spins = 0;
        }
        if (atomic_load_explicit(&st->stop, memory_order_relaxed)) {
            return 0;
        }
    }
    return 1;
}

static memory_order store_order(order_kind k) {
    switch (k) {
    case ORD_RELAXED: return memory_order_relaxed;
    case ORD_ACQREL:  return memory_order_release;
    default:          return memory_order_seq_cst;
    }
}

static memory_order load_order(order_kind k) {
    switch (k) {
    case ORD_RELAXED: return memory_order_relaxed;
    case ORD_ACQREL:  return memory_order_acquire;
    default:          return memory_order_seq_cst;
    }
}

static void *thread_main(void *p) {
    thread_arg *a = (thread_arg *)p;
    shared_state *st = a->st;
    const test_case *tc = st->tc;
    memory_order so = store_order(tc->order);
    memory_order lo = load_order(tc->order);

    for (uint64_t i = 0; i < st->iterations; i++) {
        cell *c = &st->cells[i];
        if (!barrier_wait(st, i)) {
            break;
        }
        if (tc->is_sb) {
            /* T0: x=1; r0=y     T1: y=1; r1=x     forbidden: r0==0 && r1==0 */
            if (a->id == 0) {
                atomic_store_explicit(&c->x, 1, so);
                c->r0 = atomic_load_explicit(&c->y, lo);
            } else {
                atomic_store_explicit(&c->y, 1, so);
                c->r1 = atomic_load_explicit(&c->x, lo);
            }
        } else {
            /* T0: x=1; y=1(so)  T1: r0=y(lo); r1=x   forbidden: r0==1 && r1==0 */
            if (a->id == 0) {
                atomic_store_explicit(&c->x, 1, memory_order_relaxed);
                atomic_store_explicit(&c->y, 1, so);
            } else {
                c->r0 = atomic_load_explicit(&c->y, lo);
                c->r1 = atomic_load_explicit(&c->x, memory_order_relaxed);
            }
        }
        st->done[a->id] = i + 1;
        if (a->id == 0) {
            if (st->deadline > 0 && (i & 1023) == 1023 && now_seconds() > st->deadline) {
                atomic_store_explicit(&st->stop, 1, memory_order_relaxed);
            }
        }
    }
    return NULL;
}

/* Runs one test; returns the number of forbidden outcomes, sets *done. */
static uint64_t run_test(const test_case *tc, uint64_t iterations, double seconds,
                         uint64_t *done) {
    shared_state *st = NULL;
    if (posix_memalign((void **)&st, CACHE_LINE, sizeof *st) != 0) {
        fprintf(stderr, "out of memory\n");
        exit(2);
    }
    memset(st, 0, sizeof *st);
    st->cells = (cell *)calloc(iterations, sizeof(cell));
    if (st->cells == NULL) {
        fprintf(stderr, "out of memory\n");
        exit(2);
    }
    st->iterations = iterations;
    st->tc = tc;
    st->deadline = seconds > 0 ? now_seconds() + seconds : 0;

    pthread_t th[2];
    thread_arg args[2] = {{st, 0}, {st, 1}};
    for (int i = 0; i < 2; i++) {
        if (pthread_create(&th[i], NULL, thread_main, &args[i]) != 0) {
            fprintf(stderr, "pthread_create failed\n");
            exit(2);
        }
    }
    for (int i = 0; i < 2; i++) {
        pthread_join(th[i], NULL);
    }

    /* Only iterations that both threads completed are classified. */
    uint64_t both = st->done[0] < st->done[1] ? st->done[0] : st->done[1];
    uint64_t weak = 0;
    for (uint64_t i = 0; i < both; i++) {
        const cell *c = &st->cells[i];
        if (tc->is_sb) {
            weak += (c->r0 == 0 && c->r1 == 0);
        } else {
            weak += (c->r0 == 1 && c->r1 == 0);
        }
    }
    *done = both;
    free(st->cells);
    free(st);
    return weak;
}

static void usage(const char *argv0) {
    fprintf(stderr,
            "usage: %s [--iterations N] [--seconds S] [--require-control]\n"
            "  --iterations N   iterations per test (default 200000)\n"
            "  --seconds S      time budget per test (default 1.0)\n"
            "  --require-control fail if a weak control never shows a weak outcome\n",
            argv0);
}

int main(int argc, char **argv) {
    uint64_t iterations = 200000;
    double seconds = 1.0;
    int require_control = 0;

    for (int i = 1; i < argc; i++) {
        if (strcmp(argv[i], "--iterations") == 0 && i + 1 < argc) {
            iterations = strtoull(argv[++i], NULL, 10);
        } else if (strcmp(argv[i], "--seconds") == 0 && i + 1 < argc) {
            seconds = strtod(argv[++i], NULL);
        } else if (strcmp(argv[i], "--require-control") == 0) {
            require_control = 1;
        } else {
            usage(argv[0]);
            return 2;
        }
    }
    if (iterations == 0 || iterations > 50000000ull || seconds < 0 || seconds > 60) {
        fprintf(stderr, "iterations must be in 1..50000000 and seconds in 0..60\n");
        return 2;
    }

    static const test_case cases[] = {
        {"SB seq_cst   (strong)", 1, ORD_SEQCST, 1},
        {"SB acq/rel   (control)", 1, ORD_ACQREL, 0},
        {"SB relaxed   (control)", 1, ORD_RELAXED, 0},
        {"MP rel/acq   (strong)", 0, ORD_ACQREL, 1},
        {"MP seq_cst   (strong)", 0, ORD_SEQCST, 1},
        {"MP relaxed   (control)", 0, ORD_RELAXED, 0},
    };

    printf("hqq stress litmus: hqq %s, %s\n", hqq_version_string(),
#if defined(HQQ_ARCH_ARM64)
           "arm64"
#elif defined(HQQ_ARCH_X86_64)
           "x86_64"
#else
           "unknown-arch"
#endif
    );
#ifdef HQQ_TSAN
    printf("note: built with ThreadSanitizer; timing is distorted, controls are not meaningful\n");
    require_control = 0;
#endif

    int rc = 0;
    for (size_t i = 0; i < sizeof cases / sizeof cases[0]; i++) {
        uint64_t done = 0;
        uint64_t weak = run_test(&cases[i], iterations, seconds, &done);
        const char *verdict;
        if (cases[i].must_be_clean) {
            verdict = weak == 0 ? "PASS" : "FAIL (forbidden outcome observed)";
            if (weak != 0) {
                rc = rc ? rc : 1;
            }
        } else {
            verdict = weak != 0 ? "weak outcomes observed (expected for a control)"
                                : "no weak outcome observed";
            if (weak == 0 && require_control) {
                verdict = "FAIL (control never showed a weak outcome)";
                rc = rc == 1 ? 1 : 2;
            }
        }
        printf("%-24s iterations=%-8llu weak=%-8llu %s\n", cases[i].name,
               (unsigned long long)done, (unsigned long long)weak, verdict);
    }
    return rc;
}
