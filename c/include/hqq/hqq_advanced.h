/*
 * hqq_advanced.h - the ADVANCED layer of libhqq (SPEC 10, ext/PROTOCOL_1_1_DRAFT.md).
 *
 * An Advanced link is a Standard link (the same shared segment, rings and
 * packet ABI, see hqq.h) whose receive direction is owned by a dispatcher
 * thread after negotiation. On top of it: negotiated feature bits, streams
 * (ConnectionIDs), arbitrary-size messages with chunking and reassembly,
 * request/response calls with per-request contexts, cancellation, request
 * metadata and graceful shutdown.
 *
 * The API mirrors the Go package (gosuda.org/hqq):
 *   Go                              C
 *   CreateAdvancedLink/OpenAdvancedLink   hqq_adv_create / hqq_adv_open
 *   AdvancedLink.Negotiate              hqq_adv_negotiate
 *   AdvancedLink.ListenStreams          hqq_adv_listen_streams
 *   AdvancedLink.OpenStream/AcceptStream hqq_adv_open_stream / hqq_adv_accept_stream
 *   Stream.Send / Recv / Close          hqq_stream_send / hqq_stream_recv / hqq_stream_close
 *   Stream.Call / CallWith              hqq_stream_call / hqq_stream_call_with
 *   AdvancedLink.Serve                  hqq_adv_serve (handler callback)
 *   AdvancedLink.Shutdown               hqq_adv_shutdown
 *   Message.Bytes / Release             hqq_msg_data / hqq_msg_release
 *   ResponseError                       HQQ_ERR_RESPONSE + hqq_msg_status()
 *
 * Thread safety
 * -------------
 *  Every function of this header may be called from any thread, concurrently
 *  with any other function on the same link, except that hqq_adv_close()
 *  waits for the calls in progress and must not be called from a handler.
 *  Message handles (hqq_msg) and request contexts are owned by one caller at
 *  a time and must be released by it; hqq_msg_release() is the only
 *  operation on a message that may run after the link is closed.
 *
 * Memory
 * ------
 *  The data path (send, receive, call, serve, reassembly) takes its buffers
 *  from size-class pools owned by the link; after a warm-up, no allocation
 *  happens on it. Streams are allocated when opened or accepted and freed by
 *  hqq_adv_close(). A closed stream handle stays valid until then and fails
 *  with HQQ_ERR_CLOSED.
 *
 * Timeouts: every blocking call takes a `const hqq_wait *` (hqq.h). NULL
 * means: block until done, no cancellation.
 */
#ifndef HQQ_ADVANCED_H
#define HQQ_ADVANCED_H

#include <stddef.h>
#include <stdint.h>

#include "hqq/hqq.h"

