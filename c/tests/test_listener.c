/* Listener (LISTENER_DRAFT 5-8) on one host: concurrent clients connect and each
 * exchanges one message with its accepted link; a second acceptor on the name is
 * busy; records that fail validation or name an unreachable segment are counted
 * while the acceptor keeps serving; closing the acceptor wakes a waiting Accept
 * (ACCEPTOR_CLOSED) and a waiting client (LISTENER_CLOSED); a connect to a missing
 * listener times out. Bad records are injected with raw ring writes (the producer
 * side of the ring protocol), since the public API only creates valid records. */
#include <stdatomic.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "hqq/hqq_listener.h"
#include "hqq_internal.h"
#include "hqq_test.h"
#include "hqq_testutil.h"

#define N_CLIENTS 3
#define LSN_TEST_HEADER 4096u
#define LSN_TEST_RING 4096u
#define LSN_TEST_SLOT 256u
#define LSN_TEST_RECORD 248u

/* Connect record layout (LISTENER_DRAFT 3.2). */
typedef struct test_record {
    uint64_t client_token, buffer_count, buffer_size, features, flags, name_len, client_pid, reserved;
    unsigned char name[184];
} test_record;

_Static_assert(sizeof(test_record) == LSN_TEST_RECORD, "record is 248 bytes");

static hqq_wait wait_ms(int64_t ms) {
    hqq_wait w;
    w.timeout_ns = ms * 1000000;
    w.cancel = NULL;
    return w;
}

/* ---- Clients and echo servers ---------------------------------------------------- */

typedef struct client_arg {
    const char *name;
    int index;
    hqq_status st;
    hqq_adv *link;
    int echo_ok;
} client_arg;

/* Connects, sends one message and expects it back. The link is closed by the caller. */
static void client_main(void *p) {
    client_arg *c = p;
    hqq_connect_options o;
    hqq_wait w = wait_ms(15000);
    hqq_stream *s0;
    hqq_msg *m = NULL;
    char payload[32];
    size_t len;

    hqq_connect_options_init(&o);
    c->st = hqq_connect(c->name, &o, &w, &c->link);
    if (c->st != HQQ_OK) {
        return;
    }
    s0 = hqq_adv_stream0(c->link);
    len = (size_t)snprintf(payload, sizeof payload, "client %d", c->index);
    c->st = hqq_stream_send(s0, payload, len, &w);
    if (c->st == HQQ_OK) {
        c->st = hqq_stream_recv(s0, &m, &w);
    }
    c->echo_ok = c->st == HQQ_OK && hqq_msg_len(m) == len && memcmp(hqq_msg_data(m), payload, len) == 0;
    hqq_msg_release(m);
}

typedef struct server_arg {
    hqq_adv *link;
    hqq_status st;
} server_arg;

/* Echoes one message of an accepted link. The link is closed by the caller. */
static void server_main(void *p) {
    server_arg *s = p;
    hqq_wait w = wait_ms(15000);
    hqq_stream *s0 = hqq_adv_stream0(s->link);
    hqq_msg *m = NULL;

    s->st = hqq_stream_recv(s0, &m, &w);
    if (s->st == HQQ_OK) {
        s->st = hqq_stream_send(s0, hqq_msg_data(m), hqq_msg_len(m), &w);
    }
    hqq_msg_release(m);
}

