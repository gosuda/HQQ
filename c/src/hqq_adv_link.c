/*
 * hqq_adv_link.c - the Advanced link: open and close, negotiation (SPEC 10.1,
 * 1.1 section 1), the receive dispatcher and the control sender thread.
 */
#include <limits.h>

#include "hqq_adv_internal.h"

/* ---- Users (blocking API calls in progress) ----------------------------------------- */

/* Enters an API call: HQQ_ERR_CLOSED once the link is closing. */
hqq_status adv_enter(hqq_adv *a) {
    atomic_fetch_add_explicit(&a->users, 1, memory_order_seq_cst);
    if (atomic_load_explicit(&a->closing, memory_order_seq_cst)) {
        atomic_fetch_sub_explicit(&a->users, 1, memory_order_seq_cst);
        return HQQ_ERR_CLOSED;
    }
    return HQQ_OK;
}

void adv_leave(hqq_adv *a) { atomic_fetch_sub_explicit(&a->users, 1, memory_order_seq_cst); }

/* ---- Options and statistics ----------------------------------------------------------- */

void hqq_adv_options_init(hqq_adv_options *o) {
    if (o == NULL) {
        return;
    }
    memset(o, 0, sizeof *o);
    o->struct_size = sizeof *o;
    hqq_options_init(&o->link);
}

void hqq_adv_stats_get(const hqq_adv *l, hqq_adv_stats *out) {
    hqq_adv *a = (hqq_adv *)l;
    if (a == NULL || out == NULL) {
        return;
    }
    memset(out, 0, sizeof *out);
    hqq_spin_lock(&a->lock);
    out->negotiation_ns = a->neg_ns;
    out->messages = a->st_messages;
    out->message_bytes = a->st_message_bytes;
    out->requests = a->st_requests;
    out->responses = a->st_responses;
    out->dropped_messages = a->st_dropped;
    out->abandoned_responses = a->st_abandoned;
    out->canceled_requests = a->st_canceled;
    out->refused_requests = a->st_refused;
    hqq_spin_unlock(&a->lock);
}

/* ---- Open -------------------------------------------------------------------------------- */

static hqq_options link_options(const hqq_adv_options *opts) {
    hqq_options lo;
    hqq_options_init(&lo);
    if (opts != NULL && opts->link.struct_size == sizeof(hqq_options)) {
        lo = opts->link;
    }
    return lo;
}

static void free_link_state(hqq_adv *a);

/* Builds the Advanced state on top of an open Standard link. Takes ownership of
 * base on success; on failure the caller closes base. */
