/* Standard link on caller-owned memory, in one process: message framing,
 * sizes, reads that are too small, zero-copy callbacks, reservations and
 * tombstones, deadlines and cancellation, peer close and drain, peer gone,
 * geometry mismatch, third endpoints, memory argument errors, reuse of a
 * dead segment, parked waits woken by the other side. */
#include <string.h>

#include "hqq_internal.h"
#include "hqq_test.h"
#include "hqq_testutil.h"

#define LK_COUNT 4u
#define LK_SIZE 64u

typedef struct lk_env {
    unsigned char *mem;
    size_t size;
    hqq_link *p;
    hqq_link *s;
} lk_env;

static hqq_options fast_opts(hqq_wait_policy policy) {
    hqq_options o;
    hqq_options_init(&o);
    o.wait_policy = policy;
    o.heartbeat_interval_ns = 20 * (int64_t)HQQ_NS_PER_MS;
    o.peer_timeout_ns = 300 * (int64_t)HQQ_NS_PER_MS;
    return o;
}

static void env_open(lk_env *e, hqq_wait_policy policy) {
    hqq_options o = fast_opts(policy);
    e->size = hqq_standard_size(LK_COUNT, LK_SIZE);
    e->mem = (unsigned char *)hqq_test_pages(e->size);
    HQQ_REQUIRE(hqq_standard_open_memory(e->mem, e->size, LK_COUNT, LK_SIZE, &o, &e->p, NULL) == HQQ_OK);
    HQQ_REQUIRE(hqq_standard_open_memory(e->mem, e->size, LK_COUNT, LK_SIZE, &o, &e->s, NULL) == HQQ_OK);
}

static void env_close(lk_env *e) {
    if (e->s != NULL) {
        hqq_close(e->s);
        e->s = NULL;
    }
    if (e->p != NULL) {
        hqq_close(e->p);
        e->p = NULL;
    }
    hqq_test_pages_free(e->mem, e->size);
}

static void check_roles_and_geometry(void) {
    hqq_options o = fast_opts(HQQ_WAIT_ADAPTIVE);
    size_t size = hqq_standard_size(8, LK_SIZE); /* room for the 8-buffer geometry too */
    unsigned char *mem = (unsigned char *)hqq_test_pages(size);
    hqq_link *p = NULL, *s = NULL, *third = NULL;
    hqq_open_info info;
    hqq_status st;

    HQQ_CHECK(size != 0);
    HQQ_CHECK(hqq_standard_size(LK_COUNT, LK_SIZE) <= size);

    st = hqq_standard_open_memory(mem, size, LK_COUNT, LK_SIZE, &o, &p, NULL);
    HQQ_CHECK_STATUS(st, HQQ_OK);
    HQQ_CHECK_EQ_INT(hqq_link_mode(p), HQQ_MODE_PRIMARY);
    HQQ_CHECK_EQ_U64(hqq_link_buffer_size(p), LK_SIZE);
    HQQ_CHECK_EQ_U64(hqq_link_buffer_count(p), LK_COUNT);
    HQQ_CHECK(hqq_link_name(p) == NULL);

    /* A segment of another geometry joins only with the same geometry. */
    hqq_open_info_init(&info);
    st = hqq_standard_open_memory(mem, size, 8, LK_SIZE, &o, &third, &info);
    HQQ_CHECK_STATUS(st, HQQ_ERR_GEOMETRY_MISMATCH);
    HQQ_CHECK(hqq_error_is(st, HQQ_ERR_INVALID_SIZE));
    HQQ_CHECK_EQ_U64(info.got.buffer_count, LK_COUNT);
    HQQ_CHECK_EQ_U64(info.got.buffer_size, LK_SIZE);
    HQQ_CHECK_EQ_U64(info.want.buffer_count, 8);
    HQQ_CHECK_EQ_U64(info.want.layout_size, hqq_standard_size(8, LK_SIZE));
    HQQ_CHECK(third == NULL);

    st = hqq_standard_open_memory(mem, size, LK_COUNT, LK_SIZE, &o, &s, NULL);
    HQQ_CHECK_STATUS(st, HQQ_OK);
    HQQ_CHECK_EQ_INT(hqq_link_mode(s), HQQ_MODE_SECONDARY);

    /* A third endpoint on a live segment is busy. */
    st = hqq_standard_open_memory(mem, size, LK_COUNT, LK_SIZE, &o, &third, NULL);
    HQQ_CHECK_STATUS(st, HQQ_ERR_LINK_BUSY);
    HQQ_CHECK(hqq_error_is(st, HQQ_ERR_FAILED_INIT));
    HQQ_CHECK(third == NULL);

    hqq_close(s);
    hqq_close(p);

    /* Both endpoints are gone: the memory is reusable and re-initialized (new Primary). */
    st = hqq_standard_open_memory(mem, size, LK_COUNT, LK_SIZE, &o, &p, NULL);
    HQQ_CHECK_STATUS(st, HQQ_OK);
    HQQ_CHECK_EQ_INT(hqq_link_mode(p), HQQ_MODE_PRIMARY);
    hqq_close(p);

    /* Argument errors. */
    HQQ_CHECK_STATUS(hqq_standard_open_memory(mem + 1, size, LK_COUNT, LK_SIZE, &o, &p, NULL), HQQ_ERR_MEMORY_ALIGN);
    HQQ_CHECK(p == NULL);
    HQQ_CHECK_STATUS(hqq_standard_open_memory(mem, hqq_standard_size(LK_COUNT, LK_SIZE) - 1, LK_COUNT, LK_SIZE, &o, &p, NULL), HQQ_ERR_MEMORY_SMALL);
    HQQ_CHECK_STATUS(hqq_standard_open_memory(mem, size, 3, LK_SIZE, &o, &p, NULL), HQQ_ERR_INVALID_SIZE);
    HQQ_CHECK_STATUS(hqq_standard_open_memory(mem, size, LK_COUNT, 12, &o, &p, NULL), HQQ_ERR_INVALID_SIZE);
    HQQ_CHECK_STATUS(hqq_standard_open_memory(mem, size, LK_COUNT, LK_SIZE, &o, NULL, NULL), HQQ_ERR_INVALID_ARGUMENT);
    hqq_test_pages_free(mem, size);
}

