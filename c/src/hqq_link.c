/*
 * The Standard link: open core, peer monitor, heartbeat, blocking wait,
 * read / write / reservations, shutdown and close. Ports standard.go,
 * reservation.go and the waiter of internal/mpmc (SPEC 6, 8, 9).
 */
#define HQQ_BUILDING 1
#include "hqq_internal.h"

#include <stdint.h>

/* Wait tuning (SPEC 8.4, constants.json "wait"). */
#define HQQ_SPIN_ITERATIONS 64u
#define HQQ_YIELD_ITERATIONS 256u
#define HQQ_MAX_PARK_NS (100 * HQQ_NS_PER_MS)
#define HQQ_CHECK_INTERVAL_NS (50 * HQQ_NS_PER_MS)
#define HQQ_MIN_SLEEP_NS UINT64_C(20000)
#define HQQ_MAX_SLEEP_NS UINT64_C(2000000)
#define HQQ_CANCEL_PARK_NS (10 * HQQ_NS_PER_MS)
#define HQQ_CLOSE_POLL_NS UINT64_C(50000)
#define HQQ_RING_ATTACH_POLL_NS UINT64_C(100000)

#define LIFE_OPEN 0u
#define LIFE_CLOSING 1u
#define LIFE_CLOSED 2u

static const hqq_allocator *link_alloc(const hqq_link *l) { return l->alloc_set ? &l->alloc : NULL; }

/* ---- Peer monitor (SPEC 6.3) ------------------------------------------------------------ */

static hqq_status peer_status_err(uint32_t s) {
    if (s == HQQ_PEER_CLOSED) {
        return HQQ_ERR_PEER_CLOSED;
    }
    if (s == HQQ_PEER_GONE) {
        return HQQ_ERR_PEER_GONE;
    }
    return HQQ_OK;
}

static int64_t max64(int64_t a, int64_t b) { return a > b ? a : b; }

hqq_status hqq_link_check_peer(hqq_link *l) {
    hqq_status st = peer_status_err(atomic_load_explicit(&l->peer_status, memory_order_acquire));
    hqq_endpoint_record *rec;
    uint64_t tok, now, hb;
    int64_t interval;

    if (st != HQQ_OK) {
        return st;
    }
    hqq_spin_lock(&l->peer_lock);
    st = peer_status_err(atomic_load_explicit(&l->peer_status, memory_order_relaxed));
    if (st != HQQ_OK) {
        hqq_spin_unlock(&l->peer_lock);
        return st;
    }
    /* Re-initialized under us, or this endpoint was replaced. */
    if (atomic_load_explicit(&l->sb->epoch, memory_order_seq_cst) != l->epoch ||
        atomic_load_explicit(&l->sb->endpoints[l->self].token, memory_order_seq_cst) != l->token) {
        atomic_store_explicit(&l->peer_status, HQQ_PEER_GONE, memory_order_release);
        hqq_spin_unlock(&l->peer_lock);
        return HQQ_ERR_PEER_GONE;
    }
    rec = &l->sb->endpoints[1 - l->self];
    tok = atomic_load_explicit(&rec->token, memory_order_seq_cst);
    now = hqq_plat_mono_ns();
    if (tok == HQQ_DETACHED_TOKEN) {
        atomic_store_explicit(&l->peer_status, HQQ_PEER_CLOSED, memory_order_release);
        hqq_spin_unlock(&l->peer_lock);
        return HQQ_ERR_PEER_CLOSED;
    }
    if (l->peer_token == 0) {
        if (tok != 0) {
            l->peer_token = tok;
            l->peer_last_hb = atomic_load_explicit(&rec->heartbeat, memory_order_relaxed);
            l->peer_last_change_ns = now;
            atomic_store_explicit(&l->peer_status, HQQ_PEER_CONNECTED, memory_order_release);
        }
        hqq_spin_unlock(&l->peer_lock);
        return HQQ_OK;
    }
    if (tok != l->peer_token) {
        atomic_store_explicit(&l->peer_status, HQQ_PEER_GONE, memory_order_release);
        hqq_spin_unlock(&l->peer_lock);
        return HQQ_ERR_PEER_GONE;
    }
    interval = (int64_t)atomic_load_explicit(&rec->interval, memory_order_relaxed);
    if (interval <= 0) {
        hqq_spin_unlock(&l->peer_lock);
        return HQQ_OK; /* heartbeats disabled: assume alive */
    }
    hb = atomic_load_explicit(&rec->heartbeat, memory_order_relaxed);
    if (hb != l->peer_last_hb) {
        l->peer_last_hb = hb;
        l->peer_last_change_ns = now;
        hqq_spin_unlock(&l->peer_lock);
        return HQQ_OK;
    }
    if (now - l->peer_last_change_ns > (uint64_t)max64(l->peer_timeout_ns, 4 * interval)) {
        atomic_store_explicit(&l->peer_status, HQQ_PEER_GONE, memory_order_release);
        hqq_spin_unlock(&l->peer_lock);
        return HQQ_ERR_PEER_GONE;
    }
    hqq_spin_unlock(&l->peer_lock);
    return HQQ_OK;
}

static void wake_both(hqq_link *l) {
    hqq_ring_wake_all(&l->ring[0]);
    hqq_ring_wake_all(&l->ring[1]);
}

