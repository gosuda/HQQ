/*
 * hqq_rpc_session.c - one session of the hqq-bindgen C runtime: frame receive and validation
 * (BINDGEN.md 4.4), assembly of CONT frames (4.8), request ids (4.12), the server order of 4.7.1,
 * the response reservation held in place (4.7.3), error and status encoding (4.10), teardown (5.2),
 * and the link-less test entry points (12.1, item 4).
 *
 * A session is used by one thread at a time: the session thread of the server, or the test.
 * Portable C11: no POSIX headers, no threads.
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

#include <string.h>

#define HQQ_BUILDING 1
#include "hqq_rpc_internal.h"

#define HDR HQQRPC_HEADER_SIZE

static const uint8_t k_magic[4] = {0x48, 0x51, 0x42, 0x47}; /* "HQBG" */

static uint32_t min_u32(uint32_t a, uint32_t b) {
    return a < b ? a : b;
}

/* ---- Transmit ------------------------------------------------------------------------------ */

void hqqrpc_session_set_end(hqqrpc_session *s, hqqrpc_end why) {
    if (s->end == HQQRPC_END_NONE) {
        s->end = why;
    }
}

static hqqrpc_end end_from_status(hqq_status st) {
    if (st == HQQ_ERR_TIMEOUT) {
        return HQQRPC_END_STALL;
    }
    if (st == HQQ_ERR_CANCELED) {
        return HQQRPC_END_STOP;
    }
    return HQQRPC_END_PEER;
}

/* final != 0: a best-effort frame, timeout 0 and no cancel token (5.2). Otherwise the wait is
 * bounded by write_stall_timeout and the server's cancel token. */
hqq_status hqqrpc_tx_reserve(hqqrpc_session *s, int final, uint8_t **frame, hqq_write_reservation *res) {
    hqq_wait w;
    hqq_status st;

    if (s->link == NULL) {
        memset(res, 0, sizeof *res);
        *frame = s->t_tx;
        return HQQ_OK;
    }
    w.timeout_ns = final ? 0 : s->cfg.write_stall_timeout_ns;
    w.cancel = final ? NULL : s->cancel;
    st = hqq_write_reserve(s->link, res, &w);
    if (st == HQQ_OK) {
        *frame = (uint8_t *)res->data;
    }
    return st;
}

hqq_status hqqrpc_tx_commit(hqqrpc_session *s, hqq_write_reservation *res, size_t n) {
    s->t_frames++;
    if (s->link == NULL) {
        if (s->t_out == NULL || n > s->t_out_cap - s->t_out_len) {
            s->t_out_overflow = 1;
            return HQQ_OK;
        }
        memcpy(s->t_out + s->t_out_len, s->t_tx, n);
        s->t_out_len += n;
        return HQQ_OK;
    }
    return hqq_write_commit(res, n);
}

void hqqrpc_tx_abort(hqqrpc_session *s, hqq_write_reservation *res) {
    s->t_tombstones++;
    if (s->link != NULL) {
        (void)hqq_write_abort(res, HQQ_ERRCODE_INVALID_OP);
    }
}

static void put_hdr(uint8_t *f, uint8_t kind, uint8_t flags, uint16_t op, uint32_t id, uint32_t total,
                    uint32_t aux) {
    f[0] = kind;
    f[1] = flags;
    hqqrpc_put_le16(f + 2, op);
    hqqrpc_put_le32(f + 4, id);
    hqqrpc_put_le32(f + 8, total);
    hqqrpc_put_le32(f + 12, aux);
}

/* RESPONSE frame carrying a status and its message (4.10). Returns the frame length. */
static size_t build_status_frame(uint8_t *f, uint16_t op, uint32_t id, hqqrpc_status st) {
    const char *msg = hqqrpc_status_text(st);
    size_t n = strlen(msg);

    put_hdr(f, HQQRPC_K_RESPONSE, 0, op, id, (uint32_t)(n + 5u), st);
    hqqrpc_put_le32(f + HDR, (uint32_t)n);
    memcpy(f + HDR + 4, msg, n);
    f[HDR + 4 + n] = 0;
    return HDR + 4 + n + 1;
}

