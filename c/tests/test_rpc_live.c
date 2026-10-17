/*
 * test_rpc_live: the server over real links. The client side is written here with raw frames:
 * a listener connect and the BG_OPEN call on the control link (BINDGEN.md 4.11), then a Standard
 * link to the session segment that carries HELLO, requests and responses.
 *
 *  - bootstrap: the control link and the session name are unlinked, the full vector session
 *    replayed over the link, bounds of OPEN (geometry, 2 * B * S, versions), max_control_links,
 *    max_sessions, an OPEN that is never followed by HELLO;
 *  - pipelining, oversize requests and responses (chunking at S = 128);
 *  - protocol violation, detach, peer death (a killed client process), CLOSE and CLOSE_ACK;
 *  - write stall, idle timeout, stop, shutdown with a call that never returns;
 *  - serialize(session) versus serialize(none).
 *
 * Run with the argument --death-child <listener> the program is the client that the death test
 * kills.
 */
#if defined(__linux__) && !defined(_GNU_SOURCE)
#  define _GNU_SOURCE 1
#endif
#if defined(__APPLE__) && !defined(_DARWIN_C_SOURCE)
#  define _DARWIN_C_SOURCE 1
#endif

#include <stdatomic.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#if !defined(_WIN32)
#  include <signal.h>
#  include <spawn.h>
#  include <sys/types.h>
#  include <sys/wait.h>
#  include <unistd.h>
extern char **environ;
#endif

#include "hqq/hqq_rpc.h"
#include "hqq_test.h"
#include "hqq_testutil.h"
#include "rpc_ml.h"
#include "rpc_ml_server.h"
#include "rpc_test_util.h"
#include "rpc_client.h"
#include "rpc_vectors.h"

/* ---- Server under test ---------------------------------------------------------------------- */

typedef struct srv_ctx {
    hqqrpc_server *srv;
    hqq_test_thread *thr;
    hqq_status run_st;
    char name[40];
} srv_ctx;

static void run_thread(void *arg) {
    srv_ctx *sc = (srv_ctx *)arg;

    sc->run_st = hqqrpc_server_run(sc->srv);
}

static void srv_start(srv_ctx *sc, const hqqrpc_iface *iface, hqqrpc_server_options *o, const char *tag) {
    hqqrpc_server_options def;

    memset(sc, 0, sizeof *sc);
    hqq_test_unique_name(sc->name, sizeof sc->name, tag);
    if (o == NULL) {
        hqqrpc_server_options_init(&def);
        o = &def;
    }
    HQQ_REQUIRE(hqqrpc_server_create(iface, sc->name, o, &sc->srv) == HQQ_OK);
    sc->thr = hqq_test_thread_start(run_thread, sc);
}

/* Stops the server and waits for run; destroys it if run returned HQQ_OK. Returns run's status. */
static hqq_status srv_stop(srv_ctx *sc) {
    hqq_status st;

    hqqrpc_server_stop(sc->srv);
    hqq_test_thread_join(sc->thr);
    st = sc->run_st;
    if (st == HQQ_OK) {
        HQQ_CHECK_STATUS(hqqrpc_server_destroy(sc->srv), HQQ_OK);
    }
    return st;
}

static const hqq_rpcvec_frame *vec(const char *name) {
    size_t i;

    for (i = 0; i < HQQ_RPCVEC_MICROLIB_COUNT; i++) {
        if (strcmp(hqq_rpcvec_microlib[i].name, name) == 0) {
            return &hqq_rpcvec_microlib[i];
        }
    }
    for (i = 0; i < HQQ_RPCVEC_CHUNKING_COUNT; i++) {
        if (strcmp(hqq_rpcvec_chunking[i].name, name) == 0) {
            return &hqq_rpcvec_chunking[i];
        }
    }
    fprintf(stderr, "vector %s not found\n", name);
    exit(2);
}

/* ---- Tests ----------------------------------------------------------------------------------------- */

/* The full session of 4.14 over real links, byte for byte; the control link and the session name
 * are unlinked; the session ends with CLOSE_ACK and the link closes. */