/* Foreign data in the first page: nothing is written, the open fails. */
static void check_foreign_memory(void) {
    size_t size = hqq_standard_size(LK_COUNT, LK_SIZE);
    unsigned char *mem = (unsigned char *)hqq_test_pages(size);
    hqq_link *p = NULL;
    unsigned char before[64];

    mem[100] = 0x42;
    memcpy(before, mem, sizeof before);
    HQQ_CHECK_STATUS(hqq_standard_open_memory(mem, size, LK_COUNT, LK_SIZE, NULL, &p, NULL),
                     HQQ_ERR_INCOMPATIBLE_LAYOUT);
    HQQ_CHECK(p == NULL);
    HQQ_CHECK(memcmp(before, mem, sizeof before) == 0);
    hqq_test_pages_free(mem, size);
}

static void check_messages(void) {
    lk_env e;
    unsigned char out[LK_SIZE + 8];
    unsigned char in[LK_SIZE];
    size_t len = 0;
    unsigned i;
    size_t sizes[] = {1, 7, LK_SIZE - 1, LK_SIZE};

    env_open(&e, HQQ_WAIT_ADAPTIVE);
    for (i = 0; i < sizeof sizes / sizeof sizes[0]; i++) {
        size_t n = sizes[i], k;
        for (k = 0; k < n; k++) {
            in[k] = (unsigned char)(i * 31 + k);
        }
        HQQ_CHECK_STATUS(hqq_write(e.p, in, n, NULL), HQQ_OK);
        HQQ_CHECK_STATUS(hqq_read(e.s, out, sizeof out, &len, NULL), HQQ_OK);
        HQQ_CHECK_EQ_U64(len, n);
        HQQ_CHECK(memcmp(out, in, n) == 0);
        /* And the other direction. */
        HQQ_CHECK_STATUS(hqq_write(e.s, in, n, NULL), HQQ_OK);
        HQQ_CHECK_STATUS(hqq_read(e.p, out, sizeof out, &len, NULL), HQQ_OK);
        HQQ_CHECK_EQ_U64(len, n);
        HQQ_CHECK(memcmp(out, in, n) == 0);
    }
    /* Empty writes succeed without publishing; oversize writes are refused locally. */
    HQQ_CHECK_STATUS(hqq_write(e.p, in, 0, NULL), HQQ_OK);
    {
        unsigned char big[LK_SIZE + 1];
        memset(big, 1, sizeof big);
        HQQ_CHECK_STATUS(hqq_write(e.p, big, sizeof big, NULL), HQQ_ERR_BUFFER_OVERFLOW);
    }
    /* Nothing was published by those: a zero-timeout read reports a timeout. */
    {
        hqq_wait w;
        w.timeout_ns = 0;
        w.cancel = NULL;
        HQQ_CHECK_STATUS(hqq_read(e.s, out, sizeof out, &len, &w), HQQ_ERR_TIMEOUT);
    }
    /* A message larger than the buffer is not consumed; the required size is reported. */
    {
        unsigned char big[LK_SIZE];
        memset(big, 0x5A, sizeof big);
        HQQ_CHECK_STATUS(hqq_write(e.p, big, LK_SIZE, NULL), HQQ_OK);
        len = 0;
        HQQ_CHECK_STATUS(hqq_read(e.s, out, 10, &len, NULL), HQQ_ERR_BUFFER_TOO_SMALL);
        HQQ_CHECK_EQ_U64(len, LK_SIZE);
        HQQ_CHECK_STATUS(hqq_read(e.s, out, LK_SIZE, &len, NULL), HQQ_OK);
        HQQ_CHECK_EQ_U64(len, LK_SIZE);
        HQQ_CHECK(memcmp(out, big, LK_SIZE) == 0);
    }
    /* Argument validation. */
    HQQ_CHECK_STATUS(hqq_read(e.s, out, sizeof out, NULL, NULL), HQQ_ERR_INVALID_ARGUMENT);
    HQQ_CHECK_STATUS(hqq_read(NULL, out, sizeof out, &len, NULL), HQQ_ERR_INVALID_ARGUMENT);
    HQQ_CHECK_STATUS(hqq_write(NULL, in, 1, NULL), HQQ_ERR_INVALID_ARGUMENT);
    HQQ_CHECK_EQ_INT(hqq_link_mode(e.s), HQQ_MODE_SECONDARY);
    env_close(&e);
}