/* Session-level final frame (id 0), best effort. */
static void send_final_status(hqqrpc_session *s, hqqrpc_status st) {
    uint8_t *f;
    hqq_write_reservation res;

    if (hqqrpc_tx_reserve(s, 1, &f, &res) != HQQ_OK) {
        return;
    }
    (void)hqqrpc_tx_commit(s, &res, build_status_frame(f, 0, 0, st));
}

static void send_close_ack(hqqrpc_session *s) {
    uint8_t *f;
    hqq_write_reservation res;

    if (hqqrpc_tx_reserve(s, 1, &f, &res) != HQQ_OK) {
        return;
    }
    put_hdr(f, HQQRPC_K_CLOSE_ACK, 0, 0, 0, 0, s->destroy_failures);
    (void)hqqrpc_tx_commit(s, &res, HDR);
}

/*
 * HELLO_REPLY (4.11). A reply with a nonzero aux is final: timeout 0 and zero granted values. The
 * successful reply waits like any response and ends the session if it cannot be published.
 */
static void send_hello_reply(hqqrpc_session *s, uint32_t aux, uint8_t minor) {
    uint8_t *f;
    uint8_t *b;
    hqq_write_reservation res;
    hqq_status st = hqqrpc_tx_reserve(s, aux != 0, &f, &res);

    if (st != HQQ_OK) {
        hqqrpc_session_set_end(s, end_from_status(st));
        return;
    }
    b = f + HDR;
    put_hdr(f, HQQRPC_K_HELLO_REPLY, 0, 0, 0, HQQRPC_HELLO_SIZE, aux);
    memset(b, 0, HQQRPC_HELLO_SIZE);
    memcpy(b, k_magic, 4);
    hqqrpc_put_le16(b + 4, (uint16_t)HQQRPC_PROTO_MAJOR);
    hqqrpc_put_le16(b + 6, minor);
    if (aux == 0) {
        hqqrpc_put_le32(b + 12, s->max_in_flight);
        hqqrpc_put_le32(b + 16, s->max_message);
        hqqrpc_put_le32(b + 20, s->cfg.max_handles);
    }
    memcpy(b + 32, s->iface->digest, sizeof s->iface->digest);
    st = hqqrpc_tx_commit(s, &res, HDR + HQQRPC_HELLO_SIZE);
    if (st != HQQ_OK) {
        hqqrpc_session_set_end(s, HQQRPC_END_PEER);
    }
}

/* ---- Call state --------------------------------------------------------------------------- */

static void cs_reset(hqqrpc_session *s, uint16_t op, uint32_t id) {
    hqqrpc_cstate *cs = &s->cs;
    uint32_t *reg = cs->reg;
    unsigned regcap = cs->regcap;
    hqqrpc_blk *scratch = cs->scratch;

    memset(cs, 0, sizeof *cs);
    cs->reg = reg;
    cs->regcap = regcap;
    cs->scratch = scratch;
    cs->op = op;
    cs->id = id;
}

/* ---- Response (4.7.3, 4.8) ------------------------------------------------------------------ */

