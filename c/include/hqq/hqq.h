/*
 * hqq.h - HQQ shared-memory IPC, C reference library: the STANDARD layer.
 *
 * Implements the Standard link of spec/SPEC.md (layout "HQQ_STL3", ring
 * layout 2) and interoperates bit for bit with the Go implementation.
 *
 * The header is valid C11 and valid C++17 (extern "C" linkage, opaque handles).
 * It intentionally does not include <stdatomic.h>, which C++17 does not
 * provide.
 *
 * Model
 * -----
 *  - A link connects exactly two endpoints: the Primary (the endpoint that
 *    initialized the segment) and the Secondary (the one that joined it).
 *  - One hqq_write() publishes exactly one message of 1..buffer_size bytes;
 *    one hqq_read() consumes exactly one whole message. Messages are never
 *    split or merged. A message that does not fit the reader's buffer is not
 *    consumed: the call fails with HQQ_ERR_BUFFER_TOO_SMALL and reports the
 *    required size.
 *  - The library keeps no global state (one internal one-time probe of the
 *    macOS wait primitive aside) and allocates only when a link is opened or
 *    closed. The data path (read, write, reservations, wait, wake) never
 *    allocates.
 *
 * Thread safety (summary; each function states its own contract)
 * ---------------------------------------------------------------
 *  A hqq_link may be used from any number of threads at once: every read-side
 *  call and every write-side call may run concurrently with any other call.
 *  Only hqq_close() and hqq_link_unlink() have to be the last use of the
 *  handle (see hqq_shutdown() for a concurrent-safe way to stop a link).
 *  The wire protocol itself is single-producer / single-consumer per
 *  direction *across processes*; the library serializes the threads of one
 *  process on each side.
 */
#ifndef HQQ_HQQ_H
#define HQQ_HQQ_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

/* ---- Version ---------------------------------------------------------- */

#define HQQ_VERSION_MAJOR 0
#define HQQ_VERSION_MINOR 1
#define HQQ_VERSION_PATCH 0
#define HQQ_VERSION_STRING "0.1.0"

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
#elif defined(__FreeBSD__)
#  define HQQ_OS_FREEBSD 1
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

/* ---- Constants --------------------------------------------------------- */

#define HQQ_MIN_BUFFER_COUNT 2u
#define HQQ_MAX_BUFFER_COUNT (UINT64_C(1) << 24)
#define HQQ_MIN_BUFFER_SIZE 8u
#define HQQ_MAX_BUFFER_SIZE (UINT64_C(1) << 30)
#define HQQ_BUFFER_SIZE_ALIGNMENT 8u

#define HQQ_DEFAULT_HEARTBEAT_INTERVAL_NS INT64_C(250000000)
#define HQQ_DEFAULT_PEER_TIMEOUT_NS INT64_C(5000000000)
#define HQQ_DEFAULT_ATTACH_TIMEOUT_NS INT64_C(5000000000)

/* Protocol error codes (SPEC 12): tombstone reasons and OpError operands. */
#define HQQ_ERRCODE_MEMORY_ALIGN 0x01u
#define HQQ_ERRCODE_INVALID_SIZE 0x02u
#define HQQ_ERRCODE_MEMORY_SMALL 0x03u
#define HQQ_ERRCODE_FAILED_INIT 0x04u
#define HQQ_ERRCODE_CONN_NOT_FOUND 0x05u
#define HQQ_ERRCODE_INVALID_OP 0x06u
#define HQQ_ERRCODE_BUFFER_OVERFLOW 0x07u

/* ---- Status codes ------------------------------------------------------ */

/*
 * Every function that can fail returns a hqq_status. The values map 1:1 to
 * the sentinel errors of the Go implementation (SPEC 12); hqq_error_is()
 * reproduces the Go "wraps" relation (errors.Is). The numeric values are
 * part of the ABI.
 */