static void heartbeat_main(void *arg) {
    hqq_link *l = (hqq_link *)arg;
    hqq_endpoint_record *self = &l->sb->endpoints[l->self];

    while (!hqq_plat_event_wait(l->hb_stop, (uint64_t)l->heartbeat_interval_ns)) {
        if (atomic_load_explicit(&self->token, memory_order_seq_cst) == l->token) {
            atomic_store_explicit(&self->heartbeat, hqq_plat_wall_ns(), memory_order_relaxed);
        }
        if (hqq_link_check_peer(l) != HQQ_OK &&
            atomic_exchange_explicit(&l->hb_woken, 1, memory_order_relaxed) == 0) {
            /* Wake blocked calls so they report the failure without waiting for their next check. */
            wake_both(l);
        }
    }
}

/* ---- Open -------------------------------------------------------------------------------------- */

static hqq_status attach_ring(const hqq_ctx *ctx, hqq_ring *r, void *mem, uint64_t timeout_ns) {
    uint64_t deadline = hqq_plat_mono_ns() + timeout_ns;
    for (;;) {
        int rc;
        hqq_status st = hqq_ctx_check(ctx);
        if (st != HQQ_OK) {
            return st;
        }
        rc = hqq_ring_attach(r, mem);
        if (rc == HQQ_RING_OK) {
            return HQQ_OK;
        }
        if (rc == HQQ_RING_INCOMPATIBLE || hqq_plat_mono_ns() >= deadline) {
            return HQQ_ERR_INCOMPATIBLE_LAYOUT;
        }
        st = hqq_ctx_sleep(ctx, HQQ_RING_ATTACH_POLL_NS);
        if (st != HQQ_OK) {
            return st;
        }
    }
}

static void detach(hqq_link *l) {
    uint64_t expect = l->token;
    atomic_compare_exchange_strong_explicit(&l->sb->endpoints[l->self].token, &expect, HQQ_DETACHED_TOKEN,
                                            memory_order_seq_cst, memory_order_seq_cst);
}

hqq_status hqq_link_open_core(const hqq_open_req *req, uint64_t count, uint64_t size,
                              hqq_link **out, hqq_open_info *info) {
    hqq_claim_params p;
    hqq_layout lay;
    hqq_geometry got;
    hqq_superblock *sb = (hqq_superblock *)(void *)req->base;
    const hqq_allocator *al = req->alloc_set ? &req->alloc : NULL;
    hqq_link *l;
    hqq_status st;
    hqq_mode mode;
    int must_init, i, os_err = 0;
    uint64_t epoch;

    *out = NULL;
    if (!hqq_layout_compute(&lay, hqq_plat_page_size(), count, size, (uint64_t)INTPTR_MAX)) {
        return HQQ_ERR_INVALID_SIZE;
    }
    if (info != NULL) {
        info->want.buffer_count = lay.buffer_count;
        info->want.buffer_size = lay.buffer_size;
        info->want.layout_size = lay.total;
        info->want.page_size = lay.page_size;
    }
    memset(&p, 0, sizeof p);
    p.layout = lay;
    p.creator_token = req->creator_token;
    p.role = req->role;
    p.attach_timeout_ns = req->attach_timeout_ns;
    p.peer_timeout_ns = req->peer_timeout_ns;
    p.init_stale_ns = req->init_stale_ns;
    p.heartbeat_interval_ns = req->heartbeat_interval_ns;
    p.ctx = req->ctx;
    if (hqq_new_token(&p.token) != 0) {
        if (info != NULL) {
            info->os_error = 0;
        }
        return HQQ_ERR_SYSTEM;
    }

    l = (hqq_link *)hqq_alloc(al, sizeof *l);
    if (l == NULL) {
        return HQQ_ERR_NO_MEMORY;
    }
    memset(l, 0, sizeof *l);

    st = hqq_sb_claim(sb, &p, &mode, &must_init, &got);
    if (st != HQQ_OK) {
        if (st == HQQ_ERR_GEOMETRY_MISMATCH && info != NULL) {
            info->got = got;
        }
        hqq_free(al, l, sizeof *l);
        return st;
    }
    if (must_init) {
        st = hqq_sb_initialize(req->base, &p, &epoch);
        if (st != HQQ_OK) {
            hqq_free(al, l, sizeof *l); /* leaves the segment Initializing: taken over later */
            return st;
        }
    } else {
        epoch = atomic_load_explicit(&sb->epoch, memory_order_seq_cst);
    }

    l->alloc = req->alloc;
    l->alloc_set = req->alloc_set;
    l->mode = mode;
    l->self = mode == HQQ_MODE_SECONDARY ? HQQ_ENDPOINT_SECONDARY : HQQ_ENDPOINT_PRIMARY;
    l->buffer_count = count;
    l->buffer_size = size;
    l->layout = lay;
    l->policy = req->policy;
    l->heartbeat_interval_ns = req->heartbeat_interval_ns;
    l->peer_timeout_ns = req->peer_timeout_ns;
    l->attach_timeout_ns = req->attach_timeout_ns;
    l->sb = sb;
    l->base = req->base;
    l->token = p.token;
    l->epoch = epoch;
    atomic_flag_clear(&l->rx_lock.f);
    atomic_flag_clear(&l->peer_lock.f);
    atomic_init(&l->copy_id, 0);
    atomic_init(&l->peer_status, HQQ_PEER_NOT_CONNECTED);
    atomic_init(&l->lifecycle, LIFE_OPEN);
    atomic_init(&l->hb_woken, 0);
    atomic_init(&l->rx_active, 0);
    atomic_init(&l->tx_active, 0);

    {
        const uint64_t roff[2] = {lay.ring0, lay.ring1};
        const uint64_t boff[2] = {lay.buf0, lay.buf1};
        for (i = 0; i < 2; i++) {
            st = attach_ring(&req->ctx, &l->ring[i], req->base + roff[i], (uint64_t)req->attach_timeout_ns);
            if (st != HQQ_OK) {
                detach(l);
                hqq_free(al, l, sizeof *l);
                return st;
            }
            if (l->ring[i].size != count) {
                detach(l);
                hqq_free(al, l, sizeof *l);
                return HQQ_ERR_INCOMPATIBLE_LAYOUT;
            }
            l->bufs[i] = req->base + boff[i];
            hqq_plat_wait_init(&l->wait[i], req->named != NULL ? req->named->shm : NULL, i, req->flags);
            l->ring[i].ops = hqq_plat_ring_ops();
            l->ring[i].ops_ctx = &l->wait[i];
        }
    }
    if (mode == HQQ_MODE_SECONDARY) {
        l->tx = &l->ring[1];
        l->txbuf = l->bufs[1];
        l->rx = &l->ring[0];
        l->rxbuf = l->bufs[0];
    } else {
        l->tx = &l->ring[0];
        l->txbuf = l->bufs[0];
        l->rx = &l->ring[1];
        l->rxbuf = l->bufs[1];
    }

    /* A Secondary attaches to a live primary: start monitoring it now. */
    if (mode == HQQ_MODE_SECONDARY) {
        (void)hqq_link_check_peer(l);
    }
    if (l->heartbeat_interval_ns > 0) {
        l->hb_stop = hqq_plat_event_create(al);
        if (l->hb_stop != NULL) {
            l->hb_thread = hqq_plat_thread_start(al, heartbeat_main, l, &os_err);
        }
        if (l->hb_thread == NULL) {
            st = l->hb_stop == NULL ? HQQ_ERR_NO_MEMORY : HQQ_ERR_SYSTEM;
            hqq_plat_event_destroy(al, l->hb_stop);
            detach(l);
            hqq_free(al, l, sizeof *l);
            if (info != NULL) {
                info->os_error = os_err;
            }
            return st;
        }
    }
    l->named = req->named;
    *out = l;
    return HQQ_OK;
}