hqqrpc_status hqqrpc_resp_begin(hqqrpc_call *call, uint64_t body_cap, hqqrpc_wr *w) {
    hqqrpc_session *s = call->session;
    hqqrpc_cstate *cs = &s->cs;

    memset(w, 0, sizeof *w);
    if (cs->began || s->end != HQQRPC_END_NONE) {
        return HQQRPC_E_INTERNAL;
    }
    if (body_cap > s->max_message) {
        return HQQRPC_E_LIMIT;
    }
    if ((uint64_t)HDR + body_cap <= s->S) {
        uint8_t *frame = NULL;
        hqq_status st = hqqrpc_tx_reserve(s, 0, &frame, &cs->res);

        if (st != HQQ_OK) {
            hqqrpc_session_set_end(s, end_from_status(st));
            return HQQRPC_E_INTERNAL;
        }
        cs->held = 1;
        cs->frame = frame;
        w->p = frame + HDR;
    } else {
        uint8_t *p = (uint8_t *)hqqrpc_scratch(s, (size_t)body_cap);

        if (p == NULL) {
            return HQQRPC_E_INTERNAL;
        }
        cs->body = p;
        w->p = p;
    }
    cs->began = 1;
    cs->cap = body_cap;
    w->cap = body_cap;
    w->priv = cs;
    return HQQRPC_OK;
}

hqqrpc_status hqqrpc_resp_commit(hqqrpc_call *call, hqqrpc_wr *w, uint8_t flags) {
    hqqrpc_session *s = call->session;
    hqqrpc_cstate *cs = &s->cs;
    uint32_t total;
    uint32_t sent;
    hqq_status st;

    if (w->err != 0) {
        return w->err;
    }
    if (!cs->began || cs->committed || w->priv != cs || s->end != HQQRPC_END_NONE ||
        (flags & (uint8_t)~(HQQRPC_F_DETAIL | HQQRPC_F_FAIL)) != 0 ||
        ((flags & HQQRPC_F_DETAIL) != 0 && (flags & HQQRPC_F_FAIL) == 0) || w->off > cs->cap) {
        return HQQRPC_E_INTERNAL;
    }
    total = (uint32_t)w->off;
    if (cs->held) {
        put_hdr(cs->frame, HQQRPC_K_RESPONSE, flags, cs->op, cs->id, total, 0);
        cs->held = 0;
        cs->committed = 1;
        st = hqqrpc_tx_commit(s, &cs->res, HDR + (size_t)total);
        if (st != HQQ_OK) {
            hqqrpc_session_set_end(s, HQQRPC_END_PEER);
            return HQQRPC_E_INTERNAL;
        }
        return HQQRPC_OK;
    }

    /* Built in scratch: publish n = max(1, ceil(T / CHUNK)) frames (4.8). */
    sent = 0;
    cs->committed = 1;
    do {
        uint32_t n = min_u32(total - sent, s->chunk);
        uint8_t *f = NULL;
        hqq_write_reservation res;

        st = hqqrpc_tx_reserve(s, 0, &f, &res);
        if (st != HQQ_OK) {
            hqqrpc_session_set_end(s, end_from_status(st));
            return HQQRPC_E_INTERNAL;
        }
        if (sent == 0) {
            put_hdr(f, HQQRPC_K_RESPONSE, flags, cs->op, cs->id, total, 0);
        } else {
            put_hdr(f, HQQRPC_K_CONT, 0, 0, cs->id, total, sent);
        }
        if (n != 0) {
            memcpy(f + HDR, cs->body + sent, n);
        }
        st = hqqrpc_tx_commit(s, &res, HDR + (size_t)n);
        if (st != HQQ_OK) {
            hqqrpc_session_set_end(s, HQQRPC_END_PEER);
            return HQQRPC_E_INTERNAL;
        }
        sent += n;
    } while (sent < total);
    return HQQRPC_OK;
}

/* Sends the error response of the current call: in place if a slot is held (4.7.3), else in a
 * new frame. */
static void send_call_error(hqqrpc_session *s, hqqrpc_status status) {
    hqqrpc_cstate *cs = &s->cs;
    uint8_t *f = NULL;
    hqq_write_reservation res;
    hqq_status st;
    size_t n;

    if (cs->held) {
        f = cs->frame;
        res = cs->res;
        cs->held = 0;
    } else {
        st = hqqrpc_tx_reserve(s, 0, &f, &res);
        if (st != HQQ_OK) {
            hqqrpc_session_set_end(s, end_from_status(st));
            return;
        }
    }
    n = build_status_frame(f, cs->op, cs->id, status);
    cs->committed = 1;
    st = hqqrpc_tx_commit(s, &res, n);
    if (st != HQQ_OK) {
        hqqrpc_session_set_end(s, HQQRPC_END_PEER);
    }
}

