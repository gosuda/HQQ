/* Smoke test: version macros, feature-test macros, atomics self-test. */
#include "hqq/hqq.h"
#include "hqq_test.h"

#include <string.h>

#if !defined(HQQ_C11) || !HQQ_C11
#  error "tests must be compiled as C11"
#endif
#if !HQQ_C11_ATOMICS
#  error "C11 atomics must be detected"
#endif

int main(void) {
    HQQ_CHECK(strcmp(hqq_version_string(), HQQ_VERSION_STRING) == 0);
    HQQ_CHECK_EQ_INT(hqq_version_number(), HQQ_VERSION_NUMBER);
    HQQ_CHECK(HQQ_VERSION_AT_LEAST(0, 0, 0));
    HQQ_CHECK(!HQQ_VERSION_AT_LEAST(HQQ_VERSION_MAJOR + 1, 0, 0));
    HQQ_CHECK_EQ_INT(hqq_selftest_atomics(), 0);

    /* Exactly one OS / compiler macro family is selected. */
    {
        int os = 0, cc = 0;
#ifdef HQQ_OS_WINDOWS
        os++;
#endif
#ifdef HQQ_OS_DARWIN
        os++;
#endif
#ifdef HQQ_OS_LINUX
        os++;
#endif
#ifdef HQQ_COMPILER_MSVC
        cc++;
#endif
#ifdef HQQ_COMPILER_CLANG
        cc++;
#endif
#ifdef HQQ_COMPILER_GCC
        cc++;
#endif
        HQQ_CHECK_EQ_INT(os, 1);
        HQQ_CHECK_EQ_INT(cc, 1);
    }
    return HQQ_TEST_RESULT("test_smoke");
}
