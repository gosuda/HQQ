/*
 * POSIX platform layer: Linux, macOS, FreeBSD and other POSIX systems.
 *
 *   Linux   /dev/shm/<base> regular files (SPEC 11.2), futex (shared words)
 *   macOS   shm_open("/<base>") (30 byte names), os_sync_wait_on_address with
 *           the SHARED flag when the system has it (macOS 14.4+, probed with
 *           dlsym), else the private __ulock_wait/__ulock_wake
 *           UL_COMPARE_AND_WAIT_SHARED the Go implementation uses
 *   others  shm_open; no cross-process wait: waiters use the simple backoff
 */
#if defined(__linux__)
#  ifndef _GNU_SOURCE
#    define _GNU_SOURCE 1
#  endif
#elif defined(__APPLE__)
#  ifndef _DARWIN_C_SOURCE
#    define _DARWIN_C_SOURCE 1
#  endif
#else
#  ifndef _POSIX_C_SOURCE
#    define _POSIX_C_SOURCE 200809L
#  endif
#  ifndef __BSD_VISIBLE
#    define __BSD_VISIBLE 1
#  endif
#endif

#define HQQ_BUILDING 1
#include "hqq_internal.h"

#include <errno.h>
#include <fcntl.h>
#include <limits.h>
#include <pthread.h>
#include <sched.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <time.h>
#include <unistd.h>

#if defined(__linux__)
#  include <sys/syscall.h>
#  include <sys/random.h>
#elif defined(__APPLE__)
#  include <dlfcn.h>
#  include <sys/random.h>
#  include <mach/mach_time.h>
#elif defined(__FreeBSD__) || defined(__NetBSD__) || defined(__OpenBSD__)
#  include <sys/random.h>
#endif

/* ---- Time, identity, randomness ---------------------------------------------------- */

uint64_t hqq_plat_mono_ns(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (uint64_t)ts.tv_sec * HQQ_NS_PER_SEC + (uint64_t)ts.tv_nsec;
}

uint64_t hqq_plat_wall_ns(void) {
    struct timespec ts;
    clock_gettime(CLOCK_REALTIME, &ts);
    return (uint64_t)ts.tv_sec * HQQ_NS_PER_SEC + (uint64_t)ts.tv_nsec;
}

void hqq_plat_sleep_ns(uint64_t ns) {
    struct timespec ts, rem;
    if (ns == 0) {
        sched_yield();
        return;
    }
    ts.tv_sec = (time_t)(ns / HQQ_NS_PER_SEC);
    ts.tv_nsec = (long)(ns % HQQ_NS_PER_SEC);
    while (nanosleep(&ts, &rem) != 0 && errno == EINTR) {
        ts = rem;
    }
}

void hqq_plat_yield(void) { sched_yield(); }

uint64_t hqq_plat_pid(void) { return (uint64_t)getpid(); }

uint64_t hqq_plat_page_size(void) {
    long p = sysconf(_SC_PAGESIZE);
    return p > 0 ? (uint64_t)p : HQQ_MIN_PAGE_SIZE;
}

int hqq_plat_random(void *buf, size_t n) {
    unsigned char *p = (unsigned char *)buf;
    int fd;

    while (n > 0) {
        size_t chunk = n > 256 ? 256 : n;
        if (getentropy(p, chunk) != 0) {
            break;
        }
        p += chunk;
        n -= chunk;
    }
    if (n == 0) {
        return 0;
    }
    fd = open("/dev/urandom", O_RDONLY | O_CLOEXEC);
    if (fd < 0) {
        return -1;
    }
    while (n > 0) {
        ssize_t r = read(fd, p, n);
        if (r < 0 && errno == EINTR) {
            continue;
        }
        if (r <= 0) {
            close(fd);
            return -1;
        }
        p += r;
        n -= (size_t)r;
    }
    close(fd);
    return 0;
}

/* ---- Threads and events ---------------------------------------------------------------- */

struct hqq_plat_thread {
    pthread_t t;
    void (*fn)(void *);
    void *arg;
};

static void *thread_main(void *p) {
    struct hqq_plat_thread *t = (struct hqq_plat_thread *)p;
    t->fn(t->arg);
    return NULL;
}

