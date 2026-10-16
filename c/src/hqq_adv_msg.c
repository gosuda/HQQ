/*
 * hqq_adv_msg.c - the messaging API of the Advanced layer: sending chunked
 * messages (SPEC 10.4), receiving them, calls (SPEC 10.6, 1.1 sections 2, 3, 5),
 * Serve with its request workers and deadline timer (1.1 section 4), responses,
 * cancellation and graceful shutdown (1.1 sections 2, 6).
 */
#include "hqq_adv_internal.h"

/* ---- Ids ----------------------------------------------------------------------------- */

static uint64_t next_msg_id_locked(hqq_adv *a) {
    uint64_t id = a->next_msg_id;
    a->next_msg_id++;
    if (a->next_msg_id == 0) {
        a->next_msg_id = 1; /* a wrap skips 0 (SPEC 10.2) */
    }
    return id;
}

void hqq_call_options_init(hqq_call_options *opts) {
    if (opts != NULL) {
        memset(opts, 0, sizeof *opts);
        opts->struct_size = sizeof *opts;
    }
}

/* ---- Sending (SPEC 10.4) ------------------------------------------------------------ */

/* Abort frame of a message whose chunk failed after an earlier chunk was sent (best effort). */
static void send_abort(hqq_adv *a, uint64_t conn, uint64_t msg_id, uint32_t base) {
    adv_wait w;
    uint64_t ops[7];
    memset(&w, 0, sizeof w);
    w.has_deadline = 1;
    w.deadline_mono = hqq_plat_mono_ns() + ADV_ABORT_NS;
    memset(ops, 0, sizeof ops);
    ops[0] = conn;
    ops[1] = msg_id;
    ops[5] = ADV_ABORT | base;
    (void)adv_tx(a, &w, HQQ_OP_CONN_COPY, ops, NULL, 0, 0);
}

hqq_status adv_send_message(hqq_adv *a, uint64_t conn, uint64_t msg_id, uint32_t base, uint64_t aux,
                            const void *prefix, size_t prefix_len, const void *payload, size_t payload_len,
                            const adv_wait *w) {
    uint64_t total = (uint64_t)prefix_len + (uint64_t)payload_len;
    uint64_t off;
    uint64_t bs = a->buffer_size;
    hqq_status st;

    if (total > a->max_message) {
        return HQQ_ERR_BUFFER_OVERFLOW;
    }
    if (total == 0) {
        uint64_t ops[7];
        memset(ops, 0, sizeof ops);
        ops[0] = conn;
        ops[1] = msg_id;
        ops[5] = ADV_BEGIN | ADV_END | base;
        ops[6] = aux;
        return adv_tx(a, w, HQQ_OP_CONN_COPY, ops, NULL, 0, 1);
    }
    for (off = 0; off < total; off += bs) {
        uint64_t end = total - off > bs ? off + bs : total;
        uint64_t flags = base | (off == 0 ? ADV_BEGIN : 0) | (end == total ? ADV_END : 0);
        uint64_t ops[7];
        const void *p1 = NULL, *p2 = NULL;
        size_t n1 = 0, n2 = 0;

        if (off < prefix_len) {
            uint64_t e1 = end < prefix_len ? end : prefix_len;
            p1 = (const unsigned char *)prefix + off;
            n1 = (size_t)(e1 - off);
        }
        if (end > prefix_len) {
            uint64_t s2 = off > prefix_len ? off : prefix_len;
            p2 = (const unsigned char *)payload + (s2 - prefix_len);
            n2 = (size_t)(end - s2);
        }
        memset(ops, 0, sizeof ops);
        ops[0] = conn;
        ops[1] = msg_id;
        ops[2] = total;
        ops[3] = off;
        ops[4] = end - off;
        ops[5] = flags;
        ops[6] = off == 0 ? aux : 0;
        st = adv_tx_parts(a, w, HQQ_OP_CONN_COPY, ops, p1, n1, p2, n2, 1);
        if (st != HQQ_OK) {
            if (off > 0) {
                send_abort(a, conn, msg_id, base);
            }
            return st;
        }
    }
    return HQQ_OK;
}

/* ---- Stream close support ------------------------------------------------------------- */

