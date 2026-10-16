/* Ring core (hqq_ring.h): format and attach validation, the slot protocol,
 * full/empty, FIFO, second lap, wraparound near 2^64, stale reservation copies
 * and a bounded multi-thread MPMC exactly-once check. */
#include <stdlib.h>
#include <string.h>

#include "hqq_internal.h"
#include "hqq_test.h"
#include "hqq_testutil.h"

#define RING_SLOTS 4u
#define RING_BYTES (512u + 128u * RING_SLOTS)

static unsigned char *new_ring(uint64_t slots) {
    size_t n = hqq_ring_bytes(slots);
    unsigned char *mem = (unsigned char *)hqq_test_pages(n);
    HQQ_REQUIRE(hqq_ring_format(mem, slots, NULL, NULL) == 0);
    return mem;
}

static hqq_slot *slot_of(unsigned char *mem, uint64_t i) {
    return (hqq_slot *)(void *)(mem + HQQ_RING_HEADER_SIZE + HQQ_RING_SLOT_STRIDE * i);
}

static void check_format_image(void) {
    unsigned char *mem = new_ring(RING_SLOTS);
    hqq_ring_header *h = (hqq_ring_header *)(void *)mem;
    uint64_t i;
    hqq_ring r;

    HQQ_CHECK_EQ_U64(atomic_load(&h->magic), HQQ_RING_MAGIC);
    HQQ_CHECK_EQ_U64(atomic_load(&h->flag), HQQ_RING_FLAG_INIT);
    HQQ_CHECK_EQ_U64(atomic_load(&h->size), RING_SLOTS);
    HQQ_CHECK_EQ_U64(atomic_load(&h->stride), HQQ_RING_SLOT_STRIDE);
    HQQ_CHECK_EQ_U64(atomic_load(&h->version), HQQ_RING_VERSION);
    HQQ_CHECK_EQ_U64(atomic_load(&h->r), 0);
    HQQ_CHECK_EQ_U64(atomic_load(&h->w), 0);
    HQQ_CHECK_EQ_U64(atomic_load(&h->empty_waiters), 0);
    HQQ_CHECK_EQ_U64(atomic_load(&h->full_waiters), 0);
    for (i = 0; i < RING_SLOTS; i++) {
        HQQ_CHECK_EQ_U64(atomic_load(&slot_of(mem, i)->seq), i);
    }
    HQQ_CHECK_EQ_U64(hqq_ring_bytes(RING_SLOTS), RING_BYTES);
    HQQ_CHECK_EQ_U64(hqq_ring_bytes(3), 0);
    HQQ_CHECK_EQ_U64(hqq_ring_bytes(1), 0);
    HQQ_CHECK_EQ_INT(hqq_ring_format(mem, 3, NULL, NULL), HQQ_RING_INCOMPATIBLE);
    HQQ_CHECK_EQ_INT(hqq_ring_attach(&r, mem), HQQ_RING_OK);
    HQQ_CHECK_EQ_U64(r.size, RING_SLOTS);
    HQQ_CHECK_EQ_U64(r.mask, RING_SLOTS - 1);
    hqq_test_pages_free(mem, RING_BYTES);
}