hqq_plat_thread *hqq_plat_thread_start(const hqq_allocator *a, void (*fn)(void *), void *arg, int *os_err) {
    struct hqq_plat_thread *t = (struct hqq_plat_thread *)hqq_alloc(a, sizeof *t);
    int rc;
    if (t == NULL) {
        if (os_err != NULL) {
            *os_err = ENOMEM;
        }
        return NULL;
    }
    t->fn = fn;
    t->arg = arg;
    rc = pthread_create(&t->t, NULL, thread_main, t);
    if (rc != 0) {
        if (os_err != NULL) {
            *os_err = rc;
        }
        hqq_free(a, t, sizeof *t);
        return NULL;
    }
    return t;
}

void hqq_plat_thread_join(const hqq_allocator *a, hqq_plat_thread *t) {
    if (t == NULL) {
        return;
    }
    pthread_join(t->t, NULL);
    hqq_free(a, t, sizeof *t);
}

struct hqq_plat_event {
    pthread_mutex_t m;
    pthread_cond_t c;
    int set;
};

hqq_plat_event *hqq_plat_event_create(const hqq_allocator *a) {
    struct hqq_plat_event *e = (struct hqq_plat_event *)hqq_alloc(a, sizeof *e);
    pthread_condattr_t attr;
    if (e == NULL) {
        return NULL;
    }
    e->set = 0;
    if (pthread_mutex_init(&e->m, NULL) != 0) {
        hqq_free(a, e, sizeof *e);
        return NULL;
    }
    if (pthread_condattr_init(&attr) != 0) {
        pthread_mutex_destroy(&e->m);
        hqq_free(a, e, sizeof *e);
        return NULL;
    }
#if !defined(__APPLE__)
    pthread_condattr_setclock(&attr, CLOCK_MONOTONIC);
#endif
    if (pthread_cond_init(&e->c, &attr) != 0) {
        pthread_condattr_destroy(&attr);
        pthread_mutex_destroy(&e->m);
        hqq_free(a, e, sizeof *e);
        return NULL;
    }
    pthread_condattr_destroy(&attr);
    return e;
}

void hqq_plat_event_destroy(const hqq_allocator *a, hqq_plat_event *e) {
    if (e == NULL) {
        return;
    }
    pthread_cond_destroy(&e->c);
    pthread_mutex_destroy(&e->m);
    hqq_free(a, e, sizeof *e);
}

void hqq_plat_event_signal(hqq_plat_event *e) {
    pthread_mutex_lock(&e->m);
    e->set = 1;
    pthread_cond_broadcast(&e->c);
    pthread_mutex_unlock(&e->m);
}

void hqq_plat_event_reset(hqq_plat_event *e) {
    pthread_mutex_lock(&e->m);
    e->set = 0;
    pthread_mutex_unlock(&e->m);
}

int hqq_plat_event_wait(hqq_plat_event *e, uint64_t timeout_ns) {
    int set;
#if defined(__APPLE__)
    struct timespec rel;
    uint64_t end = hqq_plat_mono_ns() + timeout_ns;
    pthread_mutex_lock(&e->m);
    while (!e->set) {
        uint64_t now = hqq_plat_mono_ns();
        if (now >= end) {
            break;
        }
        rel.tv_sec = (time_t)((end - now) / HQQ_NS_PER_SEC);
        rel.tv_nsec = (long)((end - now) % HQQ_NS_PER_SEC);
        if (pthread_cond_timedwait_relative_np(&e->c, &e->m, &rel) == ETIMEDOUT) {
            break;
        }
    }
#else
    struct timespec until;
    uint64_t end = hqq_plat_mono_ns() + timeout_ns;
    until.tv_sec = (time_t)(end / HQQ_NS_PER_SEC);
    until.tv_nsec = (long)(end % HQQ_NS_PER_SEC);
    pthread_mutex_lock(&e->m);
    while (!e->set) {
        if (pthread_cond_timedwait(&e->c, &e->m, &until) == ETIMEDOUT) {
            break;
        }
    }
#endif
    set = e->set;
    pthread_mutex_unlock(&e->m);
    return set;
}

/* ---- Named shared memory --------------------------------------------------------------- */

