/*
 * Superblock engine: claim loop, takeover, reuse, join, initialization,
 * publish and liveness at open (SPEC 5, 6.2). A line-by-line port of
 * superblock.go; every cold-path word is accessed seq_cst (MEMORY_MODEL 1),
 * heartbeat words relaxed.
 */
#define HQQ_BUILDING 1
#include "hqq_internal.h"

#define LD(p) atomic_load_explicit((p), memory_order_seq_cst)
#define ST(p, v) atomic_store_explicit((p), (v), memory_order_seq_cst)
#define LD_RLX(p) atomic_load_explicit((p), memory_order_relaxed)
#define ST_RLX(p, v) atomic_store_explicit((p), (v), memory_order_relaxed)

static inline int cas_u64(_Atomic uint64_t *p, uint64_t expected, uint64_t desired) {
    return atomic_compare_exchange_strong_explicit(p, &expected, desired, memory_order_seq_cst,
                                                   memory_order_seq_cst);
}

/* heartbeatFutureSlack: a timestamp further in the future than this says
 * nothing about liveness (wall clock stepped backwards): confirm by watching. */
#define HQQ_HEARTBEAT_FUTURE_SLACK_NS INT64_C(1000000000)
#define HQQ_CLAIM_POLL_NS UINT64_C(200000)
#define HQQ_RING_ATTACH_POLL_NS UINT64_C(100000)

static uint64_t now_unix_nanos(void) { return hqq_plat_wall_ns(); }

static int64_t max_i64(int64_t a, int64_t b) { return a > b ? a : b; }
static int64_t min_i64(int64_t a, int64_t b) { return a < b ? a : b; }

int hqq_sb_first_page_zero(hqq_superblock *sb, uint64_t page_size) {
    _Atomic uint64_t *w = (_Atomic uint64_t *)(void *)sb;
    uint64_t i, n = page_size / 8;
    for (i = 0; i < n; i++) {
        if (atomic_load_explicit(&w[i], memory_order_seq_cst) != 0) {
            return 0;
        }
    }
    return 1;
}

hqq_status hqq_sb_check_layout(hqq_superblock *sb, int initializing) {
    uint64_t magic, major;

    if ((LD(&sb->required_features) & ~HQQ_SB_KNOWN_FEATURES) != 0) {
        return HQQ_ERR_INCOMPATIBLE_LAYOUT;
    }
    magic = LD(&sb->magic);
    major = LD(&sb->major);
    if (!initializing) {
        if (magic != HQQ_SB_MAGIC || major != HQQ_SB_MAJOR) {
            return HQQ_ERR_INCOMPATIBLE_LAYOUT;
        }
        return HQQ_OK;
    }
    /* The initializer may have died right after claiming: layout words may be missing. */
    if (magic != 0 && magic != HQQ_SB_MAGIC) {
        return HQQ_ERR_INCOMPATIBLE_LAYOUT;
    }
    if (major != HQQ_SB_MAJOR && (major != 0 || LD(&sb->init_token) != 0)) {
        return HQQ_ERR_INCOMPATIBLE_LAYOUT;
    }
    return HQQ_OK;
}

static int64_t effective_peer_timeout_ns(int64_t peer_timeout_ns, int64_t interval_ns) {
    return max_i64(peer_timeout_ns, 4 * interval_ns);
}

/* SPEC 6.2 (with the F-04 fix of the Go code): a fresh wall-clock heartbeat is
 * younger than timeout and not further in the future than the slack. */
static int heartbeat_fresh(uint64_t ts, int64_t timeout_ns) {
    int64_t age = (int64_t)(now_unix_nanos() - ts);
    return age > -HQQ_HEARTBEAT_FUTURE_SLACK_NS && age < timeout_ns;
}