void adv_cancel_stream_requests_locked(hqq_adv *a, uint64_t conn) {
    hqq_req_ctx *r;
    for (r = a->reqs; r != NULL; r = r->next) {
        if (r->conn == conn && r->end == ADV_REQ_LIVE) {
            r->end = ADV_REQ_STREAM_GONE;
        }
    }
    adv_wake_locked(a);
}

/* OpCancel of a request (1.1 section 2): cancels its context; unknown keys are ignored. */
void adv_cancel_request(hqq_adv *a, uint64_t conn, uint64_t msg_id) {
    hqq_req_ctx *r;
    hqq_spin_lock(&a->lock);
    for (r = a->reqs; r != NULL; r = r->next) {
        if (r->conn == conn && r->msg_id == msg_id) {
            if (r->end == ADV_REQ_LIVE) {
                r->end = ADV_REQ_CANCELED;
                a->st_canceled++;
                adv_wake_locked(a);
            }
            break;
        }
    }
    hqq_spin_unlock(&a->lock);
}

/* Removes a request from the in-flight registry (lock held). */
static void finish_request_locked(hqq_adv *a, hqq_req_ctx *c) {
    hqq_req_ctx **pp = &a->reqs;
    while (*pp != NULL && *pp != c) {
        pp = &(*pp)->next;
    }
    if (*pp == c) {
        *pp = c->next;
    }
    a->inflight--;
    c->req = NULL;
    c->pool_next = a->reqctx_free;
    a->reqctx_free = c;
    adv_wake_locked(a);
}

/* ---- Data messages ------------------------------------------------------------------- */

hqq_status hqq_stream_send(hqq_stream *s, const void *data, size_t len, const hqq_wait *in) {
    hqq_adv *a;
    adv_wait w;
    uint64_t mid;
    hqq_status st;

    if (s == NULL || (data == NULL && len != 0)) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    a = s->a;
    st = adv_enter(a);
    if (st != HQQ_OK) {
        return st;
    }
    st = adv_check_negotiated(a, HQQ_FEATURE_MESSAGES);
    if (st == HQQ_OK) {
        hqq_spin_lock(&a->lock);
        if (s->state == ADV_ST_CLOSED_LOCAL) {
            st = HQQ_ERR_CLOSED;
        } else if (s->state == ADV_ST_CLOSED_PEER) {
            st = HQQ_ERR_PEER_CLOSED;
        }
        mid = next_msg_id_locked(a);
        hqq_spin_unlock(&a->lock);
    }
    if (st == HQQ_OK && len > a->max_message) {
        st = HQQ_ERR_BUFFER_OVERFLOW;
    }
    if (st == HQQ_OK) {
        adv_wait_init(&w, in);
        st = adv_send_message(a, s->id, mid, 0, 0, NULL, 0, data, len, &w);
        if (st == HQQ_OK) {
            hqq_spin_lock(&a->lock);
            a->st_messages++;
            a->st_message_bytes += len;
            hqq_spin_unlock(&a->lock);
        }
    }
    adv_leave(a);
    return st;
}

hqq_status hqq_stream_recv(hqq_stream *s, hqq_msg **out, const hqq_wait *in) {
    hqq_adv *a;
    adv_wait w;
    hqq_status st = HQQ_OK;

    if (s == NULL || out == NULL) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    *out = NULL;
    a = s->a;
    st = adv_enter(a);
    if (st != HQQ_OK) {
        return st;
    }
    st = adv_check_negotiated(a, 0);
    if (st != HQQ_OK) {
        adv_leave(a);
        return st;
    }
    adv_wait_init(&w, in);
    hqq_spin_lock(&a->lock);
    for (;;) {
        if (!adv_queue_empty(&s->q)) {
            *out = (hqq_msg *)adv_queue_pop(&s->q);
            adv_wake_locked(a);
            st = HQQ_OK;
            break;
        }
        if (s->state == ADV_ST_CLOSED_LOCAL) {
            st = HQQ_ERR_CLOSED;
            break;
        }
        if (s->state == ADV_ST_CLOSED_PEER) {
            st = HQQ_ERR_PEER_CLOSED;
            break;
        }
        if (a->rx_failed) {
            st = a->rx_err;
            break;
        }
        st = adv_wait_check(a, &w);
        if (st != HQQ_OK) {
            break;
        }
        if (w.nowait) {
            st = HQQ_ERR_TIMEOUT;
            break;
        }
        adv_park_locked(a, adv_slice_ns(&w));
    }
    hqq_spin_unlock(&a->lock);
    adv_leave(a);
    return st;
}

