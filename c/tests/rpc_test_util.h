/* Helpers shared by the tests of the hqq-bindgen C runtime: frame builders and comparers. */
#ifndef RPC_TEST_UTIL_H
#define RPC_TEST_UTIL_H

#include <stdint.h>
#include <stdio.h>
#include <string.h>

#include "hqq/hqq_rpc.h"
#include "hqq_rpc_internal.h"
#include "hqq_test.h"

#define RTU_HDR 16u

static inline uint32_t rtu_le32(const uint8_t *p) {
    return (uint32_t)p[0] | ((uint32_t)p[1] << 8) | ((uint32_t)p[2] << 16) | ((uint32_t)p[3] << 24);
}

static inline uint16_t rtu_le16(const uint8_t *p) {
    return (uint16_t)(p[0] | (p[1] << 8));
}

static inline void rtu_put32(uint8_t *p, uint32_t v) {
    p[0] = (uint8_t)v;
    p[1] = (uint8_t)(v >> 8);
    p[2] = (uint8_t)(v >> 16);
    p[3] = (uint8_t)(v >> 24);
}

static inline void rtu_put64(uint8_t *p, uint64_t v) {
    rtu_put32(p, (uint32_t)v);
    rtu_put32(p + 4, (uint32_t)(v >> 32));
}

static inline void rtu_hdr(uint8_t *f, unsigned kind, unsigned flags, unsigned op, uint32_t id, uint32_t total,
                           uint32_t aux) {
    f[0] = (uint8_t)kind;
    f[1] = (uint8_t)flags;
    f[2] = (uint8_t)op;
    f[3] = (uint8_t)(op >> 8);
    rtu_put32(f + 4, id);
    rtu_put32(f + 8, total);
    rtu_put32(f + 12, aux);
}

/* A single-frame REQUEST. Returns the frame length. */
static inline size_t rtu_req(uint8_t *f, unsigned op, uint32_t id, const void *body, size_t n) {
    rtu_hdr(f, HQQRPC_K_REQUEST, 0, op, id, (uint32_t)n, 0);
    if (n != 0) {
        memcpy(f + RTU_HDR, body, n);
    }
    return RTU_HDR + n;
}

static inline void rtu_dump(const char *label, const uint8_t *p, size_t n) {
    size_t i;

    fprintf(stderr, "  %s (%zu bytes):", label, n);
    for (i = 0; i < n && i < 96; i++) {
        fprintf(stderr, "%s%02x", i % 16 == 0 ? "\n    " : " ", p[i]);
    }
    fprintf(stderr, "%s\n", n > 96 ? "\n    ..." : "");
}

/* Byte-exact comparison with a readable report. */
#define RTU_CHECK_BYTES(got, got_n, want, want_n, what)                                       \
    do {                                                                                      \
        const uint8_t *rtu_g_ = (got);                                                        \
        const uint8_t *rtu_w_ = (want);                                                       \
        size_t rtu_gn_ = (got_n);                                                             \
        size_t rtu_wn_ = (want_n);                                                            \
        int rtu_ok_ = rtu_gn_ == rtu_wn_ && (rtu_gn_ == 0 || memcmp(rtu_g_, rtu_w_, rtu_gn_) == 0); \
        HQQ_CHECK(rtu_ok_);                                                                   \
        if (!rtu_ok_) {                                                                       \
            fprintf(stderr, "%s:%d: bytes differ: %s\n", __FILE__, __LINE__, (what));         \
            rtu_dump("got", rtu_g_, rtu_gn_);                                                 \
            rtu_dump("want", rtu_w_, rtu_wn_);                                                \
        }                                                                                     \
    } while (0)

/* HELLO frame with the digest of iface, geometry S x B and the requested limits. */
static inline size_t rtu_hello(uint8_t *f, const hqqrpc_iface *iface, uint32_t S, uint32_t B, uint32_t mif,
                               uint32_t mmsg) {
    uint8_t *b = f + RTU_HDR;

    rtu_hdr(f, HQQRPC_K_HELLO, 0, 0, 0, 64, 0);
    memset(b, 0, 64);
    memcpy(b, "HQBG", 4);
    b[4] = 1; /* proto major */
    rtu_put32(b + 12, S);
    rtu_put32(b + 16, B);
    rtu_put32(b + 20, mif);
    rtu_put32(b + 24, mmsg);
    memcpy(b + 32, iface->digest, 32);
    return RTU_HDR + 64;
}

/* Exchanges a frame and returns the status. */
static inline hqqrpc_status rtu_x(hqqrpc_session *s, const uint8_t *f, size_t n, uint8_t *out, size_t cap,
                                  size_t *out_len) {
    return hqqrpc_test_exchange(s, f, n, out, cap, out_len);
}

/* A new test session with a completed handshake (the reply is checked to have aux 0). */
static inline hqqrpc_session *rtu_open(const hqqrpc_iface *iface, const hqqrpc_server_options *opts) {
    uint8_t f[RTU_HDR + 64];
    uint8_t out[256];
    size_t out_len = 0;
    hqqrpc_session *s = hqqrpc_test_session_new(iface, opts);
    hqqrpc_server_options def;
    uint32_t S;
    uint32_t B;

    HQQ_REQUIRE(s != NULL);
    if (opts == NULL) {
        hqqrpc_server_options_init(&def);
        opts = &def;
    }
    S = opts->default_buffer_size != 0 ? opts->default_buffer_size : 16384u;
    B = opts->default_buffer_count != 0 ? opts->default_buffer_count : 64u;
    HQQ_REQUIRE(rtu_x(s, f, rtu_hello(f, iface, S, B, 0, 0), out, sizeof out, &out_len) == HQQRPC_OK);
    HQQ_REQUIRE(out_len == RTU_HDR + 64 && out[0] == HQQRPC_K_HELLO_REPLY && rtu_le32(out + 12) == 0);
    return s;
}

/* Options with a given geometry and message limit (everything else default). */
static inline void rtu_opts(hqqrpc_server_options *o, uint32_t S, uint32_t B, uint32_t max_message) {
    hqqrpc_server_options_init(o);
    o->default_buffer_size = S;
    o->default_buffer_count = B;
    o->max_message = max_message;
}

#endif /* RPC_TEST_UTIL_H */