hqq_status adv_wrap_base(hqq_link *base, const hqq_adv_options *opts, hqq_adv **out) {
    hqq_adv *a;
    hqq_allocator al;
    hqq_status st;
    uint64_t bc = hqq_link_buffer_count(base);
    uint64_t qs;
    adv_pool *pool;

    al = base->alloc;
    a = (hqq_adv *)hqq_alloc(base->alloc_set ? &al : NULL, sizeof *a);
    if (a == NULL) {
        return HQQ_ERR_NO_MEMORY;
    }
    memset(a, 0, sizeof *a);
    a->alloc = al;
    a->alloc_set = base->alloc_set;
    a->base = base;
    a->buffer_size = hqq_link_buffer_size(base);
    a->max_message = HQQ_ADV_DEFAULT_MAX_MESSAGE_SIZE;
    if (opts != NULL && opts->max_message_size != 0) {
        a->max_message = opts->max_message_size;
    }
    if (a->max_message > (uint64_t)INT_MAX) {
        a->max_message = (uint64_t)INT_MAX;
    }
    a->max_reassembly = a->max_message > UINT64_MAX / 4 ? UINT64_MAX : 4 * a->max_message;
    if (opts != NULL && opts->max_reassembly_bytes != 0) {
        a->max_reassembly = opts->max_reassembly_bytes;
    }
    qs = (opts != NULL && opts->queue_size != 0) ? opts->queue_size : bc;
    if (qs < ADV_MIN_QUEUE) {
        qs = ADV_MIN_QUEUE;
    }
    a->queue_size = qs;
    a->overflow = opts != NULL ? opts->overflow : HQQ_OVERFLOW_BLOCK;
    a->control_cap = bc > ADV_CONTROL_MIN ? bc : ADV_CONTROL_MIN;
    hqq_cancel_init(&a->stop);
    atomic_store_explicit(&a->closing, 0, memory_order_seq_cst);
    atomic_store_explicit(&a->users, 0, memory_order_seq_cst);
    a->next_msg_id = 1;
    a->next_stream_n = 0;
    a->neg = ADV_NEG_NONE;

    /* Pool, owned separately so that messages may outlive the link. */
    pool = (adv_pool *)hqq_alloc(base->alloc_set ? &al : NULL, sizeof *pool);
    if (pool == NULL) {
        hqq_free(base->alloc_set ? &al : NULL, a, sizeof *a);
        return HQQ_ERR_NO_MEMORY;
    }
    memset(pool, 0, sizeof *pool);
    pool->alloc = al;
    pool->alloc_set = base->alloc_set;
    pool->refs = 1; /* the owner */
    a->pool = pool;

    st = adv_queue_init(a->alloc_set ? &a->alloc : NULL, &a->acceptq, a->queue_size);
    if (st == HQQ_OK) {
        st = adv_queue_init(a->alloc_set ? &a->alloc : NULL, &a->reqq, a->queue_size);
    }
    a->abandoned = (adv_key *)hqq_alloc(a->alloc_set ? &a->alloc : NULL, ADV_ABANDONED_CAP * sizeof(adv_key));
    a->refused = (adv_key *)hqq_alloc(a->alloc_set ? &a->alloc : NULL, ADV_REFUSED_CAP * sizeof(adv_key));
    a->ctl = (adv_ctl_job *)hqq_alloc(a->alloc_set ? &a->alloc : NULL, (size_t)a->control_cap * sizeof(adv_ctl_job));
    if (st != HQQ_OK || a->abandoned == NULL || a->refused == NULL || a->ctl == NULL) {
        if (st == HQQ_OK) {
            st = HQQ_ERR_NO_MEMORY;
        }
        free_link_state(a);
        return st;
    }
    /* Stream 0: the link-level stream, always open and registered. */
    a->stream0 = NULL;
    {
        hqq_stream *s = NULL;
        st = hqq_stream_new_internal(a, 0, &s);
        if (st != HQQ_OK) {
            free_link_state(a);
            return st;
        }
        a->stream0 = s;
        st = adv_stream_register(a, s);
        if (st != HQQ_OK) {
            free_link_state(a);
            return st;
        }
    }
    *out = a;
    return HQQ_OK;
}

/* Frees everything of the link state (the link is not closed here). */
static void free_link_state(hqq_adv *a) {
    hqq_allocator al = a->alloc;
    int set = a->alloc_set;
    hqq_allocator *ap = set ? &al : NULL;
    hqq_stream *s;
    adv_waiter *w;
    adv_asm *x;
    hqq_req_ctx *c;
    adv_worker *wk;

    for (s = a->all_streams; s != NULL;) {
        hqq_stream *next = s->next_all;
        while (!adv_queue_empty(&s->q)) {
            adv_msg_put((hqq_msg *)adv_queue_pop(&s->q));
        }
        adv_queue_free(ap, &s->q);
        hqq_free(ap, s, sizeof *s);
        s = next;
    }
    a->all_streams = NULL;
    while (!adv_queue_empty(&a->reqq)) {
        hqq_msg *m = (hqq_msg *)adv_queue_pop(&a->reqq);
        adv_msg_put(m);
    }
    adv_queue_free(ap, &a->reqq);
    adv_queue_free(ap, &a->acceptq);
    for (c = a->reqs; c != NULL;) {
        hqq_req_ctx *next = c->next;
        if (c->req != NULL) {
            adv_msg_put(c->req);
        }
        hqq_free(ap, c, sizeof *c);
        c = next;
    }
    a->reqs = NULL;
    for (c = a->reqctx_free; c != NULL;) {
        hqq_req_ctx *next = c->pool_next;
        hqq_free(ap, c, sizeof *c);
        c = next;
    }
    a->reqctx_free = NULL;
    for (x = a->asms; x != NULL;) {
        adv_asm *next = x->next;
        if (x->msg != NULL) {
            adv_msg_put(x->msg);
        }
        hqq_free(ap, x, sizeof *x);
        x = next;
    }
    a->asms = NULL;
    for (x = a->asm_free; x != NULL;) {
        adv_asm *next = x->next;
        hqq_free(ap, x, sizeof *x);
        x = next;
    }
    a->asm_free = NULL;
    for (w = a->waiter_free; w != NULL;) {
        adv_waiter *next = w->next;
        hqq_plat_event_destroy(ap, w->ev);
        hqq_free(ap, w, sizeof *w);
        w = next;
    }
    a->waiter_free = NULL;
    for (wk = a->workers_free; wk != NULL;) {
        adv_worker *next = wk->next;
        hqq_free(ap, wk, sizeof *wk);
        wk = next;
    }
    a->workers_free = NULL;
    if (a->reg != NULL) {
        hqq_free(ap, a->reg, (size_t)a->capreg * sizeof(hqq_stream *));
        a->reg = NULL;
    }
    if (a->abandoned != NULL) {
        hqq_free(ap, a->abandoned, ADV_ABANDONED_CAP * sizeof(adv_key));
        a->abandoned = NULL;
    }
    if (a->refused != NULL) {
        hqq_free(ap, a->refused, ADV_REFUSED_CAP * sizeof(adv_key));
        a->refused = NULL;
    }
    if (a->ctl != NULL) {
        hqq_free(ap, a->ctl, (size_t)a->control_cap * sizeof(adv_ctl_job));
        a->ctl = NULL;
    }
}