void hqq_msg_release(hqq_msg *m) {
    if (m != NULL) {
        adv_msg_put(m);
    }
}

const void *hqq_msg_data(const hqq_msg *m) { return m == NULL ? NULL : m->data; }
size_t hqq_msg_len(const hqq_msg *m) { return m == NULL ? 0 : (size_t)m->len; }
uint64_t hqq_msg_stream_id(const hqq_msg *m) { return m == NULL ? 0 : m->conn; }
uint64_t hqq_msg_status(const hqq_msg *m) { return (m != NULL && (m->flags & ADV_ERROR) != 0) ? m->aux : 0; }

/* ---- Calls (SPEC 10.6; 1.1 sections 2, 3, 5) ------------------------------------------ */

/* Metadata block (1.1 section 3.2). Returns the block length (0: nothing to send). */
static size_t build_meta(unsigned char blk[42], const adv_wait *w, const hqq_call_options *opts, int *has_block) {
    size_t pos = 4;
    int caller = opts != NULL && opts->caller != 0;
    int trace = 0;
    uint64_t budget;
    unsigned i;

    *has_block = 0;
    if (opts != NULL) {
        for (i = 0; i < 16; i++) {
            if (opts->trace[i] != 0) {
                trace = 1;
            }
        }
    }
    if (!w->has_deadline && !caller && !trace) {
        return 0;
    }
    if (w->has_deadline) {
        uint64_t now = hqq_plat_mono_ns();
        budget = w->deadline_mono > now ? w->deadline_mono - now : 0;
        blk[pos] = 1;
        blk[pos + 1] = 8;
        for (i = 0; i < 8; i++) {
            blk[pos + 2 + i] = (unsigned char)(budget >> (8 * i));
        }
        pos += 10;
    }
    if (caller) {
        blk[pos] = 2;
        blk[pos + 1] = 8;
        for (i = 0; i < 8; i++) {
            blk[pos + 2 + i] = (unsigned char)(opts->caller >> (8 * i));
        }
        pos += 10;
    }
    if (trace) {
        blk[pos] = 3;
        blk[pos + 1] = 16;
        memcpy(blk + pos + 2, opts->trace, 16);
        pos += 18;
    }
    {
        uint64_t tlv = pos - 4;
        blk[0] = (unsigned char)(tlv & 0xFFu);
        blk[1] = (unsigned char)((tlv >> 8) & 0xFFu);
        blk[2] = 0;
        blk[3] = 0;
    }
    *has_block = 1;
    return pos;
}