typedef enum hqq_status {
    HQQ_OK = 0,

    /* Link lifecycle and peer state. */
    HQQ_ERR_CLOSED = 1,         /* Go ErrClosed: the local link was closed */
    HQQ_ERR_PEER_CLOSED = 2,    /* Go ErrPeerClosed (io.EOF): peer detached gracefully */
    HQQ_ERR_PEER_GONE = 3,      /* Go ErrPeerGone (ErrConnNotFound): peer died/replaced, or segment re-initialized */
    HQQ_ERR_TIMEOUT = 4,        /* Go ErrTimeout: deadline passed (also: a non-blocking call would block) */

    /* Opening. All of these match HQQ_ERR_FAILED_INIT in hqq_error_is(). */
    HQQ_ERR_LINK_BUSY = 5,           /* Go ErrLinkBusy */
    HQQ_ERR_INCOMPATIBLE_LAYOUT = 6, /* Go ErrIncompatibleLayout */
    HQQ_ERR_FAILED_INIT = 7,         /* Go ErrFailedInit (base) */
    HQQ_ERR_SEGMENT_NOT_READY = 8,   /* transient: object exists but is not sized/claimed yet */
    HQQ_ERR_INIT_CONTENDED = 9,      /* transient: another endpoint kept initializing */
    HQQ_ERR_NO_LIVE_ENDPOINT = 10,   /* transient: opener found a segment without a live endpoint */

    /* Sizes and geometry. */
    HQQ_ERR_INVALID_SIZE = 11,       /* Go ErrInvalidSize: bad geometry, smaller object, bad DataSize */
    HQQ_ERR_GEOMETRY_MISMATCH = 12,  /* Go GeometryError (matches HQQ_ERR_INVALID_SIZE); see hqq_open_info */
    HQQ_ERR_MEMORY_ALIGN = 13,       /* Go ErrMemoryAlign: caller memory is not page aligned */
    HQQ_ERR_MEMORY_SMALL = 14,       /* Go ErrMemorySmall */
    HQQ_ERR_BUFFER_OVERFLOW = 15,    /* Go ErrBufferOverflow: message larger than buffer_size */
    HQQ_ERR_BUFFER_TOO_SMALL = 16,   /* C only: reader buffer too small, *len = required size, message kept */

    /* Protocol. HQQ_ERR_UNEXPECTED_PACKET and HQQ_ERR_PROTOCOL match HQQ_ERR_INVALID_OP. */
    HQQ_ERR_CONN_NOT_FOUND = 17,     /* Go ErrConnNotFound (peer reported code 5) */
    HQQ_ERR_INVALID_OP = 18,         /* Go ErrInvalidOp (also: stale or foreign reservation) */
    HQQ_ERR_UNEXPECTED_PACKET = 19,  /* Go ErrUnexpectedPacket: e.g. an Advanced frame on a Standard reader */
    HQQ_ERR_PROTOCOL = 20,           /* Go ErrProtocol: peer reported an unknown error code */

    /* Names and OS. */
    HQQ_ERR_INVALID_NAME = 21,       /* Go ErrInvalidName */
    HQQ_ERR_NOT_EXIST = 22,          /* Go os.ErrNotExist: named object is missing */
    HQQ_ERR_EXISTS = 23,             /* Go os.ErrExist */
    HQQ_ERR_SYSTEM = 24,             /* operating system failure; errno / GetLastError in hqq_open_info.os_error */

    /* Library. */
    HQQ_ERR_CANCELED = 25,           /* the hqq_cancel token was signalled (Go context.Canceled) */
    HQQ_ERR_NO_MEMORY = 26,          /* the allocator failed */
    HQQ_ERR_INVALID_ARGUMENT = 27,   /* NULL pointer, bad option, struct_size too small */
    HQQ_ERR_UNSUPPORTED = 28,        /* not available on this platform */

    /* Advanced protocol (SPEC 10, 1.1 draft), appended in 1.1. */
    HQQ_ERR_NOT_NEGOTIATED = 29,        /* Go ErrNotNegotiated (matches HQQ_ERR_INVALID_OP) */
    HQQ_ERR_ALREADY_NEGOTIATED = 30,    /* Go ErrAlreadyNegotiated (matches HQQ_ERR_INVALID_OP) */
    HQQ_ERR_NEGOTIATION_FAILED = 31,    /* Go ErrNegotiationFailed: terminal Failed state (matches HQQ_ERR_INVALID_OP) */
    HQQ_ERR_NEGOTIATION_REJECTED = 32,  /* Go ErrNegotiationRejected (matches HQQ_ERR_INVALID_OP) */
    HQQ_ERR_MESSAGE_ABORTED = 33,       /* Go ErrMessageAborted: peer abandoned a response (matches HQQ_ERR_PROTOCOL) */
    HQQ_ERR_INCOMING_OVERFLOW = 34,     /* Go ErrIncomingOverflow (matches HQQ_ERR_BUFFER_OVERFLOW) */
    HQQ_ERR_REASSEMBLY_LIMIT = 35,      /* Go ErrReassemblyLimit (matches HQQ_ERR_BUFFER_OVERFLOW) */
    HQQ_ERR_SHUTTING_DOWN = 36,         /* Go ErrShuttingDown: refused during graceful shutdown (matches HQQ_ERR_INVALID_OP) */
    HQQ_ERR_RESPONSE = 37,              /* Go ResponseError: an error response; its status is hqq_msg_status() */
    HQQ_ERR_NOT_LISTENING = 38,         /* Go ErrNotListening: ListenStreams was not called */
    HQQ_ERR_ALREADY_LISTENING = 39,     /* Go ErrAlreadyListening */

    /* Acceptor and listener (ext/LISTENER_DRAFT.md). */
    HQQ_ERR_LISTENER_BUSY = 40,         /* Go ErrListenerBusy (matches HQQ_ERR_LINK_BUSY, HQQ_ERR_FAILED_INIT) */
    HQQ_ERR_LISTENER_GONE = 41,         /* Go ErrListenerGone (matches HQQ_ERR_PEER_GONE) */
    HQQ_ERR_LISTENER_CLOSED = 42,       /* Go ErrListenerClosed (matches HQQ_ERR_PEER_CLOSED) */
    HQQ_ERR_ACCEPTOR_CLOSED = 43,       /* Go ErrAcceptorClosed (matches HQQ_ERR_CLOSED) */
    HQQ_ERR_LISTENER_NOT_READY = 44     /* transient client open (matches HQQ_ERR_FAILED_INIT) */
} hqq_status;