static void test_vector_session_live(void) {
    srv_ctx sc;
    client c;
    uint8_t f[FRAME_BUF];
    size_t n = 0;
    char name[64];
    char ctl_name[64];
    int i;
    char vn[16];
    hqq_link *probe = NULL;
    hqq_status st;
    hqq_msg *resp = NULL;
    uint64_t status = 0;

    srv_start(&sc, &microlib_iface, NULL, "vec");
    memset(&c, 0, sizeof c);
    HQQ_REQUIRE(ctl_connect(sc.name, &c.ctl) == HQQ_OK);
    snprintf(ctl_name, sizeof ctl_name, "%s", hqq_link_name(hqq_adv_base(c.ctl)));
    HQQ_REQUIRE(ctl_open(c.ctl, 0, 0, 1, "HQBO", &status, &resp, 5 * SEC) == HQQ_OK);
    HQQ_CHECK_EQ_U64(status, 0);
    /* the server unlinked the control link right after accept */
    {
        hqq_options lo;

        hqq_options_init(&lo);
        lo.attach_timeout_ns = 100 * MS;
        st = hqq_standard_open(ctl_name, 4, 4096, &lo, &probe, NULL);
        HQQ_CHECK_STATUS(st, HQQ_ERR_NOT_EXIST);
        if (st == HQQ_OK) {
            (void)hqq_close(probe);
        }
    }
    HQQ_REQUIRE(attach(&c, resp) == HQQ_OK);
    snprintf(name, sizeof name, "%s", c.name);
    hqq_msg_release(resp);
    HQQ_CHECK_EQ_INT(c.B, 64);
    HQQ_CHECK_EQ_INT(c.S, 16384);
    HQQ_CHECK(strlen(c.name) <= 26);

    {
        const hqq_rpcvec_frame *hello = vec("HELLO");
        const hqq_rpcvec_frame *want = vec("HELLO_REPLY");

        HQQ_REQUIRE(cli_send(&c, hello->data, hello->len) == HQQ_OK);
        HQQ_REQUIRE(cli_recv(&c, f, sizeof f, &n, 5 * SEC) == HQQ_OK);
        RTU_CHECK_BYTES(f, n, want->data, want->len, "HELLO_REPLY over the link");
    }
    /* the session name is unlinked once HELLO was read */
    {
        hqq_options lo;

        hqq_options_init(&lo);
        lo.attach_timeout_ns = 100 * MS;
        st = hqq_standard_open(name, 64, 16384, &lo, &probe, NULL);
        HQQ_CHECK_STATUS(st, HQQ_ERR_NOT_EXIST);
        if (st == HQQ_OK) {
            (void)hqq_close(probe);
        }
    }
    cli_close_ctl(&c);

    for (i = 1; i <= 13; i++) {
        const hqq_rpcvec_frame *rq;
        const hqq_rpcvec_frame *rs;

        snprintf(vn, sizeof vn, "R%d_REQ", i);
        rq = vec(vn);
        snprintf(vn, sizeof vn, "R%d_RESP", i);
        rs = vec(vn);
        HQQ_REQUIRE(cli_send(&c, rq->data, rq->len) == HQQ_OK);
        HQQ_REQUIRE(cli_recv(&c, f, sizeof f, &n, 5 * SEC) == HQQ_OK);
        RTU_CHECK_BYTES(f, n, rs->data, rs->len, vn);
    }
    {
        const hqq_rpcvec_frame *close = vec("CLOSE");
        const hqq_rpcvec_frame *ack = vec("CLOSE_ACK");

        HQQ_REQUIRE(cli_send(&c, close->data, close->len) == HQQ_OK);
        HQQ_REQUIRE(cli_recv(&c, f, sizeof f, &n, 5 * SEC) == HQQ_OK);
        RTU_CHECK_BYTES(f, n, ack->data, ack->len, "CLOSE_ACK over the link");
        /* the server closes its end: the peer is gone for good */
        st = cli_recv(&c, f, sizeof f, &n, 5 * SEC);
        HQQ_CHECK(st == HQQ_ERR_PEER_CLOSED || st == HQQ_ERR_PEER_GONE);
    }
    cli_free(&c);
    HQQ_CHECK_STATUS(srv_stop(&sc), HQQ_OK);
    HQQ_CHECK_EQ_INT(ml_test_live_objects(), 0);
}

/* Fifty requests without reading, then fifty responses in order; five rounds. */
static void test_pipelining(void) {
    srv_ctx sc;
    client c;
    int round;

    srv_start(&sc, &microlib_iface, NULL, "pipe");
    cli_open(&c, sc.name, 0, 0);
    HQQ_CHECK_EQ_U64(cli_hello(&c, &microlib_iface, NULL, NULL), 0);
    for (round = 0; round < 5; round++) {
        uint8_t body[8];
        uint8_t rb[16];
        response r;
        int i;

        r.body = rb;
        for (i = 0; i < 50; i++) {
            add_body(body, i, round * 1000);
            HQQ_REQUIRE(cli_request(&c, 1, body, 8) == HQQ_OK);
        }
        for (i = 0; i < 50; i++) {
            HQQ_REQUIRE(cli_response(&c, &r, sizeof rb, 5 * SEC) == HQQ_OK);
            HQQ_CHECK_EQ_U64(rtu_le32(r.hdr + 4), (uint32_t)(round * 50 + i + 1)); /* FIFO: ids in order */
            HQQ_CHECK_EQ_INT(rtu_le16(r.hdr + 2), 1);
            HQQ_CHECK_EQ_INT((int32_t)rtu_le32(rb), i + round * 1000);
        }
    }
    cli_free(&c);
    HQQ_CHECK_STATUS(srv_stop(&sc), HQQ_OK);
}

/* S = 128: the oversize pair of 4.14 replayed over the link, then larger messages in both
 * directions. */
static void test_oversize_live(void) {
    srv_ctx sc;
    client c;
    uint8_t f[FRAME_BUF];
    size_t n = 0;
    const hqq_rpcvec_frame *q1 = vec("C_REQ_1");
    const hqq_rpcvec_frame *q2 = vec("C_REQ_2");
    const hqq_rpcvec_frame *s1 = vec("C_RESP_1");
    const hqq_rpcvec_frame *s2 = vec("C_RESP_2");

    srv_start(&sc, &microlib_iface, NULL, "big");
    cli_open(&c, sc.name, 64, 128);
    HQQ_CHECK_EQ_INT(c.S, 128);
    HQQ_CHECK_EQ_U64(cli_hello(&c, &microlib_iface, NULL, NULL), 0);
    HQQ_REQUIRE(cli_send(&c, q1->data, q1->len) == HQQ_OK);
    HQQ_REQUIRE(cli_send(&c, q2->data, q2->len) == HQQ_OK);
    HQQ_REQUIRE(cli_recv(&c, f, sizeof f, &n, 5 * SEC) == HQQ_OK);
    RTU_CHECK_BYTES(f, n, s1->data, s1->len, "C_RESP_1 over the link");
    HQQ_REQUIRE(cli_recv(&c, f, sizeof f, &n, 5 * SEC) == HQQ_OK);
    RTU_CHECK_BYTES(f, n, s2->data, s2->len, "C_RESP_2 over the link");
    c.id = 1;

    /* ml_xor over 5000 bytes at S = 128: 45 request frames, a response of 45 frames */
    {
        enum { N = 5000 };
        static uint8_t body[8 + 4 + N];
        static uint8_t rb[8 + 4 + N + 64];
        response r;
        int i;

        r.body = rb;
        body[0] = 0x3C;
        memset(body + 1, 0, 3);
        rtu_put32(body + 4, N);
        rtu_put32(body + 8, N);
        for (i = 0; i < N; i++) {
            body[12 + i] = (uint8_t)(i * 7);
        }
        HQQ_REQUIRE(cli_request(&c, 8, body, sizeof body) == HQQ_OK);
        HQQ_REQUIRE(cli_response(&c, &r, sizeof rb, 5 * SEC) == HQQ_OK);
        HQQ_CHECK_EQ_U64(rtu_le32(r.hdr + 12), 0);
        HQQ_CHECK_EQ_INT(r.len, 8 + 4 + N);
        HQQ_CHECK_EQ_INT(r.frames, (8 + 4 + N + 111) / 112);
        HQQ_CHECK_EQ_U64(rtu_le32(rb), N);
        HQQ_CHECK_EQ_U64(rtu_le32(rb + 8), N);
        for (i = 0; i < N; i++) {
            if (rb[12 + i] != (uint8_t)((uint8_t)(i * 7) ^ 0x3C)) {
                HQQ_CHECK(0);
                break;
            }
        }
    }
    cli_free(&c);
    HQQ_CHECK_STATUS(srv_stop(&sc), HQQ_OK);
}

