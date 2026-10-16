/*
 * hqq_ring.h - the HQQ ring core: a bounded Vyukov MPMC queue in shared
 * memory (SPEC 7, MEMORY_MODEL 2.3/2.4/4).
 *
 * This header is deliberately OS independent: it needs only <stdatomic.h>,
 * <stdint.h> and <stddef.h>. No syscall, no malloc, no platform header. The
 * two places where the protocol needs the operating system (parking a waiter
 * on a wake word, waking it) go through the hqq_ring_ops function pointers, so
 * a model checker (GenMC) can compile this file with its own trivial
 * implementations of them.
 *
 * Memory orders (MEMORY_MODEL.md):
 *   claim:   load seq ACQUIRE; cursor CAS RELAXED
 *   publish / release: store seq SEQ_CST (REL + the Dekker pair K1/K2)
 *   waker:   load waiters SEQ_CST (K2); fetch_add wake word SEQ_CST (K3)
 *   waiter:  fetch_add waiters SEQ_CST (W1); load word SEQ_CST (W2);
 *            re-check load seq SEQ_CST (W3); fetch_sub waiters RELEASE (W4)
 *   header:  fields RELAXED, `flag` stored RELEASE last; attach loads
 *            magic and flag ACQUIRE
 *   payload: plain memory (memcpy), owned by the slot's claimant
 *
 * The four wake handshake accesses W1/W3/K1/K2 form a store-buffering pair
 * and MUST stay sequentially consistent (release/acquire loses wake-ups).
 *
 * Positions are uint64_t counters modulo 2^64. Differences are computed in
 * uint64_t and then reinterpreted as int64_t (never signed arithmetic).
 */
#ifndef HQQ_RING_H
#define HQQ_RING_H

#include <stdatomic.h>
#include <stddef.h>
#include <stdint.h>

#if defined(__BYTE_ORDER__) && defined(__ORDER_LITTLE_ENDIAN__) && \
    __BYTE_ORDER__ != __ORDER_LITTLE_ENDIAN__
#  error "HQQ requires a little-endian host (SPEC 13.3)"
#endif
#if ATOMIC_INT_LOCK_FREE != 2 || ATOMIC_LLONG_LOCK_FREE != 2
#  error "HQQ requires always-lock-free 32-bit and 64-bit C11 atomics"
#endif

/* ---- Constants (constants.json "ring") --------------------------------- */

#define HQQ_RING_HEADER_SIZE 512u
#define HQQ_RING_SLOT_STRIDE 128u
#define HQQ_RING_SLOT_PACKET_OFFSET 8u
#define HQQ_RING_PACKET_SIZE 64u
#define HQQ_RING_MAGIC UINT64_C(0xC9D8C1D43F096702)
#define HQQ_RING_VERSION UINT64_C(2)
#define HQQ_RING_FLAG_INIT UINT64_C(2)
#define HQQ_RING_MIN_SLOTS 2u
#define HQQ_RING_FORMAT_SLICE 1024u /* slots written between two ownership checks */

/* The two conditions a blocked operation can wait for. */
#define HQQ_RING_NOT_EMPTY 0 /* awaited by consumers, signalled by producers */
#define HQQ_RING_NOT_FULL 1  /* awaited by producers, signalled by consumers */

/* ---- Memory image ------------------------------------------------------- */

typedef struct hqq_ring_header {
    /* line 0: immutable after format */
    _Atomic uint64_t magic;   /* @0 */
    _Atomic uint64_t size;    /* @8 */
    _Atomic uint64_t flag;    /* @16 */
    _Atomic uint64_t stride;  /* @24 */
    _Atomic uint64_t version; /* @32 */
    unsigned char pad0[128 - 40];
    /* line 1 */
    _Atomic uint64_t r;       /* @128 consumer cursor */
    unsigned char pad1[128 - 8];
    /* line 2 */
    _Atomic uint64_t w;       /* @256 producer cursor */
    unsigned char pad2[128 - 8];
    /* line 3: wake words and waiter counts */
    _Atomic uint32_t not_empty;     /* @384 */
    _Atomic uint32_t empty_waiters; /* @388 */
    _Atomic uint32_t not_full;      /* @392 */
    _Atomic uint32_t full_waiters;  /* @396 */
    unsigned char pad3[128 - 16];
} hqq_ring_header;

