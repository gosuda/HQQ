/* Status strings, cancellation tokens, options, tokens, version and the atomics self-test. */
#define HQQ_BUILDING 1
#include "hqq_internal.h"

#include <stdint.h>

#if ATOMIC_INT_LOCK_FREE != 2 || ATOMIC_LLONG_LOCK_FREE != 2
#  error "hqq requires always-lock-free 32-bit and 64-bit C11 atomics"
#endif

/* ---- Version ------------------------------------------------------------------ */

const char *hqq_version_string(void) { return HQQ_VERSION_STRING; }

unsigned hqq_version_number(void) { return (unsigned)HQQ_VERSION_NUMBER; }

int hqq_selftest_atomics(void) {
    _Atomic uint32_t a32;
    _Atomic uint64_t a64;
    uint64_t expected;

    atomic_init(&a32, 1u);
    atomic_init(&a64, UINT64_C(0x100000000));

    if (!atomic_is_lock_free(&a32) || !atomic_is_lock_free(&a64)) {
        return 1;
    }
    if (atomic_fetch_add_explicit(&a32, 1u, memory_order_seq_cst) != 1u ||
        atomic_load_explicit(&a32, memory_order_seq_cst) != 2u) {
        return 2;
    }
    if (atomic_fetch_add_explicit(&a64, 1u, memory_order_seq_cst) != UINT64_C(0x100000000) ||
        atomic_load_explicit(&a64, memory_order_seq_cst) != UINT64_C(0x100000001)) {
        return 3;
    }
    expected = UINT64_C(0x100000001);
    if (!atomic_compare_exchange_strong_explicit(&a64, &expected, UINT64_C(7),
                                                 memory_order_seq_cst,
                                                 memory_order_seq_cst) ||
        atomic_load_explicit(&a64, memory_order_seq_cst) != UINT64_C(7)) {
        return 4;
    }
    atomic_thread_fence(memory_order_seq_cst);
    return 0;
}

/* ---- Status ------------------------------------------------------------------- */

const char *hqq_strerror(hqq_status s) {
    switch (s) {
    case HQQ_OK: return "ok";
    case HQQ_ERR_CLOSED: return "hqq: link closed";
    case HQQ_ERR_PEER_CLOSED: return "hqq: peer closed the link";
    case HQQ_ERR_PEER_GONE: return "hqq: peer is gone (no heartbeat)";
    case HQQ_ERR_TIMEOUT: return "hqq: operation timed out";
    case HQQ_ERR_LINK_BUSY: return "hqq: link already has two live endpoints";
    case HQQ_ERR_INCOMPATIBLE_LAYOUT: return "hqq: incompatible shared memory layout";
    case HQQ_ERR_FAILED_INIT: return "hqq: failed to initialize buffer ring";
    case HQQ_ERR_SEGMENT_NOT_READY: return "hqq: shared memory segment is not initialized yet";
    case HQQ_ERR_INIT_CONTENDED: return "hqq: shared memory segment initialization did not complete";
    case HQQ_ERR_NO_LIVE_ENDPOINT: return "hqq: shared memory segment has no live endpoint";
    case HQQ_ERR_INVALID_SIZE: return "hqq: invalid buffer ring size";
    case HQQ_ERR_GEOMETRY_MISMATCH: return "hqq: segment geometry mismatch";
    case HQQ_ERR_MEMORY_ALIGN: return "hqq: memory alignment violation";
    case HQQ_ERR_MEMORY_SMALL: return "hqq: memory too small";
    case HQQ_ERR_BUFFER_OVERFLOW: return "hqq: buffer overflow";
    case HQQ_ERR_BUFFER_TOO_SMALL: return "hqq: read buffer too small for the message";
    case HQQ_ERR_CONN_NOT_FOUND: return "hqq: connection not found";
    case HQQ_ERR_INVALID_OP: return "hqq: invalid operation";
    case HQQ_ERR_UNEXPECTED_PACKET: return "hqq: unexpected packet for this API";
    case HQQ_ERR_PROTOCOL: return "hqq: protocol violation";
    case HQQ_ERR_INVALID_NAME: return "hqq: invalid shared memory name";
    case HQQ_ERR_NOT_EXIST: return "hqq: shared memory object does not exist";
    case HQQ_ERR_EXISTS: return "hqq: shared memory object already exists";
    case HQQ_ERR_SYSTEM: return "hqq: operating system error";
    case HQQ_ERR_CANCELED: return "hqq: operation canceled";
    case HQQ_ERR_NO_MEMORY: return "hqq: out of memory";
    case HQQ_ERR_INVALID_ARGUMENT: return "hqq: invalid argument";
    case HQQ_ERR_UNSUPPORTED: return "hqq: not supported on this platform";
    case HQQ_ERR_NOT_NEGOTIATED: return "hqq: advanced protocol not negotiated";
    case HQQ_ERR_ALREADY_NEGOTIATED: return "hqq: advanced protocol already negotiated";
    case HQQ_ERR_NEGOTIATION_FAILED: return "hqq: advanced protocol negotiation failed earlier";
    case HQQ_ERR_NEGOTIATION_REJECTED: return "hqq: advanced protocol negotiation rejected";
    case HQQ_ERR_MESSAGE_ABORTED: return "hqq: peer aborted the message";
    case HQQ_ERR_INCOMING_OVERFLOW: return "hqq: incoming queue overflow";
    case HQQ_ERR_REASSEMBLY_LIMIT: return "hqq: message refused by the reassembly limit";
    case HQQ_ERR_SHUTTING_DOWN: return "hqq: endpoint is shutting down";
    case HQQ_ERR_RESPONSE: return "hqq: error response from the peer";
    case HQQ_ERR_NOT_LISTENING: return "hqq: stream listening is not enabled";
    case HQQ_ERR_ALREADY_LISTENING: return "hqq: stream listening is already enabled";
    case HQQ_ERR_LISTENER_BUSY: return "hqq: listener is served by a live acceptor";
    case HQQ_ERR_LISTENER_GONE: return "hqq: listener was replaced or re-initialized";
    case HQQ_ERR_LISTENER_CLOSED: return "hqq: acceptor closed the listener";
    case HQQ_ERR_ACCEPTOR_CLOSED: return "hqq: acceptor closed";
    case HQQ_ERR_LISTENER_NOT_READY: return "hqq: listener is not ready";
    }
    return "hqq: unknown status";
}