/* Human-readable, static, never NULL message for a status. Thread-safe. */
HQQ_API const char *hqq_strerror(hqq_status status);

/*
 * Returns 1 if err is `base` or wraps it in the Go error hierarchy, else 0:
 *   LINK_BUSY, INCOMPATIBLE_LAYOUT, SEGMENT_NOT_READY, INIT_CONTENDED,
 *   NO_LIVE_ENDPOINT      are FAILED_INIT;
 *   GEOMETRY_MISMATCH     is INVALID_SIZE;
 *   PEER_GONE             is CONN_NOT_FOUND;
 *   UNEXPECTED_PACKET, PROTOCOL are INVALID_OP.
 * Thread-safe.
 */
HQQ_API int hqq_error_is(hqq_status err, hqq_status base);

/* Returns 1 for the statuses a retrying opener should retry (SPEC 4): the
 * three transient FAILED_INIT errors and HQQ_ERR_NOT_EXIST. Thread-safe. */
HQQ_API int hqq_error_is_transient_open(hqq_status err);

/* ---- Opaque handle, small value types ---------------------------------- */

typedef struct hqq_link hqq_link;

typedef enum hqq_mode {
    HQQ_MODE_PRIMARY = 0,   /* initialized the segment */
    HQQ_MODE_SECONDARY = 1  /* joined an initialized segment */
} hqq_mode;

typedef enum hqq_peer_status {
    HQQ_PEER_NOT_CONNECTED = 0,
    HQQ_PEER_CONNECTED = 1,
    HQQ_PEER_CLOSED = 2,
    HQQ_PEER_GONE = 3
} hqq_peer_status;

typedef enum hqq_wait_policy {
    /* Spin, then yield, then park on a kernel wait (futex / ulock /
     * semaphore); falls back to HQQ_WAIT_SIMPLE where the platform has no
     * cross-process wait. Idle links use almost no CPU. */
    HQQ_WAIT_ADAPTIVE = 0,
    /* Never park: every blocked call keeps one core busy (yielding every 64
     * iterations). Lowest latency. */
    HQQ_WAIT_SPIN = 1,
    /* Simple mode (SPEC 13.2): spin, yield, then sleep with exponential
     * backoff (20 us .. 2 ms). Never registers as a waiter and makes no
     * kernel waits; the waker duty (SPEC 8.3) is still performed. */
    HQQ_WAIT_SIMPLE = 2
} hqq_wait_policy;