/* A protocol violation: the final E_PROTOCOL frame, then the link is closed; the handles of the
 * session are destroyed and the server goes on. */
static void test_violation_live(void) {
    srv_ctx sc;
    client c;
    uint8_t f[FRAME_BUF];
    uint8_t body[8];
    size_t n = 0;
    hqq_status st;

    srv_start(&sc, &microlib_iface, NULL, "viol");
    cli_open(&c, sc.name, 0, 0);
    HQQ_CHECK_EQ_U64(cli_hello(&c, &microlib_iface, NULL, NULL), 0);
    rtu_put64(body, 5);
    HQQ_REQUIRE(cli_request(&c, 2, body, 8) == HQQ_OK); /* ml_counter_new(5) */
    {
        uint8_t rb[16];
        response r;

        r.body = rb;
        HQQ_REQUIRE(cli_response(&c, &r, sizeof rb, 5 * SEC) == HQQ_OK);
    }
    HQQ_CHECK_EQ_INT(ml_test_live_objects(), 1);
    rtu_hdr(f, 0x7E, 0, 0, 0, 0, 0);
    HQQ_REQUIRE(cli_send(&c, f, RTU_HDR) == HQQ_OK);
    HQQ_REQUIRE(cli_recv(&c, f, sizeof f, &n, 5 * SEC) == HQQ_OK);
    HQQ_CHECK_EQ_INT(n, 39);
    HQQ_CHECK_EQ_INT(f[0], HQQRPC_K_RESPONSE);
    HQQ_CHECK_EQ_U64(rtu_le32(f + 4), 0); /* a final frame */
    HQQ_CHECK_EQ_U64(rtu_le32(f + 12), HQQRPC_E_PROTOCOL);
    st = cli_recv(&c, f, sizeof f, &n, 5 * SEC);
    HQQ_CHECK(st == HQQ_ERR_PEER_CLOSED || st == HQQ_ERR_PEER_GONE);
    WAIT_FOR(ml_test_live_objects() == 0, 3000);
    HQQ_CHECK_EQ_INT(ml_test_live_objects(), 0);
    cli_free(&c);

    /* the server still serves new sessions */
    cli_open(&c, sc.name, 0, 0);
    HQQ_CHECK_EQ_U64(cli_hello(&c, &microlib_iface, NULL, NULL), 0);
    call_add(&c, 20, 22, 42);
    cli_free(&c);
    HQQ_CHECK_STATUS(srv_stop(&sc), HQQ_OK);
}

/* A digest mismatch is answered and the session ends. */
static void test_digest_mismatch_live(void) {
    srv_ctx sc;
    client c;
    uint8_t f[FRAME_BUF];
    size_t n = 0;
    hqq_status st;
    hqqrpc_iface other = microlib_iface;

    other.digest[5] ^= 0x40;
    srv_start(&sc, &microlib_iface, NULL, "dig");
    cli_open(&c, sc.name, 0, 0);
    HQQ_CHECK_EQ_U64(cli_hello(&c, &other, NULL, NULL), HQQRPC_HELLO_DIGEST_MISMATCH);
    st = cli_recv(&c, f, sizeof f, &n, 5 * SEC);
    HQQ_CHECK(st == HQQ_ERR_PEER_CLOSED || st == HQQ_ERR_PEER_GONE);
    cli_free(&c);
    HQQ_CHECK_STATUS(srv_stop(&sc), HQQ_OK);
}

/* CLOSE: the teardown destroys the handles; CLOSE_ACK counts the failed destroys (an unread
 * cursor reports ML_PARTIAL). */
static void test_close_ack_counts_failures(void) {
    srv_ctx sc;
    client c;
    uint8_t body[8];
    uint8_t rb[16];
    uint8_t f[FRAME_BUF];
    response r;
    size_t n = 0;

    srv_start(&sc, &microlib_iface, NULL, "ack");
    cli_open(&c, sc.name, 0, 0);
    HQQ_CHECK_EQ_U64(cli_hello(&c, &microlib_iface, NULL, NULL), 0);
    r.body = rb;
    rtu_put64(body, 100);
    HQQ_REQUIRE(cli_request(&c, 2, body, 8) == HQQ_OK); /* ml_counter_new -> 0x00010001 */
    HQQ_REQUIRE(cli_response(&c, &r, sizeof rb, 5 * SEC) == HQQ_OK);
    rtu_put32(body, 0x00010001u);
    rtu_put32(body + 4, 1);
    HQQ_REQUIRE(cli_request(&c, 5, body, 8) == HQQ_OK); /* ml_cursor_open -> 0x00010002 */
    HQQ_REQUIRE(cli_response(&c, &r, sizeof rb, 5 * SEC) == HQQ_OK);
    HQQ_CHECK_EQ_INT(ml_test_live_objects(), 2);
    rtu_hdr(f, HQQRPC_K_CLOSE, 0, 0, 0, 0, 0);
    HQQ_REQUIRE(cli_send(&c, f, RTU_HDR) == HQQ_OK);
    HQQ_REQUIRE(cli_recv(&c, f, sizeof f, &n, 5 * SEC) == HQQ_OK);
    HQQ_CHECK_EQ_INT(n, RTU_HDR);
    HQQ_CHECK_EQ_INT(f[0], HQQRPC_K_CLOSE_ACK);
    HQQ_CHECK_EQ_U64(rtu_le32(f + 12), 1); /* the cursor, destroyed first, was not exhausted */
    HQQ_CHECK_EQ_INT(ml_test_live_objects(), 0);
    cli_free(&c);
    HQQ_CHECK_STATUS(srv_stop(&sc), HQQ_OK);
}