hqq_status hqq_stream_call_with(hqq_stream *s, uint64_t method, const void *req, size_t len,
                                const hqq_call_options *opts, hqq_msg **resp, const hqq_wait *in) {
    hqq_adv *a;
    adv_wait w;
    adv_call c;
    unsigned char blk[42];
    size_t blen = 0;
    int has_block = 0;
    uint32_t extra = 0;
    uint64_t mid;
    hqq_status st;

    if (s == NULL || resp == NULL || (req == NULL && len != 0)) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    *resp = NULL;
    a = s->a;
    st = adv_enter(a);
    if (st != HQQ_OK) {
        return st;
    }
    st = adv_check_negotiated(a, HQQ_FEATURE_REQUEST_RESPONSE);
    if (st == HQQ_OK) {
        hqq_spin_lock(&a->lock);
        if (s->state == ADV_ST_CLOSED_LOCAL) {
            st = HQQ_ERR_CLOSED;
        } else if (s->state == ADV_ST_CLOSED_PEER) {
            st = HQQ_ERR_PEER_CLOSED;
        }
        hqq_spin_unlock(&a->lock);
    }
    if (st == HQQ_OK && len > a->max_message) {
        st = HQQ_ERR_BUFFER_OVERFLOW;
    }
    adv_wait_init(&w, in);
    if (st == HQQ_OK && w.nowait) {
        st = HQQ_ERR_TIMEOUT; /* a zero budget: nothing is sent */
    }
    if (st == HQQ_OK && w.has_deadline && hqq_plat_mono_ns() >= w.deadline_mono) {
        st = HQQ_ERR_TIMEOUT;
    }
    if (st == HQQ_OK && (a->features & HQQ_FEATURE_METADATA) != 0) {
        blen = build_meta(blk, &w, opts, &has_block);
        if (has_block) {
            extra = ADV_META;
        }
    }
    if (st != HQQ_OK) {
        adv_leave(a);
        return st;
    }

    hqq_spin_lock(&a->lock);
    mid = next_msg_id_locked(a);
    memset(&c, 0, sizeof c);
    c.conn = s->id;
    c.msg_id = mid;
    c.next = a->calls;
    a->calls = &c;
    hqq_spin_unlock(&a->lock);

    st = adv_send_message(a, s->id, mid, ADV_REQUEST | extra, method, blk, has_block ? blen : 0, req, len, &w);

    hqq_spin_lock(&a->lock);
    if (st != HQQ_OK) {
        adv_call **pp = &a->calls;
        while (*pp != NULL && *pp != &c) {
            pp = &(*pp)->next;
        }
        if (*pp == &c) {
            *pp = c.next;
        }
        hqq_spin_unlock(&a->lock);
        adv_leave(a);
        return st;
    }
    a->st_requests++;
    for (;;) {
        if (c.done) {
            break;
        }
        if (s->state == ADV_ST_CLOSED_LOCAL) {
            st = HQQ_ERR_CLOSED;
            break;
        }
        if (s->state == ADV_ST_CLOSED_PEER) {
            st = HQQ_ERR_PEER_CLOSED;
            break;
        }
        st = adv_wait_check(a, &w);
        if (st != HQQ_OK) {
            break;
        }
        adv_park_locked(a, adv_slice_ns(&w));
    }
    if (c.done) {
        st = c.status;
        *resp = c.resp;
        c.resp = NULL;
    } else {
        /* Abandoned: a late response is discarded (SPEC 10.6). A cancel is sent only when
         * the caller's own wait ended (1.1 section 5). */
        adv_abandon_locked(a, s->id, mid);
        if ((st == HQQ_ERR_TIMEOUT || st == HQQ_ERR_CANCELED) && (a->features & HQQ_FEATURE_CANCEL) != 0) {
            uint64_t ops[7];
            memset(ops, 0, sizeof ops);
            ops[0] = s->id;
            ops[1] = mid;
            adv_queue_control_locked(a, ADV_OP_CANCEL, ops, NULL);
        }
    }
    {
        adv_call **pp = &a->calls;
        while (*pp != NULL && *pp != &c) {
            pp = &(*pp)->next;
        }
        if (*pp == &c) {
            *pp = c.next;
        }
    }
    hqq_spin_unlock(&a->lock);
    if (st == HQQ_ERR_RESPONSE && *resp == NULL) {
        st = HQQ_ERR_PROTOCOL;
    }
    if (st != HQQ_OK && st != HQQ_ERR_RESPONSE && *resp != NULL) {
        adv_msg_put(*resp);
        *resp = NULL;
    }
    adv_leave(a);
    return st;
}

hqq_status hqq_stream_call(hqq_stream *s, uint64_t method, const void *req, size_t len, hqq_msg **resp,
                           const hqq_wait *in) {
    return hqq_stream_call_with(s, method, req, len, NULL, resp, in);
}

/* ---- Requests (handler side) -------------------------------------------------------- */

uint64_t hqq_request_method(const hqq_request *r) { return r == NULL ? 0 : r->msg->aux; }
const void *hqq_request_data(const hqq_request *r) { return r == NULL ? NULL : r->msg->data; }
size_t hqq_request_len(const hqq_request *r) { return r == NULL ? 0 : (size_t)r->msg->len; }
uint64_t hqq_request_stream_id(const hqq_request *r) { return r == NULL ? 0 : r->msg->conn; }
uint64_t hqq_request_caller(const hqq_request *r) {
    return (r == NULL || !r->msg->has_caller) ? 0 : r->msg->caller;
}

