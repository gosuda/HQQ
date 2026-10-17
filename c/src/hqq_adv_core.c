/*
 * hqq_adv_core.c - shared machinery of the Advanced layer: message pools,
 * queues, waiting and bounded Standard-link calls, the failure path, the
 * control queue, and the abandoned and refused key sets.
 */
#include <limits.h>
#include <stdlib.h>

#include "hqq_adv_internal.h"

/* ---- Allocation helpers ------------------------------------------------------------- */

static const hqq_allocator *adv_alloc_of(const hqq_adv *a) { return a->alloc_set ? &a->alloc : NULL; }

const hqq_allocator *adv_alloc_ptr(const hqq_adv *a) { return adv_alloc_of(a); }

/* ---- Pool ------------------------------------------------------------------------------ */

unsigned adv_class_for(uint64_t n) {
    unsigned c = ADV_MIN_CLASS;
    while (c < ADV_MAX_CLASS && (UINT64_C(1) << c) < n) {
        c++;
    }
    return c;
}

/* Drops one reference (lock held). Returns 1 when the pool must now be freed, which the
 * caller does after releasing the lock with pool_free(). */
static int pool_unref_locked(adv_pool *p) {
    p->refs--;
    return p->refs == 0 && p->closed;
}

static void pool_free(adv_pool *p) {
    hqq_allocator a = p->alloc;
    int a_set = p->alloc_set;
    hqq_free(a_set ? &a : NULL, p, sizeof *p);
}

hqq_msg *adv_msg_get(adv_pool *p, uint64_t n) {
    unsigned cls = adv_class_for(n);
    hqq_msg *m = NULL;

    hqq_spin_lock(&p->lock);
    if (p->free[cls] != NULL) {
        m = p->free[cls];
        p->free[cls] = m->next;
        p->nfree[cls]--;
    }
    p->refs++;
    hqq_spin_unlock(&p->lock);
    if (m != NULL) {
        uint64_t cap = m->cap;
        adv_pool *owner = m->pool;
        uint8_t c8 = m->cls;
        unsigned char *data = m->data;
        memset(m, 0, sizeof *m);
        m->cap = cap;
        m->pool = owner;
        m->cls = c8;
        m->data = data;
        return m;
    }
    {
        hqq_allocator a = p->alloc;
        m = (hqq_msg *)hqq_alloc(p->alloc_set ? &a : NULL, sizeof *m + (size_t)(UINT64_C(1) << cls));
    }
    if (m == NULL) {
        int dead;
        hqq_spin_lock(&p->lock);
        dead = pool_unref_locked(p);
        hqq_spin_unlock(&p->lock);
        if (dead) {
            pool_free(p);
        }
        return NULL;
    }
    memset(m, 0, sizeof *m);
    m->pool = p;
    m->cap = UINT64_C(1) << cls;
    m->cls = (uint8_t)cls;
    m->data = (unsigned char *)(m + 1);
    return m;
}

void adv_msg_put(hqq_msg *m) {
    adv_pool *p = m->pool;
    unsigned cls = m->cls;
    int dead;
    hqq_allocator a;
    int a_set;

    hqq_spin_lock(&p->lock);
    if (!p->closed && p->nfree[cls] < ADV_POOL_KEEP) {
        m->next = p->free[cls];
        p->free[cls] = m;
        p->nfree[cls]++;
        m = NULL;
    }
    dead = pool_unref_locked(p);
    hqq_spin_unlock(&p->lock);
    /* The pool may be freed only after the lock is released and after its last use. */
    a = p->alloc;
    a_set = p->alloc_set;
    if (m != NULL) {
        hqq_free(a_set ? &a : NULL, m, sizeof *m + (size_t)m->cap);
    }
    if (dead) {
        pool_free(p);
    }
}

/* Called once by the closing link, after its messages were released: frees the
 * retained buffers and the owner's reference. The pool object itself is freed
 * when the last outstanding message is released. */
void pool_drain_owner(adv_pool *p) {
    hqq_msg *list[ADV_CLASS_COUNT];
    unsigned c;
    int dead;
    hqq_allocator a;
    int a_set;

    hqq_spin_lock(&p->lock);
    p->closed = 1;
    for (c = 0; c < ADV_CLASS_COUNT; c++) {
        list[c] = p->free[c];
        p->free[c] = NULL;
        p->nfree[c] = 0;
    }
    dead = pool_unref_locked(p);
    hqq_spin_unlock(&p->lock);
    a = p->alloc;
    a_set = p->alloc_set;
    for (c = 0; c < ADV_CLASS_COUNT; c++) {
        hqq_msg *m = list[c];
        while (m != NULL) {
            hqq_msg *next = m->next;
            hqq_free(a_set ? &a : NULL, m, sizeof *m + (size_t)m->cap);
            m = next;
        }
    }
    if (dead) {
        pool_free(p);
    }
}