typedef struct hqq_slot {
    _Atomic uint64_t seq;                         /* @0 */
    unsigned char packet[HQQ_RING_PACKET_SIZE];   /* @8, plain memory */
    unsigned char pad[HQQ_RING_SLOT_STRIDE - 8 - HQQ_RING_PACKET_SIZE];
} hqq_slot;

_Static_assert(sizeof(hqq_ring_header) == HQQ_RING_HEADER_SIZE, "ring header is 512 bytes");
_Static_assert(offsetof(hqq_ring_header, r) == 128, "ring r @128");
_Static_assert(offsetof(hqq_ring_header, w) == 256, "ring w @256");
_Static_assert(offsetof(hqq_ring_header, not_empty) == 384, "ring notEmpty @384");
_Static_assert(offsetof(hqq_ring_header, empty_waiters) == 388, "ring emptyWaiters @388");
_Static_assert(offsetof(hqq_ring_header, not_full) == 392, "ring notFull @392");
_Static_assert(offsetof(hqq_ring_header, full_waiters) == 396, "ring fullWaiters @396");
_Static_assert(sizeof(hqq_slot) == HQQ_RING_SLOT_STRIDE, "slot stride is 128");
_Static_assert(offsetof(hqq_slot, packet) == HQQ_RING_SLOT_PACKET_OFFSET, "packet @8");

/* ---- Platform hooks ------------------------------------------------------ */

/*
 * The only OS-dependent operations of the protocol. `ctx` is the per-ring
 * pointer stored in the handle. Either function pointer may be NULL (then the
 * ring never parks / never issues a kernel wake: simple mode).
 *
 *   wait: block while *word == expect, for at most timeout_ns; spurious
 *         returns are allowed. `side` selects the wait object on platforms
 *         that wake through objects instead of words (Windows semaphores).
 *   wake: wake every waiter of `side`. The wake word has already been bumped
 *         (K3); `waiters` is the registered count that triggered the wake.
 */
typedef struct hqq_ring_ops {
    void (*wait)(void *ctx, int side, _Atomic uint32_t *word, uint32_t expect, uint64_t timeout_ns);
    void (*wake)(void *ctx, int side, _Atomic uint32_t *word, uint32_t waiters);
} hqq_ring_ops;

/* A process-local handle onto a formatted ring. Cursors are always read from
 * shared memory; the handle caches only geometry. */
typedef struct hqq_ring {
    hqq_ring_header *h;
    hqq_slot *slots;
    uint64_t mask;
    uint64_t size;
    const hqq_ring_ops *ops; /* NULL: no kernel wait / wake */
    void *ops_ctx;
} hqq_ring;

/* ---- Format / attach (SPEC 7.2, 7.3) ------------------------------------ */

#define HQQ_RING_OK 0
#define HQQ_RING_NOT_READY 1     /* attach: header not initialized (yet) */
#define HQQ_RING_INCOMPATIBLE (-1) /* attach: header conflicts with this layout; format: invalid size */

static inline int hqq_ring_valid_size(uint64_t n) {
    return n >= HQQ_RING_MIN_SLOTS && (n & (n - 1)) == 0;
}

/* Bytes of a ring with n slots (0 for an invalid n). */
static inline size_t hqq_ring_bytes(uint64_t n) {
    return hqq_ring_valid_size(n) ? (size_t)(HQQ_RING_HEADER_SIZE + (uint64_t)HQQ_RING_SLOT_STRIDE * n) : 0u;
}

/*
 * Formats the ring at `mem` (n slots) in the sole presence of the
 * initializer. Stores happen in the order of SPEC 7.2: flag and magic are
 * cleared first, `flag = 2` (release) is last, so a concurrent attach never
 * accepts a half-built ring; wake words and both waiter counts are zeroed.
 *
 * check(ctx), if not NULL, runs before the first write, before every
 * HQQ_RING_FORMAT_SLICE slots and before the header is completed. A non-zero
 * return stops the format at once and is returned (the ring is left without a
 * valid magic, exactly as after a crash in the middle). Returns 0 when done,
 * HQQ_RING_INCOMPATIBLE for an invalid n.
 */