/* ---- Request execution (4.7.1) -------------------------------------------------------------- */

static void finish_call(hqqrpc_session *s, hqqrpc_status st) {
    hqqrpc_cstate *cs = &s->cs;

    if (s->lock_held) {
        hqqrpc_log(s, HQQRPC_LOG_ERROR, "op %u returned with the execution lock held", (unsigned)cs->op);
        hqqrpc_exec_end(s);
    }
    if (cs->committed) {
        if (st != HQQRPC_OK) {
            hqqrpc_log(s, HQQRPC_LOG_ERROR, "op %u returned status %u after it committed a response",
                       (unsigned)cs->op, (unsigned)st);
        }
        hqqrpc_call_handles_end(s, 0);
    } else if (s->end != HQQRPC_END_NONE) {
        /* The link failed or the server stops: the teardown destroys every live handle. */
        if (cs->held) {
            hqqrpc_tx_abort(s, &cs->res);
            cs->held = 0;
        }
        hqqrpc_call_handles_end(s, 0);
    } else {
        hqqrpc_status status = st == HQQRPC_OK ? HQQRPC_E_INTERNAL : hqqrpc_request_status(st);

        hqqrpc_call_handles_end(s, 1);
        send_call_error(s, status);
    }
    hqqrpc_scratch_release(s);
}

static void run_request(hqqrpc_session *s, uint16_t op, uint32_t id, const uint8_t *body, size_t blen,
                        int copy) {
    hqqrpc_call call;
    hqqrpc_status st;

    cs_reset(s, op, id);
    call.session = s;
    call.op = op;
    call.id = id;
    call.body = body;
    call.body_len = blen;
    call.priv = &s->cs;
    if (copy && blen != 0) {
        uint8_t *c = (uint8_t *)hqqrpc_scratch(s, blen);

        if (c == NULL) {
            finish_call(s, HQQRPC_E_INTERNAL);
            return;
        }
        memcpy(c, body, blen);
        call.body = c;
    }
    if (op == 0 || op > s->iface->op_count) {
        st = HQQRPC_E_UNKNOWN_OP;
    } else {
        st = s->iface->dispatch(&call);
    }
    finish_call(s, st);
}

/* Answers a request that was received completely but cannot be executed (assembly out of memory). */
static void answer_without_call(hqqrpc_session *s, uint16_t op, uint32_t id, hqqrpc_status st) {
    cs_reset(s, op, id);
    send_call_error(s, st);
    hqqrpc_scratch_release(s);
}

/* ---- Frame receive (4.4, 4.8, 4.11, 4.12) --------------------------------------------------- */

static int violation(hqqrpc_session *s, const char *why) {
    hqqrpc_log(s, HQQRPC_LOG_WARN, "protocol violation: %s", why);
    hqqrpc_session_set_end(s, HQQRPC_END_PROTOCOL);
    return 1;
}

static void rx_release(hqqrpc_session *s) {
    if (s->rx != NULL) {
        (void)hqq_read_release(s->rx);
        s->rx = NULL;
    }
}

static void asm_drop(hqqrpc_session *s) {
    if (s->asm_blk != NULL) {
        hqqrpc_blk_release(s, s->asm_blk);
        s->asm_blk = NULL;
    }
    s->asm_active = 0;
    s->asm_discard = 0;
}

