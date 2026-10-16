/* Superblock engine (hqq_superblock.c) on crafted images, mirroring the cases of
 * the Go superblock_test.go and the claim decision table of SPEC 4 / 5.1:
 * roles, foreign pages, unknown states, layout incompatibility, joins, live and
 * dead endpoints, reuse, takeover of a stalled initializer, progress watching,
 * geometry mismatch, publish ownership and the opener never writing. */
#include <stdlib.h>
#include <string.h>

#include "hqq_internal.h"
#include "hqq_test.h"
#include "hqq_testutil.h"

#define SB_COUNT 4u
#define SB_SIZE 64u

typedef struct sb_fixture {
    unsigned char *base;
    size_t total;
    hqq_layout lay;
    uint64_t page;
    unsigned char *before; /* snapshot buffer, same size as the segment */
} sb_fixture;

static void sb_setup(sb_fixture *f) {
    f->page = hqq_plat_page_size();
    HQQ_REQUIRE(hqq_layout_compute(&f->lay, f->page, SB_COUNT, SB_SIZE, UINT64_MAX));
    f->total = (size_t)f->lay.total;
    f->base = (unsigned char *)hqq_test_pages(f->total);
    f->before = (unsigned char *)malloc(f->total);
    HQQ_REQUIRE(f->before != NULL);
}

static void sb_teardown(sb_fixture *f) {
    hqq_test_pages_free(f->base, f->total);
    free(f->before);
}

static hqq_superblock *sb_of(sb_fixture *f) { return (hqq_superblock *)(void *)f->base; }

static void make_params(const sb_fixture *f, hqq_claim_params *p, hqq_role role, uint64_t token,
                        uint64_t creator_token) {
    memset(p, 0, sizeof *p);
    p->layout = f->lay;
    p->token = token;
    p->creator_token = creator_token;
    p->role = role;
    p->attach_timeout_ns = (int64_t)(200 * HQQ_NS_PER_MS);
    p->peer_timeout_ns = (int64_t)(5 * HQQ_NS_PER_SEC);
    p->init_stale_ns = 0;
    p->heartbeat_interval_ns = (int64_t)(250 * HQQ_NS_PER_MS);
}

static void snapshot(sb_fixture *f) { memcpy(f->before, f->base, f->total); }

/* True when the whole segment is byte-identical to the last snapshot. */
static int unchanged(const sb_fixture *f) { return memcmp(f->before, f->base, f->total) == 0; }

/* Puts a Ready superblock with the given endpoint records in place (no ring data needed). */
static void craft_ready(sb_fixture *f, uint64_t prim_token, uint64_t prim_hb_age_ns, uint64_t sec_token) {
    hqq_superblock *sb = sb_of(f);
    uint64_t now = hqq_plat_wall_ns();
    atomic_store(&sb->magic, HQQ_SB_MAGIC);
    atomic_store(&sb->state, HQQ_STATE_READY);
    atomic_store(&sb->major, HQQ_SB_MAJOR);
    atomic_store(&sb->minor, HQQ_SB_MINOR);
    atomic_store(&sb->page_size, f->page);
    atomic_store(&sb->buffer_count, SB_COUNT);
    atomic_store(&sb->buffer_size, SB_SIZE);
    atomic_store(&sb->layout_size, f->lay.total);
    atomic_store(&sb->epoch, 1);
    atomic_store(&sb->init_token, 0);
    atomic_store(&sb->creator_token, 0x5151);
    atomic_store(&sb->endpoints[0].token, prim_token);
    atomic_store(&sb->endpoints[0].heartbeat, now - prim_hb_age_ns);
    atomic_store(&sb->endpoints[0].interval, 250u * 1000u * 1000u);
    atomic_store(&sb->endpoints[1].token, sec_token);
}