/* ---- Queues ---------------------------------------------------------------------------- */

hqq_status adv_queue_init(const hqq_allocator *a, adv_queue *q, uint64_t cap) {
    if (cap == 0) {
        cap = 1;
    }
    q->items = (void **)hqq_alloc(a, (size_t)cap * sizeof(void *));
    if (q->items == NULL) {
        memset(q, 0, sizeof *q);
        return HQQ_ERR_NO_MEMORY;
    }
    q->cap = cap;
    q->head = 0;
    q->count = 0;
    return HQQ_OK;
}

void adv_queue_free(const hqq_allocator *a, adv_queue *q) {
    if (q->items != NULL) {
        hqq_free(a, q->items, (size_t)q->cap * sizeof(void *));
    }
    memset(q, 0, sizeof *q);
}

int adv_queue_full(const adv_queue *q) { return q->count >= q->cap; }
int adv_queue_empty(const adv_queue *q) { return q->count == 0; }

void adv_queue_push(adv_queue *q, void *item) {
    q->items[(q->head + q->count) % q->cap] = item;
    q->count++;
}

void *adv_queue_pop(adv_queue *q) {
    void *item = q->items[q->head];
    q->head = (q->head + 1) % q->cap;
    q->count--;
    return item;
}

/* ---- Waiting ----------------------------------------------------------------------------- */

void adv_wake_locked(hqq_adv *a) {
    adv_waiter *w;
    for (w = a->wq; w != NULL; w = w->next) {
        hqq_plat_event_signal(w->ev);
    }
    a->wq = NULL;
}

void adv_wake_events_locked(hqq_adv *a, unsigned events) {
    adv_waiter **pp = &a->wq;
    while (*pp != NULL) {
        adv_waiter *w = *pp;
        if ((w->events & events) != 0) {
            *pp = w->next;
            hqq_plat_event_signal(w->ev);
        } else {
            pp = &w->next;
        }
    }
}

void adv_park_locked(hqq_adv *a, uint64_t ns) { adv_park_events_locked(a, ns, 0); }

void adv_park_events_locked(hqq_adv *a, uint64_t ns, unsigned events) {
    adv_waiter *w = a->waiter_free;
    if (ns > ADV_POLL_NS) {
        ns = ADV_POLL_NS;
    }
    if (w != NULL) {
        a->waiter_free = w->next;
    } else {
        w = (adv_waiter *)hqq_alloc(adv_alloc_of(a), sizeof *w);
        if (w != NULL) {
            w->ev = hqq_plat_event_create(adv_alloc_of(a));
            if (w->ev == NULL) {
                hqq_free(adv_alloc_of(a), w, sizeof *w);
                w = NULL;
            }
        }
    }
    if (w == NULL) {
        /* No memory for a waiter: back off instead of parking. */
        hqq_spin_unlock(&a->lock);
        hqq_plat_sleep_ns(ns < 1000000u ? ns : 1000000u);
        hqq_spin_lock(&a->lock);
        return;
    }
    hqq_plat_event_reset(w->ev);
    w->events = events;
    w->next = a->wq;
    a->wq = w;
    hqq_spin_unlock(&a->lock);
    (void)hqq_plat_event_wait(w->ev, ns);
    hqq_spin_lock(&a->lock);
    {
        adv_waiter **pp = &a->wq;
        while (*pp != NULL) {
            if (*pp == w) {
                *pp = w->next;
                break;
            }
            pp = &(*pp)->next;
        }
    }
    w->next = a->waiter_free;
    a->waiter_free = w;
}

void adv_wait_init(adv_wait *w, const hqq_wait *in) {
    memset(w, 0, sizeof *w);
    if (in != NULL) {
        w->cancel = in->cancel;
        if (in->timeout_ns == 0) {
            w->nowait = 1;
        } else if (in->timeout_ns > 0) {
            w->has_deadline = 1;
            w->deadline_mono = hqq_plat_mono_ns() + (uint64_t)in->timeout_ns;
        }
    }
}