hqq_status hqq_standard_open_memory(void *mem, size_t mem_size, uint64_t buffer_count, uint64_t buffer_size,
                                    const hqq_options *opts, hqq_link **out, hqq_open_info *info) {
    hqq_open_req req;
    hqq_status st;
    size_t need;

    if (out == NULL) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    *out = NULL;
    if (info != NULL) {
        info->os_error = 0;
        memset(&info->want, 0, sizeof info->want);
        memset(&info->got, 0, sizeof info->got);
    }
    memset(&req, 0, sizeof req);
    st = hqq_options_normalize(opts, &req, info);
    if (st != HQQ_OK) {
        return st;
    }
    if (mem == NULL || mem_size == 0) {
        return HQQ_ERR_MEMORY_SMALL;
    }
    if ((uintptr_t)mem % hqq_plat_page_size() != 0) {
        return HQQ_ERR_MEMORY_ALIGN;
    }
    need = hqq_standard_size(buffer_count, buffer_size);
    if (need == 0) {
        return HQQ_ERR_INVALID_SIZE;
    }
    if (mem_size < need) {
        return HQQ_ERR_MEMORY_SMALL;
    }
    req.base = (unsigned char *)mem;
    req.role = HQQ_ROLE_ANY;
    req.creator_token = 0;
    req.named = NULL;
    return hqq_link_open_core(&req, buffer_count, buffer_size, out, info);
}

size_t hqq_standard_size(uint64_t buffer_count, uint64_t buffer_size) {
    hqq_layout l;
    if (!hqq_layout_compute(&l, hqq_plat_page_size(), buffer_count, buffer_size, (uint64_t)INTPTR_MAX)) {
        return 0;
    }
    return (size_t)l.total;
}

/* ---- Entering and leaving calls ------------------------------------------------------------ */

/* The counter is incremented before the closed flag is read; shutdown sets the flag before it
 * waits for the counters, so no call can touch shared memory after hqq_shutdown returns. */
static hqq_status enter_rx(hqq_link *l) {
    atomic_fetch_add_explicit(&l->rx_active, 1, memory_order_seq_cst);
    if (atomic_load_explicit(&l->lifecycle, memory_order_seq_cst) != LIFE_OPEN) {
        atomic_fetch_sub_explicit(&l->rx_active, 1, memory_order_seq_cst);
        return HQQ_ERR_CLOSED;
    }
    return HQQ_OK;
}

static void exit_rx(hqq_link *l) { atomic_fetch_sub_explicit(&l->rx_active, 1, memory_order_seq_cst); }

static hqq_status enter_tx(hqq_link *l, int check_peer) {
    atomic_fetch_add_explicit(&l->tx_active, 1, memory_order_seq_cst);
    if (atomic_load_explicit(&l->lifecycle, memory_order_seq_cst) != LIFE_OPEN) {
        atomic_fetch_sub_explicit(&l->tx_active, 1, memory_order_seq_cst);
        return HQQ_ERR_CLOSED;
    }
    if (check_peer) {
        hqq_status st = peer_status_err(atomic_load_explicit(&l->peer_status, memory_order_acquire));
        if (st != HQQ_OK) {
            atomic_fetch_sub_explicit(&l->tx_active, 1, memory_order_seq_cst);
            return st;
        }
    }
    return HQQ_OK;
}

