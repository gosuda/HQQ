/* Portable helpers shared by the C tests. */
#if defined(__linux__) && !defined(_GNU_SOURCE)
#  define _GNU_SOURCE 1
#endif
#if defined(__APPLE__) && !defined(_DARWIN_C_SOURCE)
#  define _DARWIN_C_SOURCE 1
#endif

#include "hqq_testutil.h"

#include <stdio.h>
#include <stdlib.h>

#include "hqq_platform.h"

#ifdef _WIN32
#  define WIN32_LEAN_AND_MEAN
#  define NOMINMAX
#  include <windows.h>
#else
#  include <sys/mman.h>
#endif

void *hqq_test_pages(size_t size) {
#ifdef _WIN32
    void *p = VirtualAlloc(NULL, size, MEM_COMMIT | MEM_RESERVE, PAGE_READWRITE);
    if (p == NULL) {
        fprintf(stderr, "hqq_test_pages: VirtualAlloc failed\n");
        abort();
    }
    return p;
#else
    void *p = mmap(NULL, size, PROT_READ | PROT_WRITE, MAP_SHARED | MAP_ANON, -1, 0);
    if (p == MAP_FAILED) {
        fprintf(stderr, "hqq_test_pages: mmap failed\n");
        abort();
    }
    return p;
#endif
}

void hqq_test_pages_free(void *p, size_t size) {
#ifdef _WIN32
    (void)size;
    VirtualFree(p, 0, MEM_RELEASE);
#else
    munmap(p, size);
#endif
}

void hqq_test_unique_name(char *buf, size_t cap, const char *tag) {
    static unsigned counter;
    snprintf(buf, cap, "hqq%lu_%u_%s", (unsigned long)hqq_plat_pid() % 1000000ul, ++counter, tag);
}

void hqq_test_sleep_ms(unsigned ms) { hqq_plat_sleep_ns((uint64_t)ms * 1000000u); }

uint64_t hqq_test_now_ns(void) { return hqq_plat_mono_ns(); }

struct hqq_test_thread {
    hqq_plat_thread *t;
};

hqq_test_thread *hqq_test_thread_start(void (*fn)(void *), void *arg) {
    hqq_test_thread *t = (hqq_test_thread *)malloc(sizeof *t);
    int err = 0;
    if (t == NULL || (t->t = hqq_plat_thread_start(NULL, fn, arg, &err)) == NULL) {
        fprintf(stderr, "hqq_test_thread_start failed (%d)\n", err);
        abort();
    }
    return t;
}

void hqq_test_thread_join(hqq_test_thread *t) {
    hqq_plat_thread_join(NULL, t->t);
    free(t);
}
