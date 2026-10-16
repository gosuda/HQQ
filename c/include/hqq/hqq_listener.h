/*
 * hqq_listener.h - acceptor and client connect of the Advanced layer
 * (ext/LISTENER_DRAFT.md, layout HQQ_LSN1).
 *
 * A service creates one listener segment with hqq_acceptor_create(). Each
 * client calls hqq_connect(): it creates its own Advanced link as Primary,
 * announces it in the listener's connect ring and waits until the acceptor
 * attached and negotiated. The acceptor hands the links out with
 * hqq_acceptor_accept(). An accepted link is an ordinary hqq_adv link (see
 * hqq_advanced.h) whose lifecycle is independent of the acceptor.
 *
 * Mirrors the Go API: NewAcceptor / Accept / Close, Connect (Go ErrListenerBusy,
 * ErrListenerGone, ErrListenerClosed, ErrAcceptorClosed map to the HQQ_ERR_*
 * names of hqq.h).
 *
 * Thread safety: every function may be called from any thread. Accept may run
 * concurrently from several threads. hqq_acceptor_close() waits for the
 * Accept calls in progress (they return HQQ_ERR_ACCEPTOR_CLOSED).
 */
#ifndef HQQ_LISTENER_H
#define HQQ_LISTENER_H

#include <stddef.h>
#include <stdint.h>

#include "hqq/hqq.h"
#include "hqq/hqq_advanced.h"

#ifdef __cplusplus
extern "C" {
#endif

/* Defaults (zero selects them). */
#define HQQ_ACCEPTOR_DEFAULT_QUEUE_SIZE 64u
#define HQQ_ACCEPTOR_DEFAULT_MAX_BUFFER_COUNT 4096u
#define HQQ_ACCEPTOR_DEFAULT_MAX_BUFFER_SIZE (UINT64_C(1) << 20)
#define HQQ_ACCEPTOR_DEFAULT_MAX_LINK_SIZE (UINT64_C(256) << 20)
#define HQQ_ACCEPTOR_DEFAULT_BACKLOG 16u
#define HQQ_ACCEPTOR_DEFAULT_MAX_HANDSHAKES 16u

typedef struct hqq_acceptor_options {
    size_t struct_size;               /* set by hqq_acceptor_options_init() */
    unsigned perm;                    /* listener permission, 0: 0600 */
    uint64_t queue_size;              /* connect records held, power of two in [2, 65536]; 0: 64 */
    uint64_t features;                /* features offered on accepted links; 0: all of 1.1 */
    uint64_t max_buffer_count;        /* largest buffer_count a client may ask for; 0: 4096 */
    uint64_t max_buffer_size;         /* largest buffer_size; 0: 1 MiB */
    uint64_t max_link_size;           /* largest link segment; 0: 256 MiB */
    uint64_t backlog;                 /* negotiated links waiting for hqq_acceptor_accept; 0: 16 */
    uint64_t max_handshakes;          /* concurrent handshakes; 0: 16 */
    int64_t handshake_timeout_ns;     /* 0: 5 s */
    int64_t stuck_slot_timeout_ns;    /* 0: 2 s; < 0 disables the stuck-slot recovery */
    hqq_adv_options link;             /* options of accepted links; link.link also times the listener heartbeat */
} hqq_acceptor_options;

HQQ_API void hqq_acceptor_options_init(hqq_acceptor_options *opts);

typedef struct hqq_acceptor_stats {
    uint64_t records;         /* connect records taken from the queue */
    uint64_t accepted;       /* links returned by hqq_acceptor_accept */
    uint64_t malformed;      /* records dropped by validation */
    uint64_t unreachable;    /* valid records whose link segment could not be attached */
    uint64_t handshake_failed;
    uint64_t stuck_slots;    /* slots published on a dead producer's behalf */
} hqq_acceptor_stats;

typedef struct hqq_acceptor hqq_acceptor;

/* Options of a client. */
typedef struct hqq_connect_options {
    size_t struct_size;       /* set by hqq_connect_options_init() */
    uint64_t buffer_count;    /* 0: 256 */
    uint64_t buffer_size;     /* 0: 4096 */
    unsigned perm;            /* permission of the client's link segment, 0: 0600 */
    uint64_t features;        /* features requested; 0: all of 1.1 */
    hqq_adv_options link;     /* options of the client's link */
} hqq_connect_options;

HQQ_API void hqq_connect_options_init(hqq_connect_options *opts);

/*
 * Creates the listener `name` (exclusively, or re-initializes an object whose
 * acceptor is gone; see LISTENER_DRAFT section 5). HQQ_ERR_LISTENER_BUSY when
 * a live acceptor serves the name; HQQ_ERR_INVALID_SIZE for an object smaller
 * than the layout; HQQ_ERR_INCOMPATIBLE_LAYOUT for foreign data.
 */
HQQ_API hqq_status hqq_acceptor_create(const char *name, const hqq_acceptor_options *opts,
                                       hqq_acceptor **out, hqq_open_info *info);
/* Returns the next negotiated link (its lifecycle is then independent of the
 * acceptor). Waits for `wait`; HQQ_ERR_ACCEPTOR_CLOSED after hqq_acceptor_close. */
HQQ_API hqq_status hqq_acceptor_accept(hqq_acceptor *acc, hqq_adv **out, const hqq_wait *wait);
/* Stops the dispatcher and handshakes, wakes Accept, closes links that were not
 * delivered, marks the listener closed and removes the name when it is ours.
 * Links already delivered stay open. */
HQQ_API hqq_status hqq_acceptor_close(hqq_acceptor *acc);
HQQ_API void hqq_acceptor_stats_get(const hqq_acceptor *acc, hqq_acceptor_stats *out);

/*
 * Client side (Go Connect): attaches to listener `listener_name`, creates the
 * client's link, announces it and waits for the acceptor's negotiation answer.
 * Transient listener states are retried with backoff until `wait` ends.
 * On success *out is a negotiated Advanced link (Primary). HQQ_ERR_LISTENER_CLOSED,
 * HQQ_ERR_LISTENER_GONE and HQQ_ERR_TIMEOUT are the waiting errors of LISTENER_DRAFT 7.4.
 */
HQQ_API hqq_status hqq_connect(const char *listener_name, const hqq_connect_options *opts,
                               const hqq_wait *wait, hqq_adv **out);

#ifdef __cplusplus
} /* extern "C" */
#endif

#endif /* HQQ_LISTENER_H */