static void exit_tx(hqq_link *l) { atomic_fetch_sub_explicit(&l->tx_active, 1, memory_order_seq_cst); }

/* ---- Blocking wait (SPEC 8.4) --------------------------------------------------------------- */

typedef struct hqq_call {
    hqq_link *l;
    const hqq_cancel *cancel;
    uint64_t deadline_ns; /* absolute monotonic; 0 = none */
    int nowait;
    unsigned iter;
    uint64_t next_check_ns;
    uint64_t sleep_ns;
} hqq_call;

static void call_init(hqq_call *c, hqq_link *l, const hqq_wait *w) {
    memset(c, 0, sizeof *c);
    c->l = l;
    if (w != NULL) {
        c->cancel = w->cancel;
        if (w->timeout_ns == 0) {
            c->nowait = 1;
        } else if (w->timeout_ns > 0 && (uint64_t)w->timeout_ns < (UINT64_MAX >> 2)) {
            c->deadline_ns = hqq_plat_mono_ns() + (uint64_t)w->timeout_ns;
        }
    }
}

static int call_stop(void *arg) {
    const hqq_call *c = (const hqq_call *)arg;
    return atomic_load_explicit(&c->l->lifecycle, memory_order_seq_cst) != LIFE_OPEN ||
           hqq_cancel_load(c->cancel);
}

static hqq_status call_poll(const hqq_call *c) {
    if (atomic_load_explicit(&c->l->lifecycle, memory_order_seq_cst) != LIFE_OPEN) {
        return HQQ_ERR_CLOSED;
    }
    if (hqq_cancel_load(c->cancel)) {
        return HQQ_ERR_CANCELED;
    }
    return HQQ_OK;
}

static hqq_status call_slow_checks(hqq_call *c, uint64_t now) {
    hqq_status st;
    if (c->deadline_ns != 0 && now >= c->deadline_ns) {
        return HQQ_ERR_TIMEOUT;
    }
    st = peer_status_err(atomic_load_explicit(&c->l->peer_status, memory_order_acquire));
    if (st != HQQ_OK) {
        return st;
    }
    if (c->next_check_ns == 0 || now >= c->next_check_ns) {
        c->next_check_ns = now + HQQ_CHECK_INTERVAL_NS;
        return hqq_link_check_peer(c->l);
    }
    return HQQ_OK;
}

static void backoff_sleep(hqq_call *c, uint64_t limit_ns) {
    if (c->sleep_ns == 0) {
        c->sleep_ns = HQQ_MIN_SLEEP_NS;
    } else if ((c->sleep_ns *= 2) > HQQ_MAX_SLEEP_NS) {
        c->sleep_ns = HQQ_MAX_SLEEP_NS;
    }
    hqq_plat_sleep_ns(c->sleep_ns < limit_ns ? c->sleep_ns : limit_ns);
}

/*
 * One backoff step while slot `pos` of `ring` has not reached `target`. A
 * non-OK status means the operation must be abandoned.
 */
static hqq_status call_wait_step(hqq_call *c, hqq_ring *ring, int side, uint64_t pos, uint64_t target) {
    hqq_link *l = c->l;
    hqq_status st = call_poll(c);
    uint64_t now, timeout;
    int kind;

    if (st != HQQ_OK) {
        return st;
    }
    if (c->nowait) {
        return HQQ_ERR_TIMEOUT;
    }
    c->iter++;

    if (l->policy == HQQ_WAIT_SPIN) {
        if ((c->iter & 63u) == 0) {
            st = call_slow_checks(c, hqq_plat_mono_ns());
            if (st != HQQ_OK) {
                return st;
            }
            hqq_plat_yield();
        }
        return HQQ_OK;
    }
    if (c->iter <= HQQ_SPIN_ITERATIONS) {
        return HQQ_OK;
    }
    if (c->iter <= HQQ_SPIN_ITERATIONS + HQQ_YIELD_ITERATIONS) {
        if ((c->iter & 15u) == 0) {
            st = call_slow_checks(c, hqq_plat_mono_ns());
            if (st != HQQ_OK) {
                return st;
            }
        }
        hqq_plat_yield();
        return HQQ_OK;
    }

    now = hqq_plat_mono_ns();
    st = call_slow_checks(c, now);
    if (st != HQQ_OK) {
        return st;
    }
    timeout = HQQ_MAX_PARK_NS;
    if (c->deadline_ns != 0) {
        timeout = c->deadline_ns > now ? c->deadline_ns - now : 0;
        if (timeout > HQQ_MAX_PARK_NS) {
            timeout = HQQ_MAX_PARK_NS;
        }
    }
    if (c->next_check_ns > now && c->next_check_ns - now < timeout) {
        timeout = c->next_check_ns - now;
    }
    if (c->cancel != NULL && timeout > HQQ_CANCEL_PARK_NS) {
        timeout = HQQ_CANCEL_PARK_NS;
    }
    if (timeout == 0) {
        return HQQ_OK;
    }

    kind = l->wait[ring == &l->ring[0] ? 0 : 1].kind;
    if (l->policy == HQQ_WAIT_SIMPLE || kind == HQQ_PARK_NONE || ring->ops == NULL ||
        ring->ops->wait == NULL) {
        backoff_sleep(c, timeout);
        return HQQ_OK;
    }
    if (kind == HQQ_PARK_LOCAL) {
        /* Wakes come only from this process: bound the park by the backoff step. */
        if (c->sleep_ns == 0) {
            c->sleep_ns = HQQ_MIN_SLEEP_NS;
        } else if ((c->sleep_ns *= 2) > HQQ_MAX_SLEEP_NS) {
            c->sleep_ns = HQQ_MAX_SLEEP_NS;
        }
        if (c->sleep_ns < timeout) {
            timeout = c->sleep_ns;
        }
    }
    hqq_ring_park(ring, side, pos, target, timeout, call_stop, c);
    return HQQ_OK;
}