#ifdef __cplusplus
extern "C" {
#endif

/* ---- Protocol versions and feature bits ------------------------------------- */

/* Version word of OpProtoNegotiate / OpProtoAck: (major << 8) | minor. */
#define HQQ_PROTOCOL_VERSION(major, minor) ((uint16_t)(((major) << 8) | (minor)))
#define HQQ_PROTOCOL_1_0 HQQ_PROTOCOL_VERSION(1, 0)
#define HQQ_PROTOCOL_1_1 HQQ_PROTOCOL_VERSION(1, 1)

#define HQQ_FEATURE_MESSAGES 0x01u         /* plain messages (Send/Recv) */
#define HQQ_FEATURE_REQUEST_RESPONSE 0x02u /* Call/Serve */
#define HQQ_FEATURE_CANCEL 0x04u           /* 1.1: OpCancel */
#define HQQ_FEATURE_METADATA 0x08u         /* 1.1: request metadata block */
#define HQQ_FEATURES_ALL 0x0Fu             /* every feature of protocol 1.1 */

/* Error code carried by an error response or an OpError (SPEC 12). */
#define HQQ_ERRCODE_SHUTTING_DOWN 0x08u

/* Defaults of the options (zero selects them). */
#define HQQ_ADV_DEFAULT_MAX_MESSAGE_SIZE (UINT64_C(64) << 20)
#define HQQ_ADV_DEFAULT_QUEUE_SIZE 0u /* == buffer_count */

/* Negotiation state (hqq_adv_protocol_state). */
typedef enum hqq_proto_state {
    HQQ_PROTO_NONE = 0,        /* no negotiation started */
    HQQ_PROTO_NEGOTIATING = 1,
    HQQ_PROTO_NEGOTIATED = 2,
    HQQ_PROTO_FAILED = 3       /* terminal: close the link and open a new one */
} hqq_proto_state;

/* What to do when the queue a message is routed to is full (SPEC 10.7). */
typedef enum hqq_overflow {
    HQQ_OVERFLOW_BLOCK = 0, /* default: backpressure, the receive path waits */
    HQQ_OVERFLOW_DROP = 1,  /* drop the message (stream open is refused) */
    HQQ_OVERFLOW_FAIL = 2   /* fail the receive path with HQQ_ERR_INCOMING_OVERFLOW */
} hqq_overflow;

/* ---- Options and statistics ------------------------------------------------- */

typedef struct hqq_adv_options {
    size_t struct_size;             /* set by hqq_adv_options_init() */
    uint64_t max_message_size;      /* 0: 64 MiB. Larger messages are refused locally. */
    uint64_t max_reassembly_bytes;  /* 0: 4 x max_message_size. Bounds incomplete multi-chunk messages. */
    uint64_t queue_size;            /* 0: buffer_count. Capacity of every incoming queue. */
    hqq_overflow overflow;          /* default HQQ_OVERFLOW_BLOCK */
    hqq_options link;               /* wait policy, heartbeat, peer and attach timeouts, allocator */
    int64_t stream_listen_grace_ns; /* 0: 2 s. A stream request that arrives before ListenStreams is held this long; negative: refused at once. */
} hqq_adv_options;

HQQ_API void hqq_adv_options_init(hqq_adv_options *opts);

typedef struct hqq_adv_stats {
    uint64_t negotiation_ns;
    uint64_t messages;           /* messages sent with hqq_stream_send */
    uint64_t message_bytes;      /* payload bytes of those messages */
    uint64_t requests;
    uint64_t responses;
    uint64_t dropped_messages;   /* see SPEC 10.7 and the Go AdvancedStats.DroppedMessages */
    uint64_t abandoned_responses;
    uint64_t canceled_requests;
    uint64_t refused_requests;
} hqq_adv_stats;

/* ---- Handles ---------------------------------------------------------------- */

typedef struct hqq_adv hqq_adv;         /* an Advanced link (AdvancedLink) */
typedef struct hqq_stream hqq_stream;   /* a stream of an Advanced link (Stream) */
typedef struct hqq_msg hqq_msg;         /* a received message, pooled (Message) */
typedef struct hqq_request hqq_request; /* a request as seen by a handler (Request) */
typedef struct hqq_req_ctx hqq_req_ctx; /* the context of one request (SPEC 10.6, 1.1 4.2) */
typedef struct hqq_response hqq_response; /* the response writer of one request (ResponseWriter) */

/* Caller-supplied options of a call (Go CallOptions). Zero means "none". */
typedef struct hqq_call_options {
    size_t struct_size;   /* set by hqq_call_options_init() */
    uint64_t caller;      /* application caller ID (1.1 metadata); 0: not sent */
    uint8_t trace[16];    /* application trace ID (1.1 metadata); all zero: not sent */
} hqq_call_options;

HQQ_API void hqq_call_options_init(hqq_call_options *opts);

/* ---- Opening ----------------------------------------------------------------- */

/*
 * Creates (create) or opens (open) a named Advanced link. They behave like
 * hqq_standard_create() / hqq_standard_open() (hqq.h): the same name rules,
 * roles, errors and `info` output. The link starts without a negotiation:
 * call hqq_adv_negotiate() before any message or call. opts may be NULL.
 */
HQQ_API hqq_status hqq_adv_create(const char *name, uint64_t buffer_count, uint64_t buffer_size,
                                  unsigned perm, const hqq_adv_options *opts,
                                  hqq_adv **out, hqq_open_info *info);
HQQ_API hqq_status hqq_adv_open(const char *name, uint64_t buffer_count, uint64_t buffer_size,
                                const hqq_adv_options *opts, hqq_adv **out, hqq_open_info *info);
/* Caller-owned memory, like hqq_standard_open_memory(). */
HQQ_API hqq_status hqq_adv_open_memory(void *mem, size_t mem_size, uint64_t buffer_count,
                                       uint64_t buffer_size, const hqq_adv_options *opts,
                                       hqq_adv **out, hqq_open_info *info);

/*
 * Stops every stream, request and call of the link, joins the link's threads,
 * releases the link's messages and streams, and closes the underlying segment
 * (hqq_close semantics: unlinks the name only while it is this endpoint's
 * instance). Pending calls and waits fail with HQQ_ERR_CLOSED. Thread-safe;
 * must not be called from a Serve handler.
 */
HQQ_API hqq_status hqq_adv_close(hqq_adv *link);
/* Removes the name of a named link (hqq_link_unlink), when still ours. */
HQQ_API hqq_status hqq_adv_unlink(hqq_adv *link);
/* The underlying Standard link (borrowed; valid until hqq_adv_close). */
HQQ_API hqq_link *hqq_adv_base(hqq_adv *link);

HQQ_API hqq_mode hqq_adv_mode(const hqq_adv *link);
HQQ_API hqq_peer_status hqq_adv_peer_state(hqq_adv *link);
HQQ_API size_t hqq_adv_buffer_size(const hqq_adv *link);
HQQ_API size_t hqq_adv_buffer_count(const hqq_adv *link);
HQQ_API void hqq_adv_stats_get(const hqq_adv *link, hqq_adv_stats *out);

/* ---- Negotiation (SPEC 10.1, 1.1 section 1) ---------------------------------- */

/*
 * Role-agnostic negotiation: the Primary sends OpProtoNegotiate and waits for
 * the ack; the Secondary waits for the request and answers it. `features` is
 * the set to request; 0 requests every feature of protocol 1.1. Returns
 * HQQ_OK when negotiated; HQQ_ERR_ALREADY_NEGOTIATED while an attempt is in
 * progress or done; HQQ_ERR_NEGOTIATION_FAILED after a failed attempt (state
 * FAILED is terminal); HQQ_ERR_NEGOTIATION_REJECTED when the peer refuses.
 * Blocks until done or until `wait` ends. Thread-safe.
 */
HQQ_API hqq_status hqq_adv_negotiate(hqq_adv *link, uint64_t features, const hqq_wait *wait);
/* Same with an explicit version word (for tests and version interop). */
HQQ_API hqq_status hqq_adv_negotiate_protocol(hqq_adv *link, uint16_t version, uint64_t features,
                                              const hqq_wait *wait);
HQQ_API hqq_proto_state hqq_adv_protocol_state(hqq_adv *link);
HQQ_API uint16_t hqq_adv_negotiated_version(const hqq_adv *link);
HQQ_API uint64_t hqq_adv_negotiated_features(const hqq_adv *link);

/* ---- Streams (SPEC 10.2, 10.3) ------------------------------------------------ */

/* The link-level stream 0. It always exists, is owned by the link and must not
 * be closed. Valid after hqq_adv_open/create (before negotiation too). */
HQQ_API hqq_stream *hqq_adv_stream0(hqq_adv *link);
/* Enables incoming stream creation (Go ListenStreams). Idempotent-refusing:
 * HQQ_ERR_ALREADY_LISTENING on the second call. Requires negotiation. A stream
 * request that arrived before this call was held (see stream_listen_grace_ns):
 * the held requests are accepted now, in arrival order, and those whose grace
 * period had ended were refused with HQQ_ERR_CONN_NOT_FOUND. */
HQQ_API hqq_status hqq_adv_listen_streams(hqq_adv *link);
/* Waits for an incoming stream (its OpConnAccept is already queued). */
HQQ_API hqq_status hqq_adv_accept_stream(hqq_adv *link, hqq_stream **out, const hqq_wait *wait);
/* Opens a stream of this endpoint's parity; waits for the peer's accept. A
 * request that reaches a peer which is not listening yet is held there for the
 * peer's stream_listen_grace_ns first. A refusal maps to HQQ_ERR_CONN_NOT_FOUND
 * (still not listening after the grace period), HQQ_ERR_INVALID_OP (parity or
 * duplicate) or HQQ_ERR_BUFFER_OVERFLOW (accept queue full). */
HQQ_API hqq_status hqq_adv_open_stream(hqq_adv *link, hqq_stream **out, const hqq_wait *wait);

HQQ_API uint64_t hqq_stream_id(const hqq_stream *s);
/* Closes the stream (not stream 0). A second close returns HQQ_ERR_CLOSED.
 * Discards queued messages; sends OpConnClose unless the peer closed first. */
HQQ_API hqq_status hqq_stream_close(hqq_stream *s);

/* ---- Messages (SPEC 10.4, 10.5) ----------------------------------------------- */

/* Sends data as one message (chunked to the buffer size). len == 0 is an empty
 * message. Requires FeatureMessages (HQQ_ERR_NOT_NEGOTIATED otherwise). */
HQQ_API hqq_status hqq_stream_send(hqq_stream *s, const void *data, size_t len, const hqq_wait *wait);
/* Receives the next data message of the stream into *out (release it with
 * hqq_msg_release). After the peer closed the stream, the buffered messages
 * are delivered first, then HQQ_ERR_PEER_CLOSED. */
HQQ_API hqq_status hqq_stream_recv(hqq_stream *s, hqq_msg **out, const hqq_wait *wait);

HQQ_API const void *hqq_msg_data(const hqq_msg *m);
HQQ_API size_t hqq_msg_len(const hqq_msg *m);
HQQ_API uint64_t hqq_msg_stream_id(const hqq_msg *m);
/* Status of an error response (0 for every other message). */
HQQ_API uint64_t hqq_msg_status(const hqq_msg *m);
/* Returns the message to its pool. NULL is accepted. */
HQQ_API void hqq_msg_release(hqq_msg *m);

/* ---- Requests and responses (SPEC 10.6, 1.1 sections 2-5) -------------------- */

/*
 * Calls `method` on the stream with `req` and waits for the response. On
 * success *resp receives the response (release it). An error response
 * returns HQQ_ERR_RESPONSE with *resp set: hqq_msg_status() is the status
 * and hqq_msg_data() the error payload. A response refused by the reassembly
 * limit returns HQQ_ERR_REASSEMBLY_LIMIT, an aborted one HQQ_ERR_MESSAGE_ABORTED,
 * a shut-down peer HQQ_ERR_RESPONSE with status 8. Requires
 * FeatureRequestResponse. When `wait` ends while waiting: HQQ_ERR_TIMEOUT or
 * HQQ_ERR_CANCELED; with FeatureCancel an OpCancel is queued (1.1 section 5).
 * With FeatureMetadata and a deadline, the remaining time is sent (1.1 section 3).
 */
HQQ_API hqq_status hqq_stream_call(hqq_stream *s, uint64_t method, const void *req, size_t len,
                                   hqq_msg **resp, const hqq_wait *wait);
HQQ_API hqq_status hqq_stream_call_with(hqq_stream *s, uint64_t method, const void *req, size_t len,
                                        const hqq_call_options *opts, hqq_msg **resp,
                                        const hqq_wait *wait);

/* Handler of one request. It runs on a worker thread of Serve, may block, and
 * must not keep `req`, `w` or `ctx` after it returns. If it writes no response,
 * an empty response is sent (unless the request was canceled first). */
typedef void (*hqq_handler_fn)(void *user, hqq_req_ctx *ctx, hqq_response *w, const hqq_request *req);

/*
 * Serves requests of the link until `wait` ends, the link is closed or shut
 * down (then HQQ_OK after the in-flight requests completed), or the receive
 * path fails (its error). Handlers run concurrently on worker threads. Only
 * one Serve may run per link (HQQ_ERR_INVALID_OP otherwise).
 */
HQQ_API hqq_status hqq_adv_serve(hqq_adv *link, hqq_handler_fn fn, void *user, const hqq_wait *wait);

HQQ_API uint64_t hqq_request_method(const hqq_request *req);
HQQ_API const void *hqq_request_data(const hqq_request *req);
HQQ_API size_t hqq_request_len(const hqq_request *req);
HQQ_API uint64_t hqq_request_stream_id(const hqq_request *req);
HQQ_API uint64_t hqq_request_caller(const hqq_request *req);
/* Returns 1 and copies the trace ID when the request carried one, else 0. */
HQQ_API int hqq_request_trace(const hqq_request *req, uint8_t out[16]);

/* Response writer (ResponseWriter of the Go API). Write appends to the response body
 * (at most MaxMessageSize in total; a larger Write writes nothing and returns
 * HQQ_ERR_BUFFER_OVERFLOW). WriteError replaces the body with an error payload and
 * sets the status (0 is sent as HQQ_ERRCODE_INVALID_OP). The response is sent once,
 * when the handler returns, unless the request was canceled or its deadline passed. */
HQQ_API hqq_status hqq_response_write(hqq_response *w, const void *data, size_t len);
HQQ_API hqq_status hqq_response_error(hqq_response *w, uint64_t status, const void *data, size_t len);

/* Request context. hqq_req_ctx_err() is HQQ_OK while the request is live,
 * HQQ_ERR_CANCELED after an OpCancel, a closed stream, Serve's end, or the
 * handler's return, and HQQ_ERR_TIMEOUT once the budget of the request passed. */
HQQ_API hqq_status hqq_req_ctx_err(const hqq_req_ctx *ctx);
/* Blocks until the context ends or `timeout_ns` (< 0: forever) passes.
 * Returns HQQ_OK when it ended, HQQ_ERR_TIMEOUT otherwise. */
HQQ_API hqq_status hqq_req_ctx_wait_done(hqq_req_ctx *ctx, int64_t timeout_ns);
/* Returns 1 and the monotonic deadline (hqq_plat_mono_ns scale) when the request
 * has a deadline (1.1 metadata budget or Serve's own), else 0. */
HQQ_API int hqq_req_ctx_deadline(const hqq_req_ctx *ctx, uint64_t *mono_ns);

/* ---- Shutdown (1.1 section 6) ------------------------------------------------- */

/*
 * Graceful shutdown of the responder role: from now on every incoming request
 * is answered with the refusal response (status 8) and not handled. Returns
 * HQQ_OK when no request is in flight, or the wait's status (HQQ_ERR_TIMEOUT,
 * HQQ_ERR_CANCELED) when it ended first; refusals continue until close. The link
 * stays usable: streams stay open and no OpConnClose is sent.
 */
HQQ_API hqq_status hqq_adv_shutdown(hqq_adv *link, const hqq_wait *wait);

#ifdef __cplusplus
} /* extern "C" */
#endif

#endif /* HQQ_ADVANCED_H */