int hqq_request_trace(const hqq_request *r, uint8_t out[16]) {
    if (r == NULL || !r->msg->has_trace) {
        return 0;
    }
    memcpy(out, r->msg->trace, 16);
    return 1;
}

/* ---- Responses ---------------------------------------------------------------------- */

hqq_status hqq_response_write(hqq_response *w, const void *data, size_t len) {
    hqq_adv *a;
    uint64_t need;

    if (w == NULL || (data == NULL && len != 0)) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    a = w->a;
    if (len > a->max_message - (w->body_len < a->max_message ? w->body_len : a->max_message)) {
        return HQQ_ERR_BUFFER_OVERFLOW;
    }
    if (len == 0) {
        return HQQ_OK;
    }
    need = w->body_len + len;
    if (w->body == NULL || need > w->body->cap) {
        hqq_msg *nb = adv_msg_get(a->pool, need);
        if (nb == NULL) {
            return HQQ_ERR_NO_MEMORY;
        }
        if (w->body_len != 0) {
            memcpy(nb->data, w->body->data, (size_t)w->body_len);
        }
        if (w->body != NULL) {
            adv_msg_put(w->body);
        }
        w->body = nb;
    }
    memcpy(w->body->data + w->body_len, data, len);
    w->body_len = need;
    return HQQ_OK;
}

hqq_status hqq_response_error(hqq_response *w, uint64_t status, const void *data, size_t len) {
    hqq_status st;
    if (w == NULL) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    if (status == 0) {
        status = ADV_CODE_INVALID_OP;
    }
    w->failed = 1;
    w->status = status;
    w->body_len = 0;
    st = hqq_response_write(w, data, len);
    if (st == HQQ_ERR_BUFFER_OVERFLOW) {
        w->status = ADV_CODE_BUFFER_OVERFLOW;
        w->body_len = 0;
        st = HQQ_OK;
    }
    return st;
}


hqq_status hqq_req_ctx_err(const hqq_req_ctx *c) {
    hqq_adv *a = c->a;
    hqq_status st;
    hqq_spin_lock(&a->lock);
    switch (c->end) {
    case ADV_REQ_LIVE: st = HQQ_OK; break;
    case ADV_REQ_DEADLINE: st = HQQ_ERR_TIMEOUT; break;
    default: st = HQQ_ERR_CANCELED; break;
    }
    hqq_spin_unlock(&a->lock);
    return st;
}

hqq_status hqq_req_ctx_wait_done(hqq_req_ctx *c, int64_t timeout_ns) {
    hqq_adv *a = c->a;
    adv_wait w;
    hqq_wait hw;
    hqq_status st = HQQ_OK;

    hw.timeout_ns = timeout_ns;
    hw.cancel = NULL;
    adv_wait_init(&w, &hw);
    hqq_spin_lock(&a->lock);
    for (;;) {
        if (c->end != ADV_REQ_LIVE) {
            st = HQQ_OK;
            break;
        }
        if (w.nowait || (w.has_deadline && hqq_plat_mono_ns() >= w.deadline_mono)) {
            st = HQQ_ERR_TIMEOUT;
            break;
        }
        adv_park_locked(a, adv_slice_ns(&w));
    }
    hqq_spin_unlock(&a->lock);
    return st;
}

int hqq_req_ctx_deadline(const hqq_req_ctx *c, uint64_t *mono_ns) {
    hqq_adv *a = c->a;
    int has;
    hqq_spin_lock(&a->lock);
    has = c->has_deadline;
    if (has && mono_ns != NULL) {
        *mono_ns = c->deadline_mono;
    }
    hqq_spin_unlock(&a->lock);
    return has;
}

/* ---- Serve, workers, deadline timer -------------------------------------------------- */

