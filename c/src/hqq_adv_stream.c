/*
 * hqq_adv_stream.c - streams of an Advanced link (SPEC 10.2, 10.3): the stream
 * registry, OpenStream / AcceptStream / ListenStreams, Stream.Close, and the
 * receive-path handlers of OpConnCreate, OpConnAccept, OpConnClose and
 * OpError.
 */
#include "hqq_adv_internal.h"

/* ---- Registry and objects (a->lock held by the caller unless noted) -------------------- */

hqq_status hqq_stream_new_internal(hqq_adv *a, uint64_t id, hqq_stream **out) {
    hqq_stream *s = (hqq_stream *)hqq_alloc(a->alloc_set ? &a->alloc : NULL, sizeof *s);
    hqq_status st;
    if (s == NULL) {
        return HQQ_ERR_NO_MEMORY;
    }
    memset(s, 0, sizeof *s);
    st = adv_queue_init(a->alloc_set ? &a->alloc : NULL, &s->q, a->queue_size);
    if (st != HQQ_OK) {
        hqq_free(a->alloc_set ? &a->alloc : NULL, s, sizeof *s);
        return st;
    }
    s->a = a;
    s->id = id;
    s->state = ADV_ST_OPEN;
    s->next_all = a->all_streams;
    a->all_streams = s;
    *out = s;
    return HQQ_OK;
}

hqq_status adv_stream_register(hqq_adv *a, hqq_stream *s) {
    if (a->nreg == a->capreg) {
        uint64_t ncap = a->capreg == 0 ? 8 : a->capreg * 2;
        hqq_stream **nr = (hqq_stream **)hqq_alloc(a->alloc_set ? &a->alloc : NULL, (size_t)ncap * sizeof *nr);
        if (nr == NULL) {
            return HQQ_ERR_NO_MEMORY;
        }
        if (a->nreg != 0) {
            memcpy(nr, a->reg, (size_t)a->nreg * sizeof *nr);
        }
        if (a->reg != NULL) {
            hqq_free(a->alloc_set ? &a->alloc : NULL, a->reg, (size_t)a->capreg * sizeof *nr);
        }
        a->reg = nr;
        a->capreg = ncap;
    }
    a->reg[a->nreg++] = s;
    s->registered = 1;
    return HQQ_OK;
}

void adv_stream_unregister_locked(hqq_adv *a, hqq_stream *s) {
    uint64_t i;
    for (i = 0; i < a->nreg; i++) {
        if (a->reg[i] == s) {
            a->reg[i] = a->reg[a->nreg - 1];
            a->nreg--;
            break;
        }
    }
    s->registered = 0;
}

hqq_stream *adv_stream_lookup_locked(hqq_adv *a, uint64_t id) {
    uint64_t i;
    for (i = 0; i < a->nreg; i++) {
        if (a->reg[i]->id == id) {
            return a->reg[i];
        }
    }
    return NULL;
}

/* An id is owned by the peer when it has the peer's parity (SPEC 10.3): odd ids belong
 * to the Secondary, non-zero even ids to the Primary. */
int adv_peer_owns_id(const hqq_adv *a, uint64_t id) {
    int we_primary = hqq_link_mode(a->base) == HQQ_MODE_PRIMARY;
    if (id == 0) {
        return 0;
    }
    return ((id & 1u) != 0) == we_primary;
}

/* Maps a peer error code to a status (SPEC 12). */
static hqq_status status_from_code(uint64_t code) {
    switch (code) {
    case HQQ_ERRCODE_MEMORY_ALIGN: return HQQ_ERR_MEMORY_ALIGN;
    case HQQ_ERRCODE_INVALID_SIZE: return HQQ_ERR_INVALID_SIZE;
    case HQQ_ERRCODE_MEMORY_SMALL: return HQQ_ERR_MEMORY_SMALL;
    case HQQ_ERRCODE_FAILED_INIT: return HQQ_ERR_FAILED_INIT;
    case HQQ_ERRCODE_CONN_NOT_FOUND: return HQQ_ERR_CONN_NOT_FOUND;
    case HQQ_ERRCODE_INVALID_OP: return HQQ_ERR_INVALID_OP;
    case HQQ_ERRCODE_BUFFER_OVERFLOW: return HQQ_ERR_BUFFER_OVERFLOW;
    case ADV_CODE_SHUTTING_DOWN: return HQQ_ERR_SHUTTING_DOWN;
    default: return HQQ_ERR_PROTOCOL;
    }
}

