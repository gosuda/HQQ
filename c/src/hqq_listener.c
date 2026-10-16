/*
 * hqq_listener.c - acceptor and client connect of the Advanced layer
 * (ext/LISTENER_DRAFT.md, layout HQQ_LSN1; Go: listener.go, listener_layout.go,
 * connect.go, internal/mpmc).
 *
 * Segment: a 4096-byte header, then an MPMC ring of connect records. The ring is
 * the Standard ring protocol (hqq_ring.h: header, cursors, wake words, claim /
 * publish / release) with 256-byte slots (8-byte sequence word, 248-byte record),
 * so this file has its own slot accessors and reuses the header type and constants.
 *
 * Threads: the acceptor runs one heartbeat thread, one dispatcher (single ring
 * consumer) and one thread per handshake, at most max_handshakes at a time. A
 * handshake holds its slot until its link is in the backlog, so a full backlog
 * stops the dispatcher and the ring fills up (backpressure). Waits park on the
 * ring's wake words or on events, or sleep in slices of at most 1 ms.
 */
#define HQQ_BUILDING 1
#include "hqq_internal.h"
#include "hqq/hqq_listener.h"

/* ---- Layout and defaults (LISTENER_DRAFT 3, 12) ---------------------------------- */

#define LSN_MAGIC UINT64_C(0x4851515f4c534e31) /* "HQQ_LSN1" */
#define LSN_MAJOR UINT64_C(1)
#define LSN_MINOR UINT64_C(0)
#define LSN_HEADER_SIZE 4096u
#define LSN_RING_OFFSET 4096u
#define LSN_RECORD_SIZE 248u
#define LSN_SLOT_STRIDE 256u
#define LSN_NAME_MAX 184u
#define LSN_PREFIX_MAX 32u
#define LSN_SUFFIX_LEN 16u
#define LSN_STEM_MAX 12u
#define LSN_MAX_CAPACITY 65536u
#define LSN_MAX_HANDSHAKES 4096u
#define LSN_MAX_BACKLOG 65536u
#define LSN_LINK_NAME_RETRIES 8

#define LSN_MS(n) (UINT64_C(n) * UINT64_C(1000000))
#define LSN_S(n) (UINT64_C(n) * UINT64_C(1000000000))
#define LSN_HOUR_NS LSN_S(3600)
#define LSN_FUTURE_SLACK_NS LSN_S(1)
#define LSN_STALE_MIN_NS LSN_S(2)
#define LSN_ALIVE_WINDOW_MAX_NS LSN_S(2)
#define LSN_CLAIM_POLL_NS (UINT64_C(200) * 1000u)
#define LSN_POLL_NS LSN_MS(1)
#define LSN_SLICE_NS LSN_MS(10)         /* client waits: watch re-evaluated at least every 50 ms */
#define LSN_DISPATCH_SLICE_NS LSN_MS(50)
#define LSN_BACKOFF_MAX_NS LSN_MS(100)

#define LSN_LD(p) atomic_load_explicit((p), memory_order_seq_cst)
#define LSN_ST(p, v) atomic_store_explicit((p), (v), memory_order_seq_cst)
#define LSN_ADD(p, v) atomic_fetch_add_explicit((p), (v), memory_order_seq_cst)

/* Header (LISTENER_DRAFT 3.1). Every field is an atomic word except the prefix bytes. */
typedef struct lsn_header {
    _Atomic uint64_t magic;              /* 0 */
    _Atomic uint64_t state;              /* 8 */
    _Atomic uint64_t major;              /* 16 */
    _Atomic uint64_t minor;              /* 24 */
    _Atomic uint64_t ring_offset;        /* 32 */
    _Atomic uint64_t record_size;        /* 40 */
    _Atomic uint64_t capacity;           /* 48 */
    _Atomic uint64_t layout_size;        /* 56 */
    _Atomic uint64_t epoch;              /* 64 */
    _Atomic uint64_t init_token;         /* 72 */
    _Atomic uint64_t init_heartbeat;     /* 80 */
    _Atomic uint64_t required_features;  /* 88 */
    _Atomic uint64_t creator_token;      /* 96 */
    _Atomic uint64_t reserved0[3];       /* 104 */
    _Atomic uint64_t server_token;       /* 128 */
    _Atomic uint64_t server_pid;         /* 136 */
    _Atomic uint64_t server_heartbeat;   /* 144 */
    _Atomic uint64_t server_interval;    /* 152 */
    _Atomic uint64_t max_buffer_count;   /* 160 */
    _Atomic uint64_t max_buffer_size;    /* 168 */
    _Atomic uint64_t max_link_size;      /* 176 */
    _Atomic uint64_t features;           /* 184 */
    _Atomic uint64_t prefix_len;         /* 192 */
    unsigned char prefix[LSN_PREFIX_MAX]; /* 200 */
    unsigned char reserved1[LSN_HEADER_SIZE - 232];
} lsn_header;

/* Connect record (LISTENER_DRAFT 3.2): plain memory, owned by the slot's claimant. */
typedef struct lsn_record {
    uint64_t client_token; /* 0 */
    uint64_t buffer_count; /* 8 */
    uint64_t buffer_size;  /* 16 */
    uint64_t features;     /* 24 */
    uint64_t flags;        /* 32 */
    uint64_t name_len;     /* 40 */
    uint64_t client_pid;   /* 48 */
    uint64_t reserved;     /* 56 */
    unsigned char name[LSN_NAME_MAX]; /* 64 */
} lsn_record;

typedef struct lsn_slot {
    _Atomic uint64_t seq; /* 0 */
    lsn_record rec;       /* 8 */
} lsn_slot;

_Static_assert(sizeof(lsn_record) == LSN_RECORD_SIZE, "connect record is 248 bytes");
_Static_assert(sizeof(lsn_slot) == LSN_SLOT_STRIDE, "ring slot is 256 bytes");
_Static_assert(sizeof(lsn_header) == LSN_HEADER_SIZE, "listener header is 4096 bytes");
_Static_assert(offsetof(lsn_header, state) == 8, "state @8");
_Static_assert(offsetof(lsn_header, creator_token) == 96, "creatorToken @96");
_Static_assert(offsetof(lsn_header, server_token) == 128, "serverToken @128");
_Static_assert(offsetof(lsn_header, prefix_len) == 192, "prefixLen @192");
_Static_assert(offsetof(lsn_header, prefix) == 200, "prefix @200");

/* ---- Small helpers ------------------------------------------------------------------ */

static const hqq_allocator *lsn_alloc(const hqq_open_req *t) { return t->alloc_set ? &t->alloc : NULL; }

static int lsn_cas(_Atomic uint64_t *p, uint64_t expect, uint64_t desired) {
    return atomic_compare_exchange_strong_explicit(p, &expect, desired, memory_order_seq_cst,
                                                   memory_order_seq_cst);
}

static uint64_t lsn_min(uint64_t a, uint64_t b) { return a < b ? a : b; }
static uint64_t lsn_max(uint64_t a, uint64_t b) { return a > b ? a : b; }

/* A wait's deadline and cancellation as a library context (hqq.h hqq_wait). */
static void lsn_ctx_from_wait(const hqq_wait *w, hqq_ctx *c) {
    c->cancel = w != NULL ? w->cancel : NULL;
    c->deadline_ns = (w != NULL && w->timeout_ns >= 0) ? hqq_plat_mono_ns() + (uint64_t)w->timeout_ns : 0;
}

static unsigned lsn_lower(unsigned c) { return (c >= 'A' && c <= 'Z') ? c + 32u : c; }

static int lsn_iequal(const char *a, const char *b, size_t n) {
    size_t i;
    for (i = 0; i < n; i++) {
        if (lsn_lower((unsigned char)a[i]) != lsn_lower((unsigned char)b[i])) {
            return 0;
        }
    }
    return 1;
}

/* ---- Names (LISTENER_DRAFT 2) ------------------------------------------------------- */

/* Length of the scope prefix ("Global\" or "Local\", case-insensitive) of name, or 0. */
static size_t lsn_scope_len(const char *name, size_t n) {
    static const char *const scopes[] = {"Global\\", "Local\\"};
    size_t s;
    for (s = 0; s < sizeof scopes / sizeof scopes[0]; s++) {
        size_t sl = strlen(scopes[s]);
        if (n > sl && lsn_iequal(name, scopes[s], sl)) {
            return sl;
        }
    }
    return 0;
}

static int lsn_stem_char(unsigned char c) {
    return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-';
}

/* The link name prefix of a listener name (LISTENER_DRAFT 2, step 1-4). */
static hqq_status lsn_derive_prefix(const char *name, unsigned char out[LSN_PREFIX_MAX], size_t *out_len) {
    size_t n = strlen(name);
    size_t sl = lsn_scope_len(name, n);
    const char *rest = name + sl;
    size_t len = sl, i;

    memcpy(out, name, sl);
    n -= sl;
    if (n > 0 && rest[0] == '/') {
        rest++;
        n--;
    }
    for (i = 0; i < n && i < LSN_STEM_MAX; i++) {
        unsigned char c = (unsigned char)rest[i];
        out[len++] = lsn_stem_char(c) ? c : '_';
    }
    out[len++] = '.';
    *out_len = len;
    return len <= LSN_PREFIX_MAX ? HQQ_OK : HQQ_ERR_INVALID_NAME;
}