/* A client that detaches without CLOSE: its committed requests run, its handles are destroyed and
 * its session slot is free again (max_sessions 1). */
static void test_detach_live(void) {
    srv_ctx sc;
    client c;
    hqqrpc_server_options o;
    uint8_t body[8];
    uint8_t rb[16];
    response r;
    hqq_msg *resp = NULL;
    uint64_t status = 0;
    hqq_adv *ctl2 = NULL;
    hqq_status st = HQQ_ERR_TIMEOUT;

    hqqrpc_server_options_init(&o);
    o.max_sessions = 1;
    srv_start(&sc, &microlib_iface, &o, "det");
    cli_open(&c, sc.name, 0, 0);
    HQQ_CHECK_EQ_U64(cli_hello(&c, &microlib_iface, NULL, NULL), 0);
    r.body = rb;
    rtu_put64(body, 1);
    HQQ_REQUIRE(cli_request(&c, 2, body, 8) == HQQ_OK);
    HQQ_REQUIRE(cli_response(&c, &r, sizeof rb, 5 * SEC) == HQQ_OK);
    HQQ_CHECK_EQ_INT(ml_test_live_objects(), 1);

    /* the table is full: another OPEN is refused with BUSY (code 3) */
    HQQ_REQUIRE(ctl_connect(sc.name, &ctl2) == HQQ_OK);
    st = ctl_open(ctl2, 0, 0, 1, "HQBO", &status, &resp, 5 * SEC);
    HQQ_CHECK_STATUS(st, HQQ_ERR_RESPONSE);
    HQQ_CHECK_EQ_U64(status, HQQRPC_OPEN_STATUS_BASE + HQQRPC_OPEN_E_BUSY);
    hqq_msg_release(resp);

    cli_free(&c); /* detaches */
    WAIT_FOR(ml_test_live_objects() == 0, 3000);
    HQQ_CHECK_EQ_INT(ml_test_live_objects(), 0);
    /* the slot frees up: a new connection gets a session */
    {
        client c2;
        int tries;

        for (tries = 0; tries < 100; tries++) {
            hqq_adv *ctl3 = NULL;

            HQQ_REQUIRE(ctl_connect(sc.name, &ctl3) == HQQ_OK);
            st = ctl_open(ctl3, 0, 0, 1, "HQBO", &status, &resp, 5 * SEC);
            if (st == HQQ_OK) {
                memset(&c2, 0, sizeof c2);
                c2.ctl = ctl3;
                HQQ_REQUIRE(attach(&c2, resp) == HQQ_OK);
                hqq_msg_release(resp);
                HQQ_CHECK_EQ_U64(cli_hello(&c2, &microlib_iface, NULL, NULL), 0);
                call_add(&c2, 1, 2, 3);
                cli_free(&c2);
                break;
            }
            hqq_msg_release(resp);
            (void)hqq_adv_close(ctl3);
            hqq_test_sleep_ms(50);
        }
        HQQ_CHECK(tries < 100);
    }
    (void)hqq_adv_close(ctl2);
    HQQ_CHECK_STATUS(srv_stop(&sc), HQQ_OK);
}

/* ---- Timeouts and limits ---------------------------------------------------------------------- */

/* A client that keeps its heartbeat but never reads: the server blocks on the response ring and,
 * after write_stall_timeout, ends the session without a final frame (5.2). */
static void test_write_stall(void) {
    srv_ctx sc;
    client c;
    hqqrpc_server_options o;
    uint8_t body[8];
    uint8_t f[FRAME_BUF];
    size_t n = 0;
    int sent = 0;
    int got = 0;
    int i;
    hqq_status st;
    uint64_t t0;

    hqqrpc_server_options_init(&o);
    o.write_stall_timeout_ns = 300 * MS;
    srv_start(&sc, &microlib_iface, &o, "stall");
    cli_open(&c, sc.name, 2, 128);
    HQQ_CHECK_EQ_U64(cli_hello(&c, &microlib_iface, NULL, NULL), 0);
    t0 = hqq_test_now_ns();
    for (i = 0; i < 8; i++) {
        add_body(body, i, 1);
        if (cli_request(&c, 1, body, 8) != HQQ_OK) {
            break;
        }
        sent++;
    }
    HQQ_CHECK(sent >= 3);
    /* the first responses were published before the ring filled; then the link closes */
    for (;;) {
        st = cli_recv(&c, f, sizeof f, &n, 5 * SEC);
        if (st != HQQ_OK) {
            break;
        }
        HQQ_CHECK_EQ_INT(f[0], HQQRPC_K_RESPONSE);
        HQQ_CHECK_EQ_U64(rtu_le32(f + 12), 0); /* no E_SHUTTING_DOWN or E_PROTOCOL final frame */
        HQQ_CHECK(rtu_le32(f + 4) != 0);
        got++;
    }
    HQQ_CHECK(st == HQQ_ERR_PEER_CLOSED || st == HQQ_ERR_PEER_GONE);
    HQQ_CHECK(got >= 2 && got <= 3);
    HQQ_CHECK(hqq_test_now_ns() - t0 < 8u * (uint64_t)SEC);
    cli_free(&c);
    HQQ_CHECK_STATUS(srv_stop(&sc), HQQ_OK);
}