hqq_status hqq_adv_create(const char *name, uint64_t buffer_count, uint64_t buffer_size, unsigned perm,
                          const hqq_adv_options *opts, hqq_adv **out, hqq_open_info *info) {
    hqq_options lo = link_options(opts);
    hqq_link *base = NULL;
    hqq_status st;

    if (out == NULL) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    *out = NULL;
    st = hqq_standard_create(name, buffer_count, buffer_size, perm, &lo, &base, info);
    if (st != HQQ_OK) {
        return st;
    }
    st = adv_wrap_base(base, opts, out);
    if (st != HQQ_OK) {
        (void)hqq_close(base);
        return st;
    }
    return HQQ_OK;
}

hqq_status hqq_adv_open(const char *name, uint64_t buffer_count, uint64_t buffer_size,
                        const hqq_adv_options *opts, hqq_adv **out, hqq_open_info *info) {
    hqq_options lo = link_options(opts);
    hqq_link *base = NULL;
    hqq_status st;

    if (out == NULL) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    *out = NULL;
    st = hqq_standard_open(name, buffer_count, buffer_size, &lo, &base, info);
    if (st != HQQ_OK) {
        return st;
    }
    st = adv_wrap_base(base, opts, out);
    if (st != HQQ_OK) {
        (void)hqq_close(base);
        return st;
    }
    return HQQ_OK;
}

hqq_status hqq_adv_open_memory(void *mem, size_t mem_size, uint64_t buffer_count, uint64_t buffer_size,
                               const hqq_adv_options *opts, hqq_adv **out, hqq_open_info *info) {
    hqq_options lo = link_options(opts);
    hqq_link *base = NULL;
    hqq_status st;

    if (out == NULL) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    *out = NULL;
    st = hqq_standard_open_memory(mem, mem_size, buffer_count, buffer_size, &lo, &base, info);
    if (st != HQQ_OK) {
        return st;
    }
    st = adv_wrap_base(base, opts, out);
    if (st != HQQ_OK) {
        (void)hqq_close(base);
        return st;
    }
    return HQQ_OK;
}

hqq_link *hqq_adv_base(hqq_adv *a) { return a == NULL ? NULL : a->base; }
hqq_mode hqq_adv_mode(const hqq_adv *a) { return hqq_link_mode(a->base); }
hqq_peer_status hqq_adv_peer_state(hqq_adv *a) { return hqq_peer_state(a->base); }
size_t hqq_adv_buffer_size(const hqq_adv *a) { return hqq_link_buffer_size(a->base); }
size_t hqq_adv_buffer_count(const hqq_adv *a) { return hqq_link_buffer_count(a->base); }

hqq_status hqq_adv_unlink(hqq_adv *a) {
    if (a == NULL) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    return hqq_link_unlink(a->base);
}

/* ---- Close ------------------------------------------------------------------------------- */