/* HELLO (4.11). body is the 64-byte HELLO body. */
static int on_hello(hqqrpc_session *s, const uint8_t *body, uint32_t total) {
    uint8_t h[HQQRPC_HELLO_SIZE];
    uint32_t aux = HQQRPC_HELLO_OK;
    uint32_t req_mif = 0;
    uint32_t req_msg = 0;
    uint8_t minor = 0;

    /* The session name is removed as soon as a valid HELLO arrived: the client has attached. */
    if (s->link != NULL) {
        (void)hqq_link_unlink(s->link);
    }
    if (total != HQQRPC_HELLO_SIZE) {
        aux = HQQRPC_HELLO_BAD;
    } else {
        memcpy(h, body, sizeof h);
        req_mif = hqqrpc_le32(h + 20);
        req_msg = hqqrpc_le32(h + 24);
        if (memcmp(h, k_magic, 4) != 0 || hqqrpc_le32(h + 8) != 0 || hqqrpc_le32(h + 28) != 0) {
            aux = HQQRPC_HELLO_BAD;
        } else if (hqqrpc_le16(h + 4) != HQQRPC_PROTO_MAJOR) {
            aux = HQQRPC_HELLO_VERSION;
        } else if (memcmp(h + 32, s->iface->digest, sizeof s->iface->digest) != 0) {
            aux = HQQRPC_HELLO_DIGEST_MISMATCH;
        } else if (hqqrpc_le32(h + 12) != s->S || hqqrpc_le32(h + 16) != s->B) {
            aux = HQQRPC_HELLO_GEOMETRY;
        } else if ((req_msg != 0 && (req_msg < HQQRPC_MIN_MAX_MESSAGE || req_msg > HQQRPC_HARD_MAX_MESSAGE)) ||
                   req_mif > s->B) {
            aux = HQQRPC_HELLO_LIMIT;
        }
        minor = (uint8_t)min_u32(hqqrpc_le16(h + 6), HQQRPC_PROTO_MINOR);
    }
    if (s->srv != NULL) {
        hqqrpc_server_session_started(s->srv, s->ctl_id);
    }
    if (aux != HQQRPC_HELLO_OK) {
        send_hello_reply(s, aux, minor);
        hqqrpc_session_set_end(s, HQQRPC_END_HANDSHAKE);
        return 1;
    }
    {
        uint32_t cap_mif = s->cfg.max_in_flight != 0 ? min_u32(s->cfg.max_in_flight, s->B) : s->B;

        s->max_in_flight = min_u32(req_mif != 0 ? req_mif : s->B, cap_mif);
        s->max_message = min_u32(req_msg != 0 ? req_msg : s->cfg.max_message, s->cfg.max_message);
        s->proto_minor = minor;
    }
    send_hello_reply(s, HQQRPC_HELLO_OK, minor);
    if (s->end != HQQRPC_END_NONE) {
        return 1;
    }
    s->state = HQQRPC_ST_ACTIVE;
    s->last_req_ns = hqq_plat_mono_ns();
    return 0;
}