static void check_empty_creator_publish_join(void) {
    sb_fixture f;
    hqq_superblock *sb;
    hqq_claim_params pc, po, pa;
    hqq_mode mode;
    int must_init = 0;
    uint64_t epoch = 0, prim_tok = 0, sec_tok = 0, third = 0;
    hqq_geometry got;
    hqq_ring r;

    sb_setup(&f);
    sb = sb_of(&f);
    HQQ_REQUIRE(hqq_new_token(&prim_tok) == 0);
    HQQ_REQUIRE(hqq_new_token(&sec_tok) == 0);
    make_params(&f, &pc, HQQ_ROLE_CREATOR, prim_tok, 0xC0FFEEu);

    HQQ_CHECK_STATUS(hqq_sb_claim(sb, &pc, &mode, &must_init, &got), HQQ_OK);
    HQQ_CHECK_EQ_INT(mode, HQQ_MODE_PRIMARY);
    HQQ_CHECK_EQ_INT(must_init, 1);
    HQQ_CHECK_EQ_U64(atomic_load(&sb->state), HQQ_STATE_INITIALIZING);
    HQQ_CHECK_EQ_U64(atomic_load(&sb->init_token), prim_tok);
    HQQ_CHECK_EQ_U64(atomic_load(&sb->creator_token), 0xC0FFEEu);
    HQQ_CHECK_EQ_U64(atomic_load(&sb->major), HQQ_SB_MAJOR);

    HQQ_CHECK_STATUS(hqq_sb_initialize(f.base, &pc, &epoch), HQQ_OK);
    HQQ_CHECK_EQ_U64(epoch, 1);
    HQQ_CHECK_EQ_U64(atomic_load(&sb->state), HQQ_STATE_READY);
    HQQ_CHECK_EQ_U64(atomic_load(&sb->magic), HQQ_SB_MAGIC);
    HQQ_CHECK_EQ_U64(atomic_load(&sb->init_token), 0);
    HQQ_CHECK_EQ_U64(atomic_load(&sb->epoch), 1);
    HQQ_CHECK_EQ_U64(atomic_load(&sb->page_size), f.page);
    HQQ_CHECK_EQ_U64(atomic_load(&sb->buffer_count), SB_COUNT);
    HQQ_CHECK_EQ_U64(atomic_load(&sb->buffer_size), SB_SIZE);
    HQQ_CHECK_EQ_U64(atomic_load(&sb->layout_size), f.lay.total);
    HQQ_CHECK_EQ_U64(atomic_load(&sb->endpoints[0].token), prim_tok);
    HQQ_CHECK(atomic_load(&sb->endpoints[0].heartbeat) != 0);
    HQQ_CHECK_EQ_U64(atomic_load(&sb->endpoints[1].token), 0);
    HQQ_CHECK_EQ_INT(hqq_ring_attach(&r, f.base + f.lay.ring0), HQQ_RING_OK);
    HQQ_CHECK_EQ_INT(hqq_ring_attach(&r, f.base + f.lay.ring1), HQQ_RING_OK);

    /* A second creator never joins a live segment. */
    make_params(&f, &pc, HQQ_ROLE_CREATOR, sec_tok, 0x1u);
    HQQ_CHECK_STATUS(hqq_sb_claim(sb, &pc, &mode, &must_init, NULL), HQQ_ERR_LINK_BUSY);

    /* Geometry mismatch: an opener with another buffer count is refused and told both geometries. */
    {
        hqq_claim_params bad;
        make_params(&f, &bad, HQQ_ROLE_OPENER, sec_tok, 0);
        bad.layout.buffer_count = 8;
        memset(&got, 0, sizeof got);
        HQQ_CHECK_STATUS(hqq_sb_claim(sb, &bad, &mode, &must_init, &got), HQQ_ERR_GEOMETRY_MISMATCH);
        HQQ_CHECK_EQ_U64(got.buffer_count, SB_COUNT);
        HQQ_CHECK_EQ_U64(got.buffer_size, SB_SIZE);
        HQQ_CHECK_EQ_U64(got.layout_size, f.lay.total);
        HQQ_CHECK_EQ_U64(got.page_size, f.page);
        HQQ_CHECK_EQ_U64(atomic_load(&sb->endpoints[1].token), 0);
    }

    /* Opener joins as Secondary. */
    make_params(&f, &po, HQQ_ROLE_OPENER, sec_tok, 0);
    HQQ_CHECK_STATUS(hqq_sb_claim(sb, &po, &mode, &must_init, NULL), HQQ_OK);
    HQQ_CHECK_EQ_INT(mode, HQQ_MODE_SECONDARY);
    HQQ_CHECK_EQ_INT(must_init, 0);
    HQQ_CHECK_EQ_U64(atomic_load(&sb->endpoints[1].token), sec_tok);
    HQQ_CHECK(atomic_load(&sb->endpoints[1].heartbeat) != 0);
    HQQ_CHECK_EQ_U64(atomic_load(&sb->endpoints[1].interval), 250u * 1000u * 1000u);

    /* A third endpoint is refused: both alive. */
    HQQ_REQUIRE(hqq_new_token(&third) == 0);
    make_params(&f, &pa, HQQ_ROLE_OPENER, third, 0);
    HQQ_CHECK_STATUS(hqq_sb_claim(sb, &pa, &mode, &must_init, NULL), HQQ_ERR_LINK_BUSY);
    make_params(&f, &pa, HQQ_ROLE_ANY, third, 0);
    HQQ_CHECK_STATUS(hqq_sb_claim(sb, &pa, &mode, &must_init, NULL), HQQ_ERR_LINK_BUSY);
    sb_teardown(&f);
}

