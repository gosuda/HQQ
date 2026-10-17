/*
 * hqq_rpc_internal.h - internals of the hqq-bindgen C runtime (not installed).
 *
 * hqq_rpc.c         readers, writers, scratch, handle table, execution lock, status helpers
 * hqq_rpc_session.c frame receive and validation, response building, request execution, teardown,
 *                   the link-less test entry points (BINDGEN.md 12.1, item 6)
 * hqq_rpc_server.c  server, bootstrap over the listener, session threads, hqqrpc_main
 *
 * Section numbers refer to spec/toolchain/BINDGEN.md.
 */
#ifndef HQQ_RPC_INTERNAL_H
#define HQQ_RPC_INTERNAL_H

#include <stddef.h>
#include <stdint.h>

#include "hqq/hqq_rpc.h"
#include "hqq_internal.h"
#include "hqq_platform.h"

/* ---- Constants not in the public header ------------------------------------------------ */

#define HQQRPC_POLL_NS INT64_C(250000000)        /* wait for a request (5.2), fixed */
#define HQQRPC_HELLO_SIZE 64u
#define HQQRPC_MIN_MAX_MESSAGE 4096u             /* 4.11 LIMIT: max_message below this */
#define HQQRPC_MAX_HANDLES 65535u
#define HQQRPC_BODY_LIMIT UINT32_MAX             /* writers and readers: "no bound" */

#define HQQRPC_DEF_BUFFER_COUNT 64u
#define HQQRPC_DEF_BUFFER_SIZE 16384u
#define HQQRPC_DEF_MAX_SESSIONS 64u
#define HQQRPC_DEF_MAX_HANDLES 4096u
#define HQQRPC_DEF_MAX_CONTROL_LINKS 16u
#define HQQRPC_DEF_MAX_SEGMENT_BYTES (UINT64_C(64) << 20)
#define HQQRPC_DEF_MAX_MESSAGE (16u << 20)
#define HQQRPC_DEF_OPEN_TIMEOUT_NS (INT64_C(5) * 1000000000)
#define HQQRPC_DEF_SHUTDOWN_TIMEOUT_NS (INT64_C(5) * 1000000000)
#define HQQRPC_DEF_WRITE_STALL_NS (INT64_C(10) * 1000000000)
#define HQQRPC_DEF_SCRATCH_KEEP (1u << 20)
#define HQQRPC_DEF_PEER_TIMEOUT_NS (INT64_C(2) * 1000000000)

/* HELLO_REPLY status (4.11). */
#define HQQRPC_HELLO_OK 0u
#define HQQRPC_HELLO_DIGEST_MISMATCH 1u
#define HQQRPC_HELLO_VERSION 2u
#define HQQRPC_HELLO_GEOMETRY 3u
#define HQQRPC_HELLO_LIMIT 4u
#define HQQRPC_HELLO_BAD 5u

/* Bootstrap error status is 0x50000 + code (4.11). */
#define HQQRPC_OPEN_STATUS_BASE UINT64_C(0x50000)
#define HQQRPC_OPEN_E_VERSION 1u
#define HQQRPC_OPEN_E_GEOMETRY 2u
#define HQQRPC_OPEN_E_BUSY 3u
#define HQQRPC_OPEN_E_INTERNAL 4u

/* Log levels of the on_log callback. */
#define HQQRPC_LOG_ERROR 0
#define HQQRPC_LOG_WARN 1
#define HQQRPC_LOG_INFO 2

/* ---- Resolved server configuration ----------------------------------------------------- */

typedef struct hqqrpc_cfg {
    unsigned perm;
    uint32_t buffer_count;       /* default session geometry */
    uint32_t buffer_size;
    uint32_t max_buffer_count;
    uint32_t max_buffer_size;
    uint32_t max_sessions;
    uint32_t max_handles;
    uint32_t max_control_links;
    uint64_t max_segment_bytes;
    uint32_t max_in_flight;      /* 0: the session's buffer count */
    uint32_t max_message;
    int64_t open_timeout_ns;
    int64_t shutdown_timeout_ns;
    int64_t write_stall_timeout_ns;
    int64_t idle_timeout_ns;     /* 0: disabled */
    uint32_t scratch_keep;
    int copy_request;
} hqqrpc_cfg;

