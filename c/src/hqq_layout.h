/*
 * hqq_layout.h - segment layout, superblock image, packets (SPEC 2, 3, 9).
 * OS independent: C11 plus <stdatomic.h>.
 */
#ifndef HQQ_LAYOUT_H
#define HQQ_LAYOUT_H

#include <stdatomic.h>
#include <stddef.h>
#include <stdint.h>

#include "hqq_ring.h"

/* ---- Constants (constants.json) ---------------------------------------- */

#define HQQ_SB_MAGIC UINT64_C(0x4851515f53544c33) /* "HQQ_STL3" */
#define HQQ_SB_MAJOR UINT64_C(3)
#define HQQ_SB_MINOR UINT64_C(0)
#define HQQ_SB_KNOWN_FEATURES UINT64_C(0)
#define HQQ_SB_SIZE 384u
#define HQQ_MIN_PAGE_SIZE 4096u

#define HQQ_STATE_EMPTY UINT64_C(0)
#define HQQ_STATE_INITIALIZING UINT64_C(1)
#define HQQ_STATE_READY UINT64_C(2)

#define HQQ_ENDPOINT_PRIMARY 0
#define HQQ_ENDPOINT_SECONDARY 1
#define HQQ_DETACHED_TOKEN UINT64_C(1)

#define HQQ_ZERO_CHUNK_BYTES ((size_t)8 << 20) /* progress bound of the protocol */
#define HQQ_ZERO_SLICE_BYTES ((size_t)256 << 10) /* ownership check granularity */

#define HQQ_NS_PER_MS UINT64_C(1000000)
#define HQQ_NS_PER_SEC UINT64_C(1000000000)

/* ---- Superblock image ---------------------------------------------------- */

typedef struct hqq_endpoint_record {
    _Atomic uint64_t token;     /* +0   0 unused, 1 detached, else owner token */
    _Atomic uint64_t pid;       /* +8   diagnostic */
    _Atomic uint64_t heartbeat; /* +16  wall clock unix ns */
    _Atomic uint64_t interval;  /* +24  heartbeat interval ns, 0 = disabled */
    _Atomic uint64_t reserved[12];
} hqq_endpoint_record;

typedef struct hqq_superblock {
    _Atomic uint64_t magic;            /* @0 */
    _Atomic uint64_t state;            /* @8 */
    _Atomic uint64_t major;            /* @16 */
    _Atomic uint64_t minor;            /* @24 */
    _Atomic uint64_t page_size;        /* @32 */
    _Atomic uint64_t buffer_count;     /* @40 */
    _Atomic uint64_t buffer_size;      /* @48 */
    _Atomic uint64_t layout_size;      /* @56 */
    _Atomic uint64_t epoch;            /* @64 */
    _Atomic uint64_t init_token;       /* @72 */
    _Atomic uint64_t init_heartbeat;   /* @80 */
    _Atomic uint64_t required_features;/* @88 */
    _Atomic uint64_t creator_token;    /* @96 */
    _Atomic uint64_t reserved[3];      /* @104 */
    hqq_endpoint_record endpoints[2];  /* @128, @256 */
} hqq_superblock;

_Static_assert(sizeof(hqq_endpoint_record) == 128, "endpoint record is 128 bytes");
_Static_assert(offsetof(hqq_endpoint_record, heartbeat) == 16, "endpoint heartbeat @16");
_Static_assert(offsetof(hqq_endpoint_record, interval) == 24, "endpoint interval @24");
_Static_assert(sizeof(hqq_superblock) == HQQ_SB_SIZE, "superblock is 384 bytes");
_Static_assert(offsetof(hqq_superblock, state) == 8, "state @8");
_Static_assert(offsetof(hqq_superblock, page_size) == 32, "pageSize @32");
_Static_assert(offsetof(hqq_superblock, layout_size) == 56, "layoutSize @56");
_Static_assert(offsetof(hqq_superblock, epoch) == 64, "epoch @64");
_Static_assert(offsetof(hqq_superblock, init_token) == 72, "initToken @72");
_Static_assert(offsetof(hqq_superblock, init_heartbeat) == 80, "initHeartbeat @80");
_Static_assert(offsetof(hqq_superblock, required_features) == 88, "requiredFeatures @88");
_Static_assert(offsetof(hqq_superblock, creator_token) == 96, "creatorToken @96");
_Static_assert(offsetof(hqq_superblock, endpoints) == 128, "endpoints @128");

