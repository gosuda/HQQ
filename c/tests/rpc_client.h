/*
 * rpc_client.h - a raw-frame client of the hqq-bindgen C runtime for the live tests: the listener
 * connect and BG_OPEN call on the control link (BINDGEN.md 4.11), the Standard link to the session
 * segment, and frame-level helpers (chunked requests, assembled responses).
 */
#ifndef RPC_CLIENT_H
#define RPC_CLIENT_H

#include <stdint.h>
#include <string.h>

#include "hqq/hqq_rpc.h"
#include "hqq_test.h"
#include "hqq_testutil.h"
#include "rpc_test_util.h"

#define FRAME_BUF 17000u
#define SEC INT64_C(1000000000)
#define MS INT64_C(1000000)

#define WAIT_FOR(cond, ms)                                                              \
    do {                                                                                \
        uint64_t wf_deadline_ = hqq_test_now_ns() + (uint64_t)(ms) * UINT64_C(1000000); \
        while (!(cond) && hqq_test_now_ns() < wf_deadline_) {                           \
            hqq_test_sleep_ms(5);                                                       \
        }                                                                               \
    } while (0)


/* ---- Client side with raw frames ------------------------------------------------------------- */

typedef struct client {
    hqq_adv *ctl;
    hqq_link *link;
    uint32_t B;
    uint32_t S;
    char name[64];
    uint32_t id;
} client;

static inline hqq_status ctl_connect(const char *listener, hqq_adv **out) {
    hqq_connect_options co;
    hqq_wait w;

    hqq_connect_options_init(&co);
    co.buffer_count = 4;
    co.buffer_size = 4096;
    co.features = HQQ_FEATURE_REQUEST_RESPONSE;
    w.timeout_ns = 5 * SEC;
    w.cancel = NULL;
    return hqq_connect(listener, &co, &w, out);
}

/* BG_OPEN. Returns HQQ_OK (resp is the OPEN response) or HQQ_ERR_RESPONSE (*status is 0x50000 + code). */
static inline hqq_status ctl_open(hqq_adv *ctl, uint32_t B, uint32_t S, uint16_t major, const char *magic,
                                  uint64_t *status, hqq_msg **resp, int64_t timeout_ns) {
    uint8_t req[16];
    hqq_wait w;
    hqq_status st;

    memcpy(req, magic, 4);
    req[4] = (uint8_t)major;
    req[5] = 0;
    req[6] = 0;
    req[7] = 0;
    rtu_put32(req + 8, B);
    rtu_put32(req + 12, S);
    w.timeout_ns = timeout_ns;
    w.cancel = NULL;
    *resp = NULL;
    st = hqq_stream_call(hqq_adv_stream0(ctl), HQQRPC_OPEN_METHOD, req, sizeof req, resp, &w);
    *status = *resp != NULL ? hqq_msg_status(*resp) : 0;
    return st;
}

/* Heartbeat interval of the session link of the client; 0: the library default. */
static int64_t g_client_heartbeat_ns;

/* Opens the Standard link named in an OPEN response. */
static inline hqq_status attach(client *c, const hqq_msg *resp) {
    const uint8_t *d = (const uint8_t *)hqq_msg_data(resp);
    size_t len = hqq_msg_len(resp);
    size_t nl;
    hqq_options lo;
    uint64_t deadline = hqq_test_now_ns() + 5u * (uint64_t)SEC;
    hqq_status st = HQQ_ERR_INVALID_SIZE;

    HQQ_REQUIRE(len >= 12);
    c->B = rtu_le32(d);
    c->S = rtu_le32(d + 4);
    nl = rtu_le16(d + 8);
    HQQ_REQUIRE(nl >= 1 && nl <= 64 && len == 12 + nl && rtu_le16(d + 10) == 0);
    memcpy(c->name, d + 12, nl);
    c->name[nl] = '\0';
    hqq_options_init(&lo);
    lo.peer_timeout_ns = 2 * SEC;
    if (g_client_heartbeat_ns != 0) {
        lo.heartbeat_interval_ns = g_client_heartbeat_ns;
    }
    do {
        st = hqq_standard_open(c->name, c->B, c->S, &lo, &c->link, NULL);
        if (st == HQQ_OK) {
            return st;
        }
        hqq_test_sleep_ms(2);
    } while (hqq_error_is_transient_open(st) && hqq_test_now_ns() < deadline);
    return st;
}

/* Connects, opens a session with the given geometry (0: the server default) and attaches. */
static inline void cli_open(client *c, const char *listener, uint32_t B, uint32_t S) {
    hqq_msg *resp = NULL;
    uint64_t status = 0;

    memset(c, 0, sizeof *c);
    HQQ_REQUIRE(ctl_connect(listener, &c->ctl) == HQQ_OK);
    HQQ_REQUIRE(ctl_open(c->ctl, B, S, 1, "HQBO", &status, &resp, 5 * SEC) == HQQ_OK);
    HQQ_REQUIRE(attach(c, resp) == HQQ_OK);
    hqq_msg_release(resp);
}

static inline void cli_close_ctl(client *c) {
    if (c->ctl != NULL) {
        HQQ_CHECK_STATUS(hqq_adv_close(c->ctl), HQQ_OK);
        c->ctl = NULL;
    }
}