/* Runs the handler of one request and sends its response (lock not held). */
static void run_request(hqq_adv *a, hqq_msg *m, hqq_req_ctx *c, hqq_handler_fn fn, void *user) {
    hqq_request req;
    hqq_response w;
    int skip;
    adv_wait nowait_wait;

    req.msg = m;
    memset(&w, 0, sizeof w);
    w.a = a;
    w.ctx = c;
    fn(user, c, &w, &req);

    hqq_spin_lock(&a->lock);
    a->workers_busy--; /* idle again before the response can reach the requester */
    skip = c->end == ADV_REQ_CANCELED || c->end == ADV_REQ_DEADLINE;
    if (c->end == ADV_REQ_LIVE) {
        c->end = ADV_REQ_HANDLED;
    }
    hqq_spin_unlock(&a->lock);

    if (!skip) {
        uint32_t base = ADV_RESPONSE | (w.failed ? ADV_ERROR : 0);
        uint64_t aux = w.failed ? w.status : 0;
        memset(&nowait_wait, 0, sizeof nowait_wait);
        (void)adv_send_message(a, m->conn, m->msg_id, base, aux, NULL, 0,
                               w.body != NULL ? w.body->data : NULL, (size_t)w.body_len, &nowait_wait);
    }
    if (w.body != NULL) {
        adv_msg_put(w.body);
    }
    hqq_spin_lock(&a->lock);
    m->rc = NULL;
    finish_request_locked(a, c);
    hqq_spin_unlock(&a->lock);
    adv_msg_put(m);
}

void adv_worker_main(void *arg) {
    hqq_adv *a = (hqq_adv *)arg;
    hqq_spin_lock(&a->lock);
    for (;;) {
        hqq_msg *m;
        hqq_req_ctx *c;
        hqq_handler_fn fn;
        void *user;

        while (adv_queue_empty(&a->reqq) && !a->serve_stop &&
               !atomic_load_explicit(&a->closing, memory_order_seq_cst)) {
            adv_park_locked(a, ADV_POLL_NS);
        }
        if (a->serve_stop || atomic_load_explicit(&a->closing, memory_order_seq_cst)) {
            break;
        }
        m = (hqq_msg *)adv_queue_pop(&a->reqq);
        c = m->rc;
        if (c->end != ADV_REQ_LIVE) {
            /* Canceled or expired before its handler started: no handler, no response (1.1 4.3). */
            m->rc = NULL;
            finish_request_locked(a, c);
            hqq_spin_unlock(&a->lock);
            adv_msg_put(m);
            hqq_spin_lock(&a->lock);
            continue;
        }
        fn = a->serve_fn;
        user = a->serve_user;
        a->workers_busy++;
        hqq_spin_unlock(&a->lock);
        run_request(a, m, c, fn, user);
        hqq_spin_lock(&a->lock);
    }
    hqq_spin_unlock(&a->lock);
}

void adv_timer_main(void *arg) {
    hqq_adv *a = (hqq_adv *)arg;
    hqq_spin_lock(&a->lock);
    while (!a->serve_stop && !atomic_load_explicit(&a->closing, memory_order_seq_cst)) {
        hqq_req_ctx *c;
        uint64_t now = hqq_plat_mono_ns();
        int changed = 0;
        for (c = a->reqs; c != NULL; c = c->next) {
            if (c->has_deadline && c->end == ADV_REQ_LIVE && now >= c->deadline_mono) {
                c->end = ADV_REQ_DEADLINE;
                changed = 1;
            }
        }
        if (changed) {
            adv_wake_locked(a);
        }
        adv_park_locked(a, ADV_TIMER_NS);
    }
    hqq_spin_unlock(&a->lock);
}

/* Starts one worker (lock held). */
static hqq_status spawn_worker_locked(hqq_adv *a) {
    const hqq_allocator *ap = adv_alloc_ptr(a);
    adv_worker *wk = (adv_worker *)hqq_alloc(ap, sizeof *wk);
    int os_err = 0;
    if (wk == NULL) {
        return HQQ_ERR_NO_MEMORY;
    }
    wk->th = hqq_plat_thread_start(ap, adv_worker_main, a, &os_err);
    if (wk->th == NULL) {
        hqq_free(ap, wk, sizeof *wk);
        return HQQ_ERR_SYSTEM;
    }
    wk->next = a->workers;
    a->workers = wk;
    a->workers_total++;
    return HQQ_OK;
}