/* Runs n clients against acc, one accepted link and echo each. Returns the echoes that matched. */
static int serve(hqq_acceptor *acc, const char *name, int n) {
    client_arg ca[N_CLIENTS];
    hqq_test_thread *ct[N_CLIENTS];
    server_arg sa[N_CLIENTS];
    hqq_test_thread *srv[N_CLIENTS];
    int i, ok = 0, started = 0;

    for (i = 0; i < n; i++) {
        memset(&ca[i], 0, sizeof ca[i]);
        ca[i].name = name;
        ca[i].index = i;
        ct[i] = hqq_test_thread_start(client_main, &ca[i]);
    }
    for (i = 0; i < n; i++) {
        hqq_wait w = wait_ms(15000);
        hqq_adv *link = NULL;
        hqq_status st = hqq_acceptor_accept(acc, &link, &w);
        HQQ_CHECK_STATUS(st, HQQ_OK);
        if (st == HQQ_OK) {
            sa[started].link = link;
            sa[started].st = HQQ_OK;
            srv[started] = hqq_test_thread_start(server_main, &sa[started]);
            started++;
        }
    }
    for (i = 0; i < n; i++) {
        hqq_test_thread_join(ct[i]);
        HQQ_CHECK_STATUS(ca[i].st, HQQ_OK);
        HQQ_CHECK(ca[i].echo_ok);
        if (ca[i].echo_ok) {
            ok++;
        }
    }
    for (i = 0; i < started; i++) {
        hqq_test_thread_join(srv[i]);
        HQQ_CHECK_STATUS(sa[i].st, HQQ_OK);
        HQQ_CHECK_STATUS(hqq_adv_close(sa[i].link), HQQ_OK);
    }
    for (i = 0; i < n; i++) {
        if (ca[i].link != NULL) {
            HQQ_CHECK_STATUS(hqq_adv_close(ca[i].link), HQQ_OK);
        }
    }
    return ok;
}

/* ---- Raw records ----------------------------------------------------------------- */

/* The listener object, mapped for raw writes: layout size and link name prefix. */
typedef struct raw_listener {
    hqq_shm *shm;
    unsigned char *base;
    size_t layout;
    unsigned char prefix[32];
    size_t prefix_len;
    uint64_t capacity;
} raw_listener;

static void raw_open(raw_listener *r, const char *name) {
    int os_err = 0;
    unsigned char *hdr = NULL;

    HQQ_REQUIRE(hqq_plat_shm_open(NULL, name, 0, 0, 0, &r->shm, &os_err) == HQQ_OK);
    HQQ_REQUIRE(hqq_plat_shm_map(r->shm, LSN_TEST_HEADER, (void **)&hdr, &os_err) == HQQ_OK);
    r->capacity = atomic_load((_Atomic uint64_t *)(hdr + 48));
    r->layout = (size_t)atomic_load((_Atomic uint64_t *)(hdr + 56));
    r->prefix_len = (size_t)atomic_load((_Atomic uint64_t *)(hdr + 192));
    memcpy(r->prefix, hdr + 200, r->prefix_len);
    hqq_plat_shm_unmap(r->shm, hdr, LSN_TEST_HEADER);
    HQQ_REQUIRE(r->capacity >= 2);
    HQQ_REQUIRE(hqq_plat_shm_map(r->shm, r->layout, (void **)&r->base, &os_err) == HQQ_OK);
}

static void raw_close(raw_listener *r) {
    hqq_plat_shm_unmap(r->shm, r->base, r->layout);
    hqq_plat_shm_close(NULL, r->shm);
    memset(r, 0, sizeof *r);
}

/* Producer side of the listener ring: claim the slot at w, write the record, publish. */
static void raw_push(raw_listener *r, const test_record *rec) {
    unsigned char *ring = r->base + LSN_TEST_RING;
    _Atomic uint64_t *w = (_Atomic uint64_t *)(ring + 256);
    uint64_t mask = r->capacity - 1;
    int tries;

    for (tries = 0; tries < 10000; tries++) {
        uint64_t pos = atomic_load(w);
        unsigned char *slot = ring + HQQ_RING_HEADER_SIZE + (pos & mask) * LSN_TEST_SLOT;
        _Atomic uint64_t *seq = (_Atomic uint64_t *)slot;
        if (atomic_load(seq) == pos && atomic_compare_exchange_strong(w, &pos, pos + 1)) {
            memcpy(slot + 8, rec, sizeof *rec);
            atomic_store(seq, pos + 1);
            return;
        }
        hqq_test_sleep_ms(1);
    }
    HQQ_CHECK(0);
}