#if defined(__linux__)
#  define HQQ_SHM_DIR "/dev/shm/"
#  define HQQ_SHM_FILES 1
#else
#  define HQQ_SHM_FILES 0
#endif

#if defined(__APPLE__)
#  define HQQ_SHM_MAX_NAME 31u /* PSHMNAMLEN, whole string including the leading slash */
#else
#  define HQQ_SHM_MAX_NAME 1023u
#endif

struct hqq_shm {
    int fd;
    size_t alloc_size;
    char path[]; /* Linux: /dev/shm/<base>; others: /<base> */
};

int hqq_plat_names_persist(void) { return 1; }

static void set_err(int *os_err, int e) {
    if (os_err != NULL) {
        *os_err = e;
    }
}

/* Builds the path of a validated name into out (cap bytes). */
static hqq_status shm_path(const char *name, char *out, size_t cap) {
    const char *base;
    size_t base_len;
    hqq_status st;
#if HQQ_SHM_FILES
    st = hqq_name_base(name, 0, &base, &base_len);
    if (st != HQQ_OK) {
        return st;
    }
    if (cap < sizeof HQQ_SHM_DIR + base_len) {
        return HQQ_ERR_INVALID_NAME;
    }
    memcpy(out, HQQ_SHM_DIR, sizeof HQQ_SHM_DIR - 1);
    memcpy(out + sizeof HQQ_SHM_DIR - 1, base, base_len);
    out[sizeof HQQ_SHM_DIR - 1 + base_len] = '\0';
#else
    st = hqq_name_base(name, HQQ_SHM_MAX_NAME, &base, &base_len);
    if (st != HQQ_OK) {
        return st;
    }
    if (base_len + 1 > HQQ_SHM_MAX_NAME || cap < base_len + 2) {
        return HQQ_ERR_INVALID_NAME;
    }
    out[0] = '/';
    memcpy(out + 1, base, base_len);
    out[1 + base_len] = '\0';
#endif
    return HQQ_OK;
}

#define HQQ_PATH_MAX 1100

static int shm_open_path(const char *path, int oflag, unsigned perm) {
#if HQQ_SHM_FILES
    return open(path, oflag | O_CLOEXEC | O_NOFOLLOW, (mode_t)perm);
#else
    int fd = shm_open(path, oflag, (mode_t)perm);
    if (fd >= 0) {
        fcntl(fd, F_SETFD, FD_CLOEXEC);
    }
    return fd;
#endif
}

static int shm_unlink_path(const char *path) {
#if HQQ_SHM_FILES
    return unlink(path);
#else
    return shm_unlink(path);
#endif
}

#if HQQ_SHM_FILES && defined(__linux__)
/* fallocate so a full tmpfs fails here and not with SIGBUS later (SPEC 11.2);
 * ftruncate only where the file system cannot fallocate. */
static int allocate_fd(int fd, int64_t size) {
    for (;;) {
        if (fallocate(fd, 0, 0, (off_t)size) == 0) {
            return 0;
        }
        if (errno == EINTR) {
            continue;
        }
        if (errno == EOPNOTSUPP || errno == ENOSYS) {
            return ftruncate(fd, (off_t)size) == 0 ? 0 : errno;
        }
        return errno;
    }
}
#else
static int allocate_fd(int fd, int64_t size) {
    return ftruncate(fd, (off_t)size) == 0 ? 0 : errno;
}
#endif