/* idle_timeout: requests keep a session alive, silence ends it with E_SHUTTING_DOWN. */
static void test_idle_timeout(void) {
    srv_ctx sc;
    client c;
    hqqrpc_server_options o;
    uint8_t f[FRAME_BUF];
    size_t n = 0;
    hqq_status st;
    int i;

    hqqrpc_server_options_init(&o);
    o.idle_timeout_ns = 400 * MS;
    srv_start(&sc, &microlib_iface, &o, "idle");
    cli_open(&c, sc.name, 0, 0);
    HQQ_CHECK_EQ_U64(cli_hello(&c, &microlib_iface, NULL, NULL), 0);
    for (i = 0; i < 6; i++) { /* 900 ms of activity, each gap shorter than the bound */
        call_add(&c, i, i, 2 * i);
        hqq_test_sleep_ms(150);
    }
    HQQ_REQUIRE(cli_recv(&c, f, sizeof f, &n, 5 * SEC) == HQQ_OK);
    HQQ_CHECK_EQ_INT(n, 16 + 4 + 13 + 1);
    HQQ_CHECK_EQ_INT(f[0], HQQRPC_K_RESPONSE);
    HQQ_CHECK_EQ_U64(rtu_le32(f + 4), 0);
    HQQ_CHECK_EQ_U64(rtu_le32(f + 12), HQQRPC_E_SHUTTING_DOWN);
    st = cli_recv(&c, f, sizeof f, &n, 5 * SEC);
    HQQ_CHECK(st == HQQ_ERR_PEER_CLOSED || st == HQQ_ERR_PEER_GONE);
    cli_free(&c);
    HQQ_CHECK_STATUS(srv_stop(&sc), HQQ_OK);
}

/* An OPEN that is never followed by HELLO: the session ends after open_timeout, its segment name
 * is removed and its slot is free (max_sessions 1). */
static void test_open_without_hello(void) {
    srv_ctx sc;
    client c;
    hqqrpc_server_options o;
    hqq_msg *resp = NULL;
    uint64_t status = 0;
    hqq_adv *ctl2 = NULL;
    hqq_link *probe = NULL;
    hqq_options lo;
    char name[64];
    hqq_status st;

    hqqrpc_server_options_init(&o);
    o.open_timeout_ns = 300 * MS;
    o.max_sessions = 1;
    srv_start(&sc, &microlib_iface, &o, "noh");
    memset(&c, 0, sizeof c);
    HQQ_REQUIRE(ctl_connect(sc.name, &c.ctl) == HQQ_OK);
    HQQ_REQUIRE(ctl_open(c.ctl, 0, 0, 1, "HQBO", &status, &resp, 5 * SEC) == HQQ_OK);
    {
        const uint8_t *d = (const uint8_t *)hqq_msg_data(resp);

        memcpy(name, d + 12, rtu_le16(d + 8));
        name[rtu_le16(d + 8)] = '\0';
    }
    hqq_msg_release(resp);
    hqq_options_init(&lo);
    lo.attach_timeout_ns = 100 * MS;
    hqq_test_sleep_ms(900);
    st = hqq_standard_open(name, 64, 16384, &lo, &probe, NULL);
    HQQ_CHECK_STATUS(st, HQQ_ERR_NOT_EXIST); /* the server closed the link it created: the name is gone */
    if (st == HQQ_OK) {
        (void)hqq_close(probe);
    }
    HQQ_REQUIRE(ctl_connect(sc.name, &ctl2) == HQQ_OK);
    st = ctl_open(ctl2, 0, 0, 1, "HQBO", &status, &resp, 5 * SEC);
    HQQ_CHECK_STATUS(st, HQQ_OK); /* the slot is free again */
    hqq_msg_release(resp);
    (void)hqq_adv_close(ctl2);
    cli_free(&c);
    HQQ_CHECK_STATUS(srv_stop(&sc), HQQ_OK);
}

static void expect_open_error(hqq_adv *ctl, uint32_t B, uint32_t S, uint16_t major, const char *magic,
                              unsigned code, const char *what) {
    hqq_msg *resp = NULL;
    uint64_t status = 0;
    hqq_status st = ctl_open(ctl, B, S, major, magic, &status, &resp, 5 * SEC);

    HQQ_CHECK_STATUS(st, HQQ_ERR_RESPONSE);
    if (status != HQQRPC_OPEN_STATUS_BASE + code) {
        fprintf(stderr, "%s: status 0x%llx, want 0x%llx\n", what, (unsigned long long)status,
                (unsigned long long)(HQQRPC_OPEN_STATUS_BASE + code));
        HQQ_CHECK(0);
    }
    HQQ_CHECK(resp != NULL && hqq_msg_len(resp) <= HQQRPC_OPEN_MSG_MAX);
    hqq_msg_release(resp);
}

/* The bounds of OPEN (4.11): protocol version, geometry, 2 * B * S against max_segment_bytes, and
 * max_control_links. */
