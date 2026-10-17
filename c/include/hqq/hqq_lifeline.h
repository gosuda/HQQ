/*
 * hqq_lifeline.h - parent lifeline: a service exits when the process that
 * started it dies, including when that process is killed with SIGKILL.
 *
 * The supervisor keeps the write end of a pipe and makes the read end the
 * service's standard input. The OS closes the write end when the supervisor
 * dies in any way, so the service's stdin reaches end-of-file. A lifeline
 * thread blocked on that descriptor (or handle) reports it through the
 * callback, and the service then shuts itself down in an orderly way.
 *
 * Contract
 * --------
 *  - The lifeline owns the watched input: after hqq_lifeline_start() the
 *    helper thread reads it and DISCARDS every byte (the supervisor may write
 *    keepalives). Only end-of-file or a read error means that the supervisor
 *    is gone.
 *  - EOF (POSIX read() == 0; Windows ERROR_BROKEN_PIPE / ERROR_HANDLE_EOF)
 *    fires the callback with HQQ_LIFELINE_REASON_EOF. Any other read error
 *    (POSIX: errno other than EINTR and EAGAIN; Windows: any other error)
 *    fires it with HQQ_LIFELINE_REASON_ERROR. The callback fires at most once.
 *  - Terminals (POSIX isatty(), Windows console) are NOT watched unless
 *    options.force is set, so an interactive run is never shut down by a
 *    closed terminal. On Windows a descriptor that is not a pipe is not
 *    watched unless options.force is set.
 *  - grace_ms: after the callback returned, the lifeline waits grace_ms for
 *    hqq_lifeline_stop(). If that does not happen in time, the process is ended
 *    with _exit(HQQ_LIFELINE_EXIT_CODE) (no atexit handlers, no flushing). This
 *    keeps a stuck shutdown from holding the service forever. 0 disables it.
 *  - hqq_lifeline_stop() detaches without firing the callback (a callback that
 *    already started runs to completion first) and joins the thread. Call it
 *    once, from a thread other than the callback thread.
 *
 * Threading and signals
 * ---------------------
 *  The callback runs on the lifeline thread, concurrently with the rest of the
 *  process. It must only request an orderly shutdown: set an atomic flag, write
 *  to a self-pipe, or signal the main thread (for example pthread_kill() with a
 *  signal the main thread waits for in sigwait()). It must not block on the
 *  lifeline itself (no hqq_lifeline_stop() from inside the callback) and should
 *  not do blocking I/O of its own. The helper does not install signal handlers.
 *
 * Platform notes
 * --------------
 *  POSIX: the thread poll()s the input and a private self-pipe (stop and grace
 *  wake-ups) and only read()s when the input is readable, so it uses no CPU
 *  while idle. Windows: the thread blocks in ReadFile() on the input handle;
 *  hqq_lifeline_stop() sets a flag and calls CancelSynchronousIo() on the
 *  thread until it has exited, which makes the blocked ReadFile() fail with
 *  ERROR_OPERATION_ABORTED.
 *
 * The header is valid C11 and valid C++17 (extern "C" linkage, opaque handle).
 */
#ifndef HQQ_LIFELINE_H
#define HQQ_LIFELINE_H

#include <stddef.h>
#include <stdint.h>

#include "hqq/hqq.h"

#ifdef __cplusplus
extern "C" {
#endif

/* Process exit status of the forced exit after grace_ms (no graceful exit happened). */
#define HQQ_LIFELINE_EXIT_CODE 70

typedef enum hqq_lifeline_reason {
    HQQ_LIFELINE_REASON_EOF = 1,   /* the supervisor closed its end of the pipe (or the pipe broke) */
    HQQ_LIFELINE_REASON_ERROR = 2  /* the watched input failed with an error other than EINTR/EAGAIN */
} hqq_lifeline_reason;

typedef struct hqq_lifeline_options {
    size_t struct_size;             /* set by hqq_lifeline_options_init() */
    /* Called once, on the lifeline thread, when the supervisor is gone. NULL: none. */
    void (*callback)(void *ctx, hqq_lifeline_reason reason);
    void *ctx;                      /* passed to callback */
    int fd;                         /* POSIX: descriptor to watch; -1: standard input (0). Ignored on Windows. */
    void *handle;                   /* Windows: HANDLE to watch; NULL: the standard input handle. Ignored on POSIX. */
    int force;                      /* watch even when the input is a terminal (POSIX) or not a pipe (Windows) */
    uint64_t grace_ms;              /* after the callback: forced _exit(HQQ_LIFELINE_EXIT_CODE) if not stopped in time; 0: never */
    const hqq_allocator *allocator; /* NULL: malloc/free. Copied; need not outlive the call. */
} hqq_lifeline_options;

/* Sets the defaults: stdin, no force, no grace, no callback. */
HQQ_API void hqq_lifeline_options_init(hqq_lifeline_options *opts);

typedef struct hqq_lifeline hqq_lifeline;

/*
 * Starts watching. opts may be NULL (the defaults). On success *out is a
 * handle that must be released with hqq_lifeline_stop(), even when watching
 * is off (hqq_lifeline_watching() then returns 0). Returns HQQ_ERR_INVALID_ARGUMENT
 * for a NULL out or a bad struct_size, HQQ_ERR_NO_MEMORY, or HQQ_ERR_SYSTEM
 * (*out is NULL on failure).
 */
HQQ_API hqq_status hqq_lifeline_start(const hqq_lifeline_options *opts, hqq_lifeline **out);

/* 1 when the lifeline thread watches the input, 0 when it does not (a terminal without force). */
HQQ_API int hqq_lifeline_watching(const hqq_lifeline *lifeline);

/*
 * Detaches the lifeline: wakes the thread, waits until it has finished (the
 * callback is not run after this returns) and frees the handle. NULL is a
 * no-op. Must not be called from the callback.
 */
HQQ_API void hqq_lifeline_stop(hqq_lifeline *lifeline);

#ifdef __cplusplus
} /* extern "C" */
#endif

#endif /* HQQ_LIFELINE_H */
