/*
 * hqq_adv_route.c - routing of completed messages (SPEC 10.7) and the
 * refusals that the receive path answers (SPEC 10.5, 1.1 sections 4.1, 6).
 */
#include "hqq_adv_internal.h"

static adv_call *call_find_locked(hqq_adv *a, uint64_t conn, uint64_t msg_id) {
    adv_call *c;
    for (c = a->calls; c != NULL; c = c->next) {
        if (c->conn == conn && c->msg_id == msg_id && !c->done) {
            return c;
        }
    }
    return NULL;
}

static hqq_stream *stream_find_locked(hqq_adv *a, uint64_t id) {
    uint64_t i;
    for (i = 0; i < a->nreg; i++) {
        if (a->reg[i]->id == id) {
            return a->reg[i];
        }
    }
    return NULL;
}

/* Request context of a new request (lock held). NULL on no memory. */
static hqq_req_ctx *reqctx_take_locked(hqq_adv *a) {
    hqq_req_ctx *c = a->reqctx_free;
    if (c != NULL) {
        a->reqctx_free = c->pool_next;
    } else {
        c = (hqq_req_ctx *)hqq_alloc(a->alloc_set ? &a->alloc : NULL, sizeof *c);
        if (c == NULL) {
            return NULL;
        }
    }
    /* Pooled and fresh contexts alike start zeroed: a request without a budget
     * relies on has_deadline being 0, and malloc does not clear memory. */
    memset(c, 0, sizeof *c);
    return c;
}

/* Refuses a request during shutdown or for a refused body: an error response
 * (best effort) on the control queue. Lock held. */
void adv_refuse_request_locked(hqq_adv *a, uint64_t conn, uint64_t msg_id, uint64_t status) {
    uint64_t ops[7];
    memset(ops, 0, sizeof ops);
    ops[0] = conn;
    ops[1] = msg_id;
    ops[5] = ADV_BEGIN | ADV_END | ADV_RESPONSE | ADV_ERROR;
    ops[6] = status;
    adv_queue_control_locked(a, HQQ_OP_CONN_COPY, ops, NULL);
}

/* Delivers a completed message to its destination; takes ownership of m.
 * Returns the error that fails the receive path, or HQQ_OK. */
