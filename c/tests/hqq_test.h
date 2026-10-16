/* Tiny dependency-free test runner shared by the C tests. */
#ifndef HQQ_TEST_H
#define HQQ_TEST_H

#include <stdio.h>

static int hqq_test_failures = 0;
static int hqq_test_checks = 0;

#define HQQ_CHECK(cond)                                                      \
    do {                                                                     \
        hqq_test_checks++;                                                   \
        if (!(cond)) {                                                       \
            hqq_test_failures++;                                             \
            fprintf(stderr, "%s:%d: check failed: %s\n", __FILE__, __LINE__, \
                    #cond);                                                  \
        }                                                                    \
    } while (0)

#define HQQ_CHECK_EQ_INT(got, want)                                          \
    do {                                                                     \
        long long hqq_got_ = (long long)(got);                               \
        long long hqq_want_ = (long long)(want);                             \
        hqq_test_checks++;                                                   \
        if (hqq_got_ != hqq_want_) {                                         \
            hqq_test_failures++;                                             \
            fprintf(stderr, "%s:%d: %s = %lld, want %lld\n", __FILE__,       \
                    __LINE__, #got, hqq_got_, hqq_want_);                    \
        }                                                                    \
    } while (0)

/* Prints a summary and returns the process exit code. */
#define HQQ_TEST_RESULT(name)                                                \
    (hqq_test_failures == 0                                                  \
         ? (printf("%s: ok (%d checks)\n", (name), hqq_test_checks), 0)      \
         : (printf("%s: FAILED (%d of %d checks)\n", (name),                 \
                   hqq_test_failures, hqq_test_checks),                      \
            1))

#endif /* HQQ_TEST_H */