hqq_status hqq_plat_shm_open(const hqq_allocator *a, const char *name, size_t size, int create,
                             unsigned perm, hqq_shm **out, int *os_err) {
    char path[HQQ_PATH_MAX];
    struct hqq_shm *s;
    struct stat st;
    size_t plen;
    int fd, oflag, e;
    hqq_status rc;

    *out = NULL;
    set_err(os_err, 0);
    rc = shm_path(name, path, sizeof path);
    if (rc != HQQ_OK) {
        return rc;
    }
    oflag = O_RDWR | (create ? (O_CREAT | O_EXCL) : 0);
    fd = shm_open_path(path, oflag, perm);
    if (fd < 0) {
        e = errno;
        if (e == ENOENT) {
            return HQQ_ERR_NOT_EXIST;
        }
        if (e == EEXIST) {
            return HQQ_ERR_EXISTS;
        }
#if HQQ_SHM_FILES
        if (e == ELOOP) {
            return HQQ_ERR_INVALID_NAME; /* planted symlink */
        }
#endif
        set_err(os_err, e);
        return HQQ_ERR_SYSTEM;
    }
#if HQQ_SHM_FILES
    if (fstat(fd, &st) != 0) {
        e = errno;
        close(fd);
        if (create) {
            unlink(path);
        }
        set_err(os_err, e);
        return HQQ_ERR_SYSTEM;
    }
    if (!S_ISREG(st.st_mode)) {
        close(fd);
        return HQQ_ERR_INVALID_NAME; /* not a regular file: never touched */
    }
#else
    (void)st;
#endif
    if (create) {
        e = allocate_fd(fd, (int64_t)size);
        if (e != 0) {
            close(fd);
            shm_unlink_path(path);
            set_err(os_err, e);
            return HQQ_ERR_SYSTEM;
        }
    }
    plen = strlen(path) + 1;
    s = (struct hqq_shm *)hqq_alloc(a, sizeof *s + plen);
    if (s == NULL) {
        close(fd);
        if (create) {
            shm_unlink_path(path);
        }
        return HQQ_ERR_NO_MEMORY;
    }
    s->fd = fd;
    s->alloc_size = sizeof *s + plen;
    memcpy(s->path, path, plen);
    *out = s;
    return HQQ_OK;
}

int64_t hqq_plat_shm_size(hqq_shm *shm, int *os_err) {
    struct stat st;
    if (fstat(shm->fd, &st) != 0) {
        set_err(os_err, errno);
        return -1;
    }
    return (int64_t)st.st_size;
}

hqq_status hqq_plat_shm_map(hqq_shm *shm, size_t size, void **out, int *os_err) {
    void *p = mmap(NULL, size, PROT_READ | PROT_WRITE, MAP_SHARED, shm->fd, 0);
    if (p == MAP_FAILED) {
        set_err(os_err, errno);
        *out = NULL;
        return HQQ_ERR_SYSTEM;
    }
    *out = p;
    return HQQ_OK;
}

void hqq_plat_shm_unmap(hqq_shm *shm, void *p, size_t size) {
    (void)shm;
    if (p != NULL) {
        munmap(p, size);
    }
}

void hqq_plat_shm_close(const hqq_allocator *a, hqq_shm *shm) {
    if (shm == NULL) {
        return;
    }
    close(shm->fd);
    hqq_free(a, shm, shm->alloc_size);
}

hqq_status hqq_plat_shm_name_refers_to_self(hqq_shm *shm, int *known, int *same) {
#if HQQ_SHM_FILES
    struct stat ours, cur;
    *known = 1;
    *same = 0;
    if (fstat(shm->fd, &ours) != 0) {
        return HQQ_ERR_SYSTEM;
    }
    if (lstat(shm->path, &cur) != 0) {
        return errno == ENOENT ? HQQ_OK : HQQ_ERR_SYSTEM;
    }
    *same = ours.st_dev == cur.st_dev && ours.st_ino == cur.st_ino;
    return HQQ_OK;
#else
    (void)shm;
    *known = 0;
    *same = 0;
    return HQQ_OK;
#endif
}

hqq_status hqq_plat_shm_peek(const char *name, size_t page_size, uint64_t *state,
                             uint64_t *creator_token, int *found) {
    char path[HQQ_PATH_MAX];
    struct stat st;
    void *page;
    int fd;
    hqq_status rc;

    *found = 0;
    rc = shm_path(name, path, sizeof path);
    if (rc != HQQ_OK) {
        return rc;
    }
    fd = shm_open_path(path, O_RDONLY, 0);
    if (fd < 0) {
        return (errno == ENOENT || errno == ELOOP) ? HQQ_OK : HQQ_ERR_SYSTEM;
    }
    if (fstat(fd, &st) != 0 || (size_t)st.st_size < page_size) {
        close(fd);
        return HQQ_OK;
    }
    page = mmap(NULL, page_size, PROT_READ, MAP_SHARED, fd, 0);
    close(fd);
    if (page == MAP_FAILED) {
        return HQQ_ERR_SYSTEM;
    }
    /* Read-only mapping: aligned 64-bit reads, no atomic RMW. */
    *state = *(volatile const uint64_t *)((const unsigned char *)page + offsetof(hqq_superblock, state));
    *creator_token = *(volatile const uint64_t *)((const unsigned char *)page + offsetof(hqq_superblock, creator_token));
    munmap(page, page_size);
    *found = 1;
    return HQQ_OK;
}