static inline void cli_free(client *c) {
    cli_close_ctl(c);
    if (c->link != NULL) {
        HQQ_CHECK_STATUS(hqq_close(c->link), HQQ_OK);
        c->link = NULL;
    }
}

static inline hqq_status cli_send(client *c, const uint8_t *f, size_t n) {
    hqq_wait w;

    w.timeout_ns = 3 * SEC;
    w.cancel = NULL;
    return hqq_write(c->link, f, n, &w);
}

static inline hqq_status cli_recv(client *c, uint8_t *buf, size_t cap, size_t *len, int64_t timeout_ns) {
    hqq_wait w;

    w.timeout_ns = timeout_ns;
    w.cancel = NULL;
    return hqq_read(c->link, buf, cap, len, &w);
}

/* HELLO with the microlib digest and the geometry of the session. Returns the reply's aux. */
static inline uint32_t cli_hello(client *c, const hqqrpc_iface *iface, uint32_t *granted_mif, uint32_t *granted_msg) {
    uint8_t f[RTU_HDR + 64];
    uint8_t r[FRAME_BUF];
    size_t n = 0;

    HQQ_REQUIRE(cli_send(c, f, rtu_hello(f, iface, c->S, c->B, 0, 0)) == HQQ_OK);
    HQQ_REQUIRE(cli_recv(c, r, sizeof r, &n, 5 * SEC) == HQQ_OK);
    HQQ_REQUIRE(n == RTU_HDR + 64 && r[0] == HQQRPC_K_HELLO_REPLY);
    if (granted_mif != NULL) {
        *granted_mif = rtu_le32(r + RTU_HDR + 12);
    }
    if (granted_msg != NULL) {
        *granted_msg = rtu_le32(r + RTU_HDR + 16);
    }
    return rtu_le32(r + 12);
}

/* A request message of any length: the first frame and CONT frames of 4.8. */
static inline hqq_status cli_request(client *c, unsigned op, const uint8_t *body, size_t n) {
    uint8_t f[FRAME_BUF];
    size_t chunk = c->S - RTU_HDR;
    size_t sent = 0;
    uint32_t id = ++c->id;

    do {
        size_t k = n - sent < chunk ? n - sent : chunk;
        hqq_status st;

        if (sent == 0) {
            rtu_hdr(f, HQQRPC_K_REQUEST, 0, op, id, (uint32_t)n, 0);
        } else {
            rtu_hdr(f, HQQRPC_K_CONT, 0, 0, id, (uint32_t)n, (uint32_t)sent);
        }
        if (k != 0) {
            memcpy(f + RTU_HDR, body + sent, k);
        }
        st = cli_send(c, f, RTU_HDR + k);
        if (st != HQQ_OK) {
            return st;
        }
        sent += k;
    } while (sent < n);
    return HQQ_OK;
}

typedef struct response {
    uint8_t hdr[RTU_HDR];
    uint8_t *body;
    size_t len;
    unsigned frames;
} response;

/* Reads one response message (frames of 4.8) into r->body, which has `cap` bytes. */
static inline hqq_status cli_response(client *c, response *r, size_t cap, int64_t timeout_ns) {
    uint8_t f[FRAME_BUF];
    size_t n = 0;
    uint32_t total;
    size_t got;
    hqq_status st = cli_recv(c, f, sizeof f, &n, timeout_ns);

    if (st != HQQ_OK) {
        return st;
    }
    HQQ_REQUIRE(n >= RTU_HDR);
    memcpy(r->hdr, f, RTU_HDR);
    total = rtu_le32(f + 8);
    HQQ_REQUIRE(total <= cap);
    got = n - RTU_HDR;
    memcpy(r->body, f + RTU_HDR, got);
    r->frames = 1;
    while (got < total) {
        st = cli_recv(c, f, sizeof f, &n, timeout_ns);
        if (st != HQQ_OK) {
            return st;
        }
        HQQ_REQUIRE(n > RTU_HDR && f[0] == HQQRPC_K_CONT && rtu_le32(f + 12) == got);
        memcpy(r->body + got, f + RTU_HDR, n - RTU_HDR);
        got += n - RTU_HDR;
        r->frames++;
    }
    HQQ_REQUIRE(got == total);
    r->len = got;
    return HQQ_OK;
}

static inline void add_body(uint8_t *b, int32_t x, int32_t y) {
    rtu_put32(b, (uint32_t)x);
    rtu_put32(b + 4, (uint32_t)y);
}

/* A reply to a request: ml_add. */
static inline void call_add(client *c, int32_t x, int32_t y, int32_t want) {
    uint8_t b[8];
    uint8_t body[16];
    response r;

    r.body = body;
    add_body(b, x, y);
    HQQ_REQUIRE(cli_request(c, 1, b, 8) == HQQ_OK);
    HQQ_REQUIRE(cli_response(c, &r, sizeof body, 5 * SEC) == HQQ_OK);
    HQQ_CHECK_EQ_U64(rtu_le32(r.hdr + 12), 0);
    HQQ_CHECK_EQ_INT(r.len, 4);
    HQQ_CHECK_EQ_INT((int32_t)rtu_le32(body), want);
}

#endif /* RPC_CLIENT_H */