/* ---- Receive-path control handlers ------------------------------------------------------ */

/* Queues the refusal of OpConnCreate id with code. Lock held. */
static void refuse_create_locked(hqq_adv *a, uint64_t id, uint64_t code) {
    uint64_t ops[7];
    memset(ops, 0, sizeof ops);
    ops[0] = id;
    ops[1] = code;
    adv_queue_control_locked(a, HQQ_OP_ERROR, ops, NULL);
}

/* Holds OpConnCreate id, which arrived while the link is not listening, for the
 * grace period. Returns 0 when the request must be refused at once: holding is
 * disabled, or the bound is reached (the held requests and the accept slots in
 * use must fit queue_size, so ListenStreams always finds room for each held
 * request). Lock held. */
static int hold_create_locked(hqq_adv *a, uint64_t id) {
    uint64_t now;
    adv_held *h;
    if (a->listen_grace_ns == 0 || a->held_count + a->accept_reserved >= a->queue_size) {
        return 0;
    }
    now = adv_mono();
    h = &a->held[(a->held_head + a->held_count) % a->queue_size];
    h->id = id;
    h->deadline = a->listen_grace_ns > UINT64_MAX - now ? UINT64_MAX : now + a->listen_grace_ns;
    a->held_count++;
    adv_wake_locked(a); /* the control sender re-arms its timer for the new deadline */
    return 1;
}

/* Queues the accept of stream id: the ID is valid and an accept slot is free.
 * Lock held. */
static hqq_status accept_locked(hqq_adv *a, uint64_t id) {
    uint64_t ops[7];
    hqq_stream *s = NULL;
    hqq_status st;
    if (adv_control_full_locked(a)) {
        /* The accept reply cannot be queued: drop it (SPEC 10.3), counted. */
        a->st_dropped++;
        return HQQ_OK;
    }
    st = hqq_stream_new_internal(a, id, &s);
    if (st != HQQ_OK) {
        return st;
    }
    st = adv_stream_register(a, s);
    if (st != HQQ_OK) {
        return st;
    }
    a->accept_reserved++;
    memset(ops, 0, sizeof ops);
    ops[0] = id;
    /* Queued now; the control sender publishes it and then makes the stream visible
     * to AcceptStream, so data sent by the acceptor cannot overtake the accept. */
    adv_queue_control_locked(a, HQQ_OP_CONN_ACCEPT, ops, s);
    return HQQ_OK;
}

/* Admits the held requests in arrival order, as if each had just arrived while
 * listening (ListenStreams, lock held). It never waits: hold_create_locked left
 * room in the accept queue for every held request. */
static void admit_held_locked(hqq_adv *a) {
    while (a->held_count > 0) {
        uint64_t id = a->held[a->held_head].id;
        a->held_head = (a->held_head + 1) % a->queue_size;
        a->held_count--;
        if (!adv_peer_owns_id(a, id) || adv_stream_lookup_locked(a, id) != NULL) {
            refuse_create_locked(a, id, ADV_CODE_INVALID_OP);
        } else if (a->accept_reserved >= a->queue_size) {
            /* Not reachable with the bound of hold_create_locked: refused, not waited for. */
            a->st_dropped++;
            refuse_create_locked(a, id, ADV_CODE_BUFFER_OVERFLOW);
        } else if (accept_locked(a, id) != HQQ_OK) {
            a->st_dropped++; /* out of memory: the request is dropped and counted */
        }
    }
}