/* Layout parameters of a segment. */
typedef struct hqq_geometry {
    uint64_t buffer_count;
    uint64_t buffer_size;
    uint64_t layout_size; /* total bytes of the segment */
    uint64_t page_size;
} hqq_geometry;

/* ---- Cancellation, deadlines ------------------------------------------- */

/*
 * A cancellation token. Initialize with hqq_cancel_init(), signal from any
 * thread with hqq_cancel_signal(). Blocking calls that were given the token
 * notice a signal within at most 10 ms (a parked wait is bounded by that
 * while a token is attached) and return HQQ_ERR_CANCELED. A token must
 * outlive every call that references it. Signalling is idempotent.
 */
typedef struct hqq_cancel {
    uint32_t state; /* accessed atomically by the library; do not touch */
    uint32_t reserved[3];
} hqq_cancel;

/* Thread-safe w.r.t. other functions of one token once initialized. */
HQQ_API void hqq_cancel_init(hqq_cancel *token);
HQQ_API void hqq_cancel_signal(hqq_cancel *token);
HQQ_API int hqq_cancel_is_signalled(const hqq_cancel *token);

/*
 * Per-call wait control. Passing NULL where a `const hqq_wait *` is expected
 * means: block until done, no cancellation.
 *   timeout_ns <  0   no timeout
 *   timeout_ns == 0   never block: fail with HQQ_ERR_TIMEOUT instead
 *   timeout_ns >  0   relative timeout; HQQ_ERR_TIMEOUT when it expires
 * The timeout is measured on the monotonic clock from the call's entry.
 */
typedef struct hqq_wait {
    int64_t timeout_ns;
    const hqq_cancel *cancel; /* may be NULL */
} hqq_wait;

/* ---- Allocator hook ---------------------------------------------------- */

/*
 * Optional allocator for the library's own (control-plane) memory: the link
 * handle and copies of names. It is called only from hqq_standard_*()
 * and hqq_close(). `size` passed to free() is the size passed to alloc().
 * Must be thread-safe if links are opened or closed concurrently.
 */
typedef struct hqq_allocator {
    void *ctx;
    void *(*alloc)(void *ctx, size_t size);
    void (*free)(void *ctx, void *ptr, size_t size);
} hqq_allocator;

/* ---- Options ------------------------------------------------------------ */

/* hqq_options.flags */
#define HQQ_FLAG_DARWIN_FORCE_ULOCK 0x1u /* macOS: use __ulock_wait/__ulock_wake even if os_sync_* exists */
#define HQQ_FLAG_DARWIN_FORCE_OSSYNC 0x2u /* macOS: require os_sync_*; fall back to simple mode if missing */

/*
 * Options for opening a link. Initialize with hqq_options_init(); a
 * zero-filled struct with struct_size set selects every default. All time
 * values are nanoseconds.
 */
typedef struct hqq_options {
    size_t struct_size;            /* set by hqq_options_init(); sizeof(hqq_options) */
    hqq_wait_policy wait_policy;   /* default HQQ_WAIT_ADAPTIVE */
    uint32_t flags;                /* HQQ_FLAG_* */
    /* 0: default 250 ms; < 0: heartbeats disabled (peer cannot detect this
     * endpoint's crash and the segment is never considered stale). */
    int64_t heartbeat_interval_ns;
    /* 0: default 5 s. How long the peer's heartbeat may stall before this
     * endpoint reports HQQ_ERR_PEER_GONE; raised to at least four peer
     * heartbeat intervals. Also the base of the initializer stall window
     * (max(peer_timeout, 2 s)). */
    int64_t peer_timeout_ns;
    /* 0: default 5 s. Bounds the wait for another endpoint that is
     * initializing the segment, and the wait of an opener for its creator. */
    int64_t attach_timeout_ns;
    /* 0: none. Hard bound for the whole open call (claiming, initializing,
     * attaching); expiry returns HQQ_ERR_TIMEOUT. */
    int64_t open_timeout_ns;
    /* 0: default max(peer_timeout, 2 s). Tuning/testing: how long an
     * initializer may show no progress before it is taken over. */
    int64_t init_stale_ns;
    const hqq_cancel *cancel;      /* NULL: none. Bounds the open call only. */
    const hqq_allocator *allocator;/* NULL: malloc/free. Copied; need not outlive the call. */
} hqq_options;