/* ---- Geometry and region offsets (SPEC 2) ---------------------------------- */

typedef struct hqq_layout {
    uint64_t buffer_count;
    uint64_t buffer_size;
    uint64_t page_size;
    uint64_t ring0, buf0, ring1, buf1; /* byte offsets */
    uint64_t total;
} hqq_layout;

/* Valid geometry per SPEC 2.2 (independent of the page size). */
static inline int hqq_geometry_valid(uint64_t count, uint64_t size) {
    return count >= HQQ_RING_MIN_SLOTS && count <= (UINT64_C(1) << 24) && (count & (count - 1)) == 0 &&
           size >= 8 && size <= (UINT64_C(1) << 30) && (size % 8) == 0;
}

/* a + b rounded up to a page boundary; 0 on overflow of uint64_t (sets *ok = 0). */
static inline uint64_t hqq_aligned_sum(uint64_t a, uint64_t b, uint64_t page, int *ok) {
    uint64_t s = a + b;
    if (s < a || s + (page - 1) < s) {
        *ok = 0;
        return 0;
    }
    return (s + page - 1) / page * page;
}

/*
 * Computes the region offsets for a page size. Returns 1 on success, 0 for an
 * invalid geometry or a size that does not fit (the Go "size 0"). `max_total`
 * bounds the total (SIZE_MAX of the platform, so 32-bit hosts are refused).
 */
static inline int hqq_layout_compute(hqq_layout *l, uint64_t page, uint64_t count, uint64_t size,
                                     uint64_t max_total) {
    uint64_t ring_bytes, buffers;
    int ok = 1;

    l->buffer_count = count;
    l->buffer_size = size;
    l->page_size = page;
    l->ring0 = l->buf0 = l->ring1 = l->buf1 = l->total = 0;
    if (page < HQQ_MIN_PAGE_SIZE || (page & (page - 1)) != 0 || !hqq_geometry_valid(count, size)) {
        return 0;
    }
    ring_bytes = (uint64_t)HQQ_RING_HEADER_SIZE + (uint64_t)HQQ_RING_SLOT_STRIDE * count;
    buffers = count * size; /* <= 2^24 * 2^30 = 2^54: no overflow */
    l->ring0 = (HQQ_SB_SIZE + page - 1) / page * page;
    l->buf0 = hqq_aligned_sum(l->ring0, ring_bytes, page, &ok);
    l->ring1 = hqq_aligned_sum(l->buf0, buffers, page, &ok);
    l->buf1 = hqq_aligned_sum(l->ring1, ring_bytes, page, &ok);
    l->total = hqq_aligned_sum(l->buf1, buffers, page, &ok);
    if (!ok || l->total > max_total) {
        return 0;
    }
    return 1;
}

/* ---- Packets (SPEC 9) ------------------------------------------------------------ */

#define HQQ_OP_ERROR 0x00u
#define HQQ_OP_CONN_CREATE 0x01u
#define HQQ_OP_CONN_ACCEPT 0x02u
#define HQQ_OP_CONN_CLOSE 0x03u
#define HQQ_OP_CONN_COPY 0x04u
#define HQQ_OP_PROTO_NEGOTIATE 0x05u
#define HQQ_OP_PROTO_ACK 0x06u
#define HQQ_OP_STD_TOMBSTONE 0xFEu
#define HQQ_OP_STD_COPY 0xFFu

/* A packet is eight little-endian u64 words; the host is little-endian. */
typedef struct hqq_packet {
    uint64_t w[8]; /* w[0] = opcode in the low byte; w[1..7] = operands 0..6 */
} hqq_packet;

_Static_assert(sizeof(hqq_packet) == HQQ_RING_PACKET_SIZE, "packet is 64 bytes");

static inline unsigned hqq_packet_op(const hqq_packet *p) { return (unsigned)(p->w[0] & 0xFFu); }
static inline uint64_t hqq_packet_operand(const hqq_packet *p, unsigned i) { return p->w[1u + i]; }

static inline void hqq_packet_make(hqq_packet *p, unsigned op, uint64_t a0, uint64_t a1, uint64_t a2) {
    unsigned i;
    p->w[0] = (uint64_t)op;
    p->w[1] = a0;
    p->w[2] = a1;
    p->w[3] = a2;
    for (i = 4; i < 8; i++) {
        p->w[i] = 0;
    }
}

#endif /* HQQ_LAYOUT_H */