/* Attach rejects a foreign or mutated header at once, and waits for an uninitialized one. */
static void check_attach_rules(void) {
    unsigned char *mem = new_ring(RING_SLOTS);
    hqq_ring_header *h = (hqq_ring_header *)(void *)mem;
    hqq_ring r;

    HQQ_CHECK_EQ_INT(hqq_ring_attach(&r, mem), HQQ_RING_OK);

    atomic_store(&h->stride, 64);
    HQQ_CHECK_EQ_INT(hqq_ring_attach(&r, mem), HQQ_RING_INCOMPATIBLE);
    atomic_store(&h->stride, HQQ_RING_SLOT_STRIDE);

    atomic_store(&h->version, 1);
    HQQ_CHECK_EQ_INT(hqq_ring_attach(&r, mem), HQQ_RING_INCOMPATIBLE);
    atomic_store(&h->version, HQQ_RING_VERSION);

    atomic_store(&h->size, 3);
    HQQ_CHECK_EQ_INT(hqq_ring_attach(&r, mem), HQQ_RING_INCOMPATIBLE);
    atomic_store(&h->size, 1);
    HQQ_CHECK_EQ_INT(hqq_ring_attach(&r, mem), HQQ_RING_INCOMPATIBLE);
    atomic_store(&h->size, RING_SLOTS);

    atomic_store(&h->magic, 0);
    HQQ_CHECK_EQ_INT(hqq_ring_attach(&r, mem), HQQ_RING_NOT_READY);
    atomic_store(&h->magic, HQQ_RING_MAGIC);
    atomic_store(&h->flag, 0);
    HQQ_CHECK_EQ_INT(hqq_ring_attach(&r, mem), HQQ_RING_NOT_READY);
    atomic_store(&h->flag, HQQ_RING_FLAG_INIT);
    HQQ_CHECK_EQ_INT(hqq_ring_attach(&r, mem), HQQ_RING_OK);
    hqq_test_pages_free(mem, RING_BYTES);
}

static void check_protocol_single_thread(void) {
    unsigned char *mem = new_ring(RING_SLOTS);
    hqq_ring r;
    uint64_t pos = 0, got = 0, i;

    HQQ_REQUIRE(hqq_ring_attach(&r, mem) == HQQ_RING_OK);

    /* One claim and one publish: seq goes pos -> pos+1, release pos+1 -> pos+N. */
    HQQ_REQUIRE(hqq_ring_try_claim_producer(&r, &pos) == 1);
    HQQ_CHECK_EQ_U64(pos, 0);
    HQQ_CHECK_EQ_U64(atomic_load(&slot_of(mem, 0)->seq), 0);
    hqq_ring_publish(&r, pos);
    HQQ_CHECK_EQ_U64(atomic_load(&slot_of(mem, 0)->seq), 1);
    HQQ_REQUIRE(hqq_ring_try_claim_consumer(&r, &got) == 1);
    HQQ_CHECK_EQ_U64(got, 0);
    hqq_ring_release(&r, got);
    HQQ_CHECK_EQ_U64(atomic_load(&slot_of(mem, 0)->seq), RING_SLOTS);

    /* Empty: no claim; the awaited position is reported without side effects. */
    {
        uint64_t want = 99;
        HQQ_CHECK(hqq_ring_try_claim_consumer(&r, &want) == 0);
        HQQ_CHECK_EQ_U64(want, 1);
        HQQ_CHECK_EQ_U64(atomic_load(&r.h->r), 1);
    }

    /* Full: RING_SLOTS publishes fill the ring, the next claim fails. */
    for (i = 1; i <= RING_SLOTS; i++) {
        uint64_t p = 0;
        HQQ_REQUIRE(hqq_ring_try_claim_producer(&r, &p) == 1);
        HQQ_CHECK_EQ_U64(p, i);
        hqq_ring_publish(&r, p);
    }
    {
        uint64_t p = 0;
        HQQ_CHECK(hqq_ring_try_claim_producer(&r, &p) == 0);
        HQQ_CHECK_EQ_U64(p, RING_SLOTS + 1);
        HQQ_CHECK_EQ_U64(atomic_load(&r.h->w), RING_SLOTS + 1);
    }
    /* FIFO across the first lap. */
    for (i = 1; i <= RING_SLOTS; i++) {
        uint64_t p = 0;
        HQQ_REQUIRE(hqq_ring_try_claim_consumer(&r, &p) == 1);
        HQQ_CHECK_EQ_U64(p, i);
        hqq_ring_release(&r, p);
    }

    /* Several more laps; slot 0 was used by positions 0, N, 2N, ..., 6N. */
    for (i = 0; i < 5 * RING_SLOTS; i++) {
        uint64_t p = 0, c = 0;
        HQQ_REQUIRE(hqq_ring_try_claim_producer(&r, &p) == 1);
        hqq_ring_publish(&r, p);
        HQQ_REQUIRE(hqq_ring_try_claim_consumer(&r, &c) == 1);
        HQQ_CHECK_EQ_U64(c, p);
        hqq_ring_release(&r, c);
    }
    HQQ_CHECK_EQ_U64(atomic_load(&slot_of(mem, 0)->seq), 7 * RING_SLOTS);
    hqq_test_pages_free(mem, RING_BYTES);
}

