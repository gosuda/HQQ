/*
 * hqq_rpc.h - C runtime of hqq-bindgen: serves a C library to Go clients over HQQ shared memory.
 *
 * The contract is spec/toolchain/BINDGEN.md (wire format in section 4, session model in 5, this
 * interface in 9.5). Generated code (<service>_server.c) and hand-written hooks include this header
 * and nothing else of the runtime.
 *
 * Threads: one session thread per client session runs dispatch(). With HQQRPC_SERIALIZE_SESSION the
 * runtime serializes library entry across the sessions of a server with the execution lock. Every
 * function that takes a hqqrpc_call, hqqrpc_wr, hqqrpc_rd or hqqrpc_session must be called on the
 * session thread, from inside dispatch; hqqrpc_server_stop() may be called from any thread.
 */
#ifndef HQQ_RPC_H
#define HQQ_RPC_H

#include <stddef.h>
#include <stdint.h>

#include "hqq/hqq.h"
#include "hqq/hqq_listener.h"

#ifdef __cplusplus
extern "C" {
#endif

/* ---- Constants (BINDGEN.md 4.2, 4.3, 4.10) ----------------------------- */
#define HQQRPC_PROTO_MAJOR 1u
#define HQQRPC_PROTO_MINOR 0u
#define HQQRPC_HEADER_SIZE 16u
#define HQQRPC_MIN_BUFFER_SIZE 128u
#define HQQRPC_MAX_BUFFER_SIZE 1048576u
#define HQQRPC_MAX_BUFFER_COUNT 4096u
#define HQQRPC_HARD_MAX_MESSAGE 1073741824u
#define HQQRPC_DETAIL_MAX 1024u
#define HQQRPC_DETAIL_RESERVE 1032u
#define HQQRPC_MSG_MAX 32u
#define HQQRPC_ERRBODY_MAX 37u
#define HQQRPC_ALIGN 16u
#define HQQRPC_OPEN_MSG_MAX 64u
#define HQQRPC_VAR_SLACK 11u
#define HQQRPC_NULL_LEN 0xFFFFFFFFu
#define HQQRPC_OPEN_METHOD 1u            /* control-link method BG_OPEN */

#define HQQRPC_K_REQUEST 0x01u
#define HQQRPC_K_RESPONSE 0x02u
#define HQQRPC_K_CONT 0x03u
#define HQQRPC_K_HELLO 0x04u
#define HQQRPC_K_HELLO_REPLY 0x05u
#define HQQRPC_K_BATCH_REQUEST 0x06u     /* reserved, never accepted */
#define HQQRPC_K_BATCH_RESPONSE 0x07u    /* reserved, never sent */
#define HQQRPC_K_CLOSE 0x08u
#define HQQRPC_K_CLOSE_ACK 0x09u

#define HQQRPC_F_DETAIL 0x01u
#define HQQRPC_F_FAIL 0x02u

/* Response status (wire u32, 4.10). */
typedef uint32_t hqqrpc_status;
#define HQQRPC_OK 0u
#define HQQRPC_E_UNKNOWN_OP 1u
#define HQQRPC_E_BAD_REQUEST 2u
#define HQQRPC_E_BAD_HANDLE 3u
#define HQQRPC_E_LIMIT 4u
#define HQQRPC_E_DENIED 5u
#define HQQRPC_E_INTERNAL 6u
#define HQQRPC_E_PROTOCOL 7u
#define HQQRPC_E_SHUTTING_DOWN 8u
#define HQQRPC_E_RESERVED_MAX 0xFFu      /* 9..0xFF: never sent; replaced by E_INTERNAL */
#define HQQRPC_E_APP_MIN 0x0100u         /* 0x0100..0xFFFF: interceptor- or shim-defined */
#define HQQRPC_E_APP_MAX 0xFFFFu

/* ---- Opaque and public types ------------------------------------------- */
typedef struct hqqrpc_server hqqrpc_server;
typedef struct hqqrpc_session hqqrpc_session;

/* One request in progress. Valid only during dispatch. */
typedef struct hqqrpc_call {
    hqqrpc_session *session;
    uint16_t op;
    uint32_t id;
    const uint8_t *body;     /* request body: slot memory (16-byte aligned) or an ALIGN-aligned private copy */
    size_t body_len;
    void *priv;              /* runtime private */
} hqqrpc_call;

/* Response body writer. p points into the reserved response slot or into scratch. */
typedef struct hqqrpc_wr {
    uint8_t *p;
    uint64_t cap;            /* body_cap given to hqqrpc_resp_begin */
    uint64_t off;            /* bytes written so far; the body length at commit */
    hqqrpc_status err;       /* first writer failure (HQQRPC_E_INTERNAL, sticky), 0 if none */
    void *priv;
} hqqrpc_wr;

/* Request body reader. */
typedef struct hqqrpc_rd {
    const uint8_t *p;
    size_t len;
    size_t off;
} hqqrpc_rd;

/* ---- Description of a generated interface ------------------------------ */
typedef struct hqqrpc_handle_type {
    const char *name;
    uint16_t parent;                 /* handle type id of the owner, 0 for none; ids are 1-based array indices */
    int (*destroy)(void *ptr);       /* destroys the C object; 0 on success */
} hqqrpc_handle_type;

#define HQQRPC_SERIALIZE_SESSION 0u
#define HQQRPC_SERIALIZE_NONE 1u

typedef struct hqqrpc_iface {
    const char *service;
    uint8_t digest[32];                          /* BINDGEN.md 2.6 */
    uint16_t op_count;
    const char *const *op_names;                 /* op_count entries, for logs */
    uint16_t handle_type_count;
    const hqqrpc_handle_type *handle_types;
    /* Decodes call->body, runs the call, and either commits exactly one response (returns
     * HQQRPC_OK) or returns a nonzero status without having committed anything. The runtime then
     * sends the error response itself: it rewrites the held response reservation in place if there
     * is one (4.7.3), rolls back the handles this call registered, and replaces a status in
     * 9..0xFF or above 0xFFFF with HQQRPC_E_INTERNAL. The glue MUST release the execution lock
     * before it returns. */
    hqqrpc_status (*dispatch)(hqqrpc_call *call);
    unsigned serialize;                          /* HQQRPC_SERIALIZE_* */
} hqqrpc_iface;

/* ---- Request decoding (all return HQQRPC_OK or HQQRPC_E_BAD_REQUEST) ---- */
HQQ_API void hqqrpc_rd_init(hqqrpc_rd *r, const hqqrpc_call *call);
/* Aligns, then returns a pointer to `size` bytes of the body: the fixed group. When a variable group
 * follows, the glue takes the padded size (G1pad of 4.6.2) so that the reader ends at the start of V1. */
HQQ_API hqqrpc_status hqqrpc_rd_take(hqqrpc_rd *r, size_t align, size_t size, const uint8_t **out);
HQQ_API hqqrpc_status hqqrpc_rd_u32(hqqrpc_rd *r, uint32_t *v);                 /* align 4: tags, handle ids */
HQQ_API hqqrpc_status hqqrpc_rd_count(hqqrpc_rd *r, size_t minsize, uint32_t max, uint32_t *count);
HQQ_API hqqrpc_status hqqrpc_rd_str(hqqrpc_rd *r, int nullable, uint32_t max, const char **s, uint32_t *n);
HQQ_API hqqrpc_status hqqrpc_rd_bytes(hqqrpc_rd *r, int nullable, uint32_t max, const void **p, uint32_t *n);
/* Fixed-size elements: *p is aligned to elem_align, count <= max, and count * elem_size fits the body. */
HQQ_API hqqrpc_status hqqrpc_rd_array(hqqrpc_rd *r, size_t elem_size, size_t elem_align, int nullable,
                              uint32_t max, const void **p, uint32_t *count);
HQQ_API hqqrpc_status hqqrpc_rd_end(const hqqrpc_rd *r);                         /* OK iff off == len */

/* ---- Response encoding -------------------------------------------------- */
/* Reserves the response (4.7.3). body_cap is RESP_BOUND (4.6.3), computed in uint64_t. In place
 * (w->p in the reserved response slot) iff HQQRPC_HEADER_SIZE + body_cap <= S, else ALIGN-aligned
 * scratch. HQQRPC_E_LIMIT if body_cap > max_message. May block while the response ring is full
 * (cancel token and write_stall_timeout, 5.2); call it outside the execution lock. */
HQQ_API hqqrpc_status hqqrpc_resp_begin(hqqrpc_call *call, uint64_t body_cap, hqqrpc_wr *w);
/* If w->err != 0 it returns w->err and publishes nothing (the reservation stays held and the
 * dispatch return converts it). Otherwise it writes the header (status 0, `flags`, id and op of the
 * call, total = w->off), publishes the frame(s), registers nothing, releases scratch, and returns
 * HQQRPC_OK, or HQQRPC_E_INTERNAL if the link failed (the session then ends). */
HQQ_API hqqrpc_status hqqrpc_resp_commit(hqqrpc_call *call, hqqrpc_wr *w, uint8_t flags);

/* Writer. Every function aligns first (zero-filled padding), checks the capacity in uint64_t, and on
 * failure sets w->err = HQQRPC_E_INTERNAL (sticky) and returns NULL or HQQRPC_E_INTERNAL. */
HQQ_API uint8_t *hqqrpc_wr_fixed(hqqrpc_wr *w, size_t align, size_t size);   /* claims `size` zero-filled bytes, advances */
HQQ_API uint8_t *hqqrpc_wr_span(hqqrpc_wr *w, size_t align, size_t n);       /* aligns, returns n bytes, does NOT advance */
HQQ_API hqqrpc_status hqqrpc_wr_advance(hqqrpc_wr *w, size_t n);             /* advances by n <= the last span */
HQQ_API hqqrpc_status hqqrpc_wr_pad(hqqrpc_wr *w, size_t align);             /* zero padding up to a multiple of align */
HQQ_API hqqrpc_status hqqrpc_wr_put_u32(hqqrpc_wr *w, uint32_t v);
HQQ_API hqqrpc_status hqqrpc_wr_put_u64(hqqrpc_wr *w, uint64_t v);
/* len, bytes, NUL. Rejects n > max (the IDL bound, UINT32_MAX if none) and a NUL byte inside s[0..n). */
HQQ_API hqqrpc_status hqqrpc_wr_put_str(hqqrpc_wr *w, const char *s, uint32_t n, uint32_t max);
HQQ_API hqqrpc_status hqqrpc_wr_put_bytes(hqqrpc_wr *w, const void *p, uint32_t n, uint32_t max); /* len, bytes */
HQQ_API hqqrpc_status hqqrpc_wr_put_null(hqqrpc_wr *w);                      /* NULL_LEN, for a nullable value */
HQQ_API hqqrpc_status hqqrpc_wr_array_begin(hqqrpc_wr *w, size_t *mark);     /* aligns, writes a placeholder count */
HQQ_API hqqrpc_status hqqrpc_wr_array_end(hqqrpc_wr *w, size_t mark, uint32_t count, uint32_t max); /* rejects count > max */
/* Detail string D of 4.6.4: pads w->off to a multiple of 8 (G2pad), then writes D, truncated at
 * HQQRPC_DETAIL_MAX. Returns HQQRPC_F_DETAIL if msg != NULL and D was written; else returns 0 and
 * writes nothing, not even the padding. */
HQQ_API uint8_t hqqrpc_wr_put_detail(hqqrpc_wr *w, const char *msg);

/* ---- Handles (4.9, 5.1) ------------------------------------------------- */
HQQ_API hqqrpc_status hqqrpc_handle_get(hqqrpc_session *s, uint32_t id, uint16_t type, int nullable, void **ptr);
/* Checks that `n` more handles fit in the table and holds their entries until the end of the call.
 * HQQRPC_E_LIMIT otherwise. Called before the C function runs (4.6.5). */
HQQ_API hqqrpc_status hqqrpc_handle_reserve(hqqrpc_session *s, unsigned n);
/* Registers ptr in a reserved entry. parent_id is the id of the parent handle or 0. Cannot fail after
 * hqqrpc_handle_reserve; HQQRPC_E_INTERNAL if the call made no reservation or the parent is dead.
 * The runtime remembers the entries a call registered and rolls them back (destroys the objects
 * through the type adapters, frees the entries) if the call ends with a status other than OK. */
HQQ_API hqqrpc_status hqqrpc_handle_put(hqqrpc_session *s, uint16_t type, void *ptr, uint32_t parent_id, uint32_t *id);
/* Destroys the live descendants of id, newest creation first, through their adapters, and consumes
 * their entries whatever the adapters return (5.1). The CALLER HOLDS the execution lock; this function
 * does not take it. Failures are counted and logged, not returned. */
HQQ_API void hqqrpc_handle_destroy_children(hqqrpc_session *s, uint32_t id);
/* Consumes the entry of id (generation + 1, index to the free list). A destroy glue calls it after
 * the C destroy function ran, always, except when the function has @retain_on_fail and failed (5.1). */
HQQ_API void hqqrpc_handle_consume(hqqrpc_session *s, uint32_t id);

/* ---- Execution lock and scratch (8.1) ----------------------------------- */
HQQ_API void hqqrpc_exec_begin(hqqrpc_session *s);   /* no-op for HQQRPC_SERIALIZE_NONE */
HQQ_API void hqqrpc_exec_end(hqqrpc_session *s);
HQQ_API void *hqqrpc_scratch(hqqrpc_session *s, size_t n);  /* call-scoped, ALIGN-aligned, NULL on failure */
HQQ_API void *hqqrpc_session_user(hqqrpc_session *s);

/* ---- Server ------------------------------------------------------------- */
typedef struct hqqrpc_server_options {
    size_t struct_size;                  /* set by hqqrpc_server_options_init() */
    unsigned perm;                       /* listener and session segments; 0: 0600 */
    uint32_t default_buffer_count;       /* 0: 64 */
    uint32_t default_buffer_size;        /* 0: 16384 */
    uint32_t max_buffer_count;           /* 0: 4096 */
    uint32_t max_buffer_size;            /* 0: 1048576 */
    uint32_t max_sessions;               /* 0: 64 */
    uint32_t max_handles;                /* 0: 4096; at most 65535 */
    uint32_t max_control_links;          /* 0: 16 (4.11) */
    uint64_t max_segment_bytes;          /* 0: 64 MiB; limit for 2 * B * S (4.11) */
    uint32_t max_in_flight;              /* 0: the session's buffer count */
    uint32_t max_message;                /* 0: 16 MiB; at most 1 << 30 */
    int64_t open_timeout_ns;             /* 0: 5 s */
    int64_t shutdown_timeout_ns;         /* 0: 5 s */
    int64_t write_stall_timeout_ns;      /* 0: 10 s (5.2) */
    int64_t idle_timeout_ns;             /* 0: disabled (5.2) */
    uint32_t scratch_keep;               /* 0: 1 MiB */
    int copy_request;                    /* 0: zero-copy request bodies; 1: copy first (7.2) */
    hqq_options link;                    /* options of session links; hqqrpc_server_options_init sets
                                          * peer_timeout_ns = 2 s explicitly (0 would mean 5 s). */
    hqq_acceptor_options acceptor;       /* the control listener */
    void *user;                          /* passed to the callbacks below */
    void *(*on_session_start)(void *user, hqqrpc_session *s);              /* may be NULL */
    void (*on_session_end)(void *user, void *session_user, hqqrpc_session *s); /* may be NULL */
    void (*on_log)(void *user, int level, const char *msg);                 /* may be NULL */
} hqqrpc_server_options;

HQQ_API void hqqrpc_server_options_init(hqqrpc_server_options *opts);

/* Creates the listener `listener_name`. */
HQQ_API hqq_status hqqrpc_server_create(const hqqrpc_iface *iface, const char *listener_name,
                                const hqqrpc_server_options *opts, hqqrpc_server **out);
/* Accepts and serves until hqqrpc_server_stop(). HQQ_OK after every session and control thread ended.
 * HQQ_ERR_TIMEOUT if some thread was still running after shutdown_timeout_ns (stuck in the library or
 * waiting for the execution lock): those threads stay detached and running (5.2). */
HQQ_API hqq_status hqqrpc_server_run(hqqrpc_server *s);
HQQ_API void hqqrpc_server_stop(hqqrpc_server *s);       /* thread-safe, idempotent; signals the cancel token */
/* Frees the server. Returns HQQ_ERR_INVALID_OP and frees NOTHING if run did not return HQQ_OK (a
 * thread may still use the server); the caller then exits the process. */
HQQ_API hqq_status hqqrpc_server_destroy(hqqrpc_server *s);

/* The process entry point used by generated servers: parses
 *   <listener-name> [--perm OCTAL] [--max-sessions N] [--max-handles N] [--max-in-flight N]
 *   [--max-message BYTES] [--buffer-count N] [--buffer-size BYTES] [--heartbeat-ms N]
 *   [--peer-timeout-ms N] [--open-timeout-ms N] [--shutdown-timeout-ms N] [--write-stall-ms N]
 *   [--idle-timeout-ms N] [--max-control-links N] [--max-segment-bytes N] [--copy-request]
 *   [--spin] [--parent-stdin[=force]] [--lifeline-grace-ms N]
 * (the shape of hqq-sqlite-server), prints "ready <listener-name>" on stdout once the listener
 * exists, handles SIGINT and SIGTERM as hqqrpc_server_stop(), and returns the exit code (nonzero, without
 * freeing the server, if run returned HQQ_ERR_TIMEOUT).
 * `defaults` carries the callbacks and the defaults the flags override. */
HQQ_API int hqqrpc_main(const hqqrpc_iface *iface, const hqqrpc_server_options *defaults, int argc, char **argv);

/* Status filters for results of hand-written code (4.6.6, 7.3). */
HQQ_API hqqrpc_status hqqrpc_hook_status(hqqrpc_status st);  /* OK, E_DENIED and 0x0100..0xFFFF pass; others become E_INTERNAL */
HQQ_API hqqrpc_status hqqrpc_shim_status(hqqrpc_status st);  /* OK and 0x0100..0xFFFF pass; others become E_INTERNAL */

HQQ_API const char *hqqrpc_strerror(hqqrpc_status status);   /* static text, never NULL */

#ifdef __cplusplus
}
#endif
#endif /* HQQ_RPC_H */