hqq_status hqq_plat_shm_delete(hqq_shm *shm) {
    if (shm_unlink_path(shm->path) != 0) {
        return errno == ENOENT ? HQQ_ERR_NOT_EXIST : HQQ_ERR_SYSTEM;
    }
    return HQQ_OK;
}

hqq_status hqq_plat_unlink(const char *name) {
    char path[HQQ_PATH_MAX];
    hqq_status rc = shm_path(name, path, sizeof path);
    if (rc != HQQ_OK) {
        return rc;
    }
    if (shm_unlink_path(path) != 0) {
        return errno == ENOENT ? HQQ_ERR_NOT_EXIST : HQQ_ERR_SYSTEM;
    }
    return HQQ_OK;
}

/* ---- Parking ----------------------------------------------------------------------------- */

#if defined(__linux__)

static void futex_wait(void *ctx, int side, _Atomic uint32_t *word, uint32_t expect, uint64_t timeout_ns) {
    struct timespec ts;
    (void)ctx;
    (void)side;
    ts.tv_sec = (time_t)(timeout_ns / HQQ_NS_PER_SEC);
    ts.tv_nsec = (long)(timeout_ns % HQQ_NS_PER_SEC);
    /* FUTEX_WAIT (0), shared (no FUTEX_PRIVATE_FLAG): the word lives in a MAP_SHARED mapping. */
    syscall(SYS_futex, (uint32_t *)(void *)word, 0, expect, &ts, NULL, 0);
}

static void futex_wake(void *ctx, int side, _Atomic uint32_t *word, uint32_t waiters) {
    (void)ctx;
    (void)side;
    (void)waiters;
    syscall(SYS_futex, (uint32_t *)(void *)word, 1, INT_MAX, NULL, NULL, 0);
}

static const hqq_ring_ops posix_ops = {futex_wait, futex_wake};

void hqq_plat_wait_init(hqq_plat_wait *w, hqq_shm *shm, int ring_index, uint32_t flags) {
    (void)shm;
    (void)ring_index;
    (void)flags;
    w->handle[0] = w->handle[1] = NULL;
    w->mode = 0;
    w->kind = HQQ_PARK_SHARED;
}

const hqq_ring_ops *hqq_plat_ring_ops(void) { return &posix_ops; }

#elif defined(__APPLE__)

/* Private Darwin interfaces (xnu bsd/sys/ulock.h), the ones the Go implementation calls. */
#ifndef HQQ_DARWIN_NO_PRIVATE
extern int __ulock_wait(uint32_t operation, void *addr, uint64_t value, uint32_t timeout_us);
extern int __ulock_wake(uint32_t operation, void *addr, uint64_t wake_value);
#endif
#define HQQ_UL_COMPARE_AND_WAIT_SHARED 3u
#define HQQ_ULF_WAKE_ALL 0x100u

typedef int (*ossync_wait_fn)(void *addr, uint64_t value, size_t size, uint32_t flags, uint32_t clockid,
                              uint64_t timeout_ns);
typedef int (*ossync_wake_fn)(void *addr, size_t size, uint32_t flags);

#define HQQ_OS_SYNC_SHARED 1u
#define HQQ_OS_CLOCK_MACH_ABSOLUTE_TIME 32u

/* The one piece of process-wide state: the result of probing for os_sync_*. */
static _Atomic int ossync_probed;
static ossync_wait_fn ossync_wait;
static ossync_wake_fn ossync_wake;

static void ossync_probe(void) {
    if (atomic_load_explicit(&ossync_probed, memory_order_acquire)) {
        return;
    }
    {
        void *w = dlsym(RTLD_DEFAULT, "os_sync_wait_on_address_with_timeout");
        void *k = dlsym(RTLD_DEFAULT, "os_sync_wake_by_address_all");
        if (w != NULL && k != NULL) {
            memcpy(&ossync_wait, &w, sizeof w);
            memcpy(&ossync_wake, &k, sizeof k);
        }
    }
    atomic_store_explicit(&ossync_probed, 1, memory_order_release);
}