static unsigned char zc_pattern(size_t i) { return (unsigned char)(0xC0u + i); }

static hqq_status zc_write_fn(void *user, void *buf, size_t cap, size_t *n) {
    size_t i, want = *(size_t *)user;
    *n = want; /* out-of-range sizes are reported through n; the library refuses them */
    for (i = 0; i < want && i < cap; i++) {
        ((unsigned char *)buf)[i] = zc_pattern(i);
    }
    return HQQ_OK;
}

static hqq_status zc_failing_fn(void *user, void *buf, size_t cap, size_t *n) {
    (void)user;
    (void)buf;
    (void)cap;
    *n = 4;
    return HQQ_ERR_UNSUPPORTED;
}

static hqq_status zc_read_fn(void *user, const void *data, size_t len) {
    size_t i;
    const unsigned char *d = (const unsigned char *)data;
    size_t want = *(size_t *)user;
    HQQ_CHECK_EQ_U64(len, want);
    for (i = 0; i < len; i++) {
        HQQ_CHECK(d[i] == zc_pattern(i));
    }
    return HQQ_OK;
}

static hqq_status zc_read_fail_fn(void *user, const void *data, size_t len) {
    (void)user;
    (void)data;
    (void)len;
    return HQQ_ERR_INVALID_OP;
}