static void test_bootstrap_bounds(void) {
    srv_ctx sc;
    hqqrpc_server_options o;
    hqq_adv *ctl = NULL;
    hqq_adv *ctl2 = NULL;
    hqq_adv *ctl3 = NULL;
    hqq_msg *resp = NULL;
    uint64_t status = 0;
    hqq_status st;
    int tries;

    hqqrpc_server_options_init(&o);
    o.default_buffer_count = 2;
    o.default_buffer_size = 1024;
    o.max_segment_bytes = 65536;
    o.max_control_links = 2;
    o.open_timeout_ns = 1500 * MS;
    srv_start(&sc, &microlib_iface, &o, "bnd");
    HQQ_REQUIRE(ctl_connect(sc.name, &ctl) == HQQ_OK);

    expect_open_error(ctl, 64, 16384, 1, "HQBO", HQQRPC_OPEN_E_GEOMETRY, "2*B*S above max_segment_bytes");
    expect_open_error(ctl, 4, 16384, 1, "HQBO", HQQRPC_OPEN_E_GEOMETRY, "2*B*S = 128 KiB");
    expect_open_error(ctl, 3, 128, 1, "HQBO", HQQRPC_OPEN_E_GEOMETRY, "B not a power of two");
    expect_open_error(ctl, 1, 128, 1, "HQBO", HQQRPC_OPEN_E_GEOMETRY, "B below 2");
    expect_open_error(ctl, 8192, 128, 1, "HQBO", HQQRPC_OPEN_E_GEOMETRY, "B above max_buffer_count");
    expect_open_error(ctl, 2, 100, 1, "HQBO", HQQRPC_OPEN_E_GEOMETRY, "S not a multiple of 64");
    expect_open_error(ctl, 2, 64, 1, "HQBO", HQQRPC_OPEN_E_GEOMETRY, "S below MIN_S");
    expect_open_error(ctl, 2, (1u << 20) + 64u, 1, "HQBO", HQQRPC_OPEN_E_GEOMETRY, "S above max_buffer_size");
    expect_open_error(ctl, 0xFFFFFFFFu, 0xFFFFFFFFu, 1, "HQBO", HQQRPC_OPEN_E_GEOMETRY, "no 32-bit overflow");
    expect_open_error(ctl, 2, 128, 2, "HQBO", HQQRPC_OPEN_E_VERSION, "major version 2");
    expect_open_error(ctl, 2, 128, 1, "HQBX", HQQRPC_OPEN_E_INTERNAL, "bad magic");
    /* 2 * 2 * 16384 = 64 KiB is exactly the limit */
    st = ctl_open(ctl, 2, 16384, 1, "HQBO", &status, &resp, 5 * SEC);
    HQQ_CHECK_STATUS(st, HQQ_OK);
    if (resp != NULL) {
        const uint8_t *d = (const uint8_t *)hqq_msg_data(resp);

        HQQ_CHECK_EQ_U64(rtu_le32(d), 2);
        HQQ_CHECK_EQ_U64(rtu_le32(d + 4), 16384);
        hqq_msg_release(resp);
    }
    /* a control link serves one OPEN */
    expect_open_error(ctl, 2, 128, 1, "HQBO", HQQRPC_OPEN_E_INTERNAL, "second OPEN on one control link");

    /* max_control_links = 2: the third link is closed at once */
    HQQ_REQUIRE(ctl_connect(sc.name, &ctl2) == HQQ_OK);
    HQQ_REQUIRE(ctl_connect(sc.name, &ctl3) == HQQ_OK);
    st = ctl_open(ctl3, 0, 0, 1, "HQBO", &status, &resp, 1 * SEC);
    HQQ_CHECK(st != HQQ_OK);
    if (resp != NULL) {
        hqq_msg_release(resp);
    }
    (void)hqq_adv_close(ctl3);
    (void)hqq_adv_close(ctl2);
    (void)hqq_adv_close(ctl);
    for (tries = 0; tries < 100; tries++) { /* the capacity returns when the server saw the closes */
        HQQ_REQUIRE(ctl_connect(sc.name, &ctl) == HQQ_OK);
        st = ctl_open(ctl, 0, 0, 1, "HQBO", &status, &resp, 5 * SEC);
        if (resp != NULL) {
            hqq_msg_release(resp);
        }
        (void)hqq_adv_close(ctl);
        if (st == HQQ_OK) {
            break;
        }
        hqq_test_sleep_ms(50);
    }
    HQQ_CHECK(tries < 100);
    HQQ_CHECK_STATUS(srv_stop(&sc), HQQ_OK);
}

/* ---- Stop and shutdown ---------------------------------------------------------------------------- */

/* A stop ends idle sessions with E_SHUTTING_DOWN and run returns HQQ_OK; destroy is refused while
 * run is active and works after. */
static void test_stop_with_sessions(void) {
    srv_ctx sc;
    client c[2];
    uint8_t f[FRAME_BUF];
    size_t n = 0;
    hqq_status st;
    uint64_t t0;
    int i;

    srv_start(&sc, &microlib_iface, NULL, "stop");
    for (i = 0; i < 2; i++) {
        cli_open(&c[i], sc.name, 0, 0);
        HQQ_CHECK_EQ_U64(cli_hello(&c[i], &microlib_iface, NULL, NULL), 0);
        call_add(&c[i], 1, 1, 2);
    }
    HQQ_CHECK_STATUS(hqqrpc_server_destroy(sc.srv), HQQ_ERR_INVALID_OP); /* run is active: frees nothing */
    t0 = hqq_test_now_ns();
    HQQ_CHECK_STATUS(srv_stop(&sc), HQQ_OK);
    HQQ_CHECK(hqq_test_now_ns() - t0 < 3u * (uint64_t)SEC);
    for (i = 0; i < 2; i++) {
        HQQ_REQUIRE(cli_recv(&c[i], f, sizeof f, &n, 5 * SEC) == HQQ_OK);
        HQQ_CHECK_EQ_INT(f[0], HQQRPC_K_RESPONSE);
        HQQ_CHECK_EQ_U64(rtu_le32(f + 4), 0);
        HQQ_CHECK_EQ_U64(rtu_le32(f + 12), HQQRPC_E_SHUTTING_DOWN);
        st = cli_recv(&c[i], f, sizeof f, &n, 5 * SEC);
        HQQ_CHECK(st == HQQ_ERR_PEER_CLOSED || st == HQQ_ERR_PEER_GONE);
        cli_free(&c[i]);
    }
}