int hqqrpc_session_rx(hqqrpc_session *s, const void *data, size_t size) {
    uint8_t h[HDR];
    uint8_t kind;
    uint8_t flags;
    uint16_t op;
    uint32_t id;
    uint32_t total;
    uint32_t aux;
    uint64_t body_len;
    const uint8_t *body;

    if (s->end != HQQRPC_END_NONE || s->state == HQQRPC_ST_ENDED) {
        return 1;
    }
    if (size < HDR) {
        return violation(s, "frame shorter than the header");
    }
    /* Header copy (4.4 rule 8): the header is read once; the body length comes from the size of the
     * message, never from a second read of `total`. */
    memcpy(h, data, HDR);
    kind = h[0];
    flags = h[1];
    op = hqqrpc_le16(h + 2);
    id = hqqrpc_le32(h + 4);
    total = hqqrpc_le32(h + 8);
    aux = hqqrpc_le32(h + 12);
    body_len = (uint64_t)size - HDR;
    body = (const uint8_t *)data + HDR;

    if ((flags & (uint8_t)~(HQQRPC_F_DETAIL | HQQRPC_F_FAIL)) != 0 ||
        (kind != HQQRPC_K_RESPONSE && flags != 0) ||
        ((flags & HQQRPC_F_DETAIL) != 0 && (flags & HQQRPC_F_FAIL) == 0)) {
        return violation(s, "bad flags");
    }

    if (s->state == HQQRPC_ST_HELLO) {
        if (kind != HQQRPC_K_HELLO) {
            return violation(s, "frame before HELLO");
        }
        if (body_len != min_u32(total, s->chunk) || total > s->cfg.max_message || total > HQQRPC_HARD_MAX_MESSAGE) {
            return violation(s, "HELLO length");
        }
        return on_hello(s, body, total);
    }

    switch (kind) {
    case HQQRPC_K_REQUEST: {
        int single;

        if (s->asm_active) {
            return violation(s, "REQUEST inside a multi-frame message");
        }
        if (body_len != min_u32(total, s->chunk) || total > s->max_message || total > HQQRPC_HARD_MAX_MESSAGE) {
            return violation(s, "REQUEST length");
        }
        if (id != (s->last_id == 0xFFFFFFFFu ? 1u : s->last_id + 1u)) {
            return violation(s, "REQUEST id is not the successor of the last id");
        }
        s->last_id = id;
        s->last_req_ns = hqq_plat_mono_ns();
        single = total <= s->chunk;
        if (single) {
            run_request(s, op, id, body, (size_t)body_len, s->cfg.copy_request);
            return s->end != HQQRPC_END_NONE;
        }
        s->asm_blk = hqqrpc_blk_new(s, total);
        s->asm_discard = s->asm_blk == NULL;
        s->asm_active = 1;
        s->asm_op = op;
        s->asm_id = id;
        s->asm_total = total;
        s->asm_recv = (uint32_t)body_len;
        if (!s->asm_discard) {
            memcpy(s->asm_blk->p, body, (size_t)body_len);
        }
        rx_release(s);
        return 0;
    }
    case HQQRPC_K_CONT: {
        uint32_t need;

        if (!s->asm_active) {
            return violation(s, "CONT without a first frame");
        }
        if (id != s->asm_id || total != s->asm_total || aux != s->asm_recv) {
            return violation(s, "CONT id, total or offset");
        }
        need = s->asm_total - s->asm_recv;
        if (body_len != min_u32(need, s->chunk)) {
            return violation(s, "CONT length");
        }
        if (!s->asm_discard) {
            memcpy(s->asm_blk->p + s->asm_recv, body, (size_t)body_len);
        }
        s->asm_recv += (uint32_t)body_len;
        if (s->asm_recv < s->asm_total) {
            return 0;
        }
        rx_release(s);
        {
            hqqrpc_blk *blk = s->asm_blk;
            int discard = s->asm_discard;
            uint16_t aop = s->asm_op;
            uint32_t aid = s->asm_id;
            uint32_t atotal = s->asm_total;

            s->asm_blk = NULL;
            s->asm_active = 0;
            s->asm_discard = 0;
            if (discard) {
                answer_without_call(s, aop, aid, HQQRPC_E_INTERNAL);
            } else {
                run_request(s, aop, aid, blk->p, atotal, 0);
                hqqrpc_blk_release(s, blk);
            }
        }
        return s->end != HQQRPC_END_NONE;
    }
    case HQQRPC_K_CLOSE:
        if (s->asm_active) {
            return violation(s, "CLOSE inside a multi-frame message");
        }
        if (total != 0 || body_len != 0) {
            return violation(s, "CLOSE length");
        }
        hqqrpc_session_set_end(s, HQQRPC_END_CLOSE);
        return 1;
    default:
        return violation(s, "frame kind not allowed in this direction or state");
    }
}

int hqqrpc_session_rx_slot(hqqrpc_session *s, hqq_read_reservation *rr) {
    int ended;

    s->rx = rr;
    ended = hqqrpc_session_rx(s, rr->data, rr->size);
    rx_release(s);
    return ended;
}

/* ---- Session lifecycle ---------------------------------------------------------------------- */

