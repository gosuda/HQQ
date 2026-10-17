/*
 * hqq_lifeline.c - parent lifeline (public API: include/hqq/hqq_lifeline.h).
 *
 * One helper thread per lifeline runs lf_thread_main: it waits for input on the
 * watched descriptor/handle, discards bytes, and on EOF or a read error fires
 * the callback once, then waits grace_ms for the stop before a forced _exit.
 * The two backends differ only in how the thread waits and how stop wakes it:
 *
 *   POSIX    poll() on the input and a private self-pipe; stop writes one byte.
 *   Windows  ReadFile() on the input handle; stop sets a flag, signals an event
 *            and calls CancelSynchronousIo() on the thread until it has exited.
 */
#if defined(_WIN32)
#  ifndef WIN32_LEAN_AND_MEAN
#    define WIN32_LEAN_AND_MEAN 1
#  endif
#  ifndef NOMINMAX
#    define NOMINMAX 1
#  endif
#  ifndef _WIN32_WINNT
#    define _WIN32_WINNT 0x0A00
#  endif
#elif defined(__linux__)
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
#endif

#include <limits.h>

#if defined(_WIN32)
#  include <windows.h>
#else
#  include <errno.h>
#  include <fcntl.h>
#  include <poll.h>
#  include <pthread.h>
#  include <unistd.h>
#endif

#define HQQ_BUILDING 1
#include "hqq_internal.h"
#include "hqq/hqq_lifeline.h"

/* Outcomes of one wait for input (lf_wait_input). */
#define LF_EV_DATA 0
#define LF_EV_EOF 1
#define LF_EV_ERROR 2
#define LF_EV_STOP 3

#define LF_SCRATCH 512u
#define LF_NS_PER_MS UINT64_C(1000000)

struct hqq_lifeline {
    hqq_allocator alloc;              /* copy of the caller's allocator */
    const hqq_allocator *ap;          /* &alloc, or NULL for malloc/free */
    void (*callback)(void *ctx, hqq_lifeline_reason reason);
    void *ctx;
    uint64_t grace_ms;
    int watching;
    int started;                      /* the thread was created and not yet joined */
#if defined(_WIN32)
    HANDLE input;
    HANDLE thread;
    HANDLE stop_event;                /* manual reset: wakes the grace wait */
    LONG stopping;                    /* 1 once hqq_lifeline_stop() began */
#else
    int input;
    int stop_r;                       /* self-pipe: read end polled by the thread */
    int stop_w;                       /* self-pipe: write end used by stop */
    pthread_t thread;
#endif
};

/* ---- Shared logic --------------------------------------------------------------- */

static void lf_fire(hqq_lifeline *l, hqq_lifeline_reason reason) {
    if (l->callback != NULL) {
        l->callback(l->ctx, reason);
    }
}

/* Blocks until the input has data (written to buf), EOF, an error, or a stop request. */
static int lf_wait_input(hqq_lifeline *l, unsigned char *buf, size_t cap);
/* Waits up to grace_ms. Returns 1 when stop was requested in time, 0 on timeout. */
static int lf_grace_stopped(hqq_lifeline *l);

static void lf_thread_main(hqq_lifeline *l) {
    unsigned char scratch[LF_SCRATCH];
    for (;;) {
        int ev = lf_wait_input(l, scratch, sizeof scratch);
        if (ev == LF_EV_DATA) {
            continue; /* keepalive bytes: discarded */
        }
        if (ev == LF_EV_STOP) {
            return; /* detached: no callback */
        }
        lf_fire(l, ev == LF_EV_EOF ? HQQ_LIFELINE_REASON_EOF : HQQ_LIFELINE_REASON_ERROR);
        /* Fired once: the thread never reads again. Enforce the grace period. */
        if (l->grace_ms != 0 && !lf_grace_stopped(l)) {
            _exit(HQQ_LIFELINE_EXIT_CODE);
        }
        return;
    }
}

void hqq_lifeline_options_init(hqq_lifeline_options *opts) {
    if (opts == NULL) {
        return;
    }
    memset(opts, 0, sizeof *opts);
    opts->struct_size = sizeof *opts;
    opts->fd = -1;
}

int hqq_lifeline_watching(const hqq_lifeline *l) { return l != NULL ? l->watching : 0; }

/* ---- POSIX backend -------------------------------------------------------------- */

#if !defined(_WIN32)

static void *lf_thread_posix(void *arg) {
    lf_thread_main((hqq_lifeline *)arg);
    return NULL;
}