hqq_status hqq_adv_close(hqq_adv *a) {
    hqq_allocator al;
    int set;
    hqq_link *base;
    hqq_status st;
    adv_worker *wk;
    adv_call *c;

    if (a == NULL) {
        return HQQ_OK;
    }
    hqq_spin_lock(&a->lock);
    if (atomic_load_explicit(&a->closing, memory_order_seq_cst)) {
        hqq_spin_unlock(&a->lock);
        return HQQ_ERR_CLOSED;
    }
    atomic_store_explicit(&a->closing, 1, memory_order_seq_cst);
    /* Every request context ends: handlers see their context canceled. */
    {
        hqq_req_ctx *r;
        for (r = a->reqs; r != NULL; r = r->next) {
            if (r->end == ADV_REQ_LIVE) {
                r->end = ADV_REQ_CANCELED;
            }
        }
    }
    for (c = a->calls; c != NULL; c = c->next) {
        if (!c->done) {
            c->done = 1;
            c->status = HQQ_ERR_CLOSED;
        }
    }
    a->serve_stop = 1;
    adv_wake_locked(a);
    hqq_spin_unlock(&a->lock);
    hqq_cancel_signal(&a->stop);

    /* Blocking API calls leave within one poll slice. */
    while (atomic_load_explicit(&a->users, memory_order_seq_cst) != 0) {
        hqq_plat_sleep_ns(1000000u);
        hqq_spin_lock(&a->lock);
        adv_wake_locked(a);
        hqq_spin_unlock(&a->lock);
    }
    if (a->rx_thread != NULL) {
        hqq_plat_thread_join(a->alloc_set ? &a->alloc : NULL, a->rx_thread);
        a->rx_thread = NULL;
    }
    if (a->ctl_thread != NULL) {
        hqq_plat_thread_join(a->alloc_set ? &a->alloc : NULL, a->ctl_thread);
        a->ctl_thread = NULL;
    }
    if (a->timer_thread != NULL) {
        hqq_plat_thread_join(a->alloc_set ? &a->alloc : NULL, a->timer_thread);
        a->timer_thread = NULL;
    }
    hqq_spin_lock(&a->lock);
    wk = a->workers;
    a->workers = NULL;
    hqq_spin_unlock(&a->lock);
    while (wk != NULL) {
        adv_worker *next = wk->next;
        if (wk->th != NULL) {
            hqq_plat_thread_join(a->alloc_set ? &a->alloc : NULL, wk->th);
        }
        hqq_free(a->alloc_set ? &a->alloc : NULL, wk, sizeof *wk);
        wk = next;
    }

    /* Every thread is gone. Release the queued messages and the state first: they
     * hold pool references. Then close the link and drop the owner's pool reference. */
    al = a->alloc;
    set = a->alloc_set;
    base = a->base;
    free_link_state(a);
    st = hqq_close(base);
    a->base = NULL;
    pool_drain_owner(a->pool);
    a->pool = NULL;
    hqq_free(set ? &al : NULL, a, sizeof *a);
    return st;
}

/* ---- Negotiation (SPEC 10.1, 1.1 section 1) ------------------------------------------- */

/* Sends OpError(0, ErrCodeInvalidOp) and fails: a request of another major version. */
static hqq_status answer_negotiation(hqq_adv *a, const adv_wait *w, uint64_t requested, uint64_t req_features,
                                     uint64_t allowed, uint16_t *version, uint64_t *features) {
    uint64_t ops[7];
    uint64_t major = requested >> 8;
    uint64_t minor = requested & 0xFFu;
    uint64_t f;
    hqq_status st;

    if (major != ADV_MAJOR) {
        memset(ops, 0, sizeof ops);
        ops[1] = ADV_CODE_INVALID_OP;
        (void)adv_tx(a, w, HQQ_OP_ERROR, ops, NULL, 0, 0);
        return HQQ_ERR_NEGOTIATION_REJECTED;
    }
    if (minor > ADV_MINOR) {
        minor = ADV_MINOR;
    }
    f = req_features & ADV_FEATURES_SUPPORTED & allowed;
    if (minor < 1) {
        f &= ~(uint64_t)(HQQ_FEATURE_CANCEL | HQQ_FEATURE_METADATA);
    }
    memset(ops, 0, sizeof ops);
    ops[0] = ((uint64_t)ADV_MAJOR << 8) | minor;
    ops[1] = f;
    st = adv_tx(a, w, HQQ_OP_PROTO_ACK, ops, NULL, 0, 0);
    if (st != HQQ_OK) {
        return st;
    }
    *version = (uint16_t)(((uint64_t)ADV_MAJOR << 8) | minor);
    *features = f;
    return HQQ_OK;
}

