/*
 * hqq.h - HQQ shared-memory IPC, C reference library (scaffold).
 *
 * This header currently carries only the version and feature-test macros.
 * The superblock and ring implementation follows in a later milestone. The
 * header is valid C11 and valid C++17 (extern "C" linkage); it intentionally
 * does not include <stdatomic.h>, which C++17 does not provide.
 */
#ifndef HQQ_HQQ_H
#define HQQ_HQQ_H

#ifdef __cplusplus
extern "C" {
#endif

/* ---- Version ---------------------------------------------------------- */

#define HQQ_VERSION_MAJOR 0
#define HQQ_VERSION_MINOR 0
#define HQQ_VERSION_PATCH 0
#define HQQ_VERSION_STRING "0.0.0-scaffold"

/* Single comparable number: major * 10000 + minor * 100 + patch. */
#define HQQ_VERSION_NUMBER \
    (HQQ_VERSION_MAJOR * 10000 + HQQ_VERSION_MINOR * 100 + HQQ_VERSION_PATCH)

/* True when the header version is at least major.minor.patch. */
#define HQQ_VERSION_AT_LEAST(major, minor, patch) \
    (HQQ_VERSION_NUMBER >= ((major) * 10000 + (minor) * 100 + (patch)))

/* ---- Feature-test macros ---------------------------------------------- */

/* Language level of the consumer. */
#if defined(__cplusplus)
#  define HQQ_LANG_CXX 1
#  if (defined(_MSVC_LANG) && _MSVC_LANG >= 201703L) || __cplusplus >= 201703L
#    define HQQ_CXX17 1
#  endif
#else
#  define HQQ_LANG_CXX 0
#  if defined(__STDC_VERSION__) && __STDC_VERSION__ >= 201112L
#    define HQQ_C11 1
#  endif
#endif

/*
 * HQQ_C11_ATOMICS is 1 when the library is built against C11 <stdatomic.h>.
 * MSVC keeps defining __STDC_NO_ATOMICS__ even with /experimental:c11atomics,
 * so the build system (c/CMakeLists.txt) decides and passes
 * -DHQQ_FORCE_C11_ATOMICS=1 for that case.
 */
#if !defined(__cplusplus) && defined(HQQ_C11) && \
    (!defined(__STDC_NO_ATOMICS__) || defined(HQQ_FORCE_C11_ATOMICS))
#  define HQQ_C11_ATOMICS 1
#else
#  define HQQ_C11_ATOMICS 0
#endif

/* Compiler family. Exactly one of these is defined to 1. */
#if defined(_MSC_VER) && !defined(__clang__)
#  define HQQ_COMPILER_MSVC 1
#elif defined(__clang__)
#  define HQQ_COMPILER_CLANG 1
#elif defined(__GNUC__)
#  define HQQ_COMPILER_GCC 1
#endif

/* Target operating system. Exactly one of these is defined to 1. */
#if defined(_WIN32)
#  define HQQ_OS_WINDOWS 1
#elif defined(__APPLE__)
#  define HQQ_OS_DARWIN 1
#elif defined(__linux__)
#  define HQQ_OS_LINUX 1
#endif

/* Target CPU architecture. Exactly one of these is defined to 1. */
#if defined(__x86_64__) || defined(_M_X64) || defined(_M_AMD64)
#  define HQQ_ARCH_X86_64 1
#elif defined(__aarch64__) || defined(_M_ARM64)
#  define HQQ_ARCH_ARM64 1
#endif

/* Symbol visibility for the (optional) shared library build. */
#if defined(HQQ_SHARED) && defined(_WIN32)
#  if defined(HQQ_BUILDING)
#    define HQQ_API __declspec(dllexport)
#  else
#    define HQQ_API __declspec(dllimport)
#  endif
#elif defined(__GNUC__) || defined(__clang__)
#  define HQQ_API __attribute__((visibility("default")))
#else
#  define HQQ_API
#endif

/* ---- Smoke-test entry points ------------------------------------------ */

/* Returns HQQ_VERSION_STRING. */
HQQ_API const char *hqq_version_string(void);

/* Returns HQQ_VERSION_NUMBER as compiled into the library. */
HQQ_API unsigned hqq_version_number(void);

/*
 * Exercises 32-bit and 64-bit seq_cst atomics on a stack object and checks
 * that they are lock-free (a requirement for atomics in shared memory).
 * Returns 0 on success, non-zero on failure.
 */
HQQ_API int hqq_selftest_atomics(void);

#ifdef __cplusplus
} /* extern "C" */
#endif

#endif /* HQQ_HQQ_H */