static inline int hqq_ring_format(void *mem, uint64_t n, int (*check)(void *), void *check_ctx) {
    hqq_ring_header *h = (hqq_ring_header *)mem;
    hqq_slot *slots = (hqq_slot *)((unsigned char *)mem + HQQ_RING_HEADER_SIZE);
    int rc;
    uint64_t i;
    unsigned j;

    if (!hqq_ring_valid_size(n)) {
        return HQQ_RING_INCOMPATIBLE;
    }
    if (check != NULL && (rc = check(check_ctx)) != 0) {
        return rc;
    }
    atomic_store_explicit(&h->flag, 0, memory_order_relaxed);
    atomic_store_explicit(&h->magic, 0, memory_order_relaxed);
    for (i = 0; i < n; i++) {
        hqq_slot *s = &slots[i];
        if (check != NULL && i != 0 && i % HQQ_RING_FORMAT_SLICE == 0 &&
            (rc = check(check_ctx)) != 0) {
            return rc;
        }
        for (j = 0; j < HQQ_RING_PACKET_SIZE; j++) {
            s->packet[j] = 0;
        }
        atomic_store_explicit(&s->seq, i, memory_order_relaxed);
    }
    if (check != NULL && (rc = check(check_ctx)) != 0) {
        return rc;
    }
    atomic_store_explicit(&h->r, 0, memory_order_relaxed);
    atomic_store_explicit(&h->w, 0, memory_order_relaxed);
    atomic_store_explicit(&h->not_empty, 0, memory_order_relaxed);
    atomic_store_explicit(&h->empty_waiters, 0, memory_order_relaxed);
    atomic_store_explicit(&h->not_full, 0, memory_order_relaxed);
    atomic_store_explicit(&h->full_waiters, 0, memory_order_relaxed);
    atomic_store_explicit(&h->size, n, memory_order_relaxed);
    atomic_store_explicit(&h->stride, HQQ_RING_SLOT_STRIDE, memory_order_relaxed);
    atomic_store_explicit(&h->version, HQQ_RING_VERSION, memory_order_relaxed);
    atomic_store_explicit(&h->magic, HQQ_RING_MAGIC, memory_order_relaxed);
    atomic_store_explicit(&h->flag, HQQ_RING_FLAG_INIT, memory_order_release);
    return 0;
}

/*
 * One attach attempt (SPEC 7.3): HQQ_RING_OK and *r filled, HQQ_RING_NOT_READY
 * if the header is not initialized yet (the caller polls), or
 * HQQ_RING_INCOMPATIBLE at once if stride, version or size do not match.
 */
static inline int hqq_ring_attach(hqq_ring *r, void *mem) {
    hqq_ring_header *h = (hqq_ring_header *)mem;
    uint64_t size;

    if (atomic_load_explicit(&h->magic, memory_order_acquire) != HQQ_RING_MAGIC ||
        (atomic_load_explicit(&h->flag, memory_order_acquire) & HQQ_RING_FLAG_INIT) == 0) {
        return HQQ_RING_NOT_READY;
    }
    size = atomic_load_explicit(&h->size, memory_order_relaxed);
    if (atomic_load_explicit(&h->stride, memory_order_relaxed) != HQQ_RING_SLOT_STRIDE ||
        atomic_load_explicit(&h->version, memory_order_relaxed) != HQQ_RING_VERSION ||
        !hqq_ring_valid_size(size)) {
        return HQQ_RING_INCOMPATIBLE;
    }
    r->h = h;
    r->slots = (hqq_slot *)((unsigned char *)mem + HQQ_RING_HEADER_SIZE);
    r->mask = size - 1;
    r->size = size;
    r->ops = NULL;
    r->ops_ctx = NULL;
    return HQQ_RING_OK;
}

/* ---- Slots ---------------------------------------------------------------- */

static inline hqq_slot *hqq_ring_slot(const hqq_ring *r, uint64_t pos) {
    return &r->slots[pos & r->mask];
}

/* ---- Waker duty (SPEC 8.3) ------------------------------------------------ */

static inline _Atomic uint32_t *hqq_ring_word(const hqq_ring *r, int side) {
    return side == HQQ_RING_NOT_EMPTY ? &r->h->not_empty : &r->h->not_full;
}

static inline _Atomic uint32_t *hqq_ring_waiters(const hqq_ring *r, int side) {
    return side == HQQ_RING_NOT_EMPTY ? &r->h->empty_waiters : &r->h->full_waiters;
}

/* K2, K3: after the seq store (K1), wake the parked side if anyone registered. */
static inline void hqq_ring_notify(const hqq_ring *r, int side) {
    uint32_t n = atomic_load_explicit(hqq_ring_waiters(r, side), memory_order_seq_cst); /* K2 */
    if (n != 0) {
        _Atomic uint32_t *word = hqq_ring_word(r, side);
        atomic_fetch_add_explicit(word, 1, memory_order_seq_cst);                       /* K3 */
        if (r->ops != NULL && r->ops->wake != NULL) {
            r->ops->wake(r->ops_ctx, side, word, n);
        }
    }
}