hqq_status adv_wait_check(hqq_adv *a, const adv_wait *w) {
    if (atomic_load_explicit(&a->closing, memory_order_seq_cst)) {
        return HQQ_ERR_CLOSED;
    }
    if (w->cancel != NULL && hqq_cancel_load(w->cancel)) {
        return HQQ_ERR_CANCELED;
    }
    if (w->has_deadline && hqq_plat_mono_ns() >= w->deadline_mono) {
        return HQQ_ERR_TIMEOUT;
    }
    return HQQ_OK;
}

uint64_t adv_slice_ns(const adv_wait *w) {
    uint64_t ns = ADV_POLL_NS;
    if (w->nowait) {
        return 0;
    }
    if (w->has_deadline) {
        uint64_t now = hqq_plat_mono_ns();
        uint64_t left = w->deadline_mono > now ? w->deadline_mono - now : 0;
        if (left < ns) {
            ns = left;
        }
    }
    return ns;
}

/* ---- Bounded Standard-link calls ------------------------------------------------------ */

hqq_status adv_rx(hqq_adv *a, const adv_wait *w, hqq_packet *pkt, uint64_t *pos) {
    for (;;) {
        hqq_wait hw;
        hqq_status st;

        hw.timeout_ns = (int64_t)adv_slice_ns(w);
        hw.cancel = &a->stop;
        st = hqq_link_rx_frame(a->base, &hw, pkt, pos);
        if (st == HQQ_OK) {
            return HQQ_OK;
        }
        if (st == HQQ_ERR_TIMEOUT) {
            hqq_status wst = adv_wait_check(a, w);
            if (wst != HQQ_OK) {
                return wst;
            }
            if (w->nowait) {
                return HQQ_ERR_TIMEOUT;
            }
            continue;
        }
        if (st == HQQ_ERR_CANCELED) {
            return HQQ_ERR_CLOSED; /* only close signals the stop token */
        }
        return st;
    }
}

hqq_status adv_tx_parts(hqq_adv *a, const adv_wait *w, unsigned op, const uint64_t ops[7], const void *p1,
                        size_t n1, const void *p2, size_t n2, int check_peer) {
    for (;;) {
        hqq_wait hw;
        hqq_status st;

        hw.timeout_ns = (int64_t)adv_slice_ns(w);
        hw.cancel = &a->stop;
        st = hqq_link_tx_frame(a->base, &hw, op, ops, p1, n1, p2, n2, check_peer);
        if (st == HQQ_OK) {
            return HQQ_OK;
        }
        if (st == HQQ_ERR_TIMEOUT) {
            hqq_status wst = adv_wait_check(a, w);
            if (wst != HQQ_OK) {
                return wst;
            }
            if (w->nowait) {
                return HQQ_ERR_TIMEOUT;
            }
            continue;
        }
        if (st == HQQ_ERR_CANCELED) {
            return HQQ_ERR_CLOSED;
        }
        return st;
    }
}

hqq_status adv_tx(hqq_adv *a, const adv_wait *w, unsigned op, const uint64_t ops[7], const void *p, size_t n,
                  int check_peer) {
    return adv_tx_parts(a, w, op, ops, p, n, NULL, 0, check_peer);
}

/* ---- Failure ----------------------------------------------------------------------------- */

/* The receive path failed (lock held): record the error once, fail every pending
 * call and open, wake everyone. */
void adv_fail_all_locked(hqq_adv *a, hqq_status err) {
    adv_call *c;
    adv_open *o;

    if (!a->rx_failed) {
        a->rx_failed = 1;
        a->rx_err = err;
    }
    for (c = a->calls; c != NULL; c = c->next) {
        if (!c->done) {
            c->done = 1;
            c->status = a->rx_err;
        }
    }
    for (o = a->opens; o != NULL; o = o->next) {
        if (!o->done) {
            o->done = 1;
            o->status = a->rx_err;
        }
    }
    adv_wake_locked(a);
}

/* ---- Control sender queue -------------------------------------------------------------- */

int adv_control_full_locked(const hqq_adv *a) { return a->ctl_count >= a->control_cap; }

/* Queues a payload-less reply (lock held). Under a full queue it is dropped and
 * counted (SPEC 10.3). A stream accept whose reply is dropped releases its slot. */