static void test_create_errors(void) {
    srv_ctx sc;
    hqqrpc_server *second = NULL;
    hqqrpc_server_options o;
    hqqrpc_server *srv = NULL;
    char name[40];

    srv_start(&sc, &microlib_iface, NULL, "dup");
    HQQ_CHECK_STATUS(hqqrpc_server_create(&microlib_iface, sc.name, NULL, &second), HQQ_ERR_LISTENER_BUSY);
    HQQ_CHECK(second == NULL);
    HQQ_CHECK_STATUS(srv_stop(&sc), HQQ_OK);

    hqq_test_unique_name(name, sizeof name, "arg");
    HQQ_CHECK_STATUS(hqqrpc_server_create(NULL, name, NULL, &srv), HQQ_ERR_INVALID_ARGUMENT);
    HQQ_CHECK_STATUS(hqqrpc_server_create(&microlib_iface, "", NULL, &srv), HQQ_ERR_INVALID_ARGUMENT);
    HQQ_CHECK_STATUS(hqqrpc_server_create(&microlib_iface, name, NULL, NULL), HQQ_ERR_INVALID_ARGUMENT);
    hqqrpc_server_options_init(&o);
    o.max_handles = 70000;
    HQQ_CHECK_STATUS(hqqrpc_server_create(&microlib_iface, name, &o, &srv), HQQ_ERR_INVALID_ARGUMENT);
    hqqrpc_server_options_init(&o);
    o.max_message = 100;
    HQQ_CHECK_STATUS(hqqrpc_server_create(&microlib_iface, name, &o, &srv), HQQ_ERR_INVALID_ARGUMENT);
    hqqrpc_server_options_init(&o);
    o.default_buffer_count = 3;
    HQQ_CHECK_STATUS(hqqrpc_server_create(&microlib_iface, name, &o, &srv), HQQ_ERR_INVALID_ARGUMENT);
    hqqrpc_server_options_init(&o);
    o.struct_size = 8;
    HQQ_CHECK_STATUS(hqqrpc_server_create(&microlib_iface, name, &o, &srv), HQQ_ERR_INVALID_ARGUMENT);
    HQQ_CHECK(srv == NULL);

    /* a server that never ran is destroyed without a run */
    hqqrpc_server_options_init(&o);
    HQQ_CHECK_STATUS(hqqrpc_server_create(&microlib_iface, name, &o, &srv), HQQ_OK);
    HQQ_CHECK_STATUS(hqqrpc_server_destroy(srv), HQQ_OK);
    /* and its name can be used again */
    HQQ_CHECK_STATUS(hqqrpc_server_create(&microlib_iface, name, &o, &srv), HQQ_OK);
    HQQ_CHECK_STATUS(hqqrpc_server_destroy(srv), HQQ_OK);
    /* stop before run: run returns at once */
    HQQ_CHECK_STATUS(hqqrpc_server_create(&microlib_iface, name, &o, &srv), HQQ_OK);
    hqqrpc_server_stop(srv);
    hqqrpc_server_stop(srv); /* idempotent */
    HQQ_CHECK_STATUS(hqqrpc_server_run(srv), HQQ_OK);
    HQQ_CHECK_STATUS(hqqrpc_server_run(srv), HQQ_ERR_INVALID_OP); /* a server runs once */
    HQQ_CHECK_STATUS(hqqrpc_server_destroy(srv), HQQ_OK);
}

/* ---- serialize (8.1) ------------------------------------------------------------------------------ */

typedef struct gate {
    _Atomic int calls;
    _Atomic int release;
} gate;

/* Blocks the first caller inside ml_add until released. */
static void gate_probe(void *ctx) {
    gate *g = (gate *)ctx;

    if (atomic_fetch_add(&g->calls, 1) == 0) {
        while (!atomic_load(&g->release)) {
            hqq_test_sleep_ms(1);
        }
    }
}

static void serialize_case(const hqqrpc_iface *iface, int expect_overlap, const char *tag) {
    srv_ctx sc;
    client a;
    client b;
    gate g;
    uint8_t body[8];
    uint8_t rb[16];
    response r;
    hqq_status st;

    atomic_init(&g.calls, 0);
    atomic_init(&g.release, 0);
    srv_start(&sc, iface, NULL, tag);
    cli_open(&a, sc.name, 0, 0);
    cli_open(&b, sc.name, 0, 0);
    HQQ_CHECK_EQ_U64(cli_hello(&a, iface, NULL, NULL), 0);
    HQQ_CHECK_EQ_U64(cli_hello(&b, iface, NULL, NULL), 0);
    ml_test_reset();
    ml_test_set_probe(gate_probe, &g);
    r.body = rb;
    add_body(body, 1, 2);
    HQQ_REQUIRE(cli_request(&a, 1, body, 8) == HQQ_OK);
    WAIT_FOR(atomic_load(&g.calls) == 1, 5000);
    HQQ_CHECK_EQ_INT(atomic_load(&g.calls), 1); /* A is inside the library */
    add_body(body, 3, 4);
    HQQ_REQUIRE(cli_request(&b, 1, body, 8) == HQQ_OK);
    st = cli_response(&b, &r, sizeof rb, 400 * MS);
    if (expect_overlap) { /* serialize(none): B runs while A is still inside */
        HQQ_CHECK_STATUS(st, HQQ_OK);
        HQQ_CHECK_EQ_INT((int32_t)rtu_le32(rb), 7);
        HQQ_CHECK_EQ_INT(ml_test_max_overlap(), 2);
    } else { /* serialize(session): B waits for the execution lock */
        HQQ_CHECK_STATUS(st, HQQ_ERR_TIMEOUT);
        HQQ_CHECK_EQ_INT(atomic_load(&g.calls), 1);
    }
    atomic_store(&g.release, 1);
    HQQ_REQUIRE(cli_response(&a, &r, sizeof rb, 5 * SEC) == HQQ_OK);
    HQQ_CHECK_EQ_INT((int32_t)rtu_le32(rb), 3);
    if (!expect_overlap) {
        HQQ_REQUIRE(cli_response(&b, &r, sizeof rb, 5 * SEC) == HQQ_OK);
        HQQ_CHECK_EQ_INT((int32_t)rtu_le32(rb), 7);
        HQQ_CHECK_EQ_INT(ml_test_max_overlap(), 1); /* never two sessions inside the library */
    }
    ml_test_set_probe(NULL, NULL);
    cli_free(&a);
    cli_free(&b);
    HQQ_CHECK_STATUS(srv_stop(&sc), HQQ_OK);
}

static void test_serialize(void) {
    hqqrpc_iface none = microlib_iface;

    none.serialize = HQQRPC_SERIALIZE_NONE;
    serialize_case(&microlib_iface, 0, "ser");
    serialize_case(&none, 1, "non");
}

/* ---- Peer death and a stuck call ---------------------------------------------------------------------- */