static void check_zero_copy(void) {
    lk_env e;
    size_t want = 9, len = 0, bad = LK_SIZE + 5;
    unsigned char out[LK_SIZE];

    env_open(&e, HQQ_WAIT_ADAPTIVE);
    HQQ_CHECK_STATUS(hqq_write_zc(e.p, zc_write_fn, &want, &len, NULL), HQQ_OK);
    HQQ_CHECK_EQ_U64(len, want);
    HQQ_CHECK_STATUS(hqq_read_zc(e.s, zc_read_fn, &want, &len, NULL), HQQ_OK);
    HQQ_CHECK_EQ_U64(len, want);

    /* An out-of-range size: a tombstone is published, the ring keeps moving. */
    HQQ_CHECK_STATUS(hqq_write_zc(e.p, zc_write_fn, &bad, &len, NULL), HQQ_ERR_BUFFER_OVERFLOW);
    want = 3;
    HQQ_CHECK_STATUS(hqq_write_zc(e.p, zc_write_fn, &want, NULL, NULL), HQQ_OK);
    HQQ_CHECK_STATUS(hqq_read_zc(e.s, zc_read_fn, &want, NULL, NULL), HQQ_OK);

    /* A failing writer: the tombstone carries no message, the error is returned. */
    HQQ_CHECK_STATUS(hqq_write_zc(e.p, zc_failing_fn, NULL, NULL, NULL), HQQ_ERR_UNSUPPORTED);
    HQQ_CHECK_STATUS(hqq_write(e.p, "abcde", 5, NULL), HQQ_OK);
    HQQ_CHECK_STATUS(hqq_read(e.s, out, sizeof out, &len, NULL), HQQ_OK);
    HQQ_CHECK_EQ_U64(len, 5);

    /* A failing reader still consumes the message. */
    HQQ_CHECK_STATUS(hqq_write(e.p, "abcde", 5, NULL), HQQ_OK);
    HQQ_CHECK_STATUS(hqq_read_zc(e.s, zc_read_fail_fn, NULL, NULL, NULL), HQQ_ERR_INVALID_OP);
    {
        hqq_wait w;
        w.timeout_ns = 0;
        w.cancel = NULL;
        HQQ_CHECK_STATUS(hqq_read(e.s, out, sizeof out, &len, &w), HQQ_ERR_TIMEOUT);
    }
    env_close(&e);
}

static void check_reservations(void) {
    lk_env e;
    hqq_write_reservation wr;
    hqq_read_reservation rr;
    hqq_write_reservation stale;
    unsigned char out[LK_SIZE];
    size_t len = 0;

    env_open(&e, HQQ_WAIT_ADAPTIVE);
    HQQ_CHECK_STATUS(hqq_write_reserve(e.p, &wr, NULL), HQQ_OK);
    HQQ_CHECK(wr.data != NULL);
    HQQ_CHECK_EQ_U64(wr.capacity, LK_SIZE);
    memcpy(wr.data, "reserved", 8);
    stale = wr;
    HQQ_CHECK_STATUS(hqq_write_commit(&wr, 8), HQQ_OK);
    HQQ_CHECK(wr.data == NULL);
    /* A stale copy of the completed reservation fails and changes nothing. */
    HQQ_CHECK_STATUS(hqq_write_commit(&stale, 8), HQQ_ERR_INVALID_OP);
    HQQ_CHECK_STATUS(hqq_write_abort(&stale, 0), HQQ_ERR_INVALID_OP);

    /* Read reservation: data is valid until release; a stale release fails. */
    HQQ_CHECK_STATUS(hqq_read_reserve(e.s, &rr, NULL), HQQ_OK);
    HQQ_CHECK_EQ_U64(rr.size, 8);
    HQQ_CHECK(memcmp(rr.data, "reserved", 8) == 0);
    {
        hqq_read_reservation copy = rr;
        HQQ_CHECK_STATUS(hqq_read_release(&rr), HQQ_OK);
        HQQ_CHECK_STATUS(hqq_read_release(&copy), HQQ_ERR_INVALID_OP);
    }

    /* Abort publishes a tombstone which readers skip. */
    HQQ_CHECK_STATUS(hqq_write_reserve(e.p, &wr, NULL), HQQ_OK);
    HQQ_CHECK_STATUS(hqq_write_abort(&wr, 0), HQQ_OK);
    HQQ_CHECK_STATUS(hqq_write(e.p, "next", 4, NULL), HQQ_OK);
    HQQ_CHECK_STATUS(hqq_read(e.s, out, sizeof out, &len, NULL), HQQ_OK);
    HQQ_CHECK_EQ_U64(len, 4);
    HQQ_CHECK(memcmp(out, "next", 4) == 0);

    /* Length errors complete the slot with a tombstone and report the error. */
    HQQ_CHECK_STATUS(hqq_write_reserve(e.p, &wr, NULL), HQQ_OK);
    HQQ_CHECK_STATUS(hqq_write_commit(&wr, 0), HQQ_ERR_INVALID_SIZE);
    HQQ_CHECK_STATUS(hqq_write_reserve(e.p, &wr, NULL), HQQ_OK);
    HQQ_CHECK_STATUS(hqq_write_commit(&wr, LK_SIZE + 1), HQQ_ERR_BUFFER_OVERFLOW);
    HQQ_CHECK_STATUS(hqq_write(e.p, "tail", 4, NULL), HQQ_OK);
    HQQ_CHECK_STATUS(hqq_read(e.s, out, sizeof out, &len, NULL), HQQ_OK);
    HQQ_CHECK_EQ_U64(len, 4);
    env_close(&e);
}