void adv_queue_control_locked(hqq_adv *a, unsigned op, const uint64_t ops[7], hqq_stream *accept) {
    adv_ctl_job *j;
    if (adv_control_full_locked(a)) {
        a->st_dropped++;
        if (accept != NULL) {
            a->accept_reserved--;
        }
        return;
    }
    j = &a->ctl[(a->ctl_head + a->ctl_count) % a->control_cap];
    j->op = op;
    memset(j->ops, 0, sizeof j->ops);
    if (ops != NULL) {
        memcpy(j->ops, ops, sizeof j->ops);
    }
    j->accept = accept;
    a->ctl_count++;
    adv_wake_locked(a);
}

/* ---- Abandoned and refused key sets ----------------------------------------------------- */

void adv_abandon_locked(hqq_adv *a, uint64_t conn, uint64_t msg_id) {
    uint64_t idx;
    if (a->abandoned_count == ADV_ABANDONED_CAP) {
        a->abandoned_head = (a->abandoned_head + 1) % ADV_ABANDONED_CAP;
        a->abandoned_count--;
    }
    idx = (a->abandoned_head + a->abandoned_count) % ADV_ABANDONED_CAP;
    a->abandoned[idx].conn = conn;
    a->abandoned[idx].msg_id = msg_id;
    a->abandoned[idx].kind = 0;
    a->abandoned_count++;
}

int adv_take_abandoned_locked(hqq_adv *a, uint64_t conn, uint64_t msg_id) {
    uint64_t i;
    for (i = 0; i < a->abandoned_count; i++) {
        uint64_t idx = (a->abandoned_head + i) % ADV_ABANDONED_CAP;
        if (a->abandoned[idx].conn == conn && a->abandoned[idx].msg_id == msg_id) {
            /* Remove by moving the newest entry into the hole (order is not significant). */
            uint64_t last = (a->abandoned_head + a->abandoned_count - 1) % ADV_ABANDONED_CAP;
            a->abandoned[idx] = a->abandoned[last];
            a->abandoned_count--;
            return 1;
        }
    }
    return 0;
}

void adv_refuse_locked(hqq_adv *a, uint64_t conn, uint64_t msg_id, uint32_t kind) {
    uint64_t idx;
    if (a->refused_count == ADV_REFUSED_CAP) {
        a->refused_head = (a->refused_head + 1) % ADV_REFUSED_CAP;
        a->refused_count--;
        a->refused_evicted = 1;
    }
    idx = (a->refused_head + a->refused_count) % ADV_REFUSED_CAP;
    a->refused[idx].conn = conn;
    a->refused[idx].msg_id = msg_id;
    a->refused[idx].kind = kind;
    a->refused_count++;
}

static uint64_t refused_find(const hqq_adv *a, uint64_t conn, uint64_t msg_id, uint32_t kind) {
    uint64_t i;
    for (i = 0; i < a->refused_count; i++) {
        uint64_t idx = (a->refused_head + i) % ADV_REFUSED_CAP;
        if (a->refused[idx].conn == conn && a->refused[idx].msg_id == msg_id && a->refused[idx].kind == kind) {
            return i;
        }
    }
    return UINT64_MAX;
}

int adv_is_refused_locked(hqq_adv *a, uint64_t conn, uint64_t msg_id, uint32_t kind) {
    return refused_find(a, conn, msg_id, kind) != UINT64_MAX;
}

void adv_forget_refused_locked(hqq_adv *a, uint64_t conn, uint64_t msg_id, uint32_t kind) {
    uint64_t i = refused_find(a, conn, msg_id, kind);
    uint64_t j;
    if (i == UINT64_MAX) {
        return;
    }
    /* Keep the ring order: shift the later entries down by one. */
    for (j = i; j + 1 < a->refused_count; j++) {
        uint64_t from = (a->refused_head + j + 1) % ADV_REFUSED_CAP;
        uint64_t to = (a->refused_head + j) % ADV_REFUSED_CAP;
        a->refused[to] = a->refused[from];
    }
    a->refused_count--;
}

/* ---- Partial messages (dispatcher only) -------------------------------------------------- */

/* Drops a partial message: its buffer and budget are released (lock held). */
void adv_drop_asm_locked(hqq_adv *a, adv_asm *x) {
    adv_asm **pp = &a->asms;
    a->assembly_bytes -= x->charge;
    if (x->msg != NULL) {
        adv_msg_put(x->msg);
        x->msg = NULL;
    }
    while (*pp != NULL && *pp != x) {
        pp = &(*pp)->next;
    }
    if (*pp == x) {
        *pp = x->next;
    }
    x->next = a->asm_free;
    a->asm_free = x;
}