HQQ_API void hqq_options_init(hqq_options *opts);

/* Detailed result of an open call. Every field is optional output. */
typedef struct hqq_open_info {
    size_t struct_size;  /* set by hqq_open_info_init() */
    hqq_geometry want;   /* geometry requested; filled on HQQ_ERR_GEOMETRY_MISMATCH and on success */
    hqq_geometry got;    /* geometry recorded in the segment; filled on HQQ_ERR_GEOMETRY_MISMATCH */
    int os_error;        /* errno (POSIX) or GetLastError() (Windows) when HQQ_ERR_SYSTEM; else 0 */
} hqq_open_info;

HQQ_API void hqq_open_info_init(hqq_open_info *info);

/* ---- Opening ------------------------------------------------------------ */

/*
 * Bytes a segment of this geometry occupies (SPEC 2.1), using the host page
 * size, or 0 if the geometry is invalid: buffer_count must be a power of two
 * in [2, 2^24]; buffer_size a multiple of 8 in [8, 2^30]. Thread-safe.
 */
HQQ_API size_t hqq_standard_size(uint64_t buffer_count, uint64_t buffer_size);

/*
 * Creates or attaches to a link in caller-owned memory (typically a
 * MAP_SHARED mapping). `mem` must be page aligned and at least
 * hqq_standard_size() bytes; the caller keeps it mapped until hqq_close().
 *
 * The first endpoint becomes the Primary and initializes the segment, the
 * second joins as Secondary, a third gets HQQ_ERR_LINK_BUSY. A segment whose
 * endpoints both closed or died is re-initialized. A segment whose first page
 * is not entirely zero and is not a valid HQQ segment is never written
 * (HQQ_ERR_INCOMPATIBLE_LAYOUT). A different geometry yields
 * HQQ_ERR_GEOMETRY_MISMATCH with both geometries in `info`.
 *
 * opts and info may be NULL. On success *out receives the link; on failure
 * *out is NULL. Thread-safe (distinct links); concurrent opens on the same
 * memory from several threads or processes are the protocol's normal case.
 */
HQQ_API hqq_status hqq_standard_open_memory(void *mem, size_t mem_size,
                                            uint64_t buffer_count, uint64_t buffer_size,
                                            const hqq_options *opts,
                                            hqq_link **out, hqq_open_info *info);

/*
 * Creates the named segment `name` (exclusively) and opens the first
 * (Primary) endpoint, or, if the name exists, re-initializes it when all of
 * its endpoints are gone (HQQ_ERR_LINK_BUSY if one is alive,
 * HQQ_ERR_INCOMPATIBLE_LAYOUT for foreign data, HQQ_ERR_INVALID_SIZE for a
 * smaller object). hqq_close() removes the name again, but only while it
 * still refers to this endpoint's instance.
 *
 * `perm` is the permission of the created object (POSIX mode bits, e.g.
 * 0600; translated to a DACL on Windows, see SPEC 11.5). Names: SPEC 11.1
 * (HQQ_ERR_INVALID_NAME; macOS allows 30 bytes). Thread-safe.
 */
HQQ_API hqq_status hqq_standard_create(const char *name,
                                       uint64_t buffer_count, uint64_t buffer_size,
                                       unsigned perm, const hqq_options *opts,
                                       hqq_link **out, hqq_open_info *info);

/*
 * Attaches to an existing named segment as the Secondary. Never creates or
 * initializes anything: a missing name is HQQ_ERR_NOT_EXIST, a segment its
 * creator has not finished yields (after attach_timeout) the transient
 * HQQ_ERR_SEGMENT_NOT_READY / HQQ_ERR_INIT_CONTENDED, one without a live
 * endpoint HQQ_ERR_NO_LIVE_ENDPOINT. See hqq_error_is_transient_open().
 * Thread-safe.
 */