/* Peer closes after writing: the reader drains the published messages first. */
static void check_peer_close_drain(void) {
    lk_env e;
    unsigned char out[LK_SIZE];
    size_t len = 0;

    env_open(&e, HQQ_WAIT_ADAPTIVE);
    HQQ_CHECK_STATUS(hqq_write(e.s, "one", 3, NULL), HQQ_OK);
    HQQ_CHECK_STATUS(hqq_write(e.s, "two", 3, NULL), HQQ_OK);
    hqq_close(e.s);
    e.s = NULL;
    HQQ_CHECK_STATUS(hqq_read(e.p, out, sizeof out, &len, NULL), HQQ_OK);
    HQQ_CHECK(memcmp(out, "one", 3) == 0);
    HQQ_CHECK_STATUS(hqq_read(e.p, out, sizeof out, &len, NULL), HQQ_OK);
    HQQ_CHECK(memcmp(out, "two", 3) == 0);
    HQQ_CHECK_STATUS(hqq_read(e.p, out, sizeof out, &len, NULL), HQQ_ERR_PEER_CLOSED);
    HQQ_CHECK_EQ_INT(hqq_peer_state(e.p), HQQ_PEER_CLOSED);
    HQQ_CHECK_STATUS(hqq_write(e.p, "x", 1, NULL), HQQ_ERR_PEER_CLOSED);

    /* Closing again is idempotent; a closed link refuses calls. */
    hqq_shutdown(e.p);
    HQQ_CHECK_STATUS(hqq_write(e.p, "x", 1, NULL), HQQ_ERR_CLOSED);
    env_close(&e);
}

/* The peer's record is replaced under us: the peer is gone (ErrPeerGone). */
static void check_peer_replaced(void) {
    lk_env e;
    unsigned char out[LK_SIZE];
    size_t len = 0;
    hqq_superblock *sb;

    env_open(&e, HQQ_WAIT_ADAPTIVE);
    sb = (hqq_superblock *)(void *)e.mem;
    HQQ_CHECK_EQ_INT(hqq_peer_state(e.p), HQQ_PEER_CONNECTED);
    atomic_store(&sb->endpoints[1].token, UINT64_C(0x1234567));
    HQQ_CHECK_EQ_INT(hqq_peer_state(e.p), HQQ_PEER_GONE);
    HQQ_CHECK_STATUS(hqq_read(e.p, out, sizeof out, &len, NULL), HQQ_ERR_PEER_GONE);
    HQQ_CHECK(hqq_error_is(HQQ_ERR_PEER_GONE, HQQ_ERR_CONN_NOT_FOUND));
    HQQ_CHECK_STATUS(hqq_write(e.p, "x", 1, NULL), HQQ_ERR_PEER_GONE);
    env_close(&e);
}

/* Heartbeat stall: a record that keeps its token but stops heartbeating is
 * declared gone after the peer timeout (the peer monitor of SPEC 6.3). */