static void check_foreign_and_unknown(void) {
    sb_fixture f;
    hqq_claim_params p;
    hqq_mode mode;
    int must_init;
    uint64_t tok = 0x7777;

    sb_setup(&f);
    /* Non-zero word at the very end of the first page while Empty: foreign data, nothing written. */
    atomic_store((_Atomic uint64_t *)(void *)(f.base + f.page - 8), 0xABu);
    snapshot(&f);
    make_params(&f, &p, HQQ_ROLE_CREATOR, tok, 1);
    HQQ_CHECK_STATUS(hqq_sb_claim(sb_of(&f), &p, &mode, &must_init, NULL), HQQ_ERR_INCOMPATIBLE_LAYOUT);
    HQQ_CHECK(unchanged(&f));
    make_params(&f, &p, HQQ_ROLE_ANY, tok, 0);
    HQQ_CHECK_STATUS(hqq_sb_claim(sb_of(&f), &p, &mode, &must_init, NULL), HQQ_ERR_INCOMPATIBLE_LAYOUT);
    HQQ_CHECK(unchanged(&f));
    make_params(&f, &p, HQQ_ROLE_OPENER, tok, 0);
    HQQ_CHECK_STATUS(hqq_sb_claim(sb_of(&f), &p, &mode, &must_init, NULL), HQQ_ERR_INCOMPATIBLE_LAYOUT);
    HQQ_CHECK(unchanged(&f));
    atomic_store((_Atomic uint64_t *)(void *)(f.base + f.page - 8), 0);

    /* Unknown state value: incompatible for every role, nothing written. */
    atomic_store(&sb_of(&f)->state, 3);
    snapshot(&f);
    make_params(&f, &p, HQQ_ROLE_CREATOR, tok, 1);
    HQQ_CHECK_STATUS(hqq_sb_claim(sb_of(&f), &p, &mode, &must_init, NULL), HQQ_ERR_INCOMPATIBLE_LAYOUT);
    make_params(&f, &p, HQQ_ROLE_OPENER, tok, 0);
    HQQ_CHECK_STATUS(hqq_sb_claim(sb_of(&f), &p, &mode, &must_init, NULL), HQQ_ERR_INCOMPATIBLE_LAYOUT);
    HQQ_CHECK(unchanged(&f));
    sb_teardown(&f);
}

/* Ready segments whose layout does not match: HQQ_STL2, an unknown required feature, major 2. */
static void check_incompatible_ready(void) {
    sb_fixture f;
    hqq_claim_params p;
    hqq_mode mode;
    int must_init;

    sb_setup(&f);
    craft_ready(&f, 0x11, 0, 0);
    atomic_store(&sb_of(&f)->magic, UINT64_C(0x4851515f53544c32)); /* "HQQ_STL2" */
    snapshot(&f);
    make_params(&f, &p, HQQ_ROLE_CREATOR, 0x99, 1);
    HQQ_CHECK_STATUS(hqq_sb_claim(sb_of(&f), &p, &mode, &must_init, NULL), HQQ_ERR_INCOMPATIBLE_LAYOUT);
    make_params(&f, &p, HQQ_ROLE_OPENER, 0x99, 0);
    HQQ_CHECK_STATUS(hqq_sb_claim(sb_of(&f), &p, &mode, &must_init, NULL), HQQ_ERR_INCOMPATIBLE_LAYOUT);
    HQQ_CHECK(unchanged(&f));

    craft_ready(&f, 0x11, 0, 0);
    atomic_store(&sb_of(&f)->required_features, 1);
    make_params(&f, &p, HQQ_ROLE_OPENER, 0x99, 0);
    HQQ_CHECK_STATUS(hqq_sb_claim(sb_of(&f), &p, &mode, &must_init, NULL), HQQ_ERR_INCOMPATIBLE_LAYOUT);

    craft_ready(&f, 0x11, 0, 0);
    atomic_store(&sb_of(&f)->major, 2);
    make_params(&f, &p, HQQ_ROLE_OPENER, 0x99, 0);
    HQQ_CHECK_STATUS(hqq_sb_claim(sb_of(&f), &p, &mode, &must_init, NULL), HQQ_ERR_INCOMPATIBLE_LAYOUT);
    sb_teardown(&f);
}