hqq_status adv_route(hqq_adv *a, hqq_msg *m, adv_inline *inl) {
    uint32_t kind = m->flags & ADV_KIND_MASK;

    hqq_spin_lock(&a->lock);
    if (kind == ADV_RESPONSE) {
        adv_call *c = call_find_locked(a, m->conn, m->msg_id);
        if (c != NULL) {
            c->done = 1;
            c->resp = m;
            c->status = (m->flags & ADV_ERROR) != 0 ? HQQ_ERR_RESPONSE : HQQ_OK;
            a->st_responses++;
            adv_wake_locked(a);
            hqq_spin_unlock(&a->lock);
            return HQQ_OK;
        }
        if (adv_take_abandoned_locked(a, m->conn, m->msg_id)) {
            a->st_abandoned++;
        } else {
            a->st_dropped++;
        }
        hqq_spin_unlock(&a->lock);
        adv_msg_put(m);
        return HQQ_OK;
    }

    if (kind == ADV_REQUEST) {
        hqq_req_ctx *c;
        for (;;) {
            if (a->shutting) {
                adv_refuse_request_locked(a, m->conn, m->msg_id, ADV_CODE_SHUTTING_DOWN);
                a->st_refused++;
                hqq_spin_unlock(&a->lock);
                adv_msg_put(m);
                return HQQ_OK;
            }
            if (!adv_queue_full(&a->reqq)) {
                break;
            }
            if (a->overflow == HQQ_OVERFLOW_DROP) {
                a->st_dropped++;
                hqq_spin_unlock(&a->lock);
                adv_msg_put(m);
                return HQQ_OK;
            }
            if (a->overflow == HQQ_OVERFLOW_FAIL) {
                hqq_spin_unlock(&a->lock);
                adv_msg_put(m);
                return HQQ_ERR_INCOMING_OVERFLOW;
            }
            if (atomic_load_explicit(&a->closing, memory_order_seq_cst)) {
                hqq_spin_unlock(&a->lock);
                adv_msg_put(m);
                return HQQ_ERR_CLOSED;
            }
            adv_park_events_locked(a, ADV_POLL_NS, ADV_EV_DONE);
        }
        c = reqctx_take_locked(a);
        if (c == NULL) {
            hqq_spin_unlock(&a->lock);
            adv_msg_put(m);
            return HQQ_ERR_NO_MEMORY;
        }
        c->a = a;
        c->conn = m->conn;
        c->msg_id = m->msg_id;
        c->req = m;
        c->end = ADV_REQ_LIVE;
        if (m->has_budget) {
            uint64_t now = adv_mono();
            c->has_deadline = 1;
            c->deadline_mono = m->t0_mono + m->budget_ns;
            if (c->deadline_mono < m->t0_mono) {
                c->deadline_mono = UINT64_MAX; /* saturating add */
            }
            if (now >= c->deadline_mono) {
                c->end = ADV_REQ_DEADLINE; /* expired on arrival: never handled */
            }
        }
        m->rc = c;
        c->next = a->reqs;
        a->reqs = c;
        a->inflight++;
        if (c->end == ADV_REQ_LIVE && a->serving && !a->serve_stop && a->workers_busy == 0 &&
            a->esc_busy == 0 && adv_queue_empty(&a->reqq)) {
            /* Nothing runs and nothing waits: the dispatcher runs the handler itself. */
            inl->msg = m;
            inl->ctx = c;
            inl->fn = a->serve_fn;
            inl->user = a->serve_user;
            inl->escalated = 0;
            inl->retired = NULL;
            a->inl = inl;
            a->inline_active = 1;
            a->inl_start = adv_mono();
            a->inl_seq++;
            a->sv_inline++;
            if (a->timer_slow) {
                adv_wake_events_locked(a, ADV_EV_TIMER);
            }
            hqq_spin_unlock(&a->lock);
            return HQQ_OK;
        }
        adv_queue_push(&a->reqq, m);
        a->sv_pooled++;
        adv_wake_events_locked(a, ADV_EV_WORK);
        if (a->reqq.count > adv_idle_workers_locked(a)) {
            adv_wake_events_locked(a, ADV_EV_SPAWN);
        }
        hqq_spin_unlock(&a->lock);
        return HQQ_OK;
    }

    /* Data message: the queue of its stream. */
    for (;;) {
        hqq_stream *s = stream_find_locked(a, m->conn);
        if (s == NULL || s->state != ADV_ST_OPEN) {
            a->st_dropped++;
            hqq_spin_unlock(&a->lock);
            adv_msg_put(m);
            return HQQ_OK;
        }
        if (!adv_queue_full(&s->q)) {
            adv_queue_push(&s->q, m);
            adv_wake_locked(a);
            hqq_spin_unlock(&a->lock);
            return HQQ_OK;
        }
        if (a->overflow == HQQ_OVERFLOW_DROP) {
            a->st_dropped++;
            hqq_spin_unlock(&a->lock);
            adv_msg_put(m);
            return HQQ_OK;
        }
        if (a->overflow == HQQ_OVERFLOW_FAIL) {
            hqq_spin_unlock(&a->lock);
            adv_msg_put(m);
            return HQQ_ERR_INCOMING_OVERFLOW;
        }
        if (atomic_load_explicit(&a->closing, memory_order_seq_cst)) {
            hqq_spin_unlock(&a->lock);
            adv_msg_put(m);
            return HQQ_ERR_CLOSED;
        }
        adv_park_locked(a, ADV_POLL_NS);
    }
}