/* One negotiation attempt. `initiate`: send our request (NegotiateProtocol); otherwise
 * only wait for the peer's request (WaitForNegotiation). */
static hqq_status negotiate_run(hqq_adv *a, int initiate, uint16_t version, uint64_t features,
                                const adv_wait *w, uint16_t *out_version, uint64_t *out_features) {
    int primary = hqq_link_mode(a->base) == HQQ_MODE_PRIMARY;
    hqq_status st;

    if (initiate) {
        uint64_t ops[7];
        memset(ops, 0, sizeof ops);
        ops[0] = version;
        ops[1] = features;
        st = adv_tx(a, w, HQQ_OP_PROTO_NEGOTIATE, ops, NULL, 0, 0);
        if (st != HQQ_OK) {
            return st;
        }
    }
    for (;;) {
        hqq_packet pkt;
        uint64_t pos;
        unsigned op;

        st = adv_rx(a, w, &pkt, &pos);
        if (st != HQQ_OK) {
            return st;
        }
        op = hqq_packet_op(&pkt);
        if (op == HQQ_OP_PROTO_ACK && initiate) {
            uint64_t av = hqq_packet_operand(&pkt, 0);
            uint64_t af = hqq_packet_operand(&pkt, 1);
            hqq_link_rx_release(a->base, pos);
            if ((av >> 8) != ADV_MAJOR || (av & 0xFFu) > ADV_MINOR || (af & ~(features & ADV_FEATURES_SUPPORTED)) != 0) {
                return HQQ_ERR_NEGOTIATION_REJECTED;
            }
            *out_version = (uint16_t)av;
            *out_features = af;
            return HQQ_OK;
        }
        if (op == HQQ_OP_PROTO_NEGOTIATE) {
            uint64_t rv = hqq_packet_operand(&pkt, 0);
            uint64_t rf = hqq_packet_operand(&pkt, 1);
            hqq_link_rx_release(a->base, pos);
            if (initiate && primary) {
                continue; /* the Primary ignores the Secondary's simultaneous request */
            }
            return answer_negotiation(a, w, rv, rf, features, out_version, out_features);
        }
        hqq_link_rx_release(a->base, pos);
        if (op == HQQ_OP_ERROR) {
            return HQQ_ERR_NEGOTIATION_REJECTED;
        }
        return HQQ_ERR_UNEXPECTED_PACKET;
    }
}

static hqq_status negotiate_attempt(hqq_adv *a, int initiate, uint16_t version, uint64_t features,
                                    const hqq_wait *in) {
    adv_wait w;
    uint16_t nv = 0;
    uint64_t nf = 0;
    uint64_t t0;
    hqq_status st;
    int start_threads = 0;

    adv_wait_init(&w, in);
    hqq_spin_lock(&a->lock);
    if (a->neg == ADV_NEG_DONE || a->neg == ADV_NEG_NEGOTIATING) {
        hqq_spin_unlock(&a->lock);
        return HQQ_ERR_ALREADY_NEGOTIATED;
    }
    if (a->neg == ADV_NEG_FAILED) {
        hqq_spin_unlock(&a->lock);
        return HQQ_ERR_NEGOTIATION_FAILED;
    }
    a->neg = ADV_NEG_NEGOTIATING;
    hqq_spin_unlock(&a->lock);

    t0 = adv_mono();
    st = negotiate_run(a, initiate, version, features, &w, &nv, &nf);
    if (st == HQQ_OK) {
        hqq_spin_lock(&a->lock);
        a->neg = ADV_NEG_DONE;
        a->version = nv;
        a->features = nf;
        a->neg_ns = adv_mono() - t0;
        start_threads = !a->dispatch_started;
        a->dispatch_started = 1;
        hqq_spin_unlock(&a->lock);
    } else {
        hqq_spin_lock(&a->lock);
        a->neg = ADV_NEG_FAILED;
        hqq_spin_unlock(&a->lock);
        return st;
    }
    if (start_threads) {
        int os_err = 0;
        hqq_allocator al = a->alloc;
        const hqq_allocator *ap = a->alloc_set ? &al : NULL;
        a->rx_thread = hqq_plat_thread_start(ap, adv_dispatch_main, a, &os_err);
        if (a->rx_thread == NULL) {
            hqq_spin_lock(&a->lock);
            a->neg = ADV_NEG_FAILED;
            hqq_spin_unlock(&a->lock);
            return HQQ_ERR_SYSTEM;
        }
        a->ctl_thread = hqq_plat_thread_start(ap, adv_control_main, a, &os_err);
        if (a->ctl_thread == NULL) {
            hqq_cancel_signal(&a->stop);
            hqq_plat_thread_join(ap, a->rx_thread);
            a->rx_thread = NULL;
            hqq_spin_lock(&a->lock);
            a->neg = ADV_NEG_FAILED;
            hqq_spin_unlock(&a->lock);
            return HQQ_ERR_SYSTEM;
        }
    }
    return HQQ_OK;
}