/* Refuses the held requests whose grace period has ended by now (lock held). The
 * requests share one grace period and are held in arrival order, so their
 * deadlines are sorted. */
void adv_held_expire_locked(hqq_adv *a, uint64_t now) {
    while (a->held_count > 0 && a->held[a->held_head].deadline <= now) {
        refuse_create_locked(a, a->held[a->held_head].id, ADV_CODE_CONN_NOT_FOUND);
        a->held_head = (a->held_head + 1) % a->queue_size;
        a->held_count--;
    }
}

/* Ends the hold of every held request (lock held): each is refused when refuse is
 * set (the link failed), and otherwise discarded and counted (the link closes). */
void adv_held_release_locked(hqq_adv *a, int refuse) {
    while (a->held_count > 0) {
        if (refuse) {
            refuse_create_locked(a, a->held[a->held_head].id, ADV_CODE_CONN_NOT_FOUND);
        } else {
            a->st_dropped++;
        }
        a->held_head = (a->held_head + 1) % a->queue_size;
        a->held_count--;
    }
}

/* Answers an OpConnCreate (SPEC 10.3). While the link is not listening the request
 * is held (or refused at once when it cannot be held). Otherwise the request is
 * accepted or refused; a full accept queue waits for AcceptStream only under
 * HQQ_OVERFLOW_BLOCK. The receive path never waits for the tx ring. */
static hqq_status handle_create(hqq_adv *a, uint64_t id) {
    hqq_status st;

    hqq_spin_lock(&a->lock);
    if (!a->listening) {
        if (!hold_create_locked(a, id)) {
            refuse_create_locked(a, id, ADV_CODE_CONN_NOT_FOUND);
        }
        hqq_spin_unlock(&a->lock);
        return HQQ_OK;
    }
    for (;;) {
        if (!adv_peer_owns_id(a, id) || adv_stream_lookup_locked(a, id) != NULL) {
            refuse_create_locked(a, id, ADV_CODE_INVALID_OP);
            hqq_spin_unlock(&a->lock);
            return HQQ_OK;
        }
        if (a->accept_reserved < a->queue_size) {
            break;
        }
        if (a->overflow == HQQ_OVERFLOW_DROP) {
            refuse_create_locked(a, id, ADV_CODE_BUFFER_OVERFLOW);
            hqq_spin_unlock(&a->lock);
            return HQQ_OK;
        }
        if (a->overflow == HQQ_OVERFLOW_FAIL) {
            hqq_spin_unlock(&a->lock);
            return HQQ_ERR_INCOMING_OVERFLOW;
        }
        if (atomic_load_explicit(&a->closing, memory_order_seq_cst)) {
            hqq_spin_unlock(&a->lock);
            return HQQ_ERR_CLOSED;
        }
        adv_park_locked(a, ADV_POLL_NS);
    }
    st = accept_locked(a, id);
    hqq_spin_unlock(&a->lock);
    return st;
}

static void handle_accept(hqq_adv *a, uint64_t id) {
    adv_open *o;
    hqq_spin_lock(&a->lock);
    for (o = a->opens; o != NULL; o = o->next) {
        if (o->id == id && !o->done) {
            o->done = 1;
            o->status = HQQ_OK;
            adv_wake_locked(a);
            break;
        }
    }
    hqq_spin_unlock(&a->lock);
}

static void handle_peer_close(hqq_adv *a, uint64_t id) {
    adv_open *o;
    hqq_stream *s;
    hqq_spin_lock(&a->lock);
    for (o = a->opens; o != NULL; o = o->next) {
        if (o->id == id) {
            hqq_spin_unlock(&a->lock);
            return; /* still opening: ignored */
        }
    }
    s = adv_stream_lookup_locked(a, id);
    if (s != NULL && id != 0) {
        adv_stream_unregister_locked(a, s);
        if (s->state == ADV_ST_OPEN) {
            s->state = ADV_ST_CLOSED_PEER;
        }
        adv_cancel_stream_requests_locked(a, id);
        adv_wake_locked(a);
    }
    hqq_spin_unlock(&a->lock);
}