/* Positions near 2^64: start at 2^64 - 2 and cross the wrap. */
static void check_wraparound(void) {
    unsigned char *mem = new_ring(RING_SLOTS);
    hqq_ring r;
    uint64_t start = UINT64_MAX - 1, i;
    unsigned s;

    HQQ_REQUIRE(hqq_ring_attach(&r, mem) == HQQ_RING_OK);
    /* Slot s must carry the sequence of the first position >= start with that slot index. */
    for (s = 0; s < RING_SLOTS; s++) {
        uint64_t p = start + (((uint64_t)s - start) & r.mask);
        atomic_store(&slot_of(mem, s)->seq, p);
    }
    atomic_store(&r.h->r, start);
    atomic_store(&r.h->w, start);

    for (i = 0; i < 3 * RING_SLOTS; i++) {
        uint64_t p = 0, c = 0;
        uint64_t expect = start + i;
        HQQ_REQUIRE(hqq_ring_try_claim_producer(&r, &p) == 1);
        HQQ_CHECK_EQ_U64(p, expect);
        hqq_ring_publish(&r, p);
        HQQ_REQUIRE(hqq_ring_try_claim_consumer(&r, &c) == 1);
        HQQ_CHECK_EQ_U64(c, expect);
        hqq_ring_release(&r, c);
    }
    HQQ_CHECK_EQ_U64(atomic_load(&r.h->w), start + 3 * RING_SLOTS);
    HQQ_CHECK_EQ_U64(atomic_load(&r.h->r), start + 3 * RING_SLOTS);
    hqq_test_pages_free(mem, RING_BYTES);
}

/* A copied reservation completes at most once: the second completion fails and
 * leaves the ring alone. */
static void check_guarded_completion(void) {
    unsigned char *mem = new_ring(RING_SLOTS);
    hqq_ring r;
    uint64_t p = 0, c = 0;

    HQQ_REQUIRE(hqq_ring_attach(&r, mem) == HQQ_RING_OK);
    HQQ_REQUIRE(hqq_ring_try_claim_producer(&r, &p) == 1);
    HQQ_CHECK(hqq_ring_producer_owned(&r, p));
    HQQ_CHECK(hqq_ring_publish_guarded(&r, p) == 1);
    HQQ_CHECK(hqq_ring_publish_guarded(&r, p) == 0); /* stale copy: no change, no wake */
    HQQ_CHECK_EQ_U64(atomic_load(&slot_of(mem, 0)->seq), 1);

    HQQ_REQUIRE(hqq_ring_try_claim_consumer(&r, &c) == 1);
    HQQ_CHECK(hqq_ring_consumer_owned(&r, c));
    HQQ_CHECK(hqq_ring_release_guarded(&r, c) == 1);
    HQQ_CHECK(hqq_ring_release_guarded(&r, c) == 0); /* stale copy */
    HQQ_CHECK_EQ_U64(atomic_load(&slot_of(mem, 0)->seq), RING_SLOTS);
    hqq_test_pages_free(mem, RING_BYTES);
}

/* ---- Multi-thread MPMC exactly-once ------------------------------------------- */

#define MPMC_SLOTS 8u
#define MPMC_PRODUCERS 3u
#define MPMC_CONSUMERS 3u
#define MPMC_PER_PRODUCER 4000u
#define MPMC_TOTAL (MPMC_PRODUCERS * MPMC_PER_PRODUCER)

typedef struct mpmc_shared {
    unsigned char *mem;
    hqq_ring ring;
    _Atomic uint64_t consumed;
    _Atomic unsigned char *seen; /* one flag per message value */
    _Atomic int bad;
} mpmc_shared;

