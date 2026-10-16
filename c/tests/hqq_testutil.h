/* Portable helpers shared by the C tests (compiled into every test binary). */
#ifndef HQQ_TESTUTIL_H
#define HQQ_TESTUTIL_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

/* Page-aligned zero-filled memory that behaves like a shared mapping
 * (POSIX: mmap MAP_SHARED|MAP_ANON; Windows: VirtualAlloc). Never fails:
 * aborts the test on error. */
void *hqq_test_pages(size_t size);
void hqq_test_pages_free(void *p, size_t size);

/* A name valid on every platform (<= 30 bytes): "hqq<pid>_<n>_<tag>". */
void hqq_test_unique_name(char *buf, size_t cap, const char *tag);

/* Sleeps and clocks (thin wrappers over the library platform layer). */
void hqq_test_sleep_ms(unsigned ms);
uint64_t hqq_test_now_ns(void);

/* Tiny thread wrapper: runs fn(arg) on a new thread; join waits for it. */
typedef struct hqq_test_thread hqq_test_thread;
hqq_test_thread *hqq_test_thread_start(void (*fn)(void *), void *arg);
void hqq_test_thread_join(hqq_test_thread *t);

#ifdef __cplusplus
}
#endif

#endif /* HQQ_TESTUTIL_H */