/* Opener on an empty zero page: transient after attach_timeout, untouched. */
static void check_opener_empty(void) {
    sb_fixture f;
    hqq_claim_params p;
    hqq_mode mode;
    int must_init;

    sb_setup(&f);
    snapshot(&f);
    make_params(&f, &p, HQQ_ROLE_OPENER, 0x42, 0);
    p.attach_timeout_ns = (int64_t)(30 * HQQ_NS_PER_MS);
    HQQ_CHECK_STATUS(hqq_sb_claim(sb_of(&f), &p, &mode, &must_init, NULL), HQQ_ERR_SEGMENT_NOT_READY);
    HQQ_CHECK(unchanged(&f));
    HQQ_CHECK(hqq_error_is(HQQ_ERR_SEGMENT_NOT_READY, HQQ_ERR_FAILED_INIT));
    sb_teardown(&f);
}

/* Ready with both endpoints dead: the opener fails at once, the creator reuses the segment. */
static void check_reuse_and_dead(void) {
    sb_fixture f;
    hqq_claim_params p;
    hqq_mode mode;
    int must_init = 0;
    uint64_t epoch = 0;
    const uint64_t dead_tokens[2] = {0, HQQ_DETACHED_TOKEN};
    unsigned i;

    sb_setup(&f);
    for (i = 0; i < 2; i++) {
        craft_ready(&f, dead_tokens[i], 0, dead_tokens[i]);
        snapshot(&f);
        make_params(&f, &p, HQQ_ROLE_OPENER, 0x33, 0);
        HQQ_CHECK_STATUS(hqq_sb_claim(sb_of(&f), &p, &mode, &must_init, NULL), HQQ_ERR_NO_LIVE_ENDPOINT);
        HQQ_CHECK(unchanged(&f));

        make_params(&f, &p, HQQ_ROLE_CREATOR, 0x33, 0xABCDu + i);
        HQQ_CHECK_STATUS(hqq_sb_claim(sb_of(&f), &p, &mode, &must_init, NULL), HQQ_OK);
        HQQ_CHECK_EQ_INT(must_init, 1);
        HQQ_CHECK_EQ_U64(atomic_load(&sb_of(&f)->state), HQQ_STATE_INITIALIZING);
        HQQ_CHECK_STATUS(hqq_sb_initialize(f.base, &p, &epoch), HQQ_OK);
        HQQ_CHECK_EQ_U64(epoch, 2); /* every initialization increments the epoch */
        HQQ_CHECK_EQ_U64(atomic_load(&sb_of(&f)->creator_token), 0xABCDu + i);
        HQQ_CHECK_EQ_U64(atomic_load(&sb_of(&f)->state), HQQ_STATE_READY);
        HQQ_CHECK_EQ_U64(atomic_load(&sb_of(&f)->endpoints[0].token), 0x33);
        HQQ_CHECK_EQ_U64(atomic_load(&sb_of(&f)->endpoints[1].token), 0);
        sb_teardown(&f);
        sb_setup(&f);
    }
    sb_teardown(&f);
}