static void check_peer_stalled(void) {
    size_t size = hqq_standard_size(LK_COUNT, LK_SIZE);
    unsigned char *mem = (unsigned char *)hqq_test_pages(size);
    hqq_options o = fast_opts(HQQ_WAIT_ADAPTIVE);
    hqq_options quiet = o;
    hqq_link *p = NULL, *s = NULL;
    hqq_superblock *sb;
    unsigned char out[LK_SIZE];
    size_t len = 0;
    uint64_t t0;
    hqq_wait w;

    quiet.heartbeat_interval_ns = -1; /* the secondary never heartbeats itself */
    HQQ_REQUIRE(hqq_standard_open_memory(mem, size, LK_COUNT, LK_SIZE, &o, &p, NULL) == HQQ_OK);
    HQQ_REQUIRE(hqq_standard_open_memory(mem, size, LK_COUNT, LK_SIZE, &quiet, &s, NULL) == HQQ_OK);
    sb = (hqq_superblock *)(void *)mem;
    /* Publish the interval of the quiet secondary so that the primary monitors it. */
    atomic_store(&sb->endpoints[1].interval, 20u * 1000u * 1000u);
    atomic_store(&sb->endpoints[1].heartbeat, hqq_plat_wall_ns());
    w.timeout_ns = 5 * (int64_t)HQQ_NS_PER_SEC;
    w.cancel = NULL;
    t0 = hqq_plat_mono_ns();
    HQQ_CHECK_STATUS(hqq_read(p, out, sizeof out, &len, &w), HQQ_ERR_PEER_GONE);
    HQQ_CHECK(hqq_plat_mono_ns() - t0 < 3 * HQQ_NS_PER_SEC);
    hqq_close(s);
    hqq_close(p);
    hqq_test_pages_free(mem, size);
}

/* Deadlines and cancellation of blocked calls. */
typedef struct cancel_arg {
    hqq_cancel *token;
} cancel_arg;

static void cancel_main(void *p) {
    hqq_test_sleep_ms(40);
    hqq_cancel_signal(((cancel_arg *)p)->token);
}

static void check_deadline_and_cancel(void) {
    lk_env e;
    unsigned char out[LK_SIZE];
    size_t len = 0;
    uint64_t t0;
    hqq_wait w;
    hqq_cancel token;
    cancel_arg ca;
    hqq_test_thread *th;
    unsigned i;

    env_open(&e, HQQ_WAIT_ADAPTIVE);
    w.timeout_ns = 60 * (int64_t)HQQ_NS_PER_MS;
    w.cancel = NULL;
    t0 = hqq_plat_mono_ns();
    HQQ_CHECK_STATUS(hqq_read(e.s, out, sizeof out, &len, &w), HQQ_ERR_TIMEOUT);
    HQQ_CHECK(hqq_plat_mono_ns() - t0 >= 50 * HQQ_NS_PER_MS);
    HQQ_CHECK(hqq_plat_mono_ns() - t0 < 2 * HQQ_NS_PER_SEC);

    /* Full ring: a writer with a deadline gives up with a timeout, then nothing is lost. */
    for (i = 0; i < LK_COUNT; i++) {
        HQQ_CHECK_STATUS(hqq_write(e.p, "fill", 4, NULL), HQQ_OK);
    }
    w.timeout_ns = 30 * (int64_t)HQQ_NS_PER_MS;
    HQQ_CHECK_STATUS(hqq_write(e.p, "more", 4, &w), HQQ_ERR_TIMEOUT);
    for (i = 0; i < LK_COUNT; i++) {
        HQQ_CHECK_STATUS(hqq_read(e.s, out, sizeof out, &len, NULL), HQQ_OK);
        HQQ_CHECK_EQ_U64(len, 4);
    }

    /* A cancellation signalled from another thread wakes a parked reader. */
    hqq_cancel_init(&token);
    ca.token = &token;
    th = hqq_test_thread_start(cancel_main, &ca);
    w.timeout_ns = -1;
    w.cancel = &token;
    t0 = hqq_plat_mono_ns();
    HQQ_CHECK_STATUS(hqq_read(e.s, out, sizeof out, &len, &w), HQQ_ERR_CANCELED);
    HQQ_CHECK(hqq_plat_mono_ns() - t0 < 2 * HQQ_NS_PER_SEC);
    hqq_test_thread_join(th);
    HQQ_CHECK(hqq_cancel_is_signalled(&token));
    env_close(&e);
}