HQQ_API hqq_status hqq_standard_open(const char *name,
                                     uint64_t buffer_count, uint64_t buffer_size,
                                     const hqq_options *opts,
                                     hqq_link **out, hqq_open_info *info);

/* ---- Data path ---------------------------------------------------------- */

/*
 * Publishes `data[0..len)` as one message. len == 0 succeeds without sending
 * anything. len > buffer_size: HQQ_ERR_BUFFER_OVERFLOW. Blocks while the ring
 * is full, as `wait` allows.
 *
 * Errors: HQQ_ERR_CLOSED, HQQ_ERR_PEER_CLOSED / HQQ_ERR_PEER_GONE (checked
 * before publishing and while blocked), HQQ_ERR_TIMEOUT, HQQ_ERR_CANCELED.
 * Thread-safe: any number of concurrent writers; each call publishes exactly
 * one message, messages of different threads interleave in claim order.
 */
HQQ_API hqq_status hqq_write(hqq_link *link, const void *data, size_t len, const hqq_wait *wait);

/*
 * Consumes one whole message into buf (capacity `cap`) and stores its size in
 * *len. If the message is larger than cap, nothing is consumed: the result is
 * HQQ_ERR_BUFFER_TOO_SMALL and *len is the required size. Tombstones are
 * skipped. After the peer closed or died, messages it published earlier are
 * still delivered; the failure is reported only when the ring is empty
 * (HQQ_ERR_PEER_CLOSED matches Go's io.EOF).
 *
 * Other errors: HQQ_ERR_INVALID_SIZE (corrupt size field, the packet is
 * dropped), HQQ_ERR_UNEXPECTED_PACKET (Advanced frame, dropped), errors the
 * peer sent as OpError packets, HQQ_ERR_CLOSED, HQQ_ERR_TIMEOUT,
 * HQQ_ERR_CANCELED. buf may be NULL only if cap == 0.
 * Thread-safe: any number of concurrent readers; each gets whole messages.
 */
HQQ_API hqq_status hqq_read(hqq_link *link, void *buf, size_t cap, size_t *len, const hqq_wait *wait);

/*
 * Zero-copy read: fn(user, data, len) runs while the ring slot is claimed;
 * `data` points into the shared segment and is valid only until fn returns.
 * fn must return promptly (a stalled consumer blocks the whole direction).
 * Its return value is passed through (HQQ_OK on success); the message is
 * consumed either way. fn must not call hqq_close() on this link. *len
 * (optional) receives the message size.
 */
typedef hqq_status (*hqq_read_fn)(void *user, const void *data, size_t len);
HQQ_API hqq_status hqq_read_zc(hqq_link *link, hqq_read_fn fn, void *user,
                               size_t *len, const hqq_wait *wait);

/*
 * Zero-copy write: fn(user, buf, cap, &n) fills the slot-owned payload buffer
 * (cap == buffer_size) and sets n in 1..cap. If fn returns non-OK, or n is
 * out of range, a tombstone is published (the ring keeps moving) and the call
 * fails with fn's status / HQQ_ERR_INVALID_SIZE / HQQ_ERR_BUFFER_OVERFLOW.
 * *len (optional) receives n on success. Same constraints as hqq_read_zc.
 */
typedef hqq_status (*hqq_write_fn)(void *user, void *buf, size_t cap, size_t *n);
HQQ_API hqq_status hqq_write_zc(hqq_link *link, hqq_write_fn fn, void *user,
                                size_t *len, const hqq_wait *wait);

/*
 * Write reservation: claims a slot and exposes its payload buffer. Exactly
 * one of hqq_write_commit() / hqq_write_abort() MUST follow promptly, before
 * hqq_close(); an unfinished reservation stalls the direction (head-of-line).
 * The struct may be passed around by value, but only one copy may be
 * completed: completion is guarded by a compare-and-swap on the slot, so a
 * stale copy fails with HQQ_ERR_INVALID_OP and neither modifies the ring nor
 * wakes anyone. [A stale copy's `data` must not be written.]
 */