/* Is name prefix followed by exactly 16 lowercase hexadecimal digits? */
static int lsn_link_name_ok(const unsigned char *prefix, size_t plen, const unsigned char *name, size_t nlen) {
    size_t i;
    if (nlen != plen + LSN_SUFFIX_LEN || memcmp(name, prefix, plen) != 0) {
        return 0;
    }
    for (i = plen; i < nlen; i++) {
        unsigned char c = name[i];
        if (!((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f'))) {
            return 0;
        }
    }
    return 1;
}

/* A new link name: the prefix and 16 random lowercase hexadecimal digits. */
static hqq_status lsn_link_name(const unsigned char *prefix, size_t plen, char out[LSN_NAME_MAX + 1]) {
    static const char hex[] = "0123456789abcdef";
    uint64_t r;
    size_t i;
    if (hqq_plat_random(&r, sizeof r) != 0) {
        return HQQ_ERR_SYSTEM;
    }
    memcpy(out, prefix, plen);
    for (i = 0; i < LSN_SUFFIX_LEN; i++) {
        out[plen + i] = hex[(r >> (60 - 4 * i)) & 0xFu];
    }
    out[plen + LSN_SUFFIX_LEN] = '\0';
    return HQQ_OK;
}

/* ---- Header checks (LISTENER_DRAFT 5, 7.2) ------------------------------------------ */

static int lsn_header_zero(const lsn_header *h) {
    const _Atomic uint64_t *w = (const _Atomic uint64_t *)(const void *)h;
    size_t i;
    for (i = 0; i < LSN_HEADER_SIZE / 8; i++) {
        if (LSN_LD(&w[i]) != 0) {
            return 0;
        }
    }
    return 1;
}

/* checkListenerCompat: an unknown required feature is never accepted (none are defined). */
static hqq_status lsn_check_compat(const lsn_header *h, int initializing) {
    uint64_t magic, major;
    if (LSN_LD(&h->required_features) != 0) {
        return HQQ_ERR_INCOMPATIBLE_LAYOUT;
    }
    magic = LSN_LD(&h->magic);
    major = LSN_LD(&h->major);
    if (!initializing) {
        return magic == LSN_MAGIC && major == LSN_MAJOR ? HQQ_OK : HQQ_ERR_INCOMPATIBLE_LAYOUT;
    }
    if (magic != 0 && magic != LSN_MAGIC) {
        return HQQ_ERR_INCOMPATIBLE_LAYOUT;
    }
    if (major != LSN_MAJOR && (major != 0 || LSN_LD(&h->init_token) != 0)) {
        return HQQ_ERR_INCOMPATIBLE_LAYOUT;
    }
    return HQQ_OK;
}

/* Segment size of a ring of `capacity` records for a page size, 0 if invalid. */
static uint64_t lsn_layout_size(uint64_t capacity, uint64_t page) {
    int ok = 1;
    uint64_t total;
    if (capacity < HQQ_RING_MIN_SLOTS || capacity > LSN_MAX_CAPACITY || (capacity & (capacity - 1)) != 0) {
        return 0;
    }
    total = hqq_aligned_sum(LSN_RING_OFFSET, HQQ_RING_HEADER_SIZE + capacity * LSN_SLOT_STRIDE, page, &ok);
    return ok ? total : 0;
}

/*
 * Inspection of a client's view (7.2 step 2): HQQ_ERR_LISTENER_NOT_READY while the
 * listener is being initialized, HQQ_ERR_INCOMPATIBLE_LAYOUT for anything that is not
 * a listener of this layout, else HQQ_OK with the layout size in *layout.
 */
static hqq_status lsn_inspect(const lsn_header *h, int64_t cur, uint64_t page, uint64_t *layout) {
    hqq_status st;
    uint64_t size;

    switch (LSN_LD(&h->state)) {
    case HQQ_STATE_EMPTY:
        return lsn_header_zero(h) ? HQQ_ERR_LISTENER_NOT_READY : HQQ_ERR_INCOMPATIBLE_LAYOUT;
    case HQQ_STATE_INITIALIZING:
        st = lsn_check_compat(h, 1);
        return st != HQQ_OK ? st : HQQ_ERR_LISTENER_NOT_READY;
    case HQQ_STATE_READY:
        break;
    default:
        return HQQ_ERR_INCOMPATIBLE_LAYOUT;
    }
    st = lsn_check_compat(h, 0);
    if (st != HQQ_OK) {
        return st;
    }
    size = lsn_layout_size(LSN_LD(&h->capacity), page);
    if (size == 0 || LSN_LD(&h->ring_offset) != LSN_RING_OFFSET || LSN_LD(&h->record_size) != LSN_RECORD_SIZE ||
        LSN_LD(&h->layout_size) != size || cur < (int64_t)size) {
        return HQQ_ERR_INCOMPATIBLE_LAYOUT;
    }
    *layout = size;
    return HQQ_OK;
}

/* ---- Liveness (LISTENER_DRAFT 7.1) ------------------------------------------------ */

static int lsn_fresh(uint64_t hb, uint64_t timeout_ns) {
    int64_t age = (int64_t)(hqq_plat_wall_ns() - hb);
    return age > -(int64_t)LSN_FUTURE_SLACK_NS && age < (int64_t)timeout_ns;
}

/* Is the acceptor recorded in h alive? A stale heartbeat is confirmed by progress. */
static int lsn_server_alive(const lsn_header *h, uint64_t peer_ns) {
    uint64_t tok = LSN_LD(&h->server_token);
    uint64_t interval = LSN_LD(&h->server_interval);
    uint64_t hb, window, end;

    if (!hqq_is_owner_token(tok)) {
        return 0;
    }
    if (interval == 0 || interval > LSN_HOUR_NS) {
        return 1;
    }
    hb = LSN_LD(&h->server_heartbeat);
    if (hb == 0 || lsn_fresh(hb, lsn_max(peer_ns, 4 * interval))) {
        return 1;
    }
    window = lsn_min(3 * interval, LSN_ALIVE_WINDOW_MAX_NS);
    end = hqq_plat_mono_ns() + window;
    while (hqq_plat_mono_ns() < end) {
        hqq_plat_sleep_ns(LSN_POLL_NS);
        if (LSN_LD(&h->server_heartbeat) != hb || LSN_LD(&h->server_token) != tok) {
            return hqq_is_owner_token(LSN_LD(&h->server_token));
        }
    }
    return 0;
}

/* The client's watch of the acceptor (7.4), on the local monotonic clock. */
typedef struct lsn_watch {
    const lsn_header *h;
    uint64_t creator, epoch, token, hb, since, peer_ns;
} lsn_watch;

static void lsn_watch_start(lsn_watch *w, const lsn_header *h, uint64_t peer_ns) {
    w->h = h;
    w->creator = LSN_LD(&h->creator_token);
    w->epoch = LSN_LD(&h->epoch);
    w->token = LSN_LD(&h->server_token);
    w->hb = LSN_LD(&h->server_heartbeat);
    w->since = hqq_plat_mono_ns();
    w->peer_ns = peer_ns;
}

static hqq_status lsn_watch_check(lsn_watch *w) {
    const lsn_header *h = w->h;
    uint64_t tok, interval, hb, now;

    if (LSN_LD(&h->state) != HQQ_STATE_READY || LSN_LD(&h->creator_token) != w->creator ||
        LSN_LD(&h->epoch) != w->epoch) {
        return HQQ_ERR_LISTENER_GONE;
    }
    tok = LSN_LD(&h->server_token);
    if (tok == HQQ_DETACHED_TOKEN || tok == 0) {
        return HQQ_ERR_LISTENER_CLOSED;
    }
    if (tok != w->token) {
        return HQQ_ERR_LISTENER_GONE;
    }
    interval = LSN_LD(&h->server_interval);
    if (interval == 0 || interval > LSN_HOUR_NS) {
        return HQQ_OK;
    }
    now = hqq_plat_mono_ns();
    hb = LSN_LD(&h->server_heartbeat);
    if (hb != w->hb) {
        w->hb = hb;
        w->since = now;
        return HQQ_OK;
    }
    return now - w->since > lsn_max(4 * interval, w->peer_ns) ? HQQ_ERR_LISTENER_GONE : HQQ_OK;
}

/* ---- Ring (hqq_ring.h protocol, 256-byte slots) -------------------------------------- */

/* The listener's ring handle. Cursors and wake words live in the shared header. */
typedef struct lsn_ring {
    hqq_ring_header *h;
    lsn_slot *slots;
    uint64_t size;
    uint64_t mask;
    const hqq_ring_ops *ops; /* NULL: no kernel wait / wake */
    void *ops_ctx;
} lsn_ring;

/* The mapped segment, owned by the acceptor or viewed by a client. */
typedef struct lsn_seg {
    hqq_named *named;   /* object, mapping and name ownership (hqq_named.c) */
    lsn_header *h;
    lsn_ring ring;
    hqq_plat_wait wait; /* platform wait context of the ring */
} lsn_seg;

static lsn_slot *lsn_slot_at(const lsn_ring *r, uint64_t pos) { return &r->slots[pos & r->mask]; }

static _Atomic uint32_t *lsn_word(const lsn_ring *r, int side) {
    return side == HQQ_RING_NOT_EMPTY ? &r->h->not_empty : &r->h->not_full;
}

static _Atomic uint32_t *lsn_waiters(const lsn_ring *r, int side) {
    return side == HQQ_RING_NOT_EMPTY ? &r->h->empty_waiters : &r->h->full_waiters;
}

/* Waker duty (K2, K3): after the sequence store, wake the parked side if anyone waits. */
static void lsn_notify(const lsn_ring *r, int side) {
    _Atomic uint32_t *word = lsn_word(r, side);
    uint32_t n = atomic_load_explicit(lsn_waiters(r, side), memory_order_seq_cst);
    if (n != 0) {
        atomic_fetch_add_explicit(word, 1, memory_order_seq_cst);
        if (r->ops != NULL && r->ops->wake != NULL) {
            r->ops->wake(r->ops_ctx, side, word, n);
        }
    }
}

/* Formats the ring of n slots at mem (LISTENER_DRAFT 5.1). check runs before every slice. */
static hqq_status lsn_ring_format(void *mem, uint64_t n, hqq_status (*check)(void *), void *arg) {
    hqq_ring_header *h = (hqq_ring_header *)mem;
    lsn_slot *slots = (lsn_slot *)((unsigned char *)mem + HQQ_RING_HEADER_SIZE);
    hqq_status st;
    uint64_t i;

    if (!hqq_ring_valid_size(n)) {
        return HQQ_ERR_INVALID_SIZE;
    }
    if ((st = check(arg)) != HQQ_OK) {
        return st;
    }
    LSN_ST(&h->flag, 0);
    LSN_ST(&h->magic, 0);
    for (i = 0; i < n; i++) {
        if (i != 0 && i % HQQ_RING_FORMAT_SLICE == 0 && (st = check(arg)) != HQQ_OK) {
            return st;
        }
        memset(&slots[i].rec, 0, sizeof slots[i].rec);
        LSN_ST(&slots[i].seq, i);
    }
    if ((st = check(arg)) != HQQ_OK) {
        return st;
    }
    LSN_ST(&h->r, 0);
    LSN_ST(&h->w, 0);
    LSN_ST(&h->not_empty, 0);
    LSN_ST(&h->empty_waiters, 0);
    LSN_ST(&h->not_full, 0);
    LSN_ST(&h->full_waiters, 0);
    LSN_ST(&h->size, n);
    LSN_ST(&h->stride, LSN_SLOT_STRIDE);
    LSN_ST(&h->version, HQQ_RING_VERSION);
    LSN_ST(&h->magic, HQQ_RING_MAGIC);
    LSN_ST(&h->flag, HQQ_RING_FLAG_INIT);
    return HQQ_OK;
}

/* Attaches to a formatted ring; the wait context is the segment's. */
static hqq_status lsn_ring_attach(lsn_ring *r, void *mem, hqq_plat_wait *wait) {
    hqq_ring_header *h = (hqq_ring_header *)mem;
    uint64_t size;

    if (LSN_LD(&h->magic) != HQQ_RING_MAGIC || (LSN_LD(&h->flag) & HQQ_RING_FLAG_INIT) == 0) {
        return HQQ_ERR_INCOMPATIBLE_LAYOUT;
    }
    size = LSN_LD(&h->size);
    if (LSN_LD(&h->stride) != LSN_SLOT_STRIDE || LSN_LD(&h->version) != HQQ_RING_VERSION ||
        !hqq_ring_valid_size(size)) {
        return HQQ_ERR_INCOMPATIBLE_LAYOUT;
    }
    r->h = h;
    r->slots = (lsn_slot *)((unsigned char *)mem + HQQ_RING_HEADER_SIZE);
    r->size = size;
    r->mask = size - 1;
    r->ops = hqq_plat_ring_ops();
    r->ops_ctx = wait;
    return HQQ_OK;
}

/* One producer claim attempt: 1 and the claimed *pos, or 0 with *pos where the ring is full. */
static int lsn_try_claim_producer(const lsn_ring *r, uint64_t *pos) {
    uint64_t p = atomic_load_explicit(&r->h->w, memory_order_relaxed);
    for (;;) {
        int64_t d = (int64_t)(atomic_load_explicit(&lsn_slot_at(r, p)->seq, memory_order_acquire) - p);
        if (d == 0) {
            if (atomic_compare_exchange_weak_explicit(&r->h->w, &p, p + 1, memory_order_relaxed,
                                                      memory_order_relaxed)) {
                *pos = p;
                return 1;
            }
        } else if (d > 0) {
            p = atomic_load_explicit(&r->h->w, memory_order_relaxed);
        } else {
            *pos = p;
            return 0;
        }
    }
}

/* One consumer claim attempt (single consumer): 1 and *pos, or 0 with *pos awaited. */
static int lsn_try_claim_consumer(const lsn_ring *r, uint64_t *pos) {
    uint64_t p = atomic_load_explicit(&r->h->r, memory_order_relaxed);
    for (;;) {
        int64_t d = (int64_t)(atomic_load_explicit(&lsn_slot_at(r, p)->seq, memory_order_acquire) - (p + 1));
        if (d == 0) {
            if (atomic_compare_exchange_weak_explicit(&r->h->r, &p, p + 1, memory_order_relaxed,
                                                      memory_order_relaxed)) {
                *pos = p;
                return 1;
            }
        } else if (d > 0) {
            p = atomic_load_explicit(&r->h->r, memory_order_relaxed);
        } else {
            *pos = p;
            return 0;
        }
    }
}

static void lsn_publish(const lsn_ring *r, uint64_t pos) {
    LSN_ST(&lsn_slot_at(r, pos)->seq, pos + 1); /* K1 */
    lsn_notify(r, HQQ_RING_NOT_EMPTY);
}

static void lsn_release(const lsn_ring *r, uint64_t pos) {
    LSN_ST(&lsn_slot_at(r, pos)->seq, pos + r->size); /* pos + mask + 1 */
    lsn_notify(r, HQQ_RING_NOT_FULL);
}

static int lsn_blocked(const lsn_ring *r, uint64_t pos, uint64_t target) {
    return (int64_t)(LSN_LD(&lsn_slot_at(r, pos)->seq) - target) < 0;
}

/*
 * Parks until the slot at pos reaches target, a wake arrives or timeout_ns passes
 * (W1..W4). The caller re-runs its claim afterwards. Without a kernel wait the
 * park is a bounded sleep.
 */
static void lsn_park(const lsn_ring *r, int side, uint64_t pos, uint64_t target, uint64_t timeout_ns) {
    _Atomic uint32_t *word = lsn_word(r, side);
    _Atomic uint32_t *waiters = lsn_waiters(r, side);
    uint32_t v;

    atomic_fetch_add_explicit(waiters, 1, memory_order_seq_cst); /* W1 */
    v = LSN_LD(word);                                            /* W2 */
    if (lsn_blocked(r, pos, target)) {                           /* W3 */
        if (r->ops != NULL && r->ops->wait != NULL) {
            r->ops->wait(r->ops_ctx, side, word, v, timeout_ns);
        } else {
            hqq_plat_sleep_ns(lsn_min(timeout_ns, LSN_POLL_NS));
        }
    }
    atomic_fetch_sub_explicit(waiters, 1, memory_order_release); /* W4 */
}

/* ---- Named segments: mapping and name ownership (hqq_named.c) ------------------------- */

static hqq_named *lsn_named_new(const hqq_allocator *al, const char *name) {
    size_t len = strlen(name) + 1;
    hqq_named *n = hqq_alloc(al, sizeof *n);
    if (n == NULL) {
        return NULL;
    }
    memset(n, 0, sizeof *n);
    n->name = hqq_alloc(al, len);
    if (n->name == NULL) {
        hqq_free(al, n, sizeof *n);
        return NULL;
    }
    memcpy(n->name, name, len);
    n->name_size = len;
    atomic_flag_clear(&n->lock.f);
    return n;
}

/* Opens (create == 0) or exclusively creates (create != 0) the object and maps size bytes. */
static hqq_status lsn_map_object(const hqq_allocator *al, hqq_named *n, size_t size, int create, unsigned perm,
                                 int *os_err) {
    hqq_status st = hqq_plat_shm_open(al, n->name, size, create, perm, &n->shm, os_err);
    if (st != HQQ_OK) {
        return st;
    }
    n->unlink_on_close = create;
    n->fresh = create;
    if (!create) {
        int64_t cur = hqq_plat_shm_size(n->shm, os_err);
        if (cur < 0) {
            return HQQ_ERR_SYSTEM;
        }
        if (cur == 0) {
            return HQQ_ERR_SEGMENT_NOT_READY;
        }
        if (cur < (int64_t)size) {
            return HQQ_ERR_INVALID_SIZE;
        }
    }
    st = hqq_plat_shm_map(n->shm, size, &n->map, os_err);
    if (st != HQQ_OK) {
        return st;
    }
    n->map_size = size;
    return (uintptr_t)n->map % hqq_plat_page_size() == 0 ? HQQ_OK : HQQ_ERR_MEMORY_ALIGN;
}

/* Creates the listener object, or opens it when the name exists (LISTENER_DRAFT 5.1 step 1). */
static hqq_status lsn_open_or_create(const hqq_allocator *al, hqq_named *n, size_t size, unsigned perm,
                                     int *os_err) {
    int attempt;
    for (attempt = 0;; attempt++) {
        hqq_status st = lsn_map_object(al, n, size, 1, perm, os_err);
        if (st != HQQ_ERR_EXISTS) {
            return st;
        }
        st = lsn_map_object(al, n, size, 0, 0, os_err);
        if (st != HQQ_ERR_NOT_EXIST || attempt >= 3) {
            return st;
        }
    }
}

/*
 * The creatorToken of the object `name` (LISTENER 6.3 instance check, and the
 * dead-client check), read through a short-lived mapping of its first page.
 * *found is 0 for a missing or too small object.
 */
static hqq_status lsn_peek_token(const hqq_allocator *al, const char *name, uint64_t *token, int *found) {
    hqq_shm *shm = NULL;
    void *p = NULL;
    size_t page = (size_t)hqq_plat_page_size();
    int os_err = 0;
    hqq_status st;

    *found = 0;
    *token = 0;
    st = hqq_plat_shm_open(al, name, 0, 0, 0, &shm, &os_err);
    if (st == HQQ_ERR_NOT_EXIST) {
        return HQQ_OK;
    }
    if (st != HQQ_OK) {
        return st;
    }
    if (hqq_plat_shm_size(shm, &os_err) >= (int64_t)page) {
        st = hqq_plat_shm_map(shm, page, &p, &os_err);
        if (st == HQQ_OK) {
            *token = LSN_LD(&((const lsn_header *)p)->creator_token);
            *found = 1;
            hqq_plat_shm_unmap(shm, p, page);
        }
    }
    hqq_plat_shm_close(al, shm);
    return st;
}

/* Removes a dead client's link name if it still refers to the instance announced (6.3). */
static void lsn_reap(const hqq_allocator *al, const char *name, uint64_t token) {
    uint64_t seen = 0;
    int found = 0;
    if (!hqq_plat_names_persist()) {
        return;
    }
    if (lsn_peek_token(al, name, &seen, &found) == HQQ_OK && found && seen == token) {
        (void)hqq_unlink(name);
    }
}

/* ---- Acceptor -------------------------------------------------------------------------- */

/* Normalized acceptor options (LISTENER_DRAFT 12 defaults). */
typedef struct lsn_acc_cfg {
    hqq_adv_options link;    /* options of accepted links */
    hqq_open_req timing;     /* normalized link options: heartbeat, timeouts, allocator */
    uint64_t capacity, layout, features, max_bc, max_bs, max_link;
    uint64_t backlog, max_hs, handshake_ns, stuck_ns, init_stale_ns;
    unsigned perm;
} lsn_acc_cfg;

/* One handshake slot: the record it serves and the thread running it. */
typedef struct lsn_job {
    struct hqq_acceptor *acc;
    lsn_record rec;
    char name[LSN_NAME_MAX + 1];
    hqq_plat_thread *thread; /* dispatcher-owned */
    int busy;                /* dispatcher-owned: thread started and not yet joined */
    _Atomic uint32_t done;   /* set by the handshake thread when it returns */
} lsn_job;

struct hqq_acceptor {
    lsn_acc_cfg cfg;
    lsn_seg seg;
    uint64_t token;   /* serverToken and initToken of this instance */
    uint64_t creator; /* creatorToken of this instance (name ownership) */
    unsigned char prefix[LSN_PREFIX_MAX];
    size_t prefix_len;
    hqq_cancel cancel; /* stops the handshakes at close */

    hqq_spin lock;     /* guards the backlog */
    hqq_adv **backlog; /* ring of negotiated links, cfg.backlog entries */
    uint64_t bl_head, bl_count;

    lsn_job *jobs;     /* cfg.max_hs handshake slots */
    hqq_plat_thread *disp;
    hqq_plat_thread *hb;
    hqq_plat_event *hb_stop;
    hqq_plat_event *ready;     /* manual reset: a link is in the backlog, or closing */

    _Atomic uint32_t closing;
    _Atomic uint32_t inflight; /* Accept calls in progress */
    _Atomic uint64_t st_records, st_accepted, st_malformed, st_unreachable, st_failed, st_stuck;
};

static const hqq_allocator *lsn_acc_alloc(const hqq_acceptor *a) { return lsn_alloc(&a->cfg.timing); }

static hqq_status lsn_cfg_init(const hqq_acceptor_options *o, lsn_acc_cfg *c) {
    uint64_t page = hqq_plat_page_size();
    uint64_t peer;
    hqq_status st;

    memset(c, 0, sizeof *c);
    st = hqq_options_normalize(&o->link.link, &c->timing, NULL);
    if (st != HQQ_OK) {
        return st;
    }
    c->link = o->link;
    c->capacity = o->queue_size != 0 ? o->queue_size : HQQ_ACCEPTOR_DEFAULT_QUEUE_SIZE;
    c->layout = lsn_layout_size(c->capacity, page);
    if (c->layout == 0) {
        return HQQ_ERR_INVALID_SIZE;
    }
    c->features = o->features != 0 ? o->features : HQQ_FEATURES_ALL;
    c->max_bc = o->max_buffer_count != 0 ? o->max_buffer_count : HQQ_ACCEPTOR_DEFAULT_MAX_BUFFER_COUNT;
    c->max_bs = o->max_buffer_size != 0 ? o->max_buffer_size : HQQ_ACCEPTOR_DEFAULT_MAX_BUFFER_SIZE;
    c->max_link = o->max_link_size != 0 ? o->max_link_size : HQQ_ACCEPTOR_DEFAULT_MAX_LINK_SIZE;
    if (c->max_bc < HQQ_RING_MIN_SLOTS || c->max_bc > (UINT64_C(1) << 24) || c->max_bs < 8 ||
        c->max_bs > (UINT64_C(1) << 30)) {
        return HQQ_ERR_INVALID_SIZE;
    }
    c->backlog = o->backlog != 0 ? o->backlog : HQQ_ACCEPTOR_DEFAULT_BACKLOG;
    c->max_hs = o->max_handshakes != 0 ? o->max_handshakes : HQQ_ACCEPTOR_DEFAULT_MAX_HANDSHAKES;
    if (c->backlog > LSN_MAX_BACKLOG || c->max_hs > LSN_MAX_HANDSHAKES) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    c->handshake_ns = o->handshake_timeout_ns > 0 ? (uint64_t)o->handshake_timeout_ns : LSN_S(5);
    if (o->stuck_slot_timeout_ns == 0) {
        c->stuck_ns = LSN_S(2);
    } else {
        c->stuck_ns = o->stuck_slot_timeout_ns > 0 ? (uint64_t)o->stuck_slot_timeout_ns : 0;
    }
    peer = (uint64_t)c->timing.peer_timeout_ns;
    c->init_stale_ns = c->timing.init_stale_ns > 0 ? (uint64_t)c->timing.init_stale_ns : lsn_max(peer, LSN_STALE_MIN_NS);
    c->perm = o->perm != 0 ? o->perm : 0600u;
    return HQQ_OK;
}

/* Ownership check of the initializer (LISTENER 5.1): it still owns initToken. */
static hqq_status lsn_init_check(void *arg) {
    hqq_acceptor *a = arg;
    return LSN_LD(&a->seg.h->init_token) == a->token ? HQQ_OK : HQQ_ERR_INIT_CONTENDED;
}

/* beginInit (LISTENER 5 step 3): stamps the layout, takes initToken, records the instance. */
static int lsn_begin_init(hqq_acceptor *a, uint64_t expected) {
    lsn_header *h = a->seg.h;
    LSN_ST(&h->major, LSN_MAJOR);
    LSN_ST(&h->minor, LSN_MINOR);
    LSN_ST(&h->required_features, 0);
    if (!lsn_cas(&h->init_token, expected, a->token)) {
        return 0;
    }
    LSN_ST(&h->creator_token, a->creator);
    LSN_ST(&h->init_heartbeat, hqq_plat_wall_ns());
    return 1;
}

/* Claim loop (LISTENER 5 step 2): sets the segment up for this acceptor's initialization. */
static hqq_status lsn_claim(hqq_acceptor *a) {
    lsn_header *h = a->seg.h;
    uint64_t deadline = hqq_plat_mono_ns() + (uint64_t)a->cfg.timing.attach_timeout_ns;
    uint64_t stale = a->cfg.init_stale_ns;
    uint64_t peer = (uint64_t)a->cfg.timing.peer_timeout_ns;
    int watching = 0;
    uint64_t wtok = 0, whb = 0, since = 0;

    for (;;) {
        uint64_t state = LSN_LD(&h->state);
        int stalling = 0;
        uint64_t limit, now;

        if (state != HQQ_STATE_INITIALIZING) {
            watching = 0;
        }
        switch (state) {
        case HQQ_STATE_EMPTY:
            if (!lsn_header_zero(h)) {
                if (LSN_LD(&h->state) != HQQ_STATE_EMPTY) {
                    continue; /* claimed meanwhile */
                }
                return HQQ_ERR_INCOMPATIBLE_LAYOUT;
            }
            if (lsn_cas(&h->state, HQQ_STATE_EMPTY, HQQ_STATE_INITIALIZING)) {
                return lsn_begin_init(a, LSN_LD(&h->init_token)) ? HQQ_OK : HQQ_ERR_INIT_CONTENDED;
            }
            break;
        case HQQ_STATE_INITIALIZING: {
            uint64_t tok = LSN_LD(&h->init_token), hb = LSN_LD(&h->init_heartbeat);
            hqq_status st = lsn_check_compat(h, 1);
            if (st != HQQ_OK) {
                return st;
            }
            now = hqq_plat_mono_ns();
            if (!watching || tok != wtok || hb != whb) {
                watching = 1;
                wtok = tok;
                whb = hb;
                since = now;
            } else if (now - since >= stale) {
                if (lsn_begin_init(a, tok)) {
                    return HQQ_OK;
                }
                watching = 0; /* somebody moved first: watch again */
            }
            stalling = 1;
            break;
        }
        case HQQ_STATE_READY:
            if (lsn_check_compat(h, 0) != HQQ_OK) {
                return HQQ_ERR_INCOMPATIBLE_LAYOUT;
            }
            if (lsn_server_alive(h, peer)) {
                return HQQ_ERR_LISTENER_BUSY;
            }
            if (lsn_cas(&h->state, HQQ_STATE_READY, HQQ_STATE_INITIALIZING)) {
                return lsn_begin_init(a, LSN_LD(&h->init_token)) ? HQQ_OK : HQQ_ERR_INIT_CONTENDED;
            }
            continue;
        default:
            return HQQ_ERR_INCOMPATIBLE_LAYOUT;
        }

        /* A stalled initializer keeps its whole window even past the attach timeout. */
        limit = deadline;
        if (stalling && watching && since < deadline) {
            limit = lsn_max(limit, since + stale);
        }
        if (hqq_plat_mono_ns() > limit) {
            return HQQ_ERR_INIT_CONTENDED;
        }
        hqq_plat_sleep_ns(LSN_CLAIM_POLL_NS);
    }
}

/* Publishes the header and formats the ring (LISTENER 5.1), then marks Ready. */
static hqq_status lsn_initialize(hqq_acceptor *a) {
    lsn_header *h = a->seg.h;
    unsigned char *ring = (unsigned char *)h + LSN_RING_OFFSET;
    hqq_status st = lsn_ring_format(ring, a->cfg.capacity, lsn_init_check, a);
    uint64_t expected = a->token;

    if (st != HQQ_OK) {
        return st;
    }
    LSN_ST(&h->init_heartbeat, hqq_plat_wall_ns());
    st = lsn_init_check(a);
    if (st != HQQ_OK) {
        return st;
    }
    LSN_ST(&h->ring_offset, LSN_RING_OFFSET);
    LSN_ST(&h->record_size, LSN_RECORD_SIZE);
    LSN_ST(&h->capacity, a->cfg.capacity);
    LSN_ST(&h->layout_size, a->cfg.layout);
    LSN_ST(&h->max_buffer_count, a->cfg.max_bc);
    LSN_ST(&h->max_buffer_size, a->cfg.max_bs);
    LSN_ST(&h->max_link_size, a->cfg.max_link);
    LSN_ST(&h->features, a->cfg.features);
    memset(h->prefix, 0, sizeof h->prefix);
    memcpy(h->prefix, a->prefix, a->prefix_len);
    LSN_ST(&h->prefix_len, a->prefix_len);
    LSN_ST(&h->server_pid, hqq_plat_pid());
    LSN_ST(&h->server_interval, (uint64_t)a->cfg.timing.heartbeat_interval_ns);
    LSN_ST(&h->server_heartbeat, hqq_plat_wall_ns());
    LSN_ST(&h->server_token, a->token);
    LSN_ST(&h->magic, LSN_MAGIC);
    if (!lsn_cas(&h->init_token, expected, 0)) {
        return HQQ_ERR_INIT_CONTENDED;
    }
    LSN_ADD(&h->epoch, 1);
    LSN_ST(&h->state, HQQ_STATE_READY);
    return HQQ_OK;
}

static void lsn_hb_main(void *arg) {
    hqq_acceptor *a = arg;
    uint64_t interval = (uint64_t)a->cfg.timing.heartbeat_interval_ns;
    while (!hqq_plat_event_wait(a->hb_stop, interval)) {
        if (LSN_LD(&a->seg.h->server_token) == a->token) {
            LSN_ST(&a->seg.h->server_heartbeat, hqq_plat_wall_ns());
        }
    }
}

/* Stuck-slot recovery state (LISTENER 6.4), owned by the dispatcher. */
typedef struct lsn_stuck {
    int seen;
    uint64_t pos, since;
} lsn_stuck;

/* Publishes, on a dead producer's behalf, a slot it claimed but never published. */
static void lsn_stuck_check(hqq_acceptor *a, lsn_stuck *sk) {
    lsn_ring *r = &a->seg.ring;
    uint64_t rd = LSN_LD(&r->h->r), wr = LSN_LD(&r->h->w), now = hqq_plat_mono_ns();
    lsn_slot *s = lsn_slot_at(r, rd);

    if ((int64_t)(wr - rd) <= 0 || LSN_LD(&s->seq) != rd) {
        sk->seen = 0;
        return;
    }
    if (!sk->seen || sk->pos != rd) {
        sk->seen = 1;
        sk->pos = rd;
        sk->since = now;
        return;
    }
    if (now - sk->since < a->cfg.stuck_ns) {
        return;
    }
    if (lsn_cas(&s->seq, rd, rd + 1)) {
        LSN_ADD(&a->st_stuck, 1);
        lsn_notify(r, HQQ_RING_NOT_EMPTY);
    }
    sk->seen = 0;
}

/* Takes the next published record (and releases its slot), or parks one slice when none. */
static int lsn_consume(hqq_acceptor *a, lsn_stuck *sk, lsn_record *out, uint64_t slice) {
    lsn_ring *r = &a->seg.ring;
    uint64_t pos = 0;

    if (lsn_try_claim_consumer(r, &pos)) {
        *out = lsn_slot_at(r, pos)->rec;
        lsn_release(r, pos);
        LSN_ADD(&a->st_records, 1);
        return 1;
    }
    if (a->cfg.stuck_ns != 0) {
        lsn_stuck_check(a, sk);
    }
    lsn_park(r, HQQ_RING_NOT_EMPTY, pos, pos + 1, slice);
    return 0;
}

/* Validation of a record (LISTENER 6.2). On success, name receives the link name. */
static int lsn_validate(const hqq_acceptor *a, const lsn_record *rec, char name[LSN_NAME_MAX + 1]) {
    uint64_t nlen = rec->name_len, bc = rec->buffer_count, bs = rec->buffer_size, total;

    if (rec->flags != 0 || nlen == 0 || nlen > LSN_NAME_MAX) {
        return 0;
    }
    if (!lsn_link_name_ok(a->prefix, a->prefix_len, rec->name, (size_t)nlen)) {
        return 0;
    }
    if (!hqq_is_owner_token(rec->client_token)) {
        return 0;
    }
    if (bc > a->cfg.max_bc || bs > a->cfg.max_bs || !hqq_geometry_valid(bc, bs)) {
        return 0;
    }
    total = hqq_standard_size(bc, bs);
    if (total == 0 || total > a->cfg.max_link) {
        return 0;
    }
    memcpy(name, rec->name, (size_t)nlen);
    name[nlen] = '\0';
    return 1;
}

/* Hands a negotiated link to the backlog; waits (holding the slot) while it is full. */
static void lsn_deliver(hqq_acceptor *a, hqq_adv *link) {
    for (;;) {
        hqq_spin_lock(&a->lock);
        if (LSN_LD(&a->closing)) {
            hqq_spin_unlock(&a->lock);
            hqq_adv_close(link);
            return;
        }
        if (a->bl_count < a->cfg.backlog) {
            a->backlog[(a->bl_head + a->bl_count) % a->cfg.backlog] = link;
            a->bl_count++;
            hqq_plat_event_signal(a->ready);
            hqq_spin_unlock(&a->lock);
            return;
        }
        hqq_spin_unlock(&a->lock);
        hqq_plat_sleep_ns(LSN_POLL_NS);
    }
}

/* Attaches to the client's segment and negotiates as Secondary (LISTENER 6.3). */
static void lsn_handshake(hqq_acceptor *a, const lsn_record *rec, const char *name) {
    const hqq_allocator *al = lsn_acc_alloc(a);
    hqq_wait w;
    hqq_adv *link = NULL;
    hqq_status st;
    uint64_t seen = 0;
    int found = 0;

    w.timeout_ns = (int64_t)a->cfg.handshake_ns;
    w.cancel = &a->cancel;
    st = lsn_peek_token(al, name, &seen, &found);
    if (st != HQQ_OK || !found || seen != rec->client_token) {
        if (!LSN_LD(&a->closing)) {
            LSN_ADD(&a->st_unreachable, 1);
        }
        return;
    }
    st = hqq_adv_open(name, rec->buffer_count, rec->buffer_size, &a->cfg.link, &link, NULL);
    if (st != HQQ_OK) {
        if (!LSN_LD(&a->closing)) {
            LSN_ADD(&a->st_unreachable, 1);
            if (st == HQQ_ERR_NO_LIVE_ENDPOINT) {
                lsn_reap(al, name, rec->client_token);
            }
        }
        return;
    }
    if (LSN_LD(&hqq_adv_base(link)->sb->creator_token) != rec->client_token) {
        hqq_adv_close(link); /* a different instance under this name: never join it */
        if (!LSN_LD(&a->closing)) {
            LSN_ADD(&a->st_unreachable, 1);
        }
        return;
    }
    st = hqq_adv_negotiate(link, a->cfg.features, &w);
    if (st != HQQ_OK) {
        hqq_adv_close(link);
        if (!LSN_LD(&a->closing)) {
            LSN_ADD(&a->st_failed, 1);
            if (st == HQQ_ERR_PEER_GONE) {
                lsn_reap(al, name, rec->client_token);
            }
        }
        return;
    }
    lsn_deliver(a, link);
}

static void lsn_hs_main(void *arg) {
    lsn_job *j = arg;
    lsn_handshake(j->acc, &j->rec, j->name);
    LSN_ST(&j->done, 1);
}

/* A free handshake slot, joining the threads that finished. NULL once closing. */
static lsn_job *lsn_take_job(hqq_acceptor *a) {
    for (;;) {
        uint64_t i;
        if (LSN_LD(&a->closing)) {
            return NULL; /* no handshake starts after close began */
        }
        for (i = 0; i < a->cfg.max_hs; i++) {
            lsn_job *j = &a->jobs[i];
            if (j->busy && LSN_LD(&j->done)) {
                hqq_plat_thread_join(lsn_acc_alloc(a), j->thread);
                j->thread = NULL;
                j->busy = 0;
            }
            if (!j->busy) {
                return j;
            }
        }
        if (LSN_LD(&a->closing)) {
            return NULL;
        }
        hqq_plat_sleep_ns(LSN_POLL_NS);
    }
}

/*
 * Dispatcher: the single consumer of the ring. A handshake slot is taken before a
 * record is consumed, so a full backlog or full slots leave the ring to fill up.
 */
static void lsn_disp_main(void *arg) {
    hqq_acceptor *a = arg;
    lsn_stuck sk;
    uint64_t slice = LSN_DISPATCH_SLICE_NS;

    memset(&sk, 0, sizeof sk);
    if (a->cfg.stuck_ns != 0) {
        slice = lsn_min(slice, lsn_max(a->cfg.stuck_ns / 4, LSN_POLL_NS));
    }
    while (!LSN_LD(&a->closing)) {
        lsn_job *j = lsn_take_job(a);
        int os_err = 0;

        if (j == NULL) {
            break;
        }
        if (!lsn_consume(a, &sk, &j->rec, slice)) {
            continue;
        }
        if (!lsn_validate(a, &j->rec, j->name)) {
            LSN_ADD(&a->st_malformed, 1);
            continue;
        }
        LSN_ST(&j->done, 0);
        j->thread = hqq_plat_thread_start(lsn_acc_alloc(a), lsn_hs_main, j, &os_err);
        if (j->thread == NULL) {
            LSN_ADD(&a->st_failed, 1);
            continue;
        }
        j->busy = 1;
    }
}

/* Starts the heartbeat (when enabled) and the dispatcher. */
static hqq_status lsn_start_threads(hqq_acceptor *a) {
    const hqq_allocator *al = lsn_acc_alloc(a);
    int os_err = 0;

    a->ready = hqq_plat_event_create(al);
    if (a->ready == NULL) {
        return HQQ_ERR_NO_MEMORY;
    }
    if (a->cfg.timing.heartbeat_interval_ns > 0) {
        a->hb_stop = hqq_plat_event_create(al);
        if (a->hb_stop == NULL) {
            return HQQ_ERR_NO_MEMORY;
        }
        a->hb = hqq_plat_thread_start(al, lsn_hb_main, a, &os_err);
        if (a->hb == NULL) {
            return HQQ_ERR_SYSTEM;
        }
    }
    a->disp = hqq_plat_thread_start(al, lsn_disp_main, a, &os_err);
    return a->disp != NULL ? HQQ_OK : HQQ_ERR_SYSTEM;
}

static void lsn_drain_backlog(hqq_acceptor *a) {
    for (;;) {
        hqq_adv *l = NULL;
        hqq_spin_lock(&a->lock);
        if (a->bl_count > 0) {
            l = a->backlog[a->bl_head];
            a->bl_head = (a->bl_head + 1) % a->cfg.backlog;
            a->bl_count--;
        }
        hqq_spin_unlock(&a->lock);
        if (l == NULL) {
            return;
        }
        hqq_adv_close(l);
    }
}

static void lsn_destroy(hqq_acceptor *a) {
    const hqq_allocator *al = lsn_acc_alloc(a);
    if (a->ready != NULL) {
        hqq_plat_event_destroy(al, a->ready);
    }
    if (a->jobs != NULL) {
        hqq_free(al, a->jobs, a->cfg.max_hs * sizeof *a->jobs);
    }
    if (a->backlog != NULL) {
        hqq_free(al, a->backlog, a->cfg.backlog * sizeof *a->backlog);
    }
    hqq_free(al, a, sizeof *a);
}

/* Stops everything, closes the undelivered links, marks the listener closed and releases the name. */
static hqq_status lsn_shutdown(hqq_acceptor *a) {
    const hqq_allocator *al = lsn_acc_alloc(a);
    uint64_t i;
    hqq_status st;

    LSN_ST(&a->closing, 1);
    if (a->ready != NULL) {
        hqq_plat_event_signal(a->ready); /* wake Accept calls: they see closing */
    }
    hqq_cancel_signal(&a->cancel);
    if (a->disp != NULL) {
        hqq_plat_thread_join(al, a->disp);
        a->disp = NULL;
    }
    for (i = 0; i < a->cfg.max_hs; i++) {
        if (a->jobs[i].thread != NULL) {
            hqq_plat_thread_join(al, a->jobs[i].thread);
            a->jobs[i].thread = NULL;
        }
    }
    lsn_drain_backlog(a);
    if (a->hb_stop != NULL) {
        hqq_plat_event_signal(a->hb_stop);
    }
    if (a->hb != NULL) {
        hqq_plat_thread_join(al, a->hb);
        a->hb = NULL;
    }
    if (a->hb_stop != NULL) {
        hqq_plat_event_destroy(al, a->hb_stop);
        a->hb_stop = NULL;
    }
    if (a->seg.h != NULL) {
        (void)lsn_cas(&a->seg.h->server_token, a->token, HQQ_DETACHED_TOKEN);
    }
    while (LSN_LD(&a->inflight) != 0) {
        hqq_plat_sleep_ns(LSN_POLL_NS);
    }
    st = hqq_named_release(al, a->seg.named);
    lsn_destroy(a);
    return st;
}

/* Undoes a create that failed before the segment was served. */
static hqq_status lsn_create_fail(hqq_acceptor *a, hqq_status st) {
    (void)hqq_named_release(lsn_acc_alloc(a), a->seg.named);
    lsn_destroy(a);
    return st;
}

void hqq_acceptor_options_init(hqq_acceptor_options *o) {
    if (o == NULL) {
        return;
    }
    memset(o, 0, sizeof *o);
    o->struct_size = sizeof *o;
    hqq_adv_options_init(&o->link);
}

hqq_status hqq_acceptor_create(const char *name, const hqq_acceptor_options *in, hqq_acceptor **out,
                               hqq_open_info *info) {
    hqq_acceptor_options defaults;
    const hqq_acceptor_options *o = in;
    hqq_acceptor *a;
    lsn_acc_cfg cfg;
    unsigned char prefix[LSN_PREFIX_MAX];
    size_t plen = 0;
    hqq_status st;
    int os_err = 0;
    const hqq_allocator *al;

    if (out == NULL || name == NULL) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    *out = NULL;
    if (info != NULL) {
        info->os_error = 0;
    }
    if (o == NULL) {
        hqq_acceptor_options_init(&defaults);
        o = &defaults;
    }
    if (o->struct_size != sizeof *o) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    st = lsn_cfg_init(o, &cfg);
    if (st != HQQ_OK) {
        return st;
    }
    st = lsn_derive_prefix(name, prefix, &plen);
    if (st != HQQ_OK) {
        return st;
    }
    al = lsn_alloc(&cfg.timing);
    a = hqq_alloc(al, sizeof *a);
    if (a == NULL) {
        return HQQ_ERR_NO_MEMORY;
    }
    memset(a, 0, sizeof *a);
    a->cfg = cfg;
    memcpy(a->prefix, prefix, plen);
    a->prefix_len = plen;
    atomic_flag_clear(&a->lock.f);
    hqq_cancel_init(&a->cancel);
    a->jobs = hqq_alloc(al, cfg.max_hs * sizeof *a->jobs);
    a->backlog = hqq_alloc(al, cfg.backlog * sizeof *a->backlog);
    if (a->jobs == NULL || a->backlog == NULL) {
        lsn_destroy(a);
        return HQQ_ERR_NO_MEMORY;
    }
    memset(a->jobs, 0, cfg.max_hs * sizeof *a->jobs);
    for (uint64_t i = 0; i < cfg.max_hs; i++) {
        a->jobs[i].acc = a;
    }
    if (hqq_new_token(&a->token) != 0 || hqq_new_token(&a->creator) != 0) {
        lsn_destroy(a);
        return HQQ_ERR_SYSTEM;
    }
    a->seg.named = lsn_named_new(al, name);
    if (a->seg.named == NULL) {
        lsn_destroy(a);
        return HQQ_ERR_NO_MEMORY;
    }
    a->seg.named->instance = a->creator;
    st = lsn_open_or_create(al, a->seg.named, (size_t)cfg.layout, cfg.perm, &os_err);
    if (st != HQQ_OK) {
        if (info != NULL) {
            info->os_error = os_err;
        }
        return lsn_create_fail(a, st);
    }
    a->seg.h = (lsn_header *)a->seg.named->map;
    st = lsn_claim(a);
    if (st != HQQ_OK) {
        return lsn_create_fail(a, st);
    }
    /* The segment is ours now: a failure from here on removes the name (LISTENER 8). */
    a->seg.named->unlink_on_close = 1;
    a->seg.named->fresh = 0;
    st = lsn_initialize(a);
    if (st != HQQ_OK) {
        return lsn_create_fail(a, st);
    }
    hqq_plat_wait_init(&a->seg.wait, a->seg.named->shm, 0, cfg.timing.flags);
    st = lsn_ring_attach(&a->seg.ring, (unsigned char *)a->seg.h + LSN_RING_OFFSET, &a->seg.wait);
    if (st == HQQ_OK) {
        st = lsn_start_threads(a);
    }
    if (st != HQQ_OK) {
        (void)lsn_shutdown(a);
        return st;
    }
    *out = a;
    return HQQ_OK;
}

/* Waits for a negotiated link in the backlog. The ready event is reset under the lock
 * when the backlog is empty and signalled under the same lock by lsn_deliver and by
 * close, so a delivery can not be missed between the check and the wait. */
static hqq_status lsn_accept_wait(hqq_acceptor *a, hqq_adv **out, const hqq_wait *wait) {
    hqq_ctx ctx;
    lsn_ctx_from_wait(wait, &ctx);
    for (;;) {
        hqq_status st;
        if (LSN_LD(&a->closing)) {
            return HQQ_ERR_ACCEPTOR_CLOSED;
        }
        hqq_spin_lock(&a->lock);
        if (a->bl_count > 0) {
            *out = a->backlog[a->bl_head];
            a->bl_head = (a->bl_head + 1) % a->cfg.backlog;
            a->bl_count--;
            hqq_spin_unlock(&a->lock);
            LSN_ADD(&a->st_accepted, 1);
            return HQQ_OK;
        }
        hqq_plat_event_reset(a->ready);
        hqq_spin_unlock(&a->lock);
        st = hqq_ctx_check(&ctx);
        if (st != HQQ_OK) {
            return st;
        }
        (void)hqq_plat_event_wait(a->ready, LSN_SLICE_NS);
    }
}

hqq_status hqq_acceptor_accept(hqq_acceptor *a, hqq_adv **out, const hqq_wait *wait) {
    hqq_status st;
    if (a == NULL || out == NULL) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    *out = NULL;
    LSN_ADD(&a->inflight, 1);
    st = lsn_accept_wait(a, out, wait);
    atomic_fetch_sub_explicit(&a->inflight, 1, memory_order_seq_cst);
    return st;
}

hqq_status hqq_acceptor_close(hqq_acceptor *a) {
    if (a == NULL) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    return lsn_shutdown(a);
}

void hqq_acceptor_stats_get(const hqq_acceptor *a, hqq_acceptor_stats *out) {
    if (a == NULL || out == NULL) {
        return;
    }
    out->records = LSN_LD(&a->st_records);
    out->accepted = LSN_LD(&a->st_accepted);
    out->malformed = LSN_LD(&a->st_malformed);
    out->unreachable = LSN_LD(&a->st_unreachable);
    out->handshake_failed = LSN_LD(&a->st_failed);
    out->stuck_slots = LSN_LD(&a->st_stuck);
}

/* ---- Client ------------------------------------------------------------------------------ */

void hqq_connect_options_init(hqq_connect_options *o) {
    if (o == NULL) {
        return;
    }
    memset(o, 0, sizeof *o);
    o->struct_size = sizeof *o;
    hqq_adv_options_init(&o->link);
}

/* Normalized connect options. */
typedef struct lsn_conn {
    hqq_adv_options link;
    hqq_open_req timing;
    uint64_t count, size, features;
    unsigned perm;
} lsn_conn;

/* A client's view of a listener: the mapped segment, its limits and prefix, the watch. */
typedef struct lsn_view {
    lsn_seg seg;
    uint64_t max_bc, max_bs, max_link;
    unsigned char prefix[LSN_PREFIX_MAX];
    size_t prefix_len;
    lsn_watch watch;
} lsn_view;

/* Maps the listener and checks it (7.2): the header first, then the whole segment. */
static hqq_status lsn_view_map(const hqq_allocator *al, const char *name, const hqq_open_req *t, lsn_view *v) {
    hqq_named *n = v->seg.named;
    uint64_t page = hqq_plat_page_size(), layout = 0, layout2 = 0, plen;
    int os_err = 0;
    int64_t cur;
    hqq_status st = hqq_plat_shm_open(al, name, 0, 0, 0, &n->shm, &os_err);
    lsn_header *h;

    if (st != HQQ_OK) {
        return st;
    }
    cur = hqq_plat_shm_size(n->shm, &os_err);
    if (cur < 0) {
        return HQQ_ERR_SYSTEM;
    }
    if (cur == 0) {
        return HQQ_ERR_SEGMENT_NOT_READY;
    }
    if (cur < (int64_t)LSN_HEADER_SIZE) {
        return HQQ_ERR_INCOMPATIBLE_LAYOUT;
    }
    st = hqq_plat_shm_map(n->shm, LSN_HEADER_SIZE, &n->map, &os_err);
    if (st != HQQ_OK) {
        return st;
    }
    n->map_size = LSN_HEADER_SIZE;
    st = lsn_inspect((const lsn_header *)n->map, cur, page, &layout);
    hqq_plat_shm_unmap(n->shm, n->map, n->map_size);
    n->map = NULL;
    if (st != HQQ_OK) {
        return st;
    }
    st = hqq_plat_shm_map(n->shm, (size_t)layout, &n->map, &os_err);
    if (st != HQQ_OK) {
        return st;
    }
    n->map_size = (size_t)layout;
    if ((uintptr_t)n->map % page != 0) {
        return HQQ_ERR_MEMORY_ALIGN;
    }
    h = (lsn_header *)n->map;
    st = lsn_inspect(h, cur, page, &layout2);
    if (st == HQQ_OK && layout2 != layout) {
        st = HQQ_ERR_LISTENER_NOT_READY; /* replaced between the two mappings */
    }
    if (st != HQQ_OK) {
        return st;
    }
    if (!lsn_server_alive(h, (uint64_t)t->peer_timeout_ns)) {
        return HQQ_ERR_NO_LIVE_ENDPOINT;
    }
    hqq_plat_wait_init(&v->seg.wait, n->shm, 0, t->flags);
    st = lsn_ring_attach(&v->seg.ring, (unsigned char *)n->map + LSN_RING_OFFSET, &v->seg.wait);
    if (st != HQQ_OK) {
        return st;
    }
    plen = LSN_LD(&h->prefix_len);
    if (plen == 0 || plen > LSN_PREFIX_MAX) {
        return HQQ_ERR_INCOMPATIBLE_LAYOUT;
    }
    memcpy(v->prefix, h->prefix, (size_t)plen);
    v->prefix_len = (size_t)plen;
    v->max_bc = LSN_LD(&h->max_buffer_count);
    v->max_bs = LSN_LD(&h->max_buffer_size);
    v->max_link = LSN_LD(&h->max_link_size);
    v->seg.h = h;
    lsn_watch_start(&v->watch, h, (uint64_t)t->peer_timeout_ns);
    return HQQ_OK;
}

static hqq_status lsn_view_open(const hqq_allocator *al, const char *name, const hqq_open_req *t, lsn_view *v) {
    hqq_status st;
    v->seg.named = lsn_named_new(al, name);
    if (v->seg.named == NULL) {
        return HQQ_ERR_NO_MEMORY;
    }
    st = lsn_view_map(al, name, t, v);
    if (st != HQQ_OK) {
        (void)hqq_named_release(al, v->seg.named);
        v->seg.named = NULL;
    }
    return st;
}

static void lsn_view_close(const hqq_allocator *al, lsn_view *v) {
    (void)hqq_named_release(al, v->seg.named);
    v->seg.named = NULL;
}

/* Geometry check of a connect request against the limits the acceptor published (7.3 step 1). */
static hqq_status lsn_admits(const lsn_view *v, uint64_t count, uint64_t size) {
    uint64_t total;
    if (!hqq_geometry_valid(count, size)) {
        return HQQ_ERR_INVALID_SIZE;
    }
    total = hqq_standard_size(count, size);
    if (count > v->max_bc || size > v->max_bs || total == 0 || total > v->max_link) {
        return HQQ_ERR_INVALID_SIZE;
    }
    return HQQ_OK;
}

/* Enqueues a record under the claim/publish protocol; waits for room, watching the acceptor. */
static hqq_status lsn_enqueue(lsn_view *v, const lsn_record *rec, const hqq_ctx *ctx) {
    lsn_ring *r = &v->seg.ring;
    for (;;) {
        uint64_t pos = 0;
        hqq_status st;
        if (lsn_try_claim_producer(r, &pos)) {
            memcpy(&lsn_slot_at(r, pos)->rec, rec, sizeof *rec); /* the last write before publish */
            lsn_publish(r, pos);
            return HQQ_OK;
        }
        st = hqq_ctx_check(ctx);
        if (st != HQQ_OK) {
            return st;
        }
        st = lsn_watch_check(&v->watch);
        if (st != HQQ_OK) {
            return st;
        }
        lsn_park(r, HQQ_RING_NOT_FULL, pos, pos, LSN_SLICE_NS);
    }
}

/* The helper thread of the negotiation: the client's side of hqq_adv_negotiate. */
typedef struct lsn_neg {
    hqq_adv *link;
    uint64_t features;
    hqq_cancel *cancel;
    hqq_plat_event *done;
    hqq_status st;
} lsn_neg;

static void lsn_neg_main(void *arg) {
    lsn_neg *n = arg;
    hqq_wait w;
    w.timeout_ns = -1;
    w.cancel = n->cancel;
    n->st = hqq_adv_negotiate(n->link, n->features, &w);
    hqq_plat_event_signal(n->done);
}

/*
 * Negotiates as Primary on a helper thread while this thread watches the acceptor
 * (7.4) and the attach deadline, and the caller's wait. Stops the helper on any cause.
 */
static hqq_status lsn_negotiate_watched(hqq_adv *link, uint64_t features, const hqq_ctx *ctx, lsn_watch *w,
                                        uint64_t attach_at, const hqq_allocator *al) {
    hqq_cancel cancel;
    lsn_neg n;
    hqq_plat_thread *t;
    hqq_plat_event *ev;
    hqq_status cause = HQQ_OK;
    int os_err = 0;

    ev = hqq_plat_event_create(al);
    if (ev == NULL) {
        return HQQ_ERR_NO_MEMORY;
    }
    hqq_cancel_init(&cancel);
    n.link = link;
    n.features = features;
    n.cancel = &cancel;
    n.done = ev;
    n.st = HQQ_OK;
    t = hqq_plat_thread_start(al, lsn_neg_main, &n, &os_err);
    if (t == NULL) {
        hqq_plat_event_destroy(al, ev);
        return HQQ_ERR_SYSTEM;
    }
    for (;;) {
        if (hqq_plat_event_wait(ev, LSN_SLICE_NS)) {
            break;
        }
        cause = hqq_ctx_check(ctx);
        if (cause != HQQ_OK) {
            break;
        }
        if (hqq_adv_peer_state(link) == HQQ_PEER_NOT_CONNECTED) {
            cause = lsn_watch_check(w);
            if (cause != HQQ_OK) {
                break;
            }
            if (hqq_plat_mono_ns() >= attach_at) {
                cause = HQQ_ERR_TIMEOUT;
                break;
            }
        }
    }
    if (cause != HQQ_OK) {
        hqq_cancel_signal(&cancel);
    }
    hqq_plat_thread_join(al, t);
    hqq_plat_event_destroy(al, ev);
    return cause != HQQ_OK ? cause : n.st;
}

/* One attempt of hqq_connect: view, create the link, enqueue, negotiate. */
static hqq_status lsn_connect_once(const hqq_ctx *ctx, const char *listener, const lsn_conn *c, hqq_adv **out) {
    const hqq_allocator *al = lsn_alloc(&c->timing);
    lsn_view v;
    hqq_adv *link = NULL;
    hqq_status st;
    lsn_record rec;
    char name[LSN_NAME_MAX + 1];
    int attempt;
    uint64_t attach_at;

    memset(&v, 0, sizeof v);
    st = lsn_view_open(al, listener, &c->timing, &v);
    if (st != HQQ_OK) {
        return st;
    }
    st = lsn_admits(&v, c->count, c->size);
    for (attempt = 0; st == HQQ_OK; attempt++) {
        st = lsn_link_name(v.prefix, v.prefix_len, name);
        if (st != HQQ_OK) {
            break;
        }
        st = hqq_adv_create(name, c->count, c->size, c->perm, &c->link, &link, NULL);
        if (st != HQQ_ERR_LINK_BUSY || attempt >= LSN_LINK_NAME_RETRIES) {
            break;
        }
    }
    if (st != HQQ_OK) {
        lsn_view_close(al, &v);
        return st;
    }
    memset(&rec, 0, sizeof rec);
    rec.client_token = LSN_LD(&hqq_adv_base(link)->sb->creator_token);
    rec.buffer_count = c->count;
    rec.buffer_size = c->size;
    rec.features = c->features;
    rec.name_len = strlen(name);
    rec.client_pid = hqq_plat_pid();
    memcpy(rec.name, name, (size_t)rec.name_len);
    st = lsn_enqueue(&v, &rec, ctx);
    if (st == HQQ_OK) {
        attach_at = hqq_plat_mono_ns() + (uint64_t)c->timing.attach_timeout_ns;
        st = lsn_negotiate_watched(link, c->features, ctx, &v.watch, attach_at, al);
    }
    if (st != HQQ_OK) {
        hqq_adv_close(link); /* removes the client's segment */
        lsn_view_close(al, &v);
        return st;
    }
    lsn_view_close(al, &v);
    *out = link;
    return HQQ_OK;
}

/* Transient open states that a client retries (7.2, 7.3). */
static int lsn_retryable(hqq_status st) {
    return st == HQQ_ERR_NOT_EXIST || st == HQQ_ERR_SEGMENT_NOT_READY || st == HQQ_ERR_LISTENER_NOT_READY ||
           st == HQQ_ERR_NO_LIVE_ENDPOINT;
}

hqq_status hqq_connect(const char *listener_name, const hqq_connect_options *in, const hqq_wait *wait,
                       hqq_adv **out) {
    hqq_connect_options defaults;
    const hqq_connect_options *o = in;
    lsn_conn c;
    hqq_ctx ctx;
    hqq_status st;
    uint64_t backoff = LSN_MS(1);

    if (listener_name == NULL || out == NULL) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    *out = NULL;
    if (o == NULL) {
        hqq_connect_options_init(&defaults);
        o = &defaults;
    }
    if (o->struct_size != sizeof *o) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    memset(&c, 0, sizeof c);
    st = hqq_options_normalize(&o->link.link, &c.timing, NULL);
    if (st != HQQ_OK) {
        return st;
    }
    c.link = o->link;
    c.count = o->buffer_count != 0 ? o->buffer_count : 256u;
    c.size = o->buffer_size != 0 ? o->buffer_size : 4096u;
    c.perm = o->perm != 0 ? o->perm : 0600u;
    c.features = o->features != 0 ? o->features : HQQ_FEATURES_ALL;
    if (!hqq_geometry_valid(c.count, c.size)) {
        return HQQ_ERR_INVALID_SIZE;
    }
    lsn_ctx_from_wait(wait, &ctx);
    for (;;) {
        st = lsn_connect_once(&ctx, listener_name, &c, out);
        if (st == HQQ_OK || !lsn_retryable(st)) {
            return st;
        }
        st = hqq_ctx_sleep(&ctx, backoff);
        if (st != HQQ_OK) {
            return st;
        }
        backoff = lsn_min(backoff * 2, LSN_BACKOFF_MAX_NS);
    }
}