hqq_status hqq_sb_endpoint_alive_at_open(const hqq_ctx *ctx, hqq_endpoint_record *rec,
                                         int64_t peer_timeout_ns, int *alive) {
    uint64_t tok = LD(&rec->token);
    uint64_t hb;
    int64_t interval, window, step;
    uint64_t end;

    if (!hqq_is_owner_token(tok)) {
        *alive = 0;
        return HQQ_OK;
    }
    interval = (int64_t)LD_RLX(&rec->interval);
    if (interval <= 0) {
        *alive = 1; /* heartbeats disabled: liveness cannot be judged */
        return HQQ_OK;
    }
    hb = LD_RLX(&rec->heartbeat);
    if (hb == 0) {
        *alive = 1; /* attached a moment ago */
        return HQQ_OK;
    }
    if (heartbeat_fresh(hb, effective_peer_timeout_ns(peer_timeout_ns, interval))) {
        *alive = 1;
        return HQQ_OK;
    }
    /* Stale-looking: confirm by watching for progress on the local clock. */
    window = min_i64(3 * interval, (int64_t)(2 * HQQ_NS_PER_SEC));
    step = max_i64(interval / 4, (int64_t)HQQ_NS_PER_MS);
    end = hqq_plat_mono_ns() + (uint64_t)window;
    while (hqq_plat_mono_ns() < end) {
        hqq_status st = hqq_ctx_sleep(ctx, (uint64_t)step);
        if (st != HQQ_OK) {
            return st;
        }
        if (LD_RLX(&rec->heartbeat) != hb || LD(&rec->token) != tok) {
            *alive = hqq_is_owner_token(LD(&rec->token));
            return HQQ_OK;
        }
    }
    *alive = 0;
    return HQQ_OK;
}

static void clear_endpoint(hqq_endpoint_record *rec) {
    ST(&rec->token, 0);
    ST(&rec->pid, 0);
    ST(&rec->heartbeat, 0);
    ST(&rec->interval, 0);
}

static void fill_endpoint(hqq_endpoint_record *rec, int64_t heartbeat_interval_ns) {
    ST_RLX(&rec->pid, hqq_plat_pid());
    ST_RLX(&rec->interval, (uint64_t)heartbeat_interval_ns);
    ST_RLX(&rec->heartbeat, now_unix_nanos());
}

static void stamp_layout(hqq_superblock *sb) {
    ST(&sb->major, HQQ_SB_MAJOR);
    ST(&sb->minor, HQQ_SB_MINOR);
    ST(&sb->required_features, 0);
}

int hqq_sb_begin_init(hqq_superblock *sb, uint64_t token, uint64_t expected, uint64_t creator_token) {
    stamp_layout(sb);
    if (!cas_u64(&sb->init_token, expected, token)) {
        return 0;
    }
    ST(&sb->creator_token, creator_token);
    ST_RLX(&sb->init_heartbeat, now_unix_nanos());
    return 1;
}