/* Applies the defaults of 5.5 and validates. HQQ_ERR_INVALID_ARGUMENT on a value out of range. */
hqq_status hqqrpc_cfg_resolve(const hqqrpc_server_options *in, hqqrpc_cfg *out);

/* Checks the parts of an interface description that the runtime relies on. */
hqq_status hqqrpc_iface_check(const hqqrpc_iface *iface);

/* ---- Memory blocks ----------------------------------------------------------------------- */

/* One allocation with an ALIGN-aligned payload (the allocator has no alignment parameter). */
typedef struct hqqrpc_blk {
    struct hqqrpc_blk *next;
    size_t total;                /* bytes passed to the allocator */
    size_t cap;                  /* usable bytes at p */
    uint8_t *p;
} hqqrpc_blk;

/* ---- Handle table (4.9, 5.1) ------------------------------------------------------------- */

typedef struct hqqrpc_hentry {
    void *ptr;
    uint32_t parent;             /* id of the parent handle at creation, 0 if none */
    uint32_t prev;               /* neighbors in creation order (indices, 0: none) */
    uint32_t next;
    uint32_t free_next;          /* next index on the free list while the entry is free */
    uint16_t gen;                /* 1..65535 */
    uint16_t type;               /* handle type id, 1-based */
    uint8_t live;
} hqqrpc_hentry;

/* ---- State of the call in progress ------------------------------------------------------- */

typedef struct hqqrpc_cstate {
    uint16_t op;
    uint32_t id;
    int began;                   /* hqqrpc_resp_begin succeeded */
    int held;                    /* a response slot is reserved and not yet completed */
    int committed;               /* a response was published */
    hqq_write_reservation res;   /* link mode: the held reservation */
    uint8_t *frame;              /* held slot memory: header at frame, body at frame + HDR */
    uint8_t *body;               /* scratch response body (rf > 1) */
    uint64_t cap;                /* body_cap given to hqqrpc_resp_begin */
    uint64_t span;               /* bytes of the last hqqrpc_wr_span still to be advanced */
    hqqrpc_blk *scratch;         /* call-scoped blocks */
    unsigned hreserved;          /* handle entries reserved and not yet registered */
    uint32_t *reg;               /* ids registered by this call */
    unsigned nreg;
    unsigned regcap;
} hqqrpc_cstate;

/* ---- Session ---------------------------------------------------------------------------- */

struct hqqrpc_server;

enum {
    HQQRPC_ST_HELLO = 0,         /* waiting for HELLO */
    HQQRPC_ST_ACTIVE,
    HQQRPC_ST_ENDED
};

/* Why a session ends (5.2). Decides the final frame. */
typedef enum hqqrpc_end {
    HQQRPC_END_NONE = 0,
    HQQRPC_END_CLOSE,            /* CLOSE frame: CLOSE_ACK after teardown */
    HQQRPC_END_PEER,             /* peer detached or died, or the link failed: no frame */
    HQQRPC_END_PROTOCOL,         /* violation: E_PROTOCOL, best effort */
    HQQRPC_END_STOP,             /* server stop: E_SHUTTING_DOWN, best effort */
    HQQRPC_END_IDLE,             /* idle timeout: E_SHUTTING_DOWN, best effort */
    HQQRPC_END_STALL,            /* write stall: no frame */
    HQQRPC_END_HANDSHAKE         /* failed or timed-out handshake: the reply (if any) is sent */
} hqqrpc_end;

struct hqqrpc_session {
    struct hqqrpc_server *srv;   /* NULL for a test session */
    const hqqrpc_iface *iface;
    hqqrpc_cfg cfg;
    hqq_allocator alloc_copy;
    const hqq_allocator *alloc;  /* NULL: malloc */

    void *cb_user;
    void *(*on_start)(void *user, hqqrpc_session *s);
    void (*on_end)(void *user, void *session_user, hqqrpc_session *s);
    void (*on_log)(void *user, int level, const char *msg);
    void *user;                  /* result of on_start */
    int started;                 /* on_start ran */

    hqq_link *link;              /* NULL for a test session */
    const hqq_cancel *cancel;    /* the server's token, NULL for a test session */
    hqq_plat_mutex *exec_mu;     /* execution lock, NULL for serialize(none) */
    int exec_mu_owned;           /* test session: created and freed with the session */
    int lock_held;