/* Wakes every waiter of one side unconditionally (not gated by the count). */
static inline void hqq_ring_wake_side(const hqq_ring *r, int side) {
    _Atomic uint32_t *word = hqq_ring_word(r, side);
    atomic_fetch_add_explicit(word, 1, memory_order_seq_cst);
    if (r->ops != NULL && r->ops->wake != NULL) {
        uint32_t n = atomic_load_explicit(hqq_ring_waiters(r, side), memory_order_seq_cst);
        r->ops->wake(r->ops_ctx, side, word, n);
    }
}

/* WakeAll (SPEC 8.3): both sides, used on close and on peer state changes. */
static inline void hqq_ring_wake_all(const hqq_ring *r) {
    hqq_ring_wake_side(r, HQQ_RING_NOT_EMPTY);
    hqq_ring_wake_side(r, HQQ_RING_NOT_FULL);
}

/* ---- Producer ----------------------------------------------------------------- */

/*
 * One producer claim attempt (SPEC 7.4). Returns 1 and the claimed position
 * in *pos (the caller now owns slot pos & mask until it publishes), or 0 if the
 * ring is full, with *pos set to the position at which it was found full: the
 * waiter waits for seq[*pos & mask] to reach *pos. Several producers may call
 * this concurrently (CAS cursor).
 */
static inline int hqq_ring_try_claim_producer(const hqq_ring *r, uint64_t *pos) {
    uint64_t p = atomic_load_explicit(&r->h->w, memory_order_relaxed);
    for (;;) {
        hqq_slot *s = &r->slots[p & r->mask];
        int64_t d = (int64_t)(atomic_load_explicit(&s->seq, memory_order_acquire) - p);
        if (d == 0) {
            if (atomic_compare_exchange_weak_explicit(&r->h->w, &p, p + 1,
                                                      memory_order_relaxed, memory_order_relaxed)) {
                *pos = p;
                return 1;
            }
            /* CAS failure reloaded p. */
        } else if (d > 0) {
            p = atomic_load_explicit(&r->h->w, memory_order_relaxed); /* another producer took p */
        } else {
            *pos = p;
            return 0;
        }
    }
}

/* Publish (K1, K2, K3): hand the claimed slot to the consumer. */
static inline void hqq_ring_publish(const hqq_ring *r, uint64_t pos) {
    atomic_store_explicit(&hqq_ring_slot(r, pos)->seq, pos + 1, memory_order_seq_cst); /* K1 */
    hqq_ring_notify(r, HQQ_RING_NOT_EMPTY);
}

/*
 * Publish through a handle that callers may have copied: completes the slot
 * with CAS seq pos -> pos+1, so a stale copy of an already completed claim
 * fails and neither modifies the ring nor wakes anyone. Returns 1 on success.
 */
static inline int hqq_ring_publish_guarded(const hqq_ring *r, uint64_t pos) {
    uint64_t expect = pos;
    if (!atomic_compare_exchange_strong_explicit(&hqq_ring_slot(r, pos)->seq, &expect, pos + 1,
                                                 memory_order_seq_cst, memory_order_seq_cst)) {
        return 0;
    }
    hqq_ring_notify(r, HQQ_RING_NOT_EMPTY);
    return 1;
}

/* Snapshot: is a producer claim at pos still owned (not yet completed)? */
static inline int hqq_ring_producer_owned(const hqq_ring *r, uint64_t pos) {
    return atomic_load_explicit(&hqq_ring_slot(r, pos)->seq, memory_order_acquire) == pos;
}

/* ---- Consumer ------------------------------------------------------------------- */

/*
 * One consumer claim attempt. Returns 1 and the claimed position, or 0 if the
 * ring is empty with *pos = the position being waited for (wait for
 * seq[*pos & mask] to reach *pos + 1). Several consumers may call this
 * concurrently (CAS cursor).
 */
