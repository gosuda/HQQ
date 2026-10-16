/*
 * hqq_internal.h - shared internals of libhqq (not installed).
 *
 * Tests under c/tests include this header to drive the superblock engine on
 * crafted images, exactly like the Go tests of superblock.go do.
 */
#ifndef HQQ_INTERNAL_H
#define HQQ_INTERNAL_H

#include <stdatomic.h>
#include <stdlib.h>
#include <string.h>

#include "hqq/hqq.h"
#include "hqq_layout.h"
#include "hqq_platform.h"
#include "hqq_ring.h"

/* ---- Allocator -------------------------------------------------------------- */

static inline void *hqq_alloc(const hqq_allocator *a, size_t size) {
    if (a != NULL && a->alloc != NULL) {
        return a->alloc(a->ctx, size);
    }
    return malloc(size);
}

static inline void hqq_free(const hqq_allocator *a, void *p, size_t size) {
    if (p == NULL) {
        return;
    }
    if (a != NULL && a->free != NULL) {
        a->free(a->ctx, p, size);
        return;
    }
    free(p);
}

/* ---- Tiny spin lock (process-local, short critical sections only) -------------- */

typedef struct hqq_spin {
    atomic_flag f;
} hqq_spin;

#define HQQ_SPIN_INIT {ATOMIC_FLAG_INIT}

static inline void hqq_spin_lock(hqq_spin *s) {
    unsigned n = 0;
    while (atomic_flag_test_and_set_explicit(&s->f, memory_order_acquire)) {
        if (++n >= 64) {
            hqq_plat_yield();
            n = 0;
        }
    }
}

static inline void hqq_spin_unlock(hqq_spin *s) {
    atomic_flag_clear_explicit(&s->f, memory_order_release);
}

/* ---- Cancellation and deadlines ------------------------------------------------- */

typedef struct hqq_ctx {
    const hqq_cancel *cancel; /* may be NULL */
    uint64_t deadline_ns;     /* absolute monotonic ns; 0 = none */
} hqq_ctx;

int hqq_cancel_load(const hqq_cancel *c); /* 1 if signalled */

/* HQQ_OK, HQQ_ERR_CANCELED or HQQ_ERR_TIMEOUT. */
hqq_status hqq_ctx_check(const hqq_ctx *ctx);
/* Sleeps ns in slices of at most 1 ms, returning early with the ctx error. */
hqq_status hqq_ctx_sleep(const hqq_ctx *ctx, uint64_t ns);

/* ---- Names (portable, hqq_names.c) --------------------------------------------- */

/*
 * SPEC 11.1: strips one leading '/', requires a non-empty base that is not "."
 * or "..", contains no '/', '\\' (or NUL) and is at most 255 bytes. max_len > 0
 * additionally bounds strlen(name). Returns HQQ_OK or HQQ_ERR_INVALID_NAME.
 */
hqq_status hqq_name_base(const char *name, size_t max_len, const char **base, size_t *base_len);

/*
 * Windows kernel object name of a portable name (SPEC 11.1): "Global\x" and
 * "Local\x" (prefix matched ASCII case-insensitively, canonical spelling in
 * the result) are kept, other names get "Local\". The result plus the longest
 * semaphore suffix (".hqq-sem0", 9 chars) must fit 259 UTF-16 units.
 * Portable so that every platform tests the vectors. Writes a NUL-terminated
 * UTF-8 string into out (cap bytes); *out_len excludes the NUL.
 */
hqq_status hqq_windows_object_name(const char *name, char *out, size_t cap, size_t *out_len);

/* ---- Superblock engine (hqq_superblock.c, SPEC 5, 6) --------------------------- */

typedef enum hqq_role {
    HQQ_ROLE_ANY = 0,     /* caller-owned memory: may initialize or join */
    HQQ_ROLE_CREATOR = 1, /* initializes / re-initializes / takes over; never joins a live segment */
    HQQ_ROLE_OPENER = 2   /* joins an existing live segment only */
} hqq_role;

/* Everything the claim loop and the initializer need. All durations are
 * already normalized (positive, except heartbeat_interval_ns where 0 means
 * heartbeats are disabled). */
typedef struct hqq_claim_params {
    hqq_layout layout;            /* the caller's geometry (count, size, page, total) */
    uint64_t token;               /* fresh owner token (not 0 or 1) */
    uint64_t creator_token;       /* stored by an initializer; 0 for HQQ_ROLE_ANY */
    hqq_role role;
    int64_t attach_timeout_ns;
    int64_t peer_timeout_ns;
    int64_t init_stale_ns;        /* <= 0: max(peer_timeout, 2 s) */
    int64_t heartbeat_interval_ns;/* 0 = disabled */
    hqq_ctx ctx;
} hqq_claim_params;

static inline int hqq_is_owner_token(uint64_t t) { return t != 0 && t != HQQ_DETACHED_TOKEN; }

/* A fresh random owner token (never 0 or 1). Returns 0 on success. */
int hqq_new_token(uint64_t *tok);

/* Reads the whole first page atomically: 1 if every word is zero. */
int hqq_sb_first_page_zero(hqq_superblock *sb, uint64_t page_size);

/* SPEC 5.1 checkLayoutCompat: HQQ_OK or HQQ_ERR_INCOMPATIBLE_LAYOUT. */
hqq_status hqq_sb_check_layout(hqq_superblock *sb, int initializing);

/* SPEC 6.2 endpointAliveAtOpen. *alive is set; the status is the ctx error if
 * the confirmation watch was cancelled. */