    uint32_t S;                  /* buffer size */
    uint32_t B;                  /* buffer count */
    uint32_t chunk;              /* S - HDR */
    uint32_t max_message;        /* granted after HELLO */
    uint32_t max_in_flight;
    uint8_t proto_minor;

    int state;
    hqqrpc_end end;
    int torn;                    /* teardown ran */
    uint32_t last_id;            /* id of the last accepted REQUEST, 0 before the first */
    uint64_t start_ns;
    uint64_t last_req_ns;
    uint64_t ctl_id;             /* control link that created this session, 0 for none */

    /* assembly of a multi-frame request (4.8) */
    int asm_active;
    int asm_discard;             /* out of memory: validate and drop the chunks, answer E_INTERNAL */
    hqqrpc_blk *asm_blk;
    uint16_t asm_op;
    uint32_t asm_id;
    uint32_t asm_total;
    uint32_t asm_recv;

    hqq_read_reservation *rx;    /* link mode: the slot being processed, NULL once released */

    /* handle table */
    hqqrpc_hentry *tab;
    uint32_t tab_cap;            /* entries allocated, index 0 included */
    uint32_t high_water;
    uint32_t free_head;
    uint32_t free_count;
    uint32_t live_count;
    uint32_t live_head;          /* creation order: head is the oldest */
    uint32_t live_tail;
    uint32_t destroy_failures;

    hqqrpc_cstate cs;
    hqqrpc_blk *spare;           /* one retained scratch block (scratch_keep) */

    /* test session: a slot-sized buffer for the request copy and the response slot, and the sink */
    hqqrpc_blk *t_blk;           /* owns t_rx and t_tx */
    uint8_t *t_rx;
    uint8_t *t_tx;
    uint8_t *t_out;
    size_t t_out_cap;
    size_t t_out_len;
    int t_out_overflow;
    uint64_t t_tombstones;
    uint64_t t_frames;
};

/* ---- Functions shared between the source files ----------------------------------------------- */

/* hqq_rpc.c */
hqqrpc_blk *hqqrpc_blk_new(hqqrpc_session *s, size_t n);
void hqqrpc_blk_free(hqqrpc_session *s, hqqrpc_blk *b);
void hqqrpc_blk_release(hqqrpc_session *s, hqqrpc_blk *b);  /* keeps it as the spare when small */
void hqqrpc_scratch_release(hqqrpc_session *s);             /* frees the call-scoped blocks */
void hqqrpc_log(hqqrpc_session *s, int level, const char *fmt, ...);

void hqqrpc_tab_free(hqqrpc_session *s);
/* Destroys every live handle, newest first, and consumes the entries (teardown, 5.2 step 2). */
void hqqrpc_handles_destroy_all(hqqrpc_session *s);
/* Ends the registration bookkeeping of a call. rollback != 0: the call failed after it registered
 * handles; destroy them (execution lock taken here) and free their entries. */
void hqqrpc_call_handles_end(hqqrpc_session *s, int rollback);

uint32_t hqqrpc_le32(const uint8_t *p);
uint16_t hqqrpc_le16(const uint8_t *p);
void hqqrpc_put_le32(uint8_t *p, uint32_t v);
void hqqrpc_put_le16(uint8_t *p, uint16_t v);
void hqqrpc_put_le64(uint8_t *p, uint64_t v);

/* The message text sent with a status (4.10): the table text, empty for the application range. */
const char *hqqrpc_status_text(hqqrpc_status st);
/* Maps what dispatch returned to what the runtime may send for a request: 1..6 and the
 * application range pass, everything else is E_INTERNAL. */
hqqrpc_status hqqrpc_request_status(hqqrpc_status st);

/* hqq_rpc_session.c */
hqqrpc_session *hqqrpc_session_create(const hqqrpc_iface *iface, const hqqrpc_cfg *cfg,
                                      const hqqrpc_server_options *callbacks, struct hqqrpc_server *srv,
                                      hqq_link *link, const hqq_allocator *alloc, uint32_t S, uint32_t B);