static inline int hqq_ring_try_claim_consumer(const hqq_ring *r, uint64_t *pos) {
    uint64_t p = atomic_load_explicit(&r->h->r, memory_order_relaxed);
    for (;;) {
        hqq_slot *s = &r->slots[p & r->mask];
        int64_t d = (int64_t)(atomic_load_explicit(&s->seq, memory_order_acquire) - (p + 1));
        if (d == 0) {
            if (atomic_compare_exchange_weak_explicit(&r->h->r, &p, p + 1,
                                                      memory_order_relaxed, memory_order_relaxed)) {
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

/*
 * Peek: is the packet at the consumer cursor published? Returns 1 with *pos
 * set, without claiming. Only meaningful when the caller is the ONLY
 * consumer of this ring (the library serializes the consumer threads of its
 * process with a lock, and the protocol admits one consumer process): then
 * the packet cannot change until the caller claims it with
 * hqq_ring_claim_consumer_at() (which can then not fail) or leaves it.
 * Returns 0 if the ring is empty, with *pos = the position being waited for.
 */
static inline int hqq_ring_peek_consumer(const hqq_ring *r, uint64_t *pos) {
    uint64_t p = atomic_load_explicit(&r->h->r, memory_order_relaxed);
    *pos = p;
    return (int64_t)(atomic_load_explicit(&hqq_ring_slot(r, p)->seq, memory_order_acquire) - (p + 1)) >= 0;
}

/* Claims the already peeked position p (CAS cursor p -> p+1); 1 on success. */
static inline int hqq_ring_claim_consumer_at(const hqq_ring *r, uint64_t p) {
    return atomic_compare_exchange_strong_explicit(&r->h->r, &p, p + 1,
                                                   memory_order_relaxed, memory_order_relaxed);
}

/* Release (K1, K2, K3): return the consumed slot to the producers. */
static inline void hqq_ring_release(const hqq_ring *r, uint64_t pos) {
    atomic_store_explicit(&hqq_ring_slot(r, pos)->seq, pos + r->mask + 1, memory_order_seq_cst);
    hqq_ring_notify(r, HQQ_RING_NOT_FULL);
}

/* Release through a possibly copied handle: CAS seq pos+1 -> pos+mask+1. */
static inline int hqq_ring_release_guarded(const hqq_ring *r, uint64_t pos) {
    uint64_t expect = pos + 1;
    if (!atomic_compare_exchange_strong_explicit(&hqq_ring_slot(r, pos)->seq, &expect, pos + r->mask + 1,
                                                 memory_order_seq_cst, memory_order_seq_cst)) {
        return 0;
    }
    hqq_ring_notify(r, HQQ_RING_NOT_FULL);
    return 1;
}

static inline int hqq_ring_consumer_owned(const hqq_ring *r, uint64_t pos) {
    return atomic_load_explicit(&hqq_ring_slot(r, pos)->seq, memory_order_acquire) == pos + 1;
}

/* ---- Waiter (SPEC 8.2) ------------------------------------------------------------ */

/* Is the slot sequence still short of target? (SEQ_CST: part of the Dekker pair.) */
static inline int hqq_ring_blocked(const hqq_ring *r, uint64_t pos, uint64_t target) {
    return (int64_t)(atomic_load_explicit(&hqq_ring_slot(r, pos)->seq, memory_order_seq_cst) - target) < 0;
}

/*
 * Parks the calling thread until the slot at pos reaches target, a wake
 * arrives, `stop` says so, or timeout_ns elapsed (W1..W4). The caller found
 * the ring full/empty and re-runs its claim loop afterwards. target is pos for
 * a producer (NOT_FULL) and pos + 1 for a consumer (NOT_EMPTY).
 *
 * The count is per parked THREAD (SPEC 8.1 allows over-counting); it is
 * decremented exactly once per registration.
 */
static inline void hqq_ring_park(const hqq_ring *r, int side, uint64_t pos, uint64_t target,
                                 uint64_t timeout_ns, int (*stop)(void *), void *stop_ctx) {
    _Atomic uint32_t *word = hqq_ring_word(r, side);
    _Atomic uint32_t *waiters = hqq_ring_waiters(r, side);
    uint32_t v;

    atomic_fetch_add_explicit(waiters, 1, memory_order_seq_cst);                  /* W1 */
    v = atomic_load_explicit(word, memory_order_seq_cst);                         /* W2 */
    if (hqq_ring_blocked(r, pos, target) && (stop == NULL || !stop(stop_ctx))) {  /* W3 */
        if (r->ops != NULL && r->ops->wait != NULL) {
            r->ops->wait(r->ops_ctx, side, word, v, timeout_ns);
        }
    }
    atomic_fetch_sub_explicit(waiters, 1, memory_order_release);                  /* W4 */
}

#endif /* HQQ_RING_H */