/* ---- Claiming ---------------------------------------------------------------------------------- */

static hqq_status tx_claim(hqq_link *l, hqq_call *c, uint64_t *pos) {
    for (;;) {
        uint64_t p;
        hqq_status st;
        if (hqq_ring_try_claim_producer(l->tx, &p)) {
            *pos = p;
            return HQQ_OK;
        }
        st = call_wait_step(c, l->tx, HQQ_RING_NOT_FULL, p, p);
        if (st != HQQ_OK) {
            return st;
        }
    }
}

static hqq_status status_from_code(uint64_t code) {
    switch (code) {
    case HQQ_ERRCODE_MEMORY_ALIGN: return HQQ_ERR_MEMORY_ALIGN;
    case HQQ_ERRCODE_INVALID_SIZE: return HQQ_ERR_INVALID_SIZE;
    case HQQ_ERRCODE_MEMORY_SMALL: return HQQ_ERR_MEMORY_SMALL;
    case HQQ_ERRCODE_FAILED_INIT: return HQQ_ERR_FAILED_INIT;
    case HQQ_ERRCODE_CONN_NOT_FOUND: return HQQ_ERR_CONN_NOT_FOUND;
    case HQQ_ERRCODE_INVALID_OP: return HQQ_ERR_INVALID_OP;
    case HQQ_ERRCODE_BUFFER_OVERFLOW: return HQQ_ERR_BUFFER_OVERFLOW;
    case 8: return HQQ_ERR_INVALID_OP; /* Go ErrShuttingDown wraps ErrInvalidOp */
    default: return HQQ_ERR_PROTOCOL;
    }
}

/*
 * Claims the next deliverable message of the receive ring (SPEC 9.4). On HQQ_OK
 * the slot *pos is claimed and *size bytes of its buffer are the message; the
 * caller reads them and then releases the slot. If the message is larger than
 * cap, the slot is NOT claimed: HQQ_ERR_BUFFER_TOO_SMALL with *size = required.
 * Tombstones are skipped; slots with an invalid, error or foreign packet are
 * released and reported.
 *
 * Consumer threads of this process are serialized by rx_lock, which makes the
 * peek-then-claim sequence exact (the protocol admits one consumer process).
 * After the peer closed or died the ring is drained before the failure is
 * reported (SPEC 6.4).
 */
static hqq_status rx_next(hqq_link *l, hqq_call *c, size_t cap, uint64_t *pos, size_t *size) {
    hqq_ring *r = l->rx;
    int peer_failed = 0;
    hqq_status peer_err = HQQ_OK;

    for (;;) {
        uint64_t p;
        hqq_status st;

        hqq_spin_lock(&l->rx_lock);
        if (hqq_ring_peek_consumer(r, &p)) {
            hqq_packet pkt;
            uint64_t n;

            memcpy(&pkt, hqq_ring_slot(r, p)->packet, sizeof pkt);
            switch (hqq_packet_op(&pkt)) {
            case HQQ_OP_STD_COPY:
                n = hqq_packet_operand(&pkt, 2);
                if (n == 0 || n > l->buffer_size) {
                    st = HQQ_ERR_INVALID_SIZE;
                    break;
                }
                if (n > cap) {
                    hqq_spin_unlock(&l->rx_lock);
                    *size = (size_t)n;
                    return HQQ_ERR_BUFFER_TOO_SMALL;
                }
                if (!hqq_ring_claim_consumer_at(r, p)) {
                    hqq_spin_unlock(&l->rx_lock);
                    continue;
                }
                hqq_spin_unlock(&l->rx_lock);
                *pos = p;
                *size = (size_t)n;
                return HQQ_OK;
            case HQQ_OP_STD_TOMBSTONE:
                st = HQQ_OK; /* skip */
                break;
            case HQQ_OP_ERROR:
                st = status_from_code(hqq_packet_operand(&pkt, 1));
                break;
            default:
                st = HQQ_ERR_UNEXPECTED_PACKET;
                break;
            }
            /* The packet is dropped: claim and release its slot. */
            if (hqq_ring_claim_consumer_at(r, p)) {
                hqq_ring_release(r, p);
            }
            hqq_spin_unlock(&l->rx_lock);
            if (st != HQQ_OK) {
                return st;
            }
            continue; /* tombstone: next packet */
        }
        hqq_spin_unlock(&l->rx_lock);

        if (peer_failed) {
            return peer_err; /* drained: report the failure */
        }
        st = call_wait_step(c, r, HQQ_RING_NOT_EMPTY, p, p + 1);
        if (st == HQQ_ERR_PEER_CLOSED || st == HQQ_ERR_PEER_GONE) {
            peer_failed = 1;
            peer_err = st;
            continue; /* one more non-blocking attempt */
        }
        if (st != HQQ_OK) {
            return st;
        }
    }
}

static unsigned char *rx_buffer(const hqq_link *l, uint64_t pos) {
    return l->rxbuf + (size_t)(pos & l->rx->mask) * (size_t)l->buffer_size;
}