hqq_status hqq_adv_serve(hqq_adv *a, hqq_handler_fn fn, void *user, const hqq_wait *in) {
    adv_wait w;
    hqq_status st;
    int os_err = 0;
    const hqq_allocator *ap;

    if (a == NULL || fn == NULL) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    st = adv_enter(a);
    if (st != HQQ_OK) {
        return st;
    }
    st = adv_check_negotiated(a, HQQ_FEATURE_REQUEST_RESPONSE);
    if (st != HQQ_OK) {
        adv_leave(a);
        return st;
    }
    adv_wait_init(&w, in);
    ap = adv_alloc_ptr(a);

    hqq_spin_lock(&a->lock);
    if (a->serving) {
        hqq_spin_unlock(&a->lock);
        adv_leave(a);
        return HQQ_ERR_INVALID_OP;
    }
    a->serving = 1;
    a->serve_stop = 0;
    a->serve_fn = fn;
    a->serve_user = user;
    a->timer_thread = hqq_plat_thread_start(ap, adv_timer_main, a, &os_err);
    if (a->timer_thread == NULL) {
        a->serving = 0;
        hqq_spin_unlock(&a->lock);
        adv_leave(a);
        return HQQ_ERR_SYSTEM;
    }
    st = spawn_worker_locked(a);
    if (st != HQQ_OK) {
        a->serve_stop = 1;
        a->serving = 0;
        adv_wake_locked(a);
        hqq_spin_unlock(&a->lock);
        hqq_plat_thread_join(ap, a->timer_thread);
        a->timer_thread = NULL;
        adv_leave(a);
        return st;
    }
    for (;;) {
        if (a->rx_failed) {
            st = a->rx_err;
            break;
        }
        if (a->shutting && a->inflight == 0 && adv_queue_empty(&a->reqq)) {
            st = HQQ_OK; /* graceful shutdown completed */
            break;
        }
        st = adv_wait_check(a, &w);
        if (st != HQQ_OK) {
            break;
        }
        if (w.nowait) {
            st = HQQ_ERR_TIMEOUT;
            break;
        }
        if (!adv_queue_empty(&a->reqq) && a->workers_busy >= a->workers_total && a->workers_total < ADV_MAX_WORKERS) {
            hqq_status sp = spawn_worker_locked(a);
            if (sp != HQQ_OK) {
                st = sp;
                break;
            }
        }
        adv_park_locked(a, adv_slice_ns(&w));
    }

    /* Serve ends: every request context ends, the workers and the timer stop. */
    {
        hqq_req_ctx *r;
        for (r = a->reqs; r != NULL; r = r->next) {
            if (r->end == ADV_REQ_LIVE) {
                r->end = ADV_REQ_SERVE_END;
            }
        }
    }
    a->serve_stop = 1;
    a->serving = 0;
    adv_wake_locked(a);
    {
        adv_worker *wk = a->workers;
        hqq_plat_thread *tt = a->timer_thread;
        a->workers = NULL;
        a->workers_total = 0;
        a->timer_thread = NULL;
        hqq_spin_unlock(&a->lock);
        if (tt != NULL) {
            hqq_plat_thread_join(ap, tt);
        }
        while (wk != NULL) {
            adv_worker *next = wk->next;
            if (wk->th != NULL) {
                hqq_plat_thread_join(ap, wk->th);
            }
            hqq_free(ap, wk, sizeof *wk);
            wk = next;
        }
    }
    adv_leave(a);
    return st;
}

/* ---- Shutdown (1.1 section 6) --------------------------------------------------------- */

hqq_status hqq_adv_shutdown(hqq_adv *a, const hqq_wait *in) {
    adv_wait w;
    hqq_status st;

    if (a == NULL) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    st = adv_enter(a);
    if (st != HQQ_OK) {
        return st;
    }
    adv_wait_init(&w, in);
    hqq_spin_lock(&a->lock);
    a->shutting = 1;
    adv_wake_locked(a);
    for (;;) {
        if (a->inflight == 0) {
            st = HQQ_OK;
            break;
        }
        st = adv_wait_check(a, &w);
        if (st != HQQ_OK) {
            break;
        }
        if (w.nowait) {
            st = HQQ_ERR_TIMEOUT;
            break;
        }
        adv_park_locked(a, adv_slice_ns(&w));
    }
    hqq_spin_unlock(&a->lock);
    adv_leave(a);
    return st;
}