static int lf_wait_input(hqq_lifeline *l, unsigned char *buf, size_t cap) {
    for (;;) {
        struct pollfd pf[2];
        ssize_t n;
        pf[0].fd = l->input;
        pf[0].events = POLLIN;
        pf[0].revents = 0;
        pf[1].fd = l->stop_r;
        pf[1].events = POLLIN;
        pf[1].revents = 0;
        if (poll(pf, 2, -1) < 0) {
            if (errno == EINTR) {
                continue;
            }
            return LF_EV_ERROR;
        }
        if (pf[1].revents != 0) {
            return LF_EV_STOP;
        }
        if (pf[0].revents == 0) {
            continue;
        }
        /* POLLIN, POLLHUP (EOF) or POLLERR/POLLNVAL: read() reports which. */
        n = read(l->input, buf, cap);
        if (n > 0) {
            return LF_EV_DATA;
        }
        if (n == 0) {
            return LF_EV_EOF;
        }
        if (errno == EINTR || errno == EAGAIN || errno == EWOULDBLOCK) {
            continue;
        }
        return LF_EV_ERROR; /* EBADF, EIO, ... */
    }
}

static int lf_grace_stopped(hqq_lifeline *l) {
    uint64_t total = l->grace_ms * LF_NS_PER_MS;
    uint64_t start = hqq_plat_mono_ns();
    for (;;) {
        uint64_t elapsed = hqq_plat_mono_ns() - start;
        uint64_t rem;
        struct pollfd pf;
        int rc;
        if (elapsed >= total) {
            return 0;
        }
        rem = (total - elapsed + LF_NS_PER_MS - 1) / LF_NS_PER_MS;
        if (rem > (uint64_t)INT_MAX) {
            rem = (uint64_t)INT_MAX;
        }
        pf.fd = l->stop_r;
        pf.events = POLLIN;
        pf.revents = 0;
        rc = poll(&pf, 1, (int)rem);
        if (rc > 0) {
            return 1;
        }
        if (rc < 0 && errno != EINTR) {
            return 1; /* cannot wait: never force an exit on a poll failure */
        }
    }
}

static hqq_status lf_start_platform(hqq_lifeline *l, const hqq_lifeline_options *opts) {
    int pfd[2];
    int rc;
    l->input = opts->fd < 0 ? STDIN_FILENO : opts->fd;
    l->stop_r = -1;
    l->stop_w = -1;
    if (!opts->force && isatty(l->input)) {
        return HQQ_OK; /* interactive run: not watched */
    }
    if (pipe(pfd) != 0) {
        return HQQ_ERR_SYSTEM;
    }
    l->stop_r = pfd[0];
    l->stop_w = pfd[1];
    (void)fcntl(l->stop_r, F_SETFD, FD_CLOEXEC);
    (void)fcntl(l->stop_w, F_SETFD, FD_CLOEXEC);
    rc = pthread_create(&l->thread, NULL, lf_thread_posix, l);
    if (rc != 0) {
        return HQQ_ERR_SYSTEM;
    }
    l->started = 1;
    l->watching = 1;
    return HQQ_OK;
}

static void lf_stop_thread(hqq_lifeline *l) {
    unsigned char b = 1;
    if (!l->started) {
        return;
    }
    while (write(l->stop_w, &b, 1) < 0 && errno == EINTR) {
        /* retry */
    }
    (void)pthread_join(l->thread, NULL);
    l->started = 0;
}

static void lf_close_platform(hqq_lifeline *l) {
    /* The input is not ours: only the self-pipe is closed. */
    if (l->stop_r >= 0) {
        (void)close(l->stop_r);
        l->stop_r = -1;
    }
    if (l->stop_w >= 0) {
        (void)close(l->stop_w);
        l->stop_w = -1;
    }
}

#else /* _WIN32 */

/* ---- Windows backend ------------------------------------------------------------ */

static DWORD WINAPI lf_thread_win32(LPVOID arg) {
    lf_thread_main((hqq_lifeline *)arg);
    return 0;
}

static int lf_stop_requested(hqq_lifeline *l) {
    return InterlockedCompareExchange(&l->stopping, 0, 0) != 0;
}

static int lf_wait_input(hqq_lifeline *l, unsigned char *buf, size_t cap) {
    for (;;) {
        DWORD n = 0;
        BOOL ok;
        if (lf_stop_requested(l)) {
            return LF_EV_STOP;
        }
        ok = ReadFile(l->input, buf, (DWORD)cap, &n, NULL);
        if (lf_stop_requested(l)) {
            return LF_EV_STOP; /* woken by CancelSynchronousIo, or data arrived during stop */
        }
        if (ok && n > 0) {
            return LF_EV_DATA;
        }
        if (ok) {
            return LF_EV_EOF; /* zero bytes read: nothing more can come */
        }
        switch (GetLastError()) {
        case ERROR_BROKEN_PIPE:
        case ERROR_HANDLE_EOF:
            return LF_EV_EOF;
        case ERROR_OPERATION_ABORTED:
            continue; /* a cancel that was not ours: read again */
        default:
            return LF_EV_ERROR;
        }
    }
}