static hqq_status handle_error(hqq_adv *a, uint64_t ctx_id, uint64_t code) {
    adv_open *o;
    hqq_status st = status_from_code(code);
    hqq_spin_lock(&a->lock);
    for (o = a->opens; o != NULL; o = o->next) {
        if (o->id == ctx_id && !o->done) {
            o->done = 1;
            o->status = st;
            adv_wake_locked(a);
            hqq_spin_unlock(&a->lock);
            return HQQ_OK;
        }
    }
    /* Not an answer to an OpenStream: the error fails the receive path (SPEC 10.3). */
    adv_fail_all_locked(a, st);
    hqq_spin_unlock(&a->lock);
    return st;
}

hqq_status adv_handle_control(hqq_adv *a, const hqq_packet *pkt) {
    switch (hqq_packet_op(pkt)) {
    case HQQ_OP_CONN_CREATE:
        return handle_create(a, hqq_packet_operand(pkt, 0));
    case HQQ_OP_CONN_ACCEPT:
        handle_accept(a, hqq_packet_operand(pkt, 0));
        return HQQ_OK;
    case HQQ_OP_CONN_CLOSE:
        handle_peer_close(a, hqq_packet_operand(pkt, 0));
        return HQQ_OK;
    case HQQ_OP_ERROR:
        return handle_error(a, hqq_packet_operand(pkt, 0), hqq_packet_operand(pkt, 1));
    default:
        return HQQ_ERR_UNEXPECTED_PACKET;
    }
}

/* ---- Public stream API -------------------------------------------------------------------- */

hqq_stream *hqq_adv_stream0(hqq_adv *a) { return a == NULL ? NULL : a->stream0; }

uint64_t hqq_stream_id(const hqq_stream *s) { return s == NULL ? 0 : s->id; }

hqq_status hqq_adv_listen_streams(hqq_adv *a) {
    hqq_status st;
    if (a == NULL) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    st = adv_enter(a);
    if (st != HQQ_OK) {
        return st;
    }
    st = adv_check_negotiated(a, 0);
    if (st == HQQ_OK) {
        hqq_spin_lock(&a->lock);
        if (a->listening) {
            st = HQQ_ERR_ALREADY_LISTENING;
        } else {
            a->listening = 1;
            admit_held_locked(a);
        }
        hqq_spin_unlock(&a->lock);
    }
    adv_leave(a);
    return st;
}