hqq_status hqq_sb_claim(hqq_superblock *sb, const hqq_claim_params *p,
                        hqq_mode *mode, int *must_init, hqq_geometry *got) {
    const uint64_t deadline = hqq_plat_mono_ns() + (uint64_t)p->attach_timeout_ns;
    const uint64_t stale_init = p->init_stale_ns > 0
                                    ? (uint64_t)p->init_stale_ns
                                    : (uint64_t)max_i64(p->peer_timeout_ns, (int64_t)(2 * HQQ_NS_PER_SEC));
    int watch_active = 0;
    uint64_t watch_tok = 0, watch_hb = 0, watch_since = 0;
    int empty_since_set = 0;
    uint64_t empty_since = 0;

    *mode = HQQ_MODE_PRIMARY;
    *must_init = 0;
    for (;;) {
        hqq_status st = hqq_ctx_check(&p->ctx);
        uint64_t state;
        int stalling = 0, empty_wait = 0;

        if (st != HQQ_OK) {
            return st;
        }
        state = atomic_load_explicit(&sb->state, memory_order_acquire);
        if (state != HQQ_STATE_INITIALIZING) {
            watch_active = 0;
        }
        if (state != HQQ_STATE_EMPTY) {
            empty_since_set = 0;
        }

        switch (state) {
        case HQQ_STATE_EMPTY:
            if (!hqq_sb_first_page_zero(sb, p->layout.page_size)) {
                if (LD(&sb->state) != HQQ_STATE_EMPTY) {
                    continue; /* another endpoint claimed it meanwhile */
                }
                return HQQ_ERR_INCOMPATIBLE_LAYOUT; /* foreign data: write nothing */
            }
            if (p->role == HQQ_ROLE_OPENER) {
                uint64_t now = hqq_plat_mono_ns();
                if (!empty_since_set) {
                    empty_since_set = 1;
                    empty_since = now;
                }
                if (now - empty_since >= (uint64_t)p->attach_timeout_ns) {
                    return HQQ_ERR_SEGMENT_NOT_READY;
                }
                empty_wait = 1;
                break;
            }
            if (cas_u64(&sb->state, HQQ_STATE_EMPTY, HQQ_STATE_INITIALIZING)) {
                if (!hqq_sb_begin_init(sb, p->token, LD(&sb->init_token), p->creator_token)) {
                    return HQQ_ERR_INIT_CONTENDED;
                }
                *mode = HQQ_MODE_PRIMARY;
                *must_init = 1;
                return HQQ_OK;
            }
            break;

        case HQQ_STATE_INITIALIZING: {
            uint64_t tok = LD(&sb->init_token);
            uint64_t hb = LD_RLX(&sb->init_heartbeat);
            if (hqq_sb_check_layout(sb, 1) != HQQ_OK) {
                return HQQ_ERR_INCOMPATIBLE_LAYOUT;
            }
            if (p->role == HQQ_ROLE_OPENER) {
                break; /* only waits; an opener never takes over */
            }
            {
                uint64_t now = hqq_plat_mono_ns();
                if (!watch_active || watch_tok != tok || watch_hb != hb) {
                    watch_active = 1;
                    watch_tok = tok;
                    watch_hb = hb;
                    watch_since = now;
                } else if (now - watch_since >= stale_init) {
                    if (hqq_sb_begin_init(sb, p->token, tok, p->creator_token)) {
                        *mode = HQQ_MODE_PRIMARY;
                        *must_init = 1;
                        return HQQ_OK;
                    }
                    watch_active = 0; /* somebody else moved first */
                }
            }
            stalling = 1;
            break;
        }

        case HQQ_STATE_READY: {
            int primary_alive = 0, secondary_alive = 0;
            hqq_geometry g;
            hqq_endpoint_record *sec;

            if (hqq_sb_check_layout(sb, 0) != HQQ_OK) {
                return HQQ_ERR_INCOMPATIBLE_LAYOUT;
            }
            st = hqq_sb_endpoint_alive_at_open(&p->ctx, &sb->endpoints[HQQ_ENDPOINT_PRIMARY],
                                               p->peer_timeout_ns, &primary_alive);
            if (st != HQQ_OK) {
                return st;
            }
            st = hqq_sb_endpoint_alive_at_open(&p->ctx, &sb->endpoints[HQQ_ENDPOINT_SECONDARY],
                                               p->peer_timeout_ns, &secondary_alive);
            if (st != HQQ_OK) {
                return st;
            }
            if (!primary_alive && !secondary_alive) {
                if (p->role == HQQ_ROLE_OPENER) {
                    return HQQ_ERR_NO_LIVE_ENDPOINT; /* a new creator may reuse it later */
                }
                if (cas_u64(&sb->state, HQQ_STATE_READY, HQQ_STATE_INITIALIZING)) { /* REUSE */
                    if (!hqq_sb_begin_init(sb, p->token, LD(&sb->init_token), p->creator_token)) {
                        return HQQ_ERR_INIT_CONTENDED;
                    }
                    *mode = HQQ_MODE_PRIMARY;
                    *must_init = 1;
                    return HQQ_OK;
                }
                continue;
            }
            if (p->role == HQQ_ROLE_CREATOR) {
                return HQQ_ERR_LINK_BUSY; /* a creator never joins a live segment */
            }
            g.buffer_count = LD(&sb->buffer_count);
            g.buffer_size = LD(&sb->buffer_size);
            g.layout_size = LD(&sb->layout_size);
            g.page_size = LD(&sb->page_size);
            if (g.buffer_count != p->layout.buffer_count || g.buffer_size != p->layout.buffer_size ||
                g.layout_size != p->layout.total || g.page_size != p->layout.page_size) {
                if (got != NULL) {
                    *got = g;
                }
                return HQQ_ERR_GEOMETRY_MISMATCH;
            }
            sec = &sb->endpoints[HQQ_ENDPOINT_SECONDARY];
            if (primary_alive && LD(&sec->token) == 0) {
                if (cas_u64(&sec->token, 0, p->token)) { /* JOIN */
                    fill_endpoint(sec, p->heartbeat_interval_ns);
                    *mode = HQQ_MODE_SECONDARY;
                    *must_init = 0;
                    return HQQ_OK;
                }
                continue;
            }
            /* Both alive, or the live endpoint's earlier peer already left. */
            return HQQ_ERR_LINK_BUSY;
        }

        default:
            return HQQ_ERR_INCOMPATIBLE_LAYOUT;
        }

        /* Reached by Empty (creator, lost CAS), Empty (opener) and Initializing. */
        {
            uint64_t now = hqq_plat_mono_ns();
            uint64_t limit = deadline;
            if (stalling && watch_active && watch_since < deadline) {
                uint64_t alt = watch_since + stale_init;
                limit = alt > deadline ? alt : deadline;
            }
            if (!empty_wait && now > limit) {
                return HQQ_ERR_INIT_CONTENDED;
            }
        }
        st = hqq_ctx_sleep(&p->ctx, HQQ_CLAIM_POLL_NS);
        if (st != HQQ_OK) {
            return st;
        }
    }
}