#define HQQ_DARWIN_MODE_NONE 0
#define HQQ_DARWIN_MODE_OSSYNC 1
#define HQQ_DARWIN_MODE_ULOCK 2

static void darwin_wait(void *ctx, int side, _Atomic uint32_t *word, uint32_t expect, uint64_t timeout_ns) {
    const hqq_plat_wait *w = (const hqq_plat_wait *)ctx;
    (void)side;
    if (w->mode == HQQ_DARWIN_MODE_OSSYNC) {
        if (timeout_ns == 0) {
            timeout_ns = 1;
        }
        /* Relative timeout: measured on this system (100 ms request -> 102 ms). The
         * page was touched by W1 before this call: the shared wait fails with EFAULT
         * on a page this process never accessed. */
        ossync_wait((void *)word, expect, sizeof(uint32_t), HQQ_OS_SYNC_SHARED,
                    HQQ_OS_CLOCK_MACH_ABSOLUTE_TIME, timeout_ns);
#ifndef HQQ_DARWIN_NO_PRIVATE
    } else if (w->mode == HQQ_DARWIN_MODE_ULOCK) {
        uint64_t us = timeout_ns / 1000u;
        if (us == 0) {
            us = 1;
        } else if (us > UINT32_MAX) {
            us = UINT32_MAX;
        }
        __ulock_wait(HQQ_UL_COMPARE_AND_WAIT_SHARED, (void *)word, expect, (uint32_t)us);
#endif
    }
}

static void darwin_wake(void *ctx, int side, _Atomic uint32_t *word, uint32_t waiters) {
    const hqq_plat_wait *w = (const hqq_plat_wait *)ctx;
    (void)side;
    (void)waiters;
    if (w->mode == HQQ_DARWIN_MODE_OSSYNC) {
        ossync_wake((void *)word, sizeof(uint32_t), HQQ_OS_SYNC_SHARED);
#ifndef HQQ_DARWIN_NO_PRIVATE
    } else if (w->mode == HQQ_DARWIN_MODE_ULOCK) {
        __ulock_wake(HQQ_UL_COMPARE_AND_WAIT_SHARED | HQQ_ULF_WAKE_ALL, (void *)word, 0);
#endif
    }
}

static const hqq_ring_ops posix_ops = {darwin_wait, darwin_wake};

void hqq_plat_wait_init(hqq_plat_wait *w, hqq_shm *shm, int ring_index, uint32_t flags) {
    int have_ossync, mode;
    (void)shm;
    (void)ring_index;
    ossync_probe();
    have_ossync = ossync_wait != NULL && ossync_wake != NULL;
    w->handle[0] = w->handle[1] = NULL;
    if ((flags & HQQ_FLAG_DARWIN_FORCE_ULOCK) != 0) {
        mode = HQQ_DARWIN_MODE_ULOCK;
    } else if (have_ossync) {
        mode = HQQ_DARWIN_MODE_OSSYNC;
    } else if ((flags & HQQ_FLAG_DARWIN_FORCE_OSSYNC) != 0) {
        mode = HQQ_DARWIN_MODE_NONE;
    } else {
        mode = HQQ_DARWIN_MODE_ULOCK;
    }
#ifdef HQQ_DARWIN_NO_PRIVATE
    if (mode == HQQ_DARWIN_MODE_ULOCK) {
        mode = HQQ_DARWIN_MODE_NONE;
    }
#endif
    w->mode = mode;
    w->kind = mode == HQQ_DARWIN_MODE_NONE ? HQQ_PARK_NONE : HQQ_PARK_SHARED;
}

const hqq_ring_ops *hqq_plat_ring_ops(void) { return &posix_ops; }

#else /* other POSIX: simple mode only */

void hqq_plat_wait_init(hqq_plat_wait *w, hqq_shm *shm, int ring_index, uint32_t flags) {
    (void)shm;
    (void)ring_index;
    (void)flags;
    w->handle[0] = w->handle[1] = NULL;
    w->mode = 0;
    w->kind = HQQ_PARK_NONE;
}

const hqq_ring_ops *hqq_plat_ring_ops(void) { return NULL; }

#endif