/* Direct base of an error in the Go wrap hierarchy, or HQQ_OK for a root. */
static hqq_status hqq_error_parent(hqq_status err) {
    switch (err) {
    case HQQ_ERR_LINK_BUSY:
    case HQQ_ERR_INCOMPATIBLE_LAYOUT:
    case HQQ_ERR_SEGMENT_NOT_READY:
    case HQQ_ERR_INIT_CONTENDED:
    case HQQ_ERR_NO_LIVE_ENDPOINT:
    case HQQ_ERR_LISTENER_NOT_READY:
        return HQQ_ERR_FAILED_INIT;
    case HQQ_ERR_LISTENER_BUSY:
        return HQQ_ERR_LINK_BUSY;
    case HQQ_ERR_GEOMETRY_MISMATCH:
        return HQQ_ERR_INVALID_SIZE;
    case HQQ_ERR_PEER_GONE:
        return HQQ_ERR_CONN_NOT_FOUND;
    case HQQ_ERR_LISTENER_GONE:
        return HQQ_ERR_PEER_GONE;
    case HQQ_ERR_LISTENER_CLOSED:
        return HQQ_ERR_PEER_CLOSED;
    case HQQ_ERR_ACCEPTOR_CLOSED:
        return HQQ_ERR_CLOSED;
    case HQQ_ERR_UNEXPECTED_PACKET:
    case HQQ_ERR_PROTOCOL:
    case HQQ_ERR_NOT_NEGOTIATED:
    case HQQ_ERR_ALREADY_NEGOTIATED:
    case HQQ_ERR_NEGOTIATION_FAILED:
    case HQQ_ERR_NEGOTIATION_REJECTED:
    case HQQ_ERR_SHUTTING_DOWN:
        return HQQ_ERR_INVALID_OP;
    case HQQ_ERR_MESSAGE_ABORTED:
        return HQQ_ERR_PROTOCOL;
    case HQQ_ERR_INCOMING_OVERFLOW:
    case HQQ_ERR_REASSEMBLY_LIMIT:
        return HQQ_ERR_BUFFER_OVERFLOW;
    default:
        return HQQ_OK;
    }
}

int hqq_error_is(hqq_status err, hqq_status base) {
    /* Bounded: the hierarchy is at most four levels deep. */
    for (int depth = 0; depth < 8 && err != HQQ_OK; depth++) {
        if (err == base) {
            return 1;
        }
        err = hqq_error_parent(err);
    }
    return err == base;
}

int hqq_error_is_transient_open(hqq_status err) {
    return err == HQQ_ERR_SEGMENT_NOT_READY || err == HQQ_ERR_INIT_CONTENDED ||
           err == HQQ_ERR_NO_LIVE_ENDPOINT || err == HQQ_ERR_NOT_EXIST;
}

/* ---- Cancellation, deadlines -------------------------------------------------------- */

_Static_assert(sizeof(hqq_cancel) >= sizeof(_Atomic uint32_t), "hqq_cancel holds an atomic word");
_Static_assert(_Alignof(hqq_cancel) >= _Alignof(_Atomic uint32_t), "hqq_cancel alignment");

static _Atomic uint32_t *cancel_word(const hqq_cancel *c) {
    return (_Atomic uint32_t *)(uintptr_t)&c->state;
}