/* ---- Initialization (SPEC 5.3) ---------------------------------------------------------- */

typedef struct hqq_init_session {
    hqq_superblock *sb;
    uint64_t token;
    const hqq_ctx *ctx;
    hqq_status err;
} hqq_init_session;

/* Context error if done, INIT_CONTENDED if another endpoint took over; HQQ_OK otherwise. */
static hqq_status session_check(hqq_init_session *s) {
    hqq_status st = hqq_ctx_check(s->ctx);
    if (st != HQQ_OK) {
        return st;
    }
    if (LD(&s->sb->init_token) != s->token) {
        return HQQ_ERR_INIT_CONTENDED;
    }
    return HQQ_OK;
}

static int session_check_cb(void *arg) {
    hqq_init_session *s = (hqq_init_session *)arg;
    s->err = session_check(s);
    return s->err != HQQ_OK;
}

static void session_progress(hqq_init_session *s) {
    if (LD(&s->sb->init_token) == s->token) {
        ST_RLX(&s->sb->init_heartbeat, now_unix_nanos());
    }
}

/* Zeroes n bytes in slices, checking ownership before and progressing after each. */
static hqq_status session_zero(hqq_init_session *s, unsigned char *p, uint64_t n) {
    while (n > 0) {
        size_t chunk = n < HQQ_ZERO_SLICE_BYTES ? (size_t)n : HQQ_ZERO_SLICE_BYTES;
        hqq_status st = session_check(s);
        if (st != HQQ_OK) {
            return st;
        }
        memset(p, 0, chunk);
        p += chunk;
        n -= chunk;
        session_progress(s);
    }
    return HQQ_OK;
}

hqq_status hqq_sb_publish(hqq_superblock *sb, const hqq_claim_params *p, uint64_t *epoch) {
    hqq_endpoint_record *prim;

    if (LD(&sb->init_token) != p->token) {
        return HQQ_ERR_INIT_CONTENDED; /* another endpoint took over */
    }
    stamp_layout(sb);                                  /* P1 */
    ST(&sb->page_size, p->layout.page_size);           /* P2 */
    ST(&sb->buffer_count, p->layout.buffer_count);
    ST(&sb->buffer_size, p->layout.buffer_size);
    ST(&sb->layout_size, p->layout.total);
    clear_endpoint(&sb->endpoints[HQQ_ENDPOINT_SECONDARY]); /* P3 */
    prim = &sb->endpoints[HQQ_ENDPOINT_PRIMARY];            /* P4 */
    ST(&prim->token, p->token);
    fill_endpoint(prim, p->heartbeat_interval_ns);
    ST(&sb->magic, HQQ_SB_MAGIC);                      /* P5 */
    if (!cas_u64(&sb->init_token, p->token, 0)) {      /* commit */
        return HQQ_ERR_INIT_CONTENDED;
    }
    *epoch = atomic_fetch_add_explicit(&sb->epoch, 1, memory_order_seq_cst) + 1;
    atomic_store_explicit(&sb->state, HQQ_STATE_READY, memory_order_seq_cst); /* release point */
    return HQQ_OK;
}

hqq_status hqq_sb_initialize(unsigned char *base, const hqq_claim_params *p, uint64_t *epoch) {
    hqq_superblock *sb = (hqq_superblock *)(void *)base;
    hqq_init_session s;
    const uint64_t offs[2] = {p->layout.ring0, p->layout.ring1};
    const uint64_t bufs[2] = {p->layout.buf0, p->layout.buf1};
    const uint64_t buf_bytes = p->layout.buffer_count * p->layout.buffer_size;
    hqq_status st;
    int i;

    s.sb = sb;
    s.token = p->token;
    s.ctx = &p->ctx;
    s.err = HQQ_OK;
    for (i = 0; i < 2; i++) {
        int rc = hqq_ring_format(base + offs[i], p->layout.buffer_count, session_check_cb, &s);
        if (rc != 0) {
            return s.err != HQQ_OK ? s.err : HQQ_ERR_INVALID_SIZE;
        }
        session_progress(&s);
    }
    for (i = 0; i < 2; i++) {
        st = session_zero(&s, base + bufs[i], buf_bytes);
        if (st != HQQ_OK) {
            return st;
        }
    }
    return hqq_sb_publish(sb, p, epoch);
}