hqq_status hqq_adv_accept_stream(hqq_adv *a, hqq_stream **out, const hqq_wait *in) {
    adv_wait w;
    hqq_status st;

    if (a == NULL || out == NULL) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    *out = NULL;
    st = adv_enter(a);
    if (st != HQQ_OK) {
        return st;
    }
    adv_wait_init(&w, in);
    st = adv_check_negotiated(a, 0);
    if (st == HQQ_OK && !a->listening) {
        st = HQQ_ERR_NOT_LISTENING;
    }
    if (st == HQQ_OK) {
        hqq_spin_lock(&a->lock);
        for (;;) {
            if (!adv_queue_empty(&a->acceptq)) {
                *out = (hqq_stream *)adv_queue_pop(&a->acceptq);
                a->accept_reserved--;
                adv_wake_locked(a);
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
            {
                uint64_t ns = ADV_POLL_NS;
                if (w.has_deadline) {
                    uint64_t now = adv_mono();
                    uint64_t left = w.deadline_mono > now ? w.deadline_mono - now : 0;
                    if (left < ns) {
                        ns = left;
                    }
                }
                adv_park_locked(a, ns);
            }
        }
        hqq_spin_unlock(&a->lock);
    }
    adv_leave(a);
    return st;
}

hqq_status hqq_adv_open_stream(hqq_adv *a, hqq_stream **out, const hqq_wait *in) {
    adv_wait w;
    adv_open o;
    hqq_stream *s = NULL;
    uint64_t id;
    uint64_t ops[7];
    hqq_status st;

    if (a == NULL || out == NULL) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    *out = NULL;
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
    {
        uint64_t n = ++a->next_stream_n;
        id = hqq_link_mode(a->base) == HQQ_MODE_PRIMARY ? (n << 1) : (2 * n - 1);
    }
    st = hqq_stream_new_internal(a, id, &s);
    if (st == HQQ_OK) {
        st = adv_stream_register(a, s);
    }
    if (st == HQQ_OK) {
        memset(&o, 0, sizeof o);
        o.id = id;
        o.next = a->opens;
        a->opens = &o;
    }
    hqq_spin_unlock(&a->lock);
    if (st != HQQ_OK) {
        adv_leave(a);
        return st;
    }

    memset(ops, 0, sizeof ops);
    ops[0] = id;
    st = adv_tx(a, &w, HQQ_OP_CONN_CREATE, ops, NULL, 0, 1);

    hqq_spin_lock(&a->lock);
    while (st == HQQ_OK && !o.done) {
        st = adv_wait_check(a, &w);
        if (st != HQQ_OK) {
            break;
        }
        if (w.nowait) {
            st = HQQ_ERR_TIMEOUT;
            break;
        }
        {
            uint64_t ns = ADV_POLL_NS;
            if (w.has_deadline) {
                uint64_t now = adv_mono();
                uint64_t left = w.deadline_mono > now ? w.deadline_mono - now : 0;
                if (left < ns) {
                    ns = left;
                }
            }
            adv_park_locked(a, ns);
        }
    }
    if (st == HQQ_OK && o.done) {
        st = o.status;
    }
    /* Unlink the pending open (it lives on this stack). */
    {
        adv_open **pp = &a->opens;
        while (*pp != NULL && *pp != &o) {
            pp = &(*pp)->next;
        }
        if (*pp == &o) {
            *pp = o.next;
        }
    }
    if (st != HQQ_OK) {
        adv_stream_unregister_locked(a, s);
        s->state = ADV_ST_CLOSED_LOCAL;
        hqq_spin_unlock(&a->lock);
        adv_leave(a);
        return st;
    }
    hqq_spin_unlock(&a->lock);
    *out = s;
    adv_leave(a);
    return HQQ_OK;
}

hqq_status hqq_stream_close(hqq_stream *s) {
    hqq_adv *a;
    int peer_closed;
    uint64_t id;
    uint64_t ops[7];
    hqq_status st;

    if (s == NULL) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    a = s->a;
    id = s->id;
    if (id == 0) {
        return HQQ_ERR_INVALID_OP;
    }
    st = adv_enter(a);
    if (st != HQQ_OK) {
        return st;
    }
    st = adv_check_negotiated(a, 0);
    if (st != HQQ_OK) {
        adv_leave(a);
        return st;
    }
    hqq_spin_lock(&a->lock);
    if (s->state == ADV_ST_CLOSED_LOCAL) {
        hqq_spin_unlock(&a->lock);
        adv_leave(a);
        return HQQ_ERR_CLOSED;
    }
    peer_closed = s->state == ADV_ST_CLOSED_PEER;
    s->state = ADV_ST_CLOSED_LOCAL;
    if (s->registered) {
        adv_stream_unregister_locked(a, s);
    }
    adv_cancel_stream_requests_locked(a, id);
    while (!adv_queue_empty(&s->q)) {
        hqq_msg *m = (hqq_msg *)adv_queue_pop(&s->q);
        hqq_spin_unlock(&a->lock);
        adv_msg_put(m);
        hqq_spin_lock(&a->lock);
    }
    adv_wake_locked(a);
    hqq_spin_unlock(&a->lock);

    st = HQQ_OK;
    if (!peer_closed) {
        adv_wait w;
        adv_wait_init(&w, NULL);
        memset(ops, 0, sizeof ops);
        ops[0] = id;
        st = adv_tx(a, &w, HQQ_OP_CONN_CLOSE, ops, NULL, 0, 0);
    }
    adv_leave(a);
    return st;
}