static int lf_grace_stopped(hqq_lifeline *l) {
    uint64_t total = l->grace_ms * LF_NS_PER_MS;
    uint64_t start = hqq_plat_mono_ns();
    for (;;) {
        uint64_t elapsed = hqq_plat_mono_ns() - start;
        uint64_t rem;
        DWORD rc;
        if (elapsed >= total) {
            return 0;
        }
        rem = (total - elapsed + LF_NS_PER_MS - 1) / LF_NS_PER_MS;
        if (rem > 0x7FFFFFFFu) {
            rem = 0x7FFFFFFFu;
        }
        rc = WaitForSingleObject(l->stop_event, (DWORD)rem);
        if (rc != WAIT_TIMEOUT) {
            return 1; /* stop requested (or the wait failed: never force an exit) */
        }
    }
}

static hqq_status lf_start_platform(hqq_lifeline *l, const hqq_lifeline_options *opts) {
    HANDLE in = opts->handle != NULL ? (HANDLE)opts->handle : GetStdHandle(STD_INPUT_HANDLE);
    l->input = in;
    if (!opts->force) {
        DWORD type = (in == NULL || in == INVALID_HANDLE_VALUE) ? FILE_TYPE_UNKNOWN : GetFileType(in);
        if (type != FILE_TYPE_PIPE) {
            return HQQ_OK; /* console, file or no standard input: not watched */
        }
    }
    l->stop_event = CreateEventW(NULL, TRUE, FALSE, NULL);
    if (l->stop_event == NULL) {
        return HQQ_ERR_SYSTEM;
    }
    l->thread = CreateThread(NULL, 0, lf_thread_win32, l, 0, NULL);
    if (l->thread == NULL) {
        return HQQ_ERR_SYSTEM;
    }
    l->started = 1;
    l->watching = 1;
    return HQQ_OK;
}

static void lf_stop_thread(hqq_lifeline *l) {
    if (!l->started) {
        return;
    }
    (void)InterlockedExchange(&l->stopping, 1);
    (void)SetEvent(l->stop_event);
    /* A ReadFile that starts after one cancel is caught by the next one. */
    for (;;) {
        (void)CancelSynchronousIo(l->thread);
        if (WaitForSingleObject(l->thread, 10) != WAIT_TIMEOUT) {
            break;
        }
    }
    l->started = 0;
}

static void lf_close_platform(hqq_lifeline *l) {
    if (l->thread != NULL) {
        (void)CloseHandle(l->thread);
        l->thread = NULL;
    }
    if (l->stop_event != NULL) {
        (void)CloseHandle(l->stop_event);
        l->stop_event = NULL;
    }
    /* The input handle is not ours: never closed here. */
}

#endif /* _WIN32 */

/* ---- Public entry points -------------------------------------------------------- */

hqq_status hqq_lifeline_start(const hqq_lifeline_options *opts, hqq_lifeline **out) {
    hqq_lifeline_options defaults;
    hqq_lifeline *l;
    hqq_status st;

    if (out == NULL) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    *out = NULL;
    if (opts == NULL) {
        hqq_lifeline_options_init(&defaults);
        opts = &defaults;
    }
    if (opts->struct_size != sizeof *opts || opts->grace_ms > UINT64_MAX / LF_NS_PER_MS) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    l = hqq_alloc(opts->allocator, sizeof *l);
    if (l == NULL) {
        return HQQ_ERR_NO_MEMORY;
    }
    memset(l, 0, sizeof *l);
    if (opts->allocator != NULL) {
        l->alloc = *opts->allocator;
        l->ap = &l->alloc;
    }
    l->callback = opts->callback;
    l->ctx = opts->ctx;
    l->grace_ms = opts->grace_ms;

    st = lf_start_platform(l, opts);
    if (st != HQQ_OK) {
        lf_stop_thread(l);
        lf_close_platform(l);
        {
            hqq_allocator a = l->alloc;
            hqq_free(l->ap != NULL ? &a : NULL, l, sizeof *l);
        }
        return st;
    }
    *out = l;
    return HQQ_OK;
}

void hqq_lifeline_stop(hqq_lifeline *l) {
    hqq_allocator a;
    if (l == NULL) {
        return;
    }
    lf_stop_thread(l);
    lf_close_platform(l);
    /* Copy the allocator out before the handle that holds it is freed. */
    a = l->alloc;
    hqq_free(l->ap != NULL ? &a : NULL, l, sizeof *l);
}