static unsigned char *tx_buffer(const hqq_link *l, uint64_t pos) {
    return l->txbuf + (size_t)(pos & l->tx->mask) * (size_t)l->buffer_size;
}

/* ---- Write --------------------------------------------------------------------------------------- */

static void store_packet(hqq_ring *r, uint64_t pos, const hqq_packet *pkt) {
    memcpy(hqq_ring_slot(r, pos)->packet, pkt, sizeof *pkt);
}

static void publish_copy(hqq_link *l, uint64_t pos, uint64_t copy_id, size_t n) {
    hqq_packet pkt;
    hqq_packet_make(&pkt, HQQ_OP_STD_COPY, copy_id, pos & l->tx->mask, n);
    store_packet(l->tx, pos, &pkt);
    hqq_ring_publish(l->tx, pos);
}

static void publish_tombstone(hqq_link *l, uint64_t pos, uint64_t copy_id, unsigned reason) {
    hqq_packet pkt;
    hqq_packet_make(&pkt, HQQ_OP_STD_TOMBSTONE, copy_id, pos & l->tx->mask, reason);
    store_packet(l->tx, pos, &pkt);
    hqq_ring_publish(l->tx, pos);
}

hqq_status hqq_write(hqq_link *l, const void *data, size_t len, const hqq_wait *wait) {
    hqq_call c;
    hqq_status st;
    uint64_t pos, copy_id;

    if (l == NULL || (data == NULL && len != 0)) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    if (len == 0) {
        return HQQ_OK;
    }
    if (len > l->buffer_size) {
        return HQQ_ERR_BUFFER_OVERFLOW;
    }
    st = enter_tx(l, 1);
    if (st != HQQ_OK) {
        return st;
    }
    copy_id = atomic_fetch_add_explicit(&l->copy_id, 1, memory_order_relaxed) + 1;
    call_init(&c, l, wait);
    st = tx_claim(l, &c, &pos);
    if (st == HQQ_OK) {
        memcpy(tx_buffer(l, pos), data, len);
        publish_copy(l, pos, copy_id, len);
    }
    exit_tx(l);
    return st;
}

hqq_status hqq_write_zc(hqq_link *l, hqq_write_fn fn, void *user, size_t *len, const hqq_wait *wait) {
    hqq_call c;
    hqq_status st, fst;
    uint64_t pos, copy_id;
    size_t n = 0;

    if (l == NULL || fn == NULL) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    st = enter_tx(l, 1);
    if (st != HQQ_OK) {
        return st;
    }
    copy_id = atomic_fetch_add_explicit(&l->copy_id, 1, memory_order_relaxed) + 1;
    call_init(&c, l, wait);
    st = tx_claim(l, &c, &pos);
    if (st != HQQ_OK) {
        exit_tx(l);
        return st;
    }
    /* A claimed slot MUST always be published: a tombstone if the write is abandoned. */
    fst = fn(user, tx_buffer(l, pos), (size_t)l->buffer_size, &n);
    if (fst != HQQ_OK) {
        publish_tombstone(l, pos, copy_id, HQQ_ERRCODE_INVALID_OP);
        st = fst;
    } else if (n == 0) {
        publish_tombstone(l, pos, copy_id, HQQ_ERRCODE_INVALID_SIZE);
        st = HQQ_ERR_INVALID_SIZE;
    } else if (n > l->buffer_size) {
        publish_tombstone(l, pos, copy_id, HQQ_ERRCODE_BUFFER_OVERFLOW);
        st = HQQ_ERR_BUFFER_OVERFLOW;
    } else {
        publish_copy(l, pos, copy_id, n);
        if (len != NULL) {
            *len = n;
        }
    }
    exit_tx(l);
    return st;
}

hqq_status hqq_write_reserve(hqq_link *l, hqq_write_reservation *res, const hqq_wait *wait) {
    hqq_call c;
    hqq_status st;
    uint64_t pos;

    if (res == NULL) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    memset(res, 0, sizeof *res);
    if (l == NULL) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    st = enter_tx(l, 1);
    if (st != HQQ_OK) {
        return st;
    }
    call_init(&c, l, wait);
    st = tx_claim(l, &c, &pos);
    if (st == HQQ_OK) {
        res->link = l;
        res->pos = pos;
        res->copy_id = atomic_fetch_add_explicit(&l->copy_id, 1, memory_order_relaxed) + 1;
        res->data = tx_buffer(l, pos);
        res->capacity = (size_t)l->buffer_size;
        res->slot = (uint32_t)(pos & l->tx->mask);
    }
    exit_tx(l);
    return st;
}

/* Publishes pkt for the reservation through the completion guard. */
static hqq_status reservation_complete(hqq_write_reservation *res, const hqq_packet *pkt) {
    hqq_link *l = res->link;
    hqq_status st = enter_tx(l, 0);
    if (st != HQQ_OK) {
        return st;
    }
    store_packet(l->tx, res->pos, pkt);
    res->data = NULL;
    if (!hqq_ring_publish_guarded(l->tx, res->pos)) {
        st = HQQ_ERR_INVALID_OP;
    }
    exit_tx(l);
    return st;
}