/* A link name under the listener's prefix with the given 64-bit tail. */
static void make_name(const raw_listener *r, uint64_t tail, char out[64]) {
    static const char hex[] = "0123456789abcdef";
    size_t i;
    memcpy(out, r->prefix, r->prefix_len);
    for (i = 0; i < 16; i++) {
        out[r->prefix_len + i] = hex[(tail >> (60 - 4 * i)) & 0xFu];
    }
    out[r->prefix_len + 16] = '\0';
}

/* Waits until the acceptor counted at least the given records. */
static int wait_stats(hqq_acceptor *acc, uint64_t malformed, uint64_t unreachable) {
    int i;
    for (i = 0; i < 500; i++) {
        hqq_acceptor_stats st;
        hqq_acceptor_stats_get(acc, &st);
        if (st.malformed >= malformed && st.unreachable >= unreachable) {
            return 1;
        }
        hqq_test_sleep_ms(10);
    }
    return 0;
}

/* ---- Checks ------------------------------------------------------------------------- */

static void check_round_trips(void) {
    char name[64];
    hqq_acceptor_options ao;
    hqq_acceptor *acc = NULL, *second = NULL;
    hqq_acceptor_stats st;

    hqq_test_unique_name(name, sizeof name, "lsn");
    hqq_acceptor_options_init(&ao);
    HQQ_REQUIRE(hqq_acceptor_create(name, &ao, &acc, NULL) == HQQ_OK);
    HQQ_CHECK_STATUS(hqq_acceptor_create(name, &ao, &second, NULL), HQQ_ERR_LISTENER_BUSY);
    HQQ_CHECK(second == NULL);
    HQQ_CHECK_EQ_INT(serve(acc, name, N_CLIENTS), N_CLIENTS);
    hqq_acceptor_stats_get(acc, &st);
    HQQ_CHECK_EQ_U64(st.accepted, N_CLIENTS);
    HQQ_CHECK(st.records >= N_CLIENTS);
    HQQ_CHECK_STATUS(hqq_acceptor_close(acc), HQQ_OK);
}

static void check_bad_records(void) {
    char name[64], link_name[64];
    hqq_acceptor *acc = NULL;
    hqq_acceptor_options ao;
    raw_listener r;
    test_record rec;
    hqq_acceptor_stats st;

    hqq_test_unique_name(name, sizeof name, "lsx");
    hqq_acceptor_options_init(&ao);
    HQQ_REQUIRE(hqq_acceptor_create(name, &ao, &acc, NULL) == HQQ_OK);
    raw_open(&r, name);

    /* Flags set: malformed. */
    memset(&rec, 0, sizeof rec);
    rec.client_token = 0x5151u;
    rec.buffer_count = 2;
    rec.buffer_size = 8;
    rec.flags = 1;
    make_name(&r, 1, link_name);
    rec.name_len = r.prefix_len + 16;
    memcpy(rec.name, link_name, r.prefix_len + 16);
    raw_push(&r, &rec);

    /* Empty name: malformed. */
    rec.flags = 0;
    rec.name_len = 0;
    raw_push(&r, &rec);

    /* Name with a foreign prefix: malformed. */
    memset(rec.name, 'z', sizeof rec.name);
    rec.name_len = 17;
    raw_push(&r, &rec);

    /* Client token zero: malformed. */
    make_name(&r, 2, link_name);
    memset(rec.name, 0, sizeof rec.name);
    memcpy(rec.name, link_name, r.prefix_len + 16);
    rec.name_len = r.prefix_len + 16;
    rec.client_token = 0;
    raw_push(&r, &rec);

    /* Buffer count not a power of two: malformed. */
    rec.client_token = 0x5151u;
    rec.buffer_count = 3;
    raw_push(&r, &rec);

    /* Well-formed record whose link segment does not exist: unreachable. */
    rec.buffer_count = 2;
    make_name(&r, 3, link_name);
    memcpy(rec.name, link_name, r.prefix_len + 16);
    raw_push(&r, &rec);

    HQQ_CHECK(wait_stats(acc, 4, 1));
    HQQ_CHECK_EQ_INT(serve(acc, name, 1), 1);
    hqq_acceptor_stats_get(acc, &st);
    HQQ_CHECK(st.malformed >= 4);
    HQQ_CHECK(st.unreachable >= 1);
    HQQ_CHECK_EQ_U64(st.accepted, 1);
    raw_close(&r);
    HQQ_CHECK_STATUS(hqq_acceptor_close(acc), HQQ_OK);
}