hqq_status hqq_adv_negotiate_protocol(hqq_adv *a, uint16_t version, uint64_t features, const hqq_wait *wait) {
    hqq_status st;
    if (a == NULL) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    st = adv_enter(a);
    if (st != HQQ_OK) {
        return st;
    }
    st = negotiate_attempt(a, 1, version, features, wait);
    adv_leave(a);
    return st;
}

hqq_status hqq_adv_negotiate(hqq_adv *a, uint64_t features, const hqq_wait *wait) {
    hqq_status st;
    uint64_t f;
    if (a == NULL) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    f = features == 0 ? ADV_FEATURES_SUPPORTED : features & ADV_FEATURES_SUPPORTED;
    st = adv_enter(a);
    if (st != HQQ_OK) {
        return st;
    }
    if (hqq_link_mode(a->base) == HQQ_MODE_PRIMARY) {
        st = negotiate_attempt(a, 1, HQQ_PROTOCOL_1_1, f, wait);
    } else {
        st = negotiate_attempt(a, 0, HQQ_PROTOCOL_1_1, f, wait);
    }
    adv_leave(a);
    return st;
}

hqq_proto_state hqq_adv_protocol_state(hqq_adv *a) {
    hqq_proto_state s;
    hqq_spin_lock(&a->lock);
    switch (a->neg) {
    case ADV_NEG_NEGOTIATING: s = HQQ_PROTO_NEGOTIATING; break;
    case ADV_NEG_DONE: s = HQQ_PROTO_NEGOTIATED; break;
    case ADV_NEG_FAILED: s = HQQ_PROTO_FAILED; break;
    default: s = HQQ_PROTO_NONE; break;
    }
    hqq_spin_unlock(&a->lock);
    return s;
}

uint16_t hqq_adv_negotiated_version(const hqq_adv *l) {
    hqq_adv *a = (hqq_adv *)l;
    uint16_t v;
    hqq_spin_lock(&a->lock);
    v = a->neg == ADV_NEG_DONE ? a->version : 0;
    hqq_spin_unlock(&a->lock);
    return v;
}

uint64_t hqq_adv_negotiated_features(const hqq_adv *l) {
    hqq_adv *a = (hqq_adv *)l;
    uint64_t f;
    hqq_spin_lock(&a->lock);
    f = a->neg == ADV_NEG_DONE ? a->features : 0;
    hqq_spin_unlock(&a->lock);
    return f;
}

/* Requires the negotiated feature set to contain `feature` (0: just negotiated). */
hqq_status adv_check_negotiated(hqq_adv *a, uint64_t feature) {
    hqq_status st = HQQ_OK;
    hqq_spin_lock(&a->lock);
    if (a->neg != ADV_NEG_DONE || (feature != 0 && (a->features & feature) == 0)) {
        st = HQQ_ERR_NOT_NEGOTIATED;
    }
    hqq_spin_unlock(&a->lock);
    return st;
}

/* ---- Receive dispatcher ------------------------------------------------------------------ */