typedef struct hqq_write_reservation {
    hqq_link *link;
    uint64_t pos;
    uint64_t copy_id;
    void *data;       /* slot-owned payload buffer */
    size_t capacity;  /* buffer_size */
    uint32_t slot;
    uint32_t reserved;
} hqq_write_reservation;

HQQ_API hqq_status hqq_write_reserve(hqq_link *link, hqq_write_reservation *res, const hqq_wait *wait);
/* Publishes len bytes. len == 0: tombstone + HQQ_ERR_INVALID_SIZE; len >
 * capacity: tombstone + HQQ_ERR_BUFFER_OVERFLOW. Clears res->data. */
HQQ_API hqq_status hqq_write_commit(hqq_write_reservation *res, size_t len);
/* Publishes a tombstone (readers skip it). reason is a HQQ_ERRCODE_*; 0 means
 * HQQ_ERRCODE_INVALID_OP. Clears res->data. */
HQQ_API hqq_status hqq_write_abort(hqq_write_reservation *res, unsigned reason);

/* Read reservation: claims the next message and exposes its payload.
 * hqq_read_release() MUST follow promptly, before hqq_close(). A stale copy
 * fails with HQQ_ERR_INVALID_OP. */
typedef struct hqq_read_reservation {
    hqq_link *link;
    uint64_t pos;
    const void *data; /* payload, valid until released */
    size_t size;      /* message size */
    uint32_t slot;
    uint32_t reserved;
} hqq_read_reservation;

HQQ_API hqq_status hqq_read_reserve(hqq_link *link, hqq_read_reservation *res, const hqq_wait *wait);
HQQ_API hqq_status hqq_read_release(hqq_read_reservation *res);

/* ---- Introspection ------------------------------------------------------- */

/* Refreshes and returns what this endpoint knows about its peer. Thread-safe.
 * A closed link reports the last known state. */
HQQ_API hqq_peer_status hqq_peer_state(hqq_link *link);

HQQ_API hqq_mode hqq_link_mode(const hqq_link *link);
HQQ_API size_t hqq_link_buffer_size(const hqq_link *link);
HQQ_API size_t hqq_link_buffer_count(const hqq_link *link);
/* Name of a named link (valid until hqq_close()), NULL for caller memory. */
HQQ_API const char *hqq_link_name(const hqq_link *link);

/* ---- Close, unlink -------------------------------------------------------- */

/*
 * Stops the link without releasing the handle: marks it closed, wakes and
 * fails every blocked call with HQQ_ERR_CLOSED, waits for in-flight calls
 * (including running zero-copy callbacks), stops the heartbeat thread,
 * detaches the endpoint (the peer then sees HQQ_ERR_PEER_CLOSED) and wakes the
 * peer. Calls that start afterwards fail with HQQ_ERR_CLOSED. Idempotent and
 * safe to call concurrently with any other call on the link, including from
 * another thread while calls are blocked. Must not be called from inside a
 * zero-copy callback (it would wait for itself) or while a reservation is
 * outstanding.
 */
HQQ_API void hqq_shutdown(hqq_link *link);

/*
 * hqq_shutdown(), then unmaps a named segment, closes the OS handles, removes
 * the name if this endpoint is a creator and the name still refers to its
 * instance (SPEC 11.4), and frees the handle. Returns HQQ_OK or the first
 * OS failure. `link` MUST NOT be used by any thread after the call starts
 * (calls that were already in flight are waited for); NULL is a no-op.
 */
HQQ_API hqq_status hqq_close(hqq_link *link);

/*
 * Removes the name now instead of at hqq_close(), if it still refers to this
 * link's instance; a missing or replaced name is left alone and the call
 * returns HQQ_OK. Existing mappings stay valid. No-op on Windows and for
 * caller-owned memory. Thread-safe.
 */
HQQ_API hqq_status hqq_link_unlink(hqq_link *link);

/* Removes a named object, for example one a crashed process left behind.
 * Processes that map it are not affected. No-op on Windows. */
HQQ_API hqq_status hqq_unlink(const char *name);

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
