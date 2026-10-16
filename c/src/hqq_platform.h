/*
 * hqq_platform.h - the operating-system interface of libhqq (internal).
 *
 * One implementation per platform family:
 *   hqq_platform_posix.c  Linux, macOS, FreeBSD (and other POSIX systems)
 *   hqq_platform_win32.c  Windows (named file mappings + named semaphores)
 *
 * Everything above this interface (the superblock engine, the link, the
 * named-segment logic) is portable C11. Functions here never keep global
 * state, with one exception documented at hqq_plat_wait_init (macOS probes
 * the available wait primitive once).
 */
#ifndef HQQ_PLATFORM_H
#define HQQ_PLATFORM_H

#include <stddef.h>
#include <stdint.h>

#include "hqq/hqq.h"
#include "hqq_ring.h"

/* ---- Time, identity, randomness -------------------------------------------- */

uint64_t hqq_plat_mono_ns(void);     /* monotonic clock, ns */
uint64_t hqq_plat_wall_ns(void);     /* wall clock as Unix time in ns (CLOCK_REALTIME / FILETIME) */
void hqq_plat_sleep_ns(uint64_t ns); /* sleeps at least ns (may be longer); ns == 0 yields */
void hqq_plat_yield(void);           /* sched_yield / SwitchToThread */
uint64_t hqq_plat_pid(void);
uint64_t hqq_plat_page_size(void);   /* power of two >= 4096 */
int hqq_plat_random(void *buf, size_t n); /* cryptographic-quality bytes; 0 on success */

/* ---- Threads and events (heartbeat thread) ------------------------------------ */

typedef struct hqq_plat_thread hqq_plat_thread;
typedef struct hqq_plat_event hqq_plat_event;

/* Starts fn(arg) on a new thread. NULL on failure (*os_err set). */
hqq_plat_thread *hqq_plat_thread_start(const hqq_allocator *a, void (*fn)(void *), void *arg, int *os_err);
/* Joins the thread and frees its handle. */
void hqq_plat_thread_join(const hqq_allocator *a, hqq_plat_thread *t);

/* A manual-reset event: once signalled it stays signalled. */
hqq_plat_event *hqq_plat_event_create(const hqq_allocator *a);
void hqq_plat_event_destroy(const hqq_allocator *a, hqq_plat_event *e);
void hqq_plat_event_signal(hqq_plat_event *e);
void hqq_plat_event_reset(hqq_plat_event *e);
/* Waits up to timeout_ns for the signal; returns 1 if signalled, 0 on timeout. */
int hqq_plat_event_wait(hqq_plat_event *e, uint64_t timeout_ns);

/* ---- Named shared memory (SPEC 11) ---------------------------------------------- */

typedef struct hqq_shm hqq_shm;

/* 1 where names outlive their handles (POSIX), 0 on Windows. */
int hqq_plat_names_persist(void);

/*
 * Opens (create == 0) or exclusively creates (create != 0: O_CREAT|O_EXCL;
 * the object is sized to `size`) the named object. Name validation per SPEC
 * 11.1. Results:
 *   HQQ_OK, HQQ_ERR_INVALID_NAME (bad name; on Linux also "not a regular
 *   file"), HQQ_ERR_NOT_EXIST (open: no such name), HQQ_ERR_EXISTS (create:
 *   name exists), HQQ_ERR_NO_MEMORY, HQQ_ERR_SYSTEM (*os_err).
 * A failed creation removes the object again. `perm` is the POSIX mode of a
 * created object (Windows: translated to a DACL per SPEC 11.5; the four wake
 * semaphores are created before the mapping, and opened after it when
 * opening).
 */
hqq_status hqq_plat_shm_open(const hqq_allocator *a, const char *name, size_t size, int create,
                             unsigned perm, hqq_shm **out, int *os_err);

/* Current size of the object (0 while its creator has not sized it); -1 on error. */
int64_t hqq_plat_shm_size(hqq_shm *shm, int *os_err);

/* Maps `size` bytes read-write shared. The result is page aligned. */
hqq_status hqq_plat_shm_map(hqq_shm *shm, size_t size, void **out, int *os_err);
void hqq_plat_shm_unmap(hqq_shm *shm, void *p, size_t size);

/* Closes the descriptor/handles and frees the object. Mappings stay valid until unmapped. */
void hqq_plat_shm_close(const hqq_allocator *a, hqq_shm *shm);

/*
 * Safe-unlink identity (SPEC 11.4). *known = 1 when the platform can compare
 * the name with the open object (Linux: fstat vs lstat), then *same tells
 * whether the name still refers to it (a missing name: same = 0). *known = 0
 * on macOS/FreeBSD/Windows: the caller identifies the object through the
 * creatorToken instead.
 */
hqq_status hqq_plat_shm_name_refers_to_self(hqq_shm *shm, int *known, int *same);

/*
 * Reads state and creatorToken from the first page of whatever object `name`
 * refers to right now (read-only, no modification). *found = 0 when the name
 * does not exist or the object is smaller than `page_size`. (macOS/FreeBSD
 * safe-unlink path.) Windows: *found = 0.
 */
hqq_status hqq_plat_shm_peek(const char *name, size_t page_size, uint64_t *state,
                             uint64_t *creator_token, int *found);

/* Removes the name of the object shm was opened from (no identity check).
 * Missing name: HQQ_ERR_NOT_EXIST. Windows: HQQ_OK, no-op. */
hqq_status hqq_plat_shm_delete(hqq_shm *shm);

/* Removes a name (validates it first). Missing name: HQQ_ERR_NOT_EXIST. Windows: validates only. */
hqq_status hqq_plat_unlink(const char *name);

/* ---- Parking ------------------------------------------------------------------------ */

/*
 * Per-ring wait context, embedded in the link; the ring's hqq_ring_ops get a
 * pointer to it. Fields are platform private (zero-initialized by the link,
 * filled by hqq_plat_wait_init).
 */
typedef struct hqq_plat_wait {
    void *handle[2]; /* Windows: semaphores [NOT_EMPTY, NOT_FULL]; unused elsewhere */
    int mode;        /* platform private selector (macOS: which primitive) */
    int kind;        /* HQQ_PARK_* */
} hqq_plat_wait;

#define HQQ_PARK_NONE 0        /* no kernel wait: waiters sleep with backoff */
#define HQQ_PARK_LOCAL 1       /* parks, but wakes come only from this process (Windows WaitOnAddress);
                                  waiters bound each park by the backoff step */
#define HQQ_PARK_SHARED 2      /* wake words work across processes (futex, ulock, semaphores) */

/*
 * Prepares the wait context of ring `ring_index` (0 or 1). `shm` is NULL for
 * caller-owned memory. `flags` are the HQQ_FLAG_* of the options. On macOS
 * the first call probes (dlsym) whether os_sync_wait_on_address exists; the
 * result is cached in a process-wide atomic (the only global state).
 */
void hqq_plat_wait_init(hqq_plat_wait *w, hqq_shm *shm, int ring_index, uint32_t flags);

/* The wait/wake hooks (NULL on platforms without any). ctx is a hqq_plat_wait *. */
const hqq_ring_ops *hqq_plat_ring_ops(void);

#endif /* HQQ_PLATFORM_H */