/* Processes one received frame (header copy, frame rules, handshake, assembly, dispatch). Returns
 * nonzero once the session must end (s->end is set). */
int hqqrpc_session_rx(hqqrpc_session *s, const void *data, size_t size);
/* Link mode: processes the frame of a read reservation and releases the slot (early for a
 * multi-frame message, else after the response was published, 4.7.1 step 15). */
int hqqrpc_session_rx_slot(hqqrpc_session *s, hqq_read_reservation *rr);
/* Teardown (5.2): final frame, destroy handles, on_session_end, CLOSE_ACK. Runs once. */
void hqqrpc_session_finish(hqqrpc_session *s);
void hqqrpc_session_destroy(hqqrpc_session *s);
void hqqrpc_session_start_callback(hqqrpc_session *s);
void hqqrpc_session_set_end(hqqrpc_session *s, hqqrpc_end why);

/* Link mode: ring access with the waits of 5.2. final: timeout 0, no cancel token. */
hqq_status hqqrpc_tx_reserve(hqqrpc_session *s, int final, uint8_t **frame, hqq_write_reservation *res);
hqq_status hqqrpc_tx_commit(hqqrpc_session *s, hqq_write_reservation *res, size_t n);
void hqqrpc_tx_abort(hqqrpc_session *s, hqq_write_reservation *res);

/* ---- Test entry points (12.1, item 6) -------------------------------------------------------- */

/* Creates a session without a link. The geometry is opts->default_buffer_size and
 * opts->default_buffer_count; opts == NULL selects the defaults. The session starts in the state
 * "waiting for HELLO", like a live one, and runs on_session_start. NULL on failure. */
hqqrpc_session *hqqrpc_test_session_new(const hqqrpc_iface *iface, const hqqrpc_server_options *opts);
/* Tears the session down if it is still running (as a server stop would: no frames) and frees it. */
void hqqrpc_test_session_free(hqqrpc_session *s);

/*
 * Runs one frame through validation, dispatch and encoding. `frame` is the whole frame (n is the
 * packet's DataSize, n <= S). The frames the server publishes for it are appended to out[0..cap)
 * back to back and *out_len is their total length; a response of several frames is split at the
 * chunk boundary (every frame but the last has HDR + CHUNK bytes).
 *
 * Returns HQQRPC_OK when the frame was accepted, HQQRPC_E_PROTOCOL when it was a protocol
 * violation (the session ended; the final frame is in out), HQQRPC_E_SHUTTING_DOWN when the session
 * had already ended (nothing is processed), HQQRPC_E_LIMIT when n > S or out is too small.
 * A CLOSE frame ends the session with HQQRPC_OK and the CLOSE_ACK in out.
 */
hqqrpc_status hqqrpc_test_exchange(hqqrpc_session *s, const uint8_t *frame, size_t n, uint8_t *out,
                                   size_t cap, size_t *out_len);

/* Hooks for tests of the state machine (not part of the protocol). */
void hqqrpc_test_set_last_id(hqqrpc_session *s, uint32_t id);   /* as if `id` was the last request */
uint64_t hqqrpc_test_tombstones(const hqqrpc_session *s);       /* reservations aborted so far */
uint64_t hqqrpc_test_frames(const hqqrpc_session *s);           /* frames published so far */
uint32_t hqqrpc_test_destroy_failures(const hqqrpc_session *s);
uint32_t hqqrpc_test_live_handles(const hqqrpc_session *s);
/* Handle-table tests: the runtime's begin and end of a call without a request. */
void hqqrpc_test_call_begin(hqqrpc_session *s);
void hqqrpc_test_call_end(hqqrpc_session *s, int ok);
int hqqrpc_test_ended(const hqqrpc_session *s);                 /* the session ended (CLOSE or violation) */
/* The memory of the received frame and of the response slot of a test session. */
const uint8_t *hqqrpc_test_rx_buffer(const hqqrpc_session *s);
const uint8_t *hqqrpc_test_tx_buffer(const hqqrpc_session *s);

/* hqq_rpc_server.c: a valid HELLO arrived on a session that came from control link ctl_id. */
void hqqrpc_server_session_started(struct hqqrpc_server *srv, uint64_t ctl_id);

#endif /* HQQ_RPC_INTERNAL_H */