hqq_status hqq_sb_endpoint_alive_at_open(const hqq_ctx *ctx, hqq_endpoint_record *rec,
                                         int64_t peer_timeout_ns, int *alive);

/*
 * SPEC 5.1 claim loop. On HQQ_OK: *mode is the role reached; *must_init is 1
 * when the caller won the right to (re)initialize the segment and must run
 * hqq_sb_initialize(). HQQ_ERR_GEOMETRY_MISMATCH fills *got (may be NULL).
 */
hqq_status hqq_sb_claim(hqq_superblock *sb, const hqq_claim_params *p,
                        hqq_mode *mode, int *must_init, hqq_geometry *got);

/* SPEC 5.2 beginInit: 1 on success, 0 if another endpoint took over. */
int hqq_sb_begin_init(hqq_superblock *sb, uint64_t token, uint64_t expected, uint64_t creator_token);

/* SPEC 5.3 publishSuperblock + commit. *epoch receives the new epoch. */
hqq_status hqq_sb_publish(hqq_superblock *sb, const hqq_claim_params *p, uint64_t *epoch);

/*
 * SPEC 5.3 steps 1-3 for the winner of hqq_sb_claim(): format both rings,
 * zero both buffer pools, publish. `base` is the segment start. Every step
 * first checks ownership and the ctx.
 */
hqq_status hqq_sb_initialize(unsigned char *base, const hqq_claim_params *p, uint64_t *epoch);

/* ---- Link ------------------------------------------------------------------------ */

/* Named-segment state owned by a link (SPEC 11.3, 11.4). */
typedef struct hqq_named {
    char *name;            /* copy, NUL terminated */
    size_t name_size;      /* bytes allocated */
    hqq_shm *shm;
    void *map;
    size_t map_size;
    hqq_spin lock;         /* serializes release and unlink */
    int unlink_on_close;
    int fresh;             /* this endpoint created the object with O_EXCL */
    uint64_t instance;     /* creatorToken of the instance this endpoint joined */
} hqq_named;

#define HQQ_CACHELINE 128

struct hqq_link {
    hqq_allocator alloc;
    int alloc_set;
    hqq_mode mode;
    int self;
    uint64_t buffer_count;
    uint64_t buffer_size;
    hqq_layout layout;

    hqq_wait_policy policy;
    int64_t heartbeat_interval_ns; /* 0: disabled */
    int64_t peer_timeout_ns;
    int64_t attach_timeout_ns;

    hqq_superblock *sb;
    unsigned char *base;
    uint64_t token;
    uint64_t epoch;

    hqq_ring ring[2];            /* 0: primary->secondary, 1: secondary->primary */
    hqq_plat_wait wait[2];
    unsigned char *bufs[2];
    hqq_ring *tx, *rx;
    unsigned char *txbuf, *rxbuf;

    _Atomic uint64_t copy_id;
    hqq_spin rx_lock;            /* serializes the consumer threads of this process */

    /* peer monitor (SPEC 6.3) */
    _Atomic uint32_t peer_status;
    hqq_spin peer_lock;
    uint64_t peer_token, peer_last_hb, peer_last_change_ns;

    /* lifecycle */
    _Atomic uint32_t lifecycle;  /* 0 open, 1 closing, 2 closed */
    hqq_plat_thread *hb_thread;
    hqq_plat_event *hb_stop;
    _Atomic uint32_t hb_woken;

    unsigned char pad0[HQQ_CACHELINE];
    _Atomic int64_t rx_active;
    unsigned char pad1[HQQ_CACHELINE];
    _Atomic int64_t tx_active;
    unsigned char pad2[HQQ_CACHELINE];

    hqq_named *named;            /* NULL for caller-owned memory */
};

/* What an open call needs besides the geometry. */
typedef struct hqq_open_req {
    unsigned char *base;         /* page-aligned segment */
    hqq_role role;
    uint64_t creator_token;
    hqq_named *named;            /* ownership moves to the link on success */
    hqq_allocator alloc;
    int alloc_set;
    hqq_wait_policy policy;
    uint32_t flags;
    int64_t heartbeat_interval_ns;  /* normalized: 0 = disabled */
    int64_t peer_timeout_ns;        /* normalized: > 0 */
    int64_t attach_timeout_ns;      /* normalized: > 0 */
    int64_t init_stale_ns;
    hqq_ctx ctx;
} hqq_open_req;

/* Core of every open: claim, initialize, attach rings, start the heartbeat.
 * On failure nothing is retained (req->named stays with the caller). */
hqq_status hqq_link_open_core(const hqq_open_req *req, uint64_t count, uint64_t size,
                              hqq_link **out, hqq_open_info *info);

/* The SPEC 6.3 peer monitor. HQQ_OK, HQQ_ERR_PEER_CLOSED or HQQ_ERR_PEER_GONE. */
hqq_status hqq_link_check_peer(hqq_link *l);

/* Named segments (hqq_named.c). */
hqq_status hqq_named_open(const char *name, uint64_t count, uint64_t size, int create, unsigned perm,
                          const hqq_options *opts, hqq_link **out, hqq_open_info *info);
/* Unmaps, closes handles, removes the name when owned. Frees *n. */
hqq_status hqq_named_release(const hqq_allocator *a, hqq_named *n);
hqq_status hqq_named_unlink(hqq_named *n);

/* Normalized options, shared by the memory and named entry points. */
hqq_status hqq_options_normalize(const hqq_options *opts, hqq_open_req *req, hqq_open_info *info);

#endif /* HQQ_INTERNAL_H */
