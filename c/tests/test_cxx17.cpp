// C++17 consumer compile check: hqq.h is usable from C++ with C linkage.
#include "hqq/hqq.h"
#include "hqq/hqq_lifeline.h"

#include <cstdio>
#include <cstring>

#if !defined(HQQ_CXX17)
#  error "consumer must be compiled as C++17 or newer"
#endif
#if HQQ_C11_ATOMICS
#  error "C11 atomics macro must be 0 in C++"
#endif

static_assert(HQQ_VERSION_NUMBER >= 0, "version number");

int main() {
    int failures = 0;
    if (std::strcmp(hqq_version_string(), HQQ_VERSION_STRING) != 0) {
        std::fprintf(stderr, "version string mismatch\n");
        failures++;
    }
    if (hqq_version_number() != static_cast<unsigned>(HQQ_VERSION_NUMBER)) {
        std::fprintf(stderr, "version number mismatch\n");
        failures++;
    }
    if (hqq_selftest_atomics() != 0) {
        std::fprintf(stderr, "atomics self-test failed\n");
        failures++;
    }
    std::printf("test_cxx17: %s\n", failures == 0 ? "ok" : "FAILED");
    return failures == 0 ? 0 : 1;
}