/* Parked waits: a reader that blocks on an empty ring is woken by the writer,
 * and a writer that blocks on a full ring is woken by the reader. */
typedef struct park_arg {
    hqq_link *link;
    unsigned count;
    _Atomic int ok;
} park_arg;

static void park_reader_main(void *p) {
    park_arg *a = (park_arg *)p;
    unsigned char out[LK_SIZE];
    unsigned i;
    int good = 1;
    for (i = 0; i < a->count; i++) {
        size_t len = 0;
        if (hqq_read(a->link, out, sizeof out, &len, NULL) != HQQ_OK || len != 2 || out[0] != (unsigned char)i) {
            good = 0;
        }
    }
    atomic_store(&a->ok, good);
}

static void park_writer_main(void *p) {
    park_arg *a = (park_arg *)p;
    unsigned i;
    int good = 1;
    for (i = 0; i < a->count; i++) {
        unsigned char msg[2] = {(unsigned char)i, 0x77};
        if (hqq_write(a->link, msg, 2, NULL) != HQQ_OK) {
            good = 0;
        }
    }
    atomic_store(&a->ok, good);
}

static void check_parked_wakeups(void) {
    lk_env e;
    hqq_test_thread *th;
    park_arg a;
    uint64_t t0;
    unsigned i;

    env_open(&e, HQQ_WAIT_ADAPTIVE);
    /* Reader parks first; the writer delivers after a pause. */
    a.link = e.s;
    a.count = 3;
    atomic_store(&a.ok, 0);
    th = hqq_test_thread_start(park_reader_main, &a);
    hqq_test_sleep_ms(120);
    for (i = 0; i < 3; i++) {
        unsigned char msg[2] = {(unsigned char)i, 0x77};
        HQQ_CHECK_STATUS(hqq_write(e.p, msg, 2, NULL), HQQ_OK);
        hqq_test_sleep_ms(30);
    }
    hqq_test_thread_join(th);
    HQQ_CHECK_EQ_INT(atomic_load(&a.ok), 1);

    /* Writer parks on a full ring (2 slots of 4 are filled first); the reader drains it. */
    a.link = e.p;
    a.count = 12;
    atomic_store(&a.ok, 0);
    th = hqq_test_thread_start(park_writer_main, &a);
    hqq_test_sleep_ms(50);
    t0 = hqq_plat_mono_ns();
    {
        unsigned char out[LK_SIZE];
        for (i = 0; i < 12; i++) {
            size_t len = 0;
            HQQ_CHECK_STATUS(hqq_read(e.s, out, sizeof out, &len, NULL), HQQ_OK);
            HQQ_CHECK(out[0] == (unsigned char)i);
        }
    }
    hqq_test_thread_join(th);
    HQQ_CHECK_EQ_INT(atomic_load(&a.ok), 1);
    HQQ_CHECK(hqq_plat_mono_ns() - t0 < 5 * HQQ_NS_PER_SEC);
    env_close(&e);
}

static void check_simple_policy_basic(void) {
    lk_env e;
    unsigned char out[LK_SIZE];
    size_t len = 0;

    env_open(&e, HQQ_WAIT_SIMPLE);
    HQQ_CHECK_STATUS(hqq_write(e.p, "simple", 6, NULL), HQQ_OK);
    HQQ_CHECK_STATUS(hqq_read(e.s, out, sizeof out, &len, NULL), HQQ_OK);
    HQQ_CHECK(memcmp(out, "simple", 6) == 0);
    env_close(&e);
}

int main(void) {
    check_roles_and_geometry();
    check_foreign_memory();
    check_messages();
    check_zero_copy();
    check_reservations();
    check_peer_close_drain();
    check_peer_replaced();
    check_peer_stalled();
    check_deadline_and_cancel();
    check_parked_wakeups();
    check_simple_policy_basic();
    return HQQ_TEST_RESULT("test_link");
}