typedef struct mpmc_arg {
    mpmc_shared *s;
    unsigned id;
} mpmc_arg;

static void mpmc_producer(void *p) {
    mpmc_arg *a = (mpmc_arg *)p;
    unsigned k;
    for (k = 0; k < MPMC_PER_PRODUCER; k++) {
        uint64_t pos = 0, value = (uint64_t)a->id * MPMC_PER_PRODUCER + k;
        while (hqq_ring_try_claim_producer(&a->s->ring, &pos) == 0) {
            hqq_plat_yield();
        }
        memcpy(hqq_ring_slot(&a->s->ring, pos)->packet, &value, sizeof value);
        hqq_ring_publish(&a->s->ring, pos);
    }
}

static void mpmc_consumer(void *p) {
    mpmc_shared *s = ((mpmc_arg *)p)->s;
    for (;;) {
        uint64_t pos = 0, word = 0;
        if (atomic_load(&s->consumed) >= MPMC_TOTAL) {
            return;
        }
        if (hqq_ring_try_claim_consumer(&s->ring, &pos) == 0) {
            hqq_plat_yield();
            continue;
        }
        memcpy(&word, hqq_ring_slot(&s->ring, pos)->packet, sizeof word);
        hqq_ring_release(&s->ring, pos);
        if (word >= MPMC_TOTAL) {
            atomic_store(&s->bad, 1);
        } else if (atomic_exchange(&s->seen[word], 1) != 0) {
            atomic_store(&s->bad, 1); /* delivered twice */
        }
        atomic_fetch_add(&s->consumed, 1);
    }
}

static void check_mpmc_threads(void) {
    mpmc_shared s;
    mpmc_arg args[MPMC_PRODUCERS + MPMC_CONSUMERS];
    hqq_test_thread *th[MPMC_PRODUCERS + MPMC_CONSUMERS];
    unsigned i, missing = 0;

    memset(&s, 0, sizeof s);
    s.mem = new_ring(MPMC_SLOTS);
    HQQ_REQUIRE(hqq_ring_attach(&s.ring, s.mem) == HQQ_RING_OK);
    s.seen = (_Atomic unsigned char *)calloc(MPMC_TOTAL, sizeof(unsigned char));
    HQQ_REQUIRE(s.seen != NULL);
    for (i = 0; i < MPMC_PRODUCERS + MPMC_CONSUMERS; i++) {
        args[i].s = &s;
        args[i].id = i;
    }
    for (i = 0; i < MPMC_CONSUMERS; i++) {
        th[MPMC_PRODUCERS + i] = hqq_test_thread_start(mpmc_consumer, &args[MPMC_PRODUCERS + i]);
    }
    for (i = 0; i < MPMC_PRODUCERS; i++) {
        th[i] = hqq_test_thread_start(mpmc_producer, &args[i]);
    }
    for (i = 0; i < MPMC_PRODUCERS + MPMC_CONSUMERS; i++) {
        hqq_test_thread_join(th[i]);
    }
    for (i = 0; i < MPMC_TOTAL; i++) {
        missing += atomic_load(&s.seen[i]) == 0;
    }
    HQQ_CHECK_EQ_INT(atomic_load(&s.bad), 0);
    HQQ_CHECK_EQ_INT(missing, 0);
    HQQ_CHECK_EQ_U64(atomic_load(&s.consumed), MPMC_TOTAL);
    HQQ_CHECK_EQ_U64(atomic_load(&s.ring.h->w), MPMC_TOTAL);
    HQQ_CHECK_EQ_U64(atomic_load(&s.ring.h->r), MPMC_TOTAL);
    free((void *)s.seen);
    hqq_test_pages_free(s.mem, hqq_ring_bytes(MPMC_SLOTS));
}

int main(void) {
    check_format_image();
    check_attach_rules();
    check_protocol_single_thread();
    check_wraparound();
    check_guarded_completion();
    check_mpmc_threads();
    return HQQ_TEST_RESULT("test_ring");
}