static hqq_status dispatch_one(hqq_adv *a, hqq_packet *pkt, uint64_t pos) {
    unsigned op = hqq_packet_op(pkt);

    if (op == HQQ_OP_CONN_COPY) {
        adv_copy_out out;
        hqq_status st;
        const unsigned char *payload = hqq_link_rx_payload(a->base, pos);

        st = adv_handle_copy(a, pkt, payload, &out); /* copies the payload out of the slot */
        hqq_link_rx_release(a->base, pos);
        if (st != HQQ_OK) {
            return st;
        }
        if (out.refused_request) {
            hqq_spin_lock(&a->lock);
            adv_refuse_request_locked(a, out.refused_conn, out.refused_msg, ADV_CODE_BUFFER_OVERFLOW);
            a->st_dropped++;
            hqq_spin_unlock(&a->lock);
        }
        if (out.refused_response) {
            adv_call *c;
            hqq_spin_lock(&a->lock);
            for (c = a->calls; c != NULL; c = c->next) {
                if (c->conn == out.refused_conn && c->msg_id == out.refused_msg && !c->done) {
                    c->done = 1;
                    c->status = HQQ_ERR_REASSEMBLY_LIMIT;
                    adv_wake_locked(a);
                    break;
                }
            }
            a->st_dropped++; /* every refusal counts (AdvancedStats.DroppedMessages) */
            hqq_spin_unlock(&a->lock);
        }
        if (out.complete != NULL) {
            return adv_route(a, out.complete);
        }
        return HQQ_OK;
    }
    if (op == HQQ_OP_CONN_CREATE || op == HQQ_OP_CONN_ACCEPT || op == HQQ_OP_CONN_CLOSE || op == HQQ_OP_ERROR) {
        hqq_packet local = *pkt;
        hqq_link_rx_release(a->base, pos);
        return adv_handle_control(a, &local);
    }
    if (op == ADV_OP_CANCEL) {
        hqq_packet local = *pkt;
        hqq_link_rx_release(a->base, pos);
        if ((a->features & HQQ_FEATURE_CANCEL) == 0) {
            return HQQ_ERR_UNEXPECTED_PACKET;
        }
        adv_cancel_request(a, hqq_packet_operand(&local, 0), hqq_packet_operand(&local, 1));
        return HQQ_OK;
    }
    hqq_link_rx_release(a->base, pos);
    return HQQ_ERR_UNEXPECTED_PACKET;
}

void adv_dispatch_main(void *arg) {
    hqq_adv *a = (hqq_adv *)arg;
    adv_wait w;

    adv_wait_init(&w, NULL);
    for (;;) {
        hqq_packet pkt;
        uint64_t pos;
        hqq_status st = adv_rx(a, &w, &pkt, &pos);
        if (st == HQQ_OK) {
            st = dispatch_one(a, &pkt, pos);
        }
        if (st != HQQ_OK) {
            hqq_spin_lock(&a->lock);
            if (atomic_load_explicit(&a->closing, memory_order_seq_cst)) {
                st = HQQ_ERR_CLOSED;
            }
            adv_fail_all_locked(a, st);
            hqq_spin_unlock(&a->lock);
            return;
        }
    }
}

/* ---- Control sender ---------------------------------------------------------------------- */

void adv_control_main(void *arg) {
    hqq_adv *a = (hqq_adv *)arg;
    adv_wait w;

    adv_wait_init(&w, NULL);
    for (;;) {
        adv_ctl_job job;
        hqq_status st;

        hqq_spin_lock(&a->lock);
        while (a->ctl_count == 0 && !atomic_load_explicit(&a->closing, memory_order_seq_cst)) {
            adv_park_locked(a, ADV_POLL_NS);
        }
        if (atomic_load_explicit(&a->closing, memory_order_seq_cst)) {
            hqq_spin_unlock(&a->lock);
            return;
        }
        job = a->ctl[a->ctl_head];
        a->ctl_head = (a->ctl_head + 1) % a->control_cap;
        a->ctl_count--;
        hqq_spin_unlock(&a->lock);

        st = adv_tx(a, &w, job.op, job.ops, NULL, 0, 1);
        if (job.accept != NULL) {
            hqq_spin_lock(&a->lock);
            if (st == HQQ_OK) {
                /* Published on the ring: now AcceptStream may return it (SPEC 10.3). */
                adv_queue_push(&a->acceptq, job.accept);
            } else {
                adv_stream_unregister_locked(a, job.accept);
                a->accept_reserved--;
            }
            adv_wake_locked(a);
            hqq_spin_unlock(&a->lock);
        }
    }
}