#if !defined(_WIN32)
static const char *g_argv0;

/* The client of the death test: opens a session, creates two handles and waits to be killed. */
static int death_child(const char *listener) {
    client c;
    uint8_t body[8];
    uint8_t rb[16];
    response r;

    g_client_heartbeat_ns = 50 * MS;
    cli_open(&c, listener, 0, 0);
    if (cli_hello(&c, &microlib_iface, NULL, NULL) != 0) {
        return 3;
    }
    r.body = rb;
    rtu_put64(body, 1);
    if (cli_request(&c, 2, body, 8) != HQQ_OK || cli_response(&c, &r, sizeof rb, 5 * SEC) != HQQ_OK) {
        return 4;
    }
    rtu_put32(body, 0x00010001u);
    rtu_put32(body + 4, 1);
    if (cli_request(&c, 5, body, 8) != HQQ_OK || cli_response(&c, &r, sizeof rb, 5 * SEC) != HQQ_OK) {
        return 5;
    }
    for (;;) {
        sleep(1);
    }
}

/* The client process is killed with SIGKILL while it holds handles: no CLOSE, no detach. The
 * server sees the heartbeat stop (HQQ_ERR_PEER_GONE) and runs the teardown. */
static void test_peer_death(void) {
    srv_ctx sc;
    hqqrpc_server_options o;
    char *args[4];
    pid_t pid = 0;
    int rc;
    int wst = 0;

    hqqrpc_server_options_init(&o);
    o.link.heartbeat_interval_ns = 50 * MS;
    o.link.peer_timeout_ns = 400 * MS;
    srv_start(&sc, &microlib_iface, &o, "dead");
    args[0] = (char *)g_argv0;
    args[1] = (char *)"--death-child";
    args[2] = sc.name;
    args[3] = NULL;
    rc = posix_spawn(&pid, g_argv0, NULL, NULL, args, environ);
    HQQ_REQUIRE(rc == 0);
    WAIT_FOR(ml_test_live_objects() == 2, 20000);
    HQQ_CHECK_EQ_INT(ml_test_live_objects(), 2);
    HQQ_REQUIRE(kill(pid, SIGKILL) == 0);
    HQQ_REQUIRE(waitpid(pid, &wst, 0) == pid);
    HQQ_CHECK(WIFSIGNALED(wst));
    WAIT_FOR(ml_test_live_objects() == 0, 15000);
    HQQ_CHECK_EQ_INT(ml_test_live_objects(), 0);
    HQQ_CHECK_STATUS(srv_stop(&sc), HQQ_OK);
}
#endif

static srv_ctx g_stuck; /* kept reachable: a server whose run timed out is never freed */

/* A call that never returns: stop, run returns HQQ_ERR_TIMEOUT after shutdown_timeout, the thread
 * stays detached and destroy refuses (5.2). The test releases the call afterwards so that the thread
 * ends; the server itself is not freed. */
static void test_stuck_call_shutdown(void) {
    hqqrpc_server_options o;
    client c;
    gate g;
    uint8_t body[8];
    uint8_t f[FRAME_BUF];
    size_t n = 0;
    uint64_t t0;
    hqq_status st;

    atomic_init(&g.calls, 0);
    atomic_init(&g.release, 0);
    hqqrpc_server_options_init(&o);
    o.shutdown_timeout_ns = 300 * MS;
    srv_start(&g_stuck, &microlib_iface, &o, "stuck");
    cli_open(&c, g_stuck.name, 0, 0);
    HQQ_CHECK_EQ_U64(cli_hello(&c, &microlib_iface, NULL, NULL), 0);
    ml_test_set_probe(gate_probe, &g);
    add_body(body, 5, 6);
    HQQ_REQUIRE(cli_request(&c, 1, body, 8) == HQQ_OK);
    WAIT_FOR(atomic_load(&g.calls) == 1, 5000);
    HQQ_CHECK_EQ_INT(atomic_load(&g.calls), 1);

    t0 = hqq_test_now_ns();
    hqqrpc_server_stop(g_stuck.srv);
    hqq_test_thread_join(g_stuck.thr);
    HQQ_CHECK_STATUS(g_stuck.run_st, HQQ_ERR_TIMEOUT);
    HQQ_CHECK(hqq_test_now_ns() - t0 >= 250u * (uint64_t)MS);
    HQQ_CHECK(hqq_test_now_ns() - t0 < 5u * (uint64_t)SEC);
    HQQ_CHECK_STATUS(hqqrpc_server_destroy(g_stuck.srv), HQQ_ERR_INVALID_OP); /* frees nothing */
    HQQ_CHECK_STATUS(hqqrpc_server_run(g_stuck.srv), HQQ_ERR_INVALID_OP);

    atomic_store(&g.release, 1); /* the call returns; the detached session thread winds down */
    for (;;) {
        st = cli_recv(&c, f, sizeof f, &n, 5 * SEC);
        if (st != HQQ_OK) {
            break;
        }
    }
    HQQ_CHECK(st == HQQ_ERR_PEER_CLOSED || st == HQQ_ERR_PEER_GONE);
    ml_test_set_probe(NULL, NULL);
    cli_free(&c);
}

int main(int argc, char **argv) {
#if !defined(_WIN32)
    g_argv0 = argv[0];
    if (argc == 3 && strcmp(argv[1], "--death-child") == 0) {
        return death_child(argv[2]);
    }
#else
    (void)argc;
    (void)argv;
#endif
    test_vector_session_live();
    test_pipelining();
    test_oversize_live();
    test_violation_live();
    test_digest_mismatch_live();
    test_close_ack_counts_failures();
    test_detach_live();
    test_write_stall();
    test_idle_timeout();
    test_open_without_hello();
    test_bootstrap_bounds();
    test_stop_with_sessions();
    test_create_errors();
    test_serialize();
#if !defined(_WIN32)
    test_peer_death();
#endif
    test_stuck_call_shutdown();
    return HQQ_TEST_RESULT("rpc_live");
}