typedef struct accept_arg {
    hqq_acceptor *acc;
    hqq_status st;
} accept_arg;

static void accept_main(void *p) {
    accept_arg *a = p;
    hqq_adv *link = NULL;
    a->st = hqq_acceptor_accept(a->acc, &link, NULL);
}

static void check_accept_closed(void) {
    char name[64];
    hqq_acceptor_options ao;
    accept_arg a;
    hqq_test_thread *t;

    hqq_test_unique_name(name, sizeof name, "lsc");
    hqq_acceptor_options_init(&ao);
    HQQ_REQUIRE(hqq_acceptor_create(name, &ao, &a.acc, NULL) == HQQ_OK);
    a.st = HQQ_OK;
    t = hqq_test_thread_start(accept_main, &a);
    hqq_test_sleep_ms(100);
    HQQ_CHECK_STATUS(hqq_acceptor_close(a.acc), HQQ_OK);
    hqq_test_thread_join(t);
    HQQ_CHECK_STATUS(a.st, HQQ_ERR_ACCEPTOR_CLOSED);
}

typedef struct connect_arg {
    const char *name;
    hqq_status st;
    hqq_adv *link;
} connect_arg;

static void connect_main(void *p) {
    connect_arg *c = p;
    hqq_wait w = wait_ms(15000);
    c->st = hqq_connect(c->name, NULL, &w, &c->link);
}

/* The backlog holds one link and the only handshake slot is waiting for room; a third
 * client's record stays queued. Closing the acceptor wakes that client with LISTENER_CLOSED. */
static void check_waiting_client_closed(void) {
    char name[64];
    hqq_acceptor_options ao;
    hqq_acceptor *acc = NULL;
    connect_arg held[2], waiting;
    hqq_test_thread *t;
    int i;

    hqq_test_unique_name(name, sizeof name, "lsw");
    hqq_acceptor_options_init(&ao);
    ao.max_handshakes = 1;
    ao.backlog = 1;
    HQQ_REQUIRE(hqq_acceptor_create(name, &ao, &acc, NULL) == HQQ_OK);

    /* Both connects complete: the acceptor answered each negotiation. */
    for (i = 0; i < 2; i++) {
        held[i].name = name;
        connect_main(&held[i]);
        HQQ_CHECK_STATUS(held[i].st, HQQ_OK);
    }
    waiting.name = name;
    waiting.link = NULL;
    t = hqq_test_thread_start(connect_main, &waiting);
    hqq_test_sleep_ms(300);
    HQQ_CHECK_STATUS(hqq_acceptor_close(acc), HQQ_OK);
    hqq_test_thread_join(t);
    HQQ_CHECK_STATUS(waiting.st, HQQ_ERR_LISTENER_CLOSED);
    HQQ_CHECK(waiting.link == NULL);
    for (i = 0; i < 2; i++) {
        HQQ_CHECK_STATUS(hqq_adv_close(held[i].link), HQQ_OK);
    }
}

static void check_missing_listener(void) {
    hqq_wait w = wait_ms(200);
    hqq_adv *link = NULL;
    HQQ_CHECK_STATUS(hqq_connect("hqq_test_missing_lsn", NULL, &w, &link), HQQ_ERR_TIMEOUT);
    HQQ_CHECK(link == NULL);
}

int main(void) {
    fprintf(stderr, "trace: round trips\n");
    check_round_trips();
    fprintf(stderr, "trace: bad records\n");
    check_bad_records();
    fprintf(stderr, "trace: accept closed\n");
    check_accept_closed();
    fprintf(stderr, "trace: waiting client closed\n");
    check_waiting_client_closed();
    fprintf(stderr, "trace: missing listener\n");
    check_missing_listener();
    return HQQ_TEST_RESULT("test_listener");
}