hqq_status hqq_write_commit(hqq_write_reservation *res, size_t len) {
    hqq_packet pkt;
    hqq_status st, ret = HQQ_OK;

    if (res == NULL || res->link == NULL || res->data == NULL || !hqq_ring_producer_owned(res->link->tx, res->pos)) {
        return HQQ_ERR_INVALID_OP;
    }
    if (len == 0) {
        hqq_packet_make(&pkt, HQQ_OP_STD_TOMBSTONE, res->copy_id, res->slot, HQQ_ERRCODE_INVALID_SIZE);
        ret = HQQ_ERR_INVALID_SIZE;
    } else if (len > res->capacity) {
        hqq_packet_make(&pkt, HQQ_OP_STD_TOMBSTONE, res->copy_id, res->slot, HQQ_ERRCODE_BUFFER_OVERFLOW);
        ret = HQQ_ERR_BUFFER_OVERFLOW;
    } else {
        hqq_packet_make(&pkt, HQQ_OP_STD_COPY, res->copy_id, res->slot, len);
    }
    st = reservation_complete(res, &pkt);
    return st != HQQ_OK ? st : ret;
}

hqq_status hqq_write_abort(hqq_write_reservation *res, unsigned reason) {
    hqq_packet pkt;

    if (res == NULL || res->link == NULL || res->data == NULL || !hqq_ring_producer_owned(res->link->tx, res->pos)) {
        return HQQ_ERR_INVALID_OP;
    }
    if (reason == 0) {
        reason = HQQ_ERRCODE_INVALID_OP;
    }
    hqq_packet_make(&pkt, HQQ_OP_STD_TOMBSTONE, res->copy_id, res->slot, reason);
    return reservation_complete(res, &pkt);
}

/* ---- Read ---------------------------------------------------------------------------------------- */

hqq_status hqq_read(hqq_link *l, void *buf, size_t cap, size_t *len, const hqq_wait *wait) {
    hqq_call c;
    hqq_status st;
    uint64_t pos;
    size_t n = 0;

    if (l == NULL || len == NULL || (buf == NULL && cap != 0)) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    st = enter_rx(l);
    if (st != HQQ_OK) {
        return st;
    }
    call_init(&c, l, wait);
    st = rx_next(l, &c, cap, &pos, &n);
    if (st == HQQ_OK) {
        memcpy(buf, rx_buffer(l, pos), n);
        hqq_ring_release(l->rx, pos);
        *len = n;
    } else if (st == HQQ_ERR_BUFFER_TOO_SMALL) {
        *len = n;
    }
    exit_rx(l);
    return st;
}

hqq_status hqq_read_zc(hqq_link *l, hqq_read_fn fn, void *user, size_t *len, const hqq_wait *wait) {
    hqq_call c;
    hqq_status st, fst;
    uint64_t pos;
    size_t n = 0;

    if (l == NULL || fn == NULL) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    st = enter_rx(l);
    if (st != HQQ_OK) {
        return st;
    }
    call_init(&c, l, wait);
    st = rx_next(l, &c, SIZE_MAX, &pos, &n);
    if (st == HQQ_OK) {
        fst = fn(user, rx_buffer(l, pos), n);
        hqq_ring_release(l->rx, pos); /* released even if fn failed */
        if (len != NULL) {
            *len = n;
        }
        st = fst;
    }
    exit_rx(l);
    return st;
}

hqq_status hqq_read_reserve(hqq_link *l, hqq_read_reservation *res, const hqq_wait *wait) {
    hqq_call c;
    hqq_status st;
    uint64_t pos;
    size_t n = 0;

    if (res == NULL) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    memset(res, 0, sizeof *res);
    if (l == NULL) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    st = enter_rx(l);
    if (st != HQQ_OK) {
        return st;
    }
    call_init(&c, l, wait);
    st = rx_next(l, &c, SIZE_MAX, &pos, &n);
    if (st == HQQ_OK) {
        res->link = l;
        res->pos = pos;
        res->data = rx_buffer(l, pos);
        res->size = n;
        res->slot = (uint32_t)(pos & l->rx->mask);
    }
    exit_rx(l);
    return st;
}

hqq_status hqq_read_release(hqq_read_reservation *res) {
    hqq_link *l;
    hqq_status st;

    if (res == NULL || res->link == NULL || res->data == NULL) {
        return HQQ_ERR_INVALID_OP;
    }
    l = res->link;
    if (!hqq_ring_consumer_owned(l->rx, res->pos)) {
        return HQQ_ERR_INVALID_OP;
    }
    st = enter_rx(l);
    if (st != HQQ_OK) {
        return st;
    }
    res->data = NULL;
    if (!hqq_ring_release_guarded(l->rx, res->pos)) {
        st = HQQ_ERR_INVALID_OP;
    }
    exit_rx(l);
    return st;
}

/* ---- Introspection --------------------------------------------------------------------------------- */

hqq_peer_status hqq_peer_state(hqq_link *l) {
    if (l == NULL) {
        return HQQ_PEER_NOT_CONNECTED;
    }
    if (enter_rx(l) == HQQ_OK) {
        (void)hqq_link_check_peer(l);
        exit_rx(l);
    }
    return (hqq_peer_status)atomic_load_explicit(&l->peer_status, memory_order_acquire);
}

hqq_mode hqq_link_mode(const hqq_link *l) { return l->mode; }
size_t hqq_link_buffer_size(const hqq_link *l) { return l == NULL ? 0 : (size_t)l->buffer_size; }
size_t hqq_link_buffer_count(const hqq_link *l) { return l == NULL ? 0 : (size_t)l->buffer_count; }
const char *hqq_link_name(const hqq_link *l) { return (l == NULL || l->named == NULL) ? NULL : l->named->name; }

/* ---- Shutdown, close ----------------------------------------------------------------------------------- */