void hqq_cancel_init(hqq_cancel *c) {
    if (c == NULL) {
        return;
    }
    c->reserved[0] = c->reserved[1] = c->reserved[2] = 0;
    atomic_store_explicit(cancel_word(c), 0, memory_order_release);
}

void hqq_cancel_signal(hqq_cancel *c) {
    if (c != NULL) {
        atomic_store_explicit(cancel_word(c), 1, memory_order_release);
    }
}

int hqq_cancel_load(const hqq_cancel *c) {
    return c != NULL && atomic_load_explicit(cancel_word(c), memory_order_acquire) != 0;
}

int hqq_cancel_is_signalled(const hqq_cancel *c) { return hqq_cancel_load(c); }

hqq_status hqq_ctx_check(const hqq_ctx *ctx) {
    if (ctx == NULL) {
        return HQQ_OK;
    }
    if (hqq_cancel_load(ctx->cancel)) {
        return HQQ_ERR_CANCELED;
    }
    if (ctx->deadline_ns != 0 && hqq_plat_mono_ns() >= ctx->deadline_ns) {
        return HQQ_ERR_TIMEOUT;
    }
    return HQQ_OK;
}

hqq_status hqq_ctx_sleep(const hqq_ctx *ctx, uint64_t ns) {
    uint64_t end = hqq_plat_mono_ns() + ns;
    for (;;) {
        uint64_t now = hqq_plat_mono_ns();
        hqq_status st = hqq_ctx_check(ctx);
        if (st != HQQ_OK) {
            return st;
        }
        if (now >= end) {
            return HQQ_OK;
        }
        {
            uint64_t slice = end - now;
            if (slice > HQQ_NS_PER_MS) {
                slice = HQQ_NS_PER_MS;
            }
            hqq_plat_sleep_ns(slice);
        }
    }
}

/* ---- Tokens ----------------------------------------------------------------------------- */

int hqq_new_token(uint64_t *tok) {
    for (;;) {
        uint64_t t;
        if (hqq_plat_random(&t, sizeof t) != 0) {
            return -1;
        }
        if (t != 0 && t != HQQ_DETACHED_TOKEN) {
            *tok = t;
            return 0;
        }
    }
}

/* ---- Options ------------------------------------------------------------------------------ */

void hqq_options_init(hqq_options *o) {
    if (o == NULL) {
        return;
    }
    memset(o, 0, sizeof *o);
    o->struct_size = sizeof *o;
}

void hqq_open_info_init(hqq_open_info *i) {
    if (i == NULL) {
        return;
    }
    memset(i, 0, sizeof *i);
    i->struct_size = sizeof *i;
}

hqq_status hqq_options_normalize(const hqq_options *opts, hqq_open_req *req, hqq_open_info *info) {
    hqq_options d;

    if (info != NULL && info->struct_size != 0 && info->struct_size < sizeof *info) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    if (opts == NULL) {
        hqq_options_init(&d);
        opts = &d;
    } else if (opts->struct_size < sizeof *opts) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    if (opts->wait_policy != HQQ_WAIT_ADAPTIVE && opts->wait_policy != HQQ_WAIT_SPIN &&
        opts->wait_policy != HQQ_WAIT_SIMPLE) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    req->policy = opts->wait_policy;
    req->flags = opts->flags;
    if (opts->heartbeat_interval_ns == 0) {
        req->heartbeat_interval_ns = HQQ_DEFAULT_HEARTBEAT_INTERVAL_NS;
    } else if (opts->heartbeat_interval_ns < 0) {
        req->heartbeat_interval_ns = 0;
    } else {
        req->heartbeat_interval_ns = opts->heartbeat_interval_ns;
    }
    req->peer_timeout_ns = opts->peer_timeout_ns > 0 ? opts->peer_timeout_ns : HQQ_DEFAULT_PEER_TIMEOUT_NS;
    req->attach_timeout_ns = opts->attach_timeout_ns > 0 ? opts->attach_timeout_ns : HQQ_DEFAULT_ATTACH_TIMEOUT_NS;
    req->init_stale_ns = opts->init_stale_ns;
    req->ctx.cancel = opts->cancel;
    req->ctx.deadline_ns = 0;
    if (opts->open_timeout_ns > 0) {
        req->ctx.deadline_ns = hqq_plat_mono_ns() + (uint64_t)opts->open_timeout_ns;
    }
    if (opts->allocator != NULL) {
        req->alloc = *opts->allocator;
        req->alloc_set = 1;
        if (req->alloc.alloc == NULL || req->alloc.free == NULL) {
            return HQQ_ERR_INVALID_ARGUMENT;
        }
    } else {
        memset(&req->alloc, 0, sizeof req->alloc);
        req->alloc_set = 0;
    }
    return HQQ_OK;
}