hqqrpc_session *hqqrpc_session_create(const hqqrpc_iface *iface, const hqqrpc_cfg *cfg,
                                      const hqqrpc_server_options *callbacks, struct hqqrpc_server *srv,
                                      hqq_link *link, const hqq_allocator *alloc, uint32_t S, uint32_t B) {
    hqqrpc_session *s = (hqqrpc_session *)hqq_alloc(alloc, sizeof *s);

    if (s == NULL) {
        return NULL;
    }
    memset(s, 0, sizeof *s);
    s->srv = srv;
    s->iface = iface;
    s->cfg = *cfg;
    s->alloc = alloc;
    if (callbacks != NULL) {
        s->cb_user = callbacks->user;
        s->on_start = callbacks->on_session_start;
        s->on_end = callbacks->on_session_end;
        s->on_log = callbacks->on_log;
    }
    s->link = link;
    s->S = S;
    s->B = B;
    s->chunk = S - HDR;
    s->max_message = cfg->max_message;
    s->state = HQQRPC_ST_HELLO;
    s->start_ns = hqq_plat_mono_ns();
    s->last_req_ns = s->start_ns;
    return s;
}

void hqqrpc_session_start_callback(hqqrpc_session *s) {
    if (s->on_start != NULL && !s->started) {
        s->user = s->on_start(s->cb_user, s);
        s->started = 1;
    }
}

/* Teardown (5.2). Runs once. The final frame of a violation or a stop goes out first, because the
 * destroy functions of the library may take long; CLOSE_ACK follows the teardown. */
void hqqrpc_session_finish(hqqrpc_session *s) {
    if (s->torn) {
        return;
    }
    s->torn = 1;
    asm_drop(s);
    if (s->cs.held) {
        hqqrpc_tx_abort(s, &s->cs.res);
        s->cs.held = 0;
    }
    switch (s->end) {
    case HQQRPC_END_PROTOCOL:
        send_final_status(s, HQQRPC_E_PROTOCOL);
        break;
    case HQQRPC_END_STOP:
    case HQQRPC_END_IDLE:
        send_final_status(s, HQQRPC_E_SHUTTING_DOWN);
        break;
    default:
        break;
    }
    hqqrpc_scratch_release(s);
    hqqrpc_handles_destroy_all(s);
    if (s->started && s->on_end != NULL) {
        s->on_end(s->cb_user, s->user, s);
    }
    s->started = 0;
    if (s->end == HQQRPC_END_CLOSE) {
        send_close_ack(s);
    }
    s->state = HQQRPC_ST_ENDED;
}

void hqqrpc_session_destroy(hqqrpc_session *s) {
    hqq_allocator a;
    const hqq_allocator *ap;
    int own;

    if (s == NULL) {
        return;
    }
    hqqrpc_session_finish(s);
    if (s->link != NULL) {
        (void)hqq_close(s->link);
        s->link = NULL;
    }
    hqqrpc_blk_free(s, s->spare);
    hqqrpc_blk_free(s, s->t_blk);
    hqqrpc_tab_free(s);
    if (s->exec_mu_owned) {
        hqq_plat_mutex_destroy(s->alloc, s->exec_mu);
    }
    own = s->alloc == &s->alloc_copy;
    a = s->alloc_copy;
    ap = own ? &a : s->alloc;
    hqq_free(ap, s, sizeof *s);
}

/* ---- Test entry points (12.1, item 4) --------------------------------------------------------- */