/* Ready with a live primary (fresh heartbeat), a stale one, and other endpoint records. */
static void check_liveness_at_open(void) {
    sb_fixture f;
    hqq_claim_params p;
    hqq_mode mode;
    int must_init = 0;
    uint64_t t0, dt;

    /* Stale heartbeat older than the timeout: not confirmed by a progress watch, so dead. */
    sb_setup(&f);
    craft_ready(&f, 0x44, 10 * HQQ_NS_PER_SEC, 0);
    make_params(&f, &p, HQQ_ROLE_CREATOR, 0x55, 0x1);
    t0 = hqq_plat_mono_ns();
    HQQ_CHECK_STATUS(hqq_sb_claim(sb_of(&f), &p, &mode, &must_init, NULL), HQQ_OK);
    dt = hqq_plat_mono_ns() - t0;
    HQQ_CHECK_EQ_INT(must_init, 1);
    HQQ_CHECK(dt < 3 * HQQ_NS_PER_SEC);
    sb_teardown(&f);

    /* Fresh heartbeat: alive, so a creator is refused. */
    sb_setup(&f);
    craft_ready(&f, 0x44, 0, 0);
    make_params(&f, &p, HQQ_ROLE_CREATOR, 0x55, 0x1);
    HQQ_CHECK_STATUS(hqq_sb_claim(sb_of(&f), &p, &mode, &must_init, NULL), HQQ_ERR_LINK_BUSY);
    sb_teardown(&f);

    /* Primary detached but the secondary record taken: the joiner is refused (dead peer may own slots). */
    sb_setup(&f);
    craft_ready(&f, HQQ_DETACHED_TOKEN, 0, 0x66);
    make_params(&f, &p, HQQ_ROLE_OPENER, 0x77, 0);
    HQQ_CHECK_STATUS(hqq_sb_claim(sb_of(&f), &p, &mode, &must_init, NULL), HQQ_ERR_LINK_BUSY);
    sb_teardown(&f);

    /* Heartbeats disabled on the primary: alive by definition, so an opener joins. */
    sb_setup(&f);
    craft_ready(&f, 0x44, 0, 0);
    atomic_store(&sb_of(&f)->endpoints[0].interval, 0);
    make_params(&f, &p, HQQ_ROLE_OPENER, 0x77, 0);
    HQQ_CHECK_STATUS(hqq_sb_claim(sb_of(&f), &p, &mode, &must_init, NULL), HQQ_OK);
    HQQ_CHECK_EQ_INT(mode, HQQ_MODE_SECONDARY);
    sb_teardown(&f);
}

/* Background writer that keeps the initializer's progress word moving. */
typedef struct progress_arg {
    hqq_superblock *sb;
    _Atomic int stop;
} progress_arg;

static void progress_main(void *p) {
    progress_arg *a = (progress_arg *)p;
    uint64_t n = 1000;
    while (!atomic_load(&a->stop)) {
        atomic_store_explicit(&a->sb->init_heartbeat, ++n, memory_order_relaxed);
        hqq_test_sleep_ms(5);
    }
}

/* A stalled initializer: an opener waits and gives up transiently; a creator takes over
 * after the stale window; an initializer that keeps showing progress is not taken over. */