void hqq_shutdown(hqq_link *l) {
    uint32_t expect = LIFE_OPEN;

    if (l == NULL) {
        return;
    }
    if (!atomic_compare_exchange_strong_explicit(&l->lifecycle, &expect, LIFE_CLOSING,
                                                 memory_order_seq_cst, memory_order_seq_cst)) {
        while (atomic_load_explicit(&l->lifecycle, memory_order_seq_cst) != LIFE_CLOSED) {
            hqq_plat_sleep_ns(HQQ_CLOSE_POLL_NS);
        }
        return;
    }
    wake_both(l);
    while (atomic_load_explicit(&l->rx_active, memory_order_seq_cst) != 0 ||
           atomic_load_explicit(&l->tx_active, memory_order_seq_cst) != 0) {
        hqq_plat_sleep_ns(HQQ_CLOSE_POLL_NS);
    }
    if (l->hb_thread != NULL) {
        hqq_plat_event_signal(l->hb_stop);
        hqq_plat_thread_join(link_alloc(l), l->hb_thread);
        l->hb_thread = NULL;
        hqq_plat_event_destroy(link_alloc(l), l->hb_stop);
        l->hb_stop = NULL;
    }
    detach(l);
    wake_both(l); /* let the peer's blocked calls notice the detach promptly */
    atomic_store_explicit(&l->lifecycle, LIFE_CLOSED, memory_order_seq_cst);
}

hqq_status hqq_close(hqq_link *l) {
    hqq_status st = HQQ_OK;
    hqq_allocator a;
    int a_set;

    if (l == NULL) {
        return HQQ_OK;
    }
    hqq_shutdown(l);
    a = l->alloc;
    a_set = l->alloc_set;
    if (l->named != NULL) {
        st = hqq_named_release(a_set ? &a : NULL, l->named);
        l->named = NULL;
    }
    hqq_free(a_set ? &a : NULL, l, sizeof *l);
    return st;
}

hqq_status hqq_link_unlink(hqq_link *l) {
    if (l == NULL) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    if (l->named == NULL) {
        return HQQ_OK;
    }
    return hqq_named_unlink(l->named);
}

/* ---- Frame access for the Advanced layer (internal, see hqq_internal.h) ---------------------- */

hqq_status hqq_link_rx_frame(hqq_link *l, const hqq_wait *w, hqq_packet *pkt, uint64_t *pos) {
    hqq_call c;
    hqq_ring *r = l->rx;
    int peer_failed = 0;
    hqq_status peer_err = HQQ_OK;
    hqq_status st = enter_rx(l);

    if (st != HQQ_OK) {
        return st;
    }
    call_init(&c, l, w);
    for (;;) {
        uint64_t p;

        hqq_spin_lock(&l->rx_lock);
        if (hqq_ring_peek_consumer(r, &p)) {
            hqq_packet tmp;

            memcpy(&tmp, hqq_ring_slot(r, p)->packet, sizeof tmp);
            if (hqq_packet_op(&tmp) == HQQ_OP_STD_TOMBSTONE) {
                if (hqq_ring_claim_consumer_at(r, p)) {
                    hqq_ring_release(r, p);
                }
                hqq_spin_unlock(&l->rx_lock);
                continue;
            }
            if (hqq_ring_claim_consumer_at(r, p)) {
                hqq_spin_unlock(&l->rx_lock);
                *pkt = tmp;
                *pos = p;
                exit_rx(l);
                return HQQ_OK;
            }
            hqq_spin_unlock(&l->rx_lock);
            continue;
        }
        hqq_spin_unlock(&l->rx_lock);
        if (peer_failed) {
            exit_rx(l);
            return peer_err; /* drained: report the failure (SPEC 6.4) */
        }
        st = call_wait_step(&c, r, HQQ_RING_NOT_EMPTY, p, p + 1);
        if (st == HQQ_ERR_PEER_CLOSED || st == HQQ_ERR_PEER_GONE) {
            peer_failed = 1;
            peer_err = st;
            continue;
        }
        if (st != HQQ_OK) {
            exit_rx(l);
            return st;
        }
    }
}

void hqq_link_rx_release(hqq_link *l, uint64_t pos) { hqq_ring_release(l->rx, pos); }

unsigned char *hqq_link_rx_payload(hqq_link *l, uint64_t pos) { return rx_buffer(l, pos); }

hqq_status hqq_link_tx_frame(hqq_link *l, const hqq_wait *w, unsigned op, const uint64_t ops[7],
                             const void *p1, size_t n1, const void *p2, size_t n2, int check_peer) {
    hqq_call c;
    hqq_packet pkt;
    uint64_t pos;
    hqq_status st;

    if (n1 + n2 > l->buffer_size || n1 + n2 < n1) {
        return HQQ_ERR_BUFFER_OVERFLOW;
    }
    st = enter_tx(l, check_peer);
    if (st != HQQ_OK) {
        return st;
    }
    call_init(&c, l, w);
    st = tx_claim(l, &c, &pos);
    if (st == HQQ_OK) {
        unsigned i;
        if (n1 != 0) {
            memcpy(tx_buffer(l, pos), p1, n1);
        }
        if (n2 != 0) {
            memcpy(tx_buffer(l, pos) + n1, p2, n2);
        }
        memset(&pkt, 0, sizeof pkt);
        pkt.w[0] = op;
        for (i = 0; i < 7; i++) {
            pkt.w[1u + i] = ops != NULL ? ops[i] : 0;
        }
        store_packet(l->tx, pos, &pkt);
        hqq_ring_publish(l->tx, pos);
    }
    exit_tx(l);
    return st;
}