hqqrpc_session *hqqrpc_test_session_new(const hqqrpc_iface *iface, const hqqrpc_server_options *opts) {
    hqqrpc_server_options def;
    hqqrpc_cfg cfg;
    hqqrpc_session *s;
    const hqq_allocator *user_alloc;

    if (hqqrpc_iface_check(iface) != HQQ_OK) {
        return NULL;
    }
    if (opts == NULL) {
        hqqrpc_server_options_init(&def);
        opts = &def;
    }
    if (hqqrpc_cfg_resolve(opts, &cfg) != HQQ_OK) {
        return NULL;
    }
    user_alloc = opts->link.allocator;
    s = hqqrpc_session_create(iface, &cfg, opts, NULL, NULL, user_alloc, cfg.buffer_size, cfg.buffer_count);
    if (s == NULL) {
        return NULL;
    }
    if (user_alloc != NULL) {
        s->alloc_copy = *user_alloc;
        s->alloc = &s->alloc_copy;
    }
    s->t_blk = hqqrpc_blk_new(s, 2u * (size_t)cfg.buffer_size);
    if (s->t_blk == NULL) {
        hqqrpc_session_destroy(s);
        return NULL;
    }
    s->t_rx = s->t_blk->p;
    s->t_tx = s->t_blk->p + cfg.buffer_size;
    if (iface->serialize == HQQRPC_SERIALIZE_SESSION) {
        s->exec_mu = hqq_plat_mutex_create(s->alloc);
        if (s->exec_mu == NULL) {
            hqqrpc_session_destroy(s);
            return NULL;
        }
        s->exec_mu_owned = 1;
    }
    hqqrpc_session_start_callback(s);
    return s;
}

void hqqrpc_test_session_free(hqqrpc_session *s) {
    if (s == NULL) {
        return;
    }
    hqqrpc_session_set_end(s, HQQRPC_END_PEER);
    hqqrpc_session_destroy(s);
}

hqqrpc_status hqqrpc_test_exchange(hqqrpc_session *s, const uint8_t *frame, size_t n, uint8_t *out,
                                   size_t cap, size_t *out_len) {
    int ended;
    hqqrpc_end why;

    *out_len = 0;
    if (s->link != NULL) {
        return HQQRPC_E_INTERNAL;
    }
    if (s->end != HQQRPC_END_NONE || s->state == HQQRPC_ST_ENDED) {
        return HQQRPC_E_SHUTTING_DOWN;
    }
    if (n > s->S) {
        return HQQRPC_E_LIMIT;
    }
    if (n != 0) {
        memcpy(s->t_rx, frame, n);
    }
    s->t_out = out;
    s->t_out_cap = cap;
    s->t_out_len = 0;
    s->t_out_overflow = 0;
    ended = hqqrpc_session_rx(s, s->t_rx, n);
    why = s->end;
    if (ended) {
        hqqrpc_session_finish(s);
    }
    *out_len = s->t_out_len;
    s->t_out = NULL;
    if (s->t_out_overflow) {
        return HQQRPC_E_LIMIT;
    }
    return ended && why == HQQRPC_END_PROTOCOL ? HQQRPC_E_PROTOCOL : HQQRPC_OK;
}

void hqqrpc_test_set_last_id(hqqrpc_session *s, uint32_t id) {
    s->last_id = id;
}

uint64_t hqqrpc_test_tombstones(const hqqrpc_session *s) {
    return s->t_tombstones;
}

uint64_t hqqrpc_test_frames(const hqqrpc_session *s) {
    return s->t_frames;
}

uint32_t hqqrpc_test_destroy_failures(const hqqrpc_session *s) {
    return s->destroy_failures;
}

uint32_t hqqrpc_test_live_handles(const hqqrpc_session *s) {
    return s->live_count;
}

void hqqrpc_test_call_begin(hqqrpc_session *s) {
    cs_reset(s, 0, 0);
}

void hqqrpc_test_call_end(hqqrpc_session *s, int ok) {
    hqqrpc_call_handles_end(s, !ok);
    hqqrpc_scratch_release(s);
}

int hqqrpc_test_ended(const hqqrpc_session *s) {
    return s->state == HQQRPC_ST_ENDED || s->end != HQQRPC_END_NONE;
}

const uint8_t *hqqrpc_test_rx_buffer(const hqqrpc_session *s) {
    return s->t_rx;
}

const uint8_t *hqqrpc_test_tx_buffer(const hqqrpc_session *s) {
    return s->t_tx;
}
