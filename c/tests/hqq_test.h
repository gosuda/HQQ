/* Tiny dependency-free test runner shared by the C tests (thread-safe counters). */
#ifndef HQQ_TEST_H
#define HQQ_TEST_H

#include <stdatomic.h>
#include <stdio.h>
#include <stdlib.h>

#include "hqq/hqq.h"

/* A translation unit may include this header without using the counters (a libFuzzer harness). */
#if defined(__GNUC__) || defined(__clang__)
#  define HQQ_TEST_UNUSED __attribute__((unused))
#else
#  define HQQ_TEST_UNUSED
#endif

static HQQ_TEST_UNUSED _Atomic int hqq_test_failures;
static HQQ_TEST_UNUSED _Atomic int hqq_test_checks;

#define HQQ_CHECK(cond)                                                      \
    do {                                                                     \
        atomic_fetch_add(&hqq_test_checks, 1);                               \
        if (!(cond)) {                                                       \
            atomic_fetch_add(&hqq_test_failures, 1);                         \
            fprintf(stderr, "%s:%d: check failed: %s\n", __FILE__, __LINE__, \
                    #cond);                                                  \
        }                                                                    \
    } while (0)

#define HQQ_CHECK_EQ_INT(got, want)                                          \
    do {                                                                     \
        long long hqq_got_ = (long long)(got);                               \
        long long hqq_want_ = (long long)(want);                             \
        atomic_fetch_add(&hqq_test_checks, 1);                               \
        if (hqq_got_ != hqq_want_) {                                         \
            atomic_fetch_add(&hqq_test_failures, 1);                         \
            fprintf(stderr, "%s:%d: %s = %lld, want %lld\n", __FILE__,       \
                    __LINE__, #got, hqq_got_, hqq_want_);                    \
        }                                                                    \
    } while (0)

/* Unsigned 64-bit variant (hex on failure). */
#define HQQ_CHECK_EQ_U64(got, want)                                          \
    do {                                                                     \
        unsigned long long hqq_got_ = (unsigned long long)(got);             \
        unsigned long long hqq_want_ = (unsigned long long)(want);           \
        atomic_fetch_add(&hqq_test_checks, 1);                               \
        if (hqq_got_ != hqq_want_) {                                         \
            atomic_fetch_add(&hqq_test_failures, 1);                         \
            fprintf(stderr, "%s:%d: %s = 0x%llx, want 0x%llx\n", __FILE__,   \
                    __LINE__, #got, hqq_got_, hqq_want_);                    \
        }                                                                    \
    } while (0)

/* Compares hqq_status values, printing both as text on failure. */
#define HQQ_CHECK_STATUS(got, want)                                          \
    do {                                                                     \
        hqq_status hqq_got_ = (got);                                         \
        hqq_status hqq_want_ = (want);                                       \
        atomic_fetch_add(&hqq_test_checks, 1);                               \
        if (hqq_got_ != hqq_want_) {                                         \
            atomic_fetch_add(&hqq_test_failures, 1);                         \
            fprintf(stderr, "%s:%d: %s = %d (%s), want %d (%s)\n", __FILE__, \
                    __LINE__, #got, (int)hqq_got_, hqq_strerror(hqq_got_),   \
                    (int)hqq_want_, hqq_strerror(hqq_want_));                \
        }                                                                    \
    } while (0)

/* Like HQQ_CHECK but aborts the test binary on failure (use for setup). */
#define HQQ_REQUIRE(cond)                                                    \
    do {                                                                     \
        if (!(cond)) {                                                       \
            fprintf(stderr, "%s:%d: REQUIRE failed: %s\n", __FILE__,         \
                    __LINE__, #cond);                                        \
            fflush(stderr);                                                  \
            exit(2);                                                         \
        }                                                                    \
    } while (0)

/* Prints a summary and returns the process exit code. */
#define HQQ_TEST_RESULT(name)                                                \
    (atomic_load(&hqq_test_failures) == 0                                    \
         ? (printf("%s: ok (%d checks)\n", (name),                           \
                   atomic_load(&hqq_test_checks)),                           \
            0)                                                               \
         : (printf("%s: FAILED (%d of %d checks)\n", (name),                 \
                   atomic_load(&hqq_test_failures),                          \
                   atomic_load(&hqq_test_checks)),                           \
            1))

#endif /* HQQ_TEST_H */