static void check_takeover(void) {
    sb_fixture f;
    hqq_claim_params p;
    hqq_mode mode;
    int must_init = 0;
    uint64_t epoch = 0;
    const uint64_t dead = 0xDEAD;

    sb_setup(&f);
    atomic_store(&sb_of(&f)->state, HQQ_STATE_INITIALIZING);
    atomic_store(&sb_of(&f)->init_token, dead);
    atomic_store(&sb_of(&f)->init_heartbeat, 12345);
    atomic_store(&sb_of(&f)->major, HQQ_SB_MAJOR);
    atomic_store(&sb_of(&f)->magic, HQQ_SB_MAGIC);

    /* An opener never takes over: transient errInitContended after attach_timeout, no write. */
    snapshot(&f);
    make_params(&f, &p, HQQ_ROLE_OPENER, 0x88, 0);
    p.attach_timeout_ns = (int64_t)(60 * HQQ_NS_PER_MS);
    HQQ_CHECK_STATUS(hqq_sb_claim(sb_of(&f), &p, &mode, &must_init, NULL), HQQ_ERR_INIT_CONTENDED);
    HQQ_CHECK(unchanged(&f));

    /* A creator takes over once the watch saw no progress for the stale window. */
    make_params(&f, &p, HQQ_ROLE_CREATOR, 0x88, 0x99);
    p.attach_timeout_ns = (int64_t)(2 * HQQ_NS_PER_SEC);
    p.init_stale_ns = (int64_t)(50 * HQQ_NS_PER_MS);
    HQQ_CHECK_STATUS(hqq_sb_claim(sb_of(&f), &p, &mode, &must_init, NULL), HQQ_OK);
    HQQ_CHECK_EQ_INT(must_init, 1);
    HQQ_CHECK_EQ_U64(atomic_load(&sb_of(&f)->init_token), 0x88);
    HQQ_CHECK_EQ_U64(atomic_load(&sb_of(&f)->creator_token), 0x99);
    HQQ_CHECK_STATUS(hqq_sb_initialize(f.base, &p, &epoch), HQQ_OK);
    HQQ_CHECK_EQ_U64(atomic_load(&sb_of(&f)->state), HQQ_STATE_READY);
    sb_teardown(&f);

    /* Progress keeps the initializer alive: no takeover, the creator gives up at its deadline. */
    sb_setup(&f);
    atomic_store(&sb_of(&f)->state, HQQ_STATE_INITIALIZING);
    atomic_store(&sb_of(&f)->init_token, dead);
    atomic_store(&sb_of(&f)->magic, HQQ_SB_MAGIC);
    atomic_store(&sb_of(&f)->major, HQQ_SB_MAJOR);
    {
        progress_arg pa;
        hqq_test_thread *th;
        pa.sb = sb_of(&f);
        atomic_store(&pa.stop, 0);
        th = hqq_test_thread_start(progress_main, &pa);
        make_params(&f, &p, HQQ_ROLE_CREATOR, 0x88, 0x99);
        p.attach_timeout_ns = (int64_t)(150 * HQQ_NS_PER_MS);
        p.init_stale_ns = (int64_t)(60 * HQQ_NS_PER_MS);
        HQQ_CHECK_STATUS(hqq_sb_claim(sb_of(&f), &p, &mode, &must_init, NULL), HQQ_ERR_INIT_CONTENDED);
        atomic_store(&pa.stop, 1);
        hqq_test_thread_join(th);
        HQQ_CHECK_EQ_U64(atomic_load(&sb_of(&f)->init_token), dead);
    }
    sb_teardown(&f);
}

/* Publish: the commit CAS fails when the ownership was lost; nothing is published. */
static void check_publish_ownership(void) {
    sb_fixture f;
    hqq_claim_params p;
    hqq_mode mode;
    int must_init = 0;
    uint64_t epoch = 0;

    sb_setup(&f);
    make_params(&f, &p, HQQ_ROLE_CREATOR, 0x31, 0x32);
    HQQ_CHECK_STATUS(hqq_sb_claim(sb_of(&f), &p, &mode, &must_init, NULL), HQQ_OK);
    atomic_store(&sb_of(&f)->init_token, 0x3333); /* somebody took over */
    HQQ_CHECK_STATUS(hqq_sb_publish(sb_of(&f), &p, &epoch), HQQ_ERR_INIT_CONTENDED);
    HQQ_CHECK_EQ_U64(atomic_load(&sb_of(&f)->state), HQQ_STATE_INITIALIZING);
    HQQ_CHECK_EQ_U64(atomic_load(&sb_of(&f)->epoch), 0);
    HQQ_CHECK_EQ_U64(atomic_load(&sb_of(&f)->magic), 0);
    sb_teardown(&f);
}

/* The first-page zero test covers the whole page, not only the superblock. */
static void check_page_zero_scan(void) {
    sb_fixture f;
    sb_setup(&f);
    HQQ_CHECK(hqq_sb_first_page_zero(sb_of(&f), f.page) == 1);
    f.base[f.page - 8] = 1; /* last word of the page */
    HQQ_CHECK(hqq_sb_first_page_zero(sb_of(&f), f.page) == 0);
    sb_teardown(&f);
}

int main(void) {
    check_empty_creator_publish_join();
    check_foreign_and_unknown();
    check_incompatible_ready();
    check_opener_empty();
    check_reuse_and_dead();
    check_liveness_at_open();
    check_takeover();
    check_publish_ownership();
    check_page_zero_scan();
    return HQQ_TEST_RESULT("test_superblock");
}
