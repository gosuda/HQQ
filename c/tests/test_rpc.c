/*
 * test_rpc: the wire behavior of the C runtime without a link (hqqrpc_test_exchange).
 *
 *  - the golden vectors of BINDGEN.md 4.14, byte for byte, through the microlib glue of 9.6;
 *  - the handshake of 4.11, statuses and granted limits;
 *  - the frame rules of 4.4 and the assembly rules of 4.8, one violation at a time;
 *  - request ids (4.12), unknown opcodes (4.10);
 *  - every decoder of hqqrpc_rd_* at its boundaries and every writer of hqqrpc_wr_*.
 */
#include <stdint.h>
#include <string.h>

#include "hqq_test.h"
#include "rpc_ml.h"
#include "rpc_ml_server.h"
#include "rpc_test_util.h"
#include "rpc_vectors.h"

#define OUTCAP 70000u

static uint8_t g_out[OUTCAP];

/* The vector tables carry sqlite digests this test does not use. */
static void use_unused_vectors(void) {
    (void)hqq_rpcvec_sqlite_lowlevel_digest;
    (void)hqq_rpcvec_sqlite_lowlevel_canonical;
    (void)hqq_rpcvec_microlib_canonical;
}

static const hqq_rpcvec_frame *find(const hqq_rpcvec_frame *t, size_t n, const char *name) {
    size_t i;

    for (i = 0; i < n; i++) {
        if (strcmp(t[i].name, name) == 0) {
            return &t[i];
        }
    }
    fprintf(stderr, "vector %s not found\n", name);
    exit(2);
}

#define ML(name) find(hqq_rpcvec_microlib, HQQ_RPCVEC_MICROLIB_COUNT, (name))
#define CH(name) find(hqq_rpcvec_chunking, HQQ_RPCVEC_CHUNKING_COUNT, (name))
#define WR(name) find(hqq_rpcvec_wrap, HQQ_RPCVEC_WRAP_COUNT, (name))

static hqqrpc_status xf(hqqrpc_session *s, const hqq_rpcvec_frame *f, size_t *out_len) {
    return rtu_x(s, f->data, f->len, g_out, sizeof g_out, out_len);
}

/* The final frame of a protocol violation: RESPONSE, id 0, aux 7, "protocol violation". */
static size_t expected_violation(uint8_t *f) {
    rtu_hdr(f, HQQRPC_K_RESPONSE, 0, 0, 0, 23, HQQRPC_E_PROTOCOL);
    rtu_put32(f + 16, 18);
    memcpy(f + 20, "protocol violation", 18);
    f[38] = 0;
    return 39;
}

/* RESPONSE of a call-level status with its message. */
static size_t expected_status(uint8_t *f, unsigned op, uint32_t id, hqqrpc_status st, const char *msg) {
    size_t n = strlen(msg);

    rtu_hdr(f, HQQRPC_K_RESPONSE, 0, op, id, (uint32_t)(n + 5), st);
    rtu_put32(f + 16, (uint32_t)n);
    memcpy(f + 20, msg, n);
    f[20 + n] = 0;
    return 20 + n + 1;
}

/* ---- Vectors (4.14) ------------------------------------------------------------------------ */

static void test_vector_session(void) {
    hqqrpc_session *s = hqqrpc_test_session_new(&microlib_iface, NULL);
    size_t n = 0;
    char name[32];
    int i;
    const hqq_rpcvec_frame *want;

    HQQ_REQUIRE(s != NULL);
    HQQ_CHECK_EQ_INT(ml_test_live_objects(), 0);

    HQQ_CHECK_EQ_INT(xf(s, ML("HELLO"), &n), HQQRPC_OK);
    want = ML("HELLO_REPLY");
    RTU_CHECK_BYTES(g_out, n, want->data, want->len, "HELLO_REPLY");

    for (i = 1; i <= 13; i++) {
        snprintf(name, sizeof name, "R%d_REQ", i);
        HQQ_CHECK_EQ_INT(xf(s, ML(name), &n), HQQRPC_OK);
        snprintf(name, sizeof name, "R%d_RESP", i);
        want = ML(name);
        RTU_CHECK_BYTES(g_out, n, want->data, want->len, name);
    }
    HQQ_CHECK_EQ_INT(hqqrpc_test_live_handles(s), 0);
    HQQ_CHECK_EQ_INT(ml_test_live_objects(), 0); /* the counter and the cursor were destroyed by R13 and R10 */

    HQQ_CHECK_EQ_INT(xf(s, ML("CLOSE"), &n), HQQRPC_OK);
    want = ML("CLOSE_ACK");
    RTU_CHECK_BYTES(g_out, n, want->data, want->len, "CLOSE_ACK");
    HQQ_CHECK_EQ_INT(xf(s, ML("CLOSE"), &n), HQQRPC_E_SHUTTING_DOWN);
    HQQ_CHECK_EQ_INT(n, 0);

    /* No tombstone on any path: R2, R5, R6, R7, R10, R11 and R12 are not plain successes. */
    HQQ_CHECK_EQ_INT(hqqrpc_test_tombstones(s), 0);
    HQQ_CHECK_EQ_INT(hqqrpc_test_frames(s), 1 + 13 + 1);
    HQQ_CHECK_EQ_INT(hqqrpc_test_destroy_failures(s), 0);
    hqqrpc_test_session_free(s);
}

/* The digest mismatch reply of 4.14 carries the server digest and zero grants. */
static void test_vector_digest_mismatch(void) {
    hqqrpc_session *s = hqqrpc_test_session_new(&microlib_iface, NULL);
    uint8_t f[RTU_HDR + 64];
    size_t n = 0;
    const hqq_rpcvec_frame *hello = ML("HELLO");
    const hqq_rpcvec_frame *want = ML("HELLO_REPLY_DIGEST");

    HQQ_REQUIRE(s != NULL);
    memcpy(f, hello->data, hello->len);
    f[RTU_HDR + 32] ^= 0xFFu;
    HQQ_CHECK_EQ_INT(rtu_x(s, f, hello->len, g_out, sizeof g_out, &n), HQQRPC_OK);
    RTU_CHECK_BYTES(g_out, n, want->data, want->len, "HELLO_REPLY_DIGEST");
    HQQ_CHECK(hqqrpc_test_ended(s));
    HQQ_CHECK_EQ_INT(xf(s, ML("R3_REQ"), &n), HQQRPC_E_SHUTTING_DOWN);
    hqqrpc_test_session_free(s);
}

/* WRAP_A_REQ carries id 0xFFFFFFFF and WRAP_B_REQ the id 1 that follows it (4.12). A session cannot
 * issue 2^32 requests, so the hook sets the last id. */
static void test_vector_wrap(void) {
    hqqrpc_session *s = rtu_open(&microlib_iface, NULL);
    size_t n = 0;

    hqqrpc_test_set_last_id(s, 0xFFFFFFFEu);
    HQQ_CHECK_EQ_INT(xf(s, WR("WRAP_A_REQ"), &n), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(n, 20);
    HQQ_CHECK_EQ_U64(rtu_le32(g_out + 4), 0xFFFFFFFFu);
    HQQ_CHECK_EQ_U64(rtu_le32(g_out + 16), 3); /* ml_add(1, 2) */
    HQQ_CHECK_EQ_INT(xf(s, WR("WRAP_B_REQ"), &n), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(n, 20);
    HQQ_CHECK_EQ_U64(rtu_le32(g_out + 4), 1);
    HQQ_CHECK_EQ_U64(rtu_le32(g_out + 16), 7); /* ml_add(3, 4) */
    HQQ_CHECK_EQ_INT(hqqrpc_test_ended(s), 0);
    hqqrpc_test_session_free(s);

    /* After the wrap the id 2 is next; 0 and a repeat are violations. */
    s = rtu_open(&microlib_iface, NULL);
    hqqrpc_test_set_last_id(s, 0xFFFFFFFFu);
    HQQ_CHECK_EQ_INT(xf(s, WR("WRAP_A_REQ"), &n), HQQRPC_E_PROTOCOL); /* 0xFFFFFFFF repeats */
    hqqrpc_test_session_free(s);
    s = rtu_open(&microlib_iface, NULL);
    hqqrpc_test_set_last_id(s, 0xFFFFFFFFu);
    HQQ_CHECK_EQ_INT(xf(s, WR("WRAP_B_REQ"), &n), HQQRPC_OK); /* 1 follows 0xFFFFFFFF */
    hqqrpc_test_session_free(s);
}

/* Oversize request and response: S = 128, CHUNK = 112. The request arrives in two frames and the
 * response (RESP_BOUND 129 > 112) is built in scratch and published as two frames. */
static void test_vector_chunking(void) {
    hqqrpc_server_options o;
    hqqrpc_session *s;
    size_t n = 0;
    uint8_t want[128 + 26];
    const hqq_rpcvec_frame *r1 = CH("C_RESP_1");
    const hqq_rpcvec_frame *r2 = CH("C_RESP_2");

    rtu_opts(&o, HQQ_RPCVEC_CHUNKING_BUFFER_SIZE, 64, 0);
    s = rtu_open(&microlib_iface, &o);
    ml_test_reset();
    HQQ_CHECK_EQ_INT(xf(s, CH("C_REQ_1"), &n), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(n, 0); /* nothing is answered before the message is complete */
    HQQ_CHECK_EQ_INT(ml_test_entries(), 0);
    HQQ_CHECK_EQ_INT(xf(s, CH("C_REQ_2"), &n), HQQRPC_OK);
    HQQ_REQUIRE(r1->len + r2->len == sizeof want);
    memcpy(want, r1->data, r1->len);
    memcpy(want + r1->len, r2->data, r2->len);
    RTU_CHECK_BYTES(g_out, n, want, sizeof want, "C_RESP_1 + C_RESP_2");
    HQQ_CHECK_EQ_INT(hqqrpc_test_tombstones(s), 0);
    hqqrpc_test_session_free(s);
}

/* ---- Handshake (4.11) ---------------------------------------------------------------------- */

typedef struct hello_case {
    const char *name;
    uint32_t S, B, mif, mmsg;
    int off;           /* offset in the frame of a byte to xor, 0: none */
    uint8_t xor_;
    int poke_off;      /* offset of a 32-bit value to set, 0: none */
    uint32_t poke;
    uint32_t want_aux;
} hello_case;

static void test_hello_statuses(void) {
    static const hello_case cases[] = {
        {"ok", 16384, 64, 0, 0, 0, 0, 0, 0, HQQRPC_HELLO_OK},
        {"newer minor", 16384, 64, 0, 0, 0, 0, 22, 0, HQQRPC_HELLO_OK}, /* poke replaced below */
        {"bad magic", 16384, 64, 0, 0, 16, 1, 0, 0, HQQRPC_HELLO_BAD},
        {"flags", 16384, 64, 0, 0, 0, 0, 24, 1, HQQRPC_HELLO_BAD},
        {"reserved", 16384, 64, 0, 0, 0, 0, 44, 1, HQQRPC_HELLO_BAD},
        {"version", 16384, 64, 0, 0, 20, 1, 0, 0, HQQRPC_HELLO_VERSION}, /* major 1 -> 0 */
        {"digest", 16384, 64, 0, 0, 16 + 32 + 5, 0x40, 0, 0, HQQRPC_HELLO_DIGEST_MISMATCH},
        {"geometry S", 16384 + 64, 64, 0, 0, 0, 0, 0, 0, HQQRPC_HELLO_GEOMETRY},
        {"geometry B", 16384, 128, 0, 0, 0, 0, 0, 0, HQQRPC_HELLO_GEOMETRY},
        {"max_message too small", 16384, 64, 0, 4095, 0, 0, 0, 0, HQQRPC_HELLO_LIMIT},
        {"max_message above hard max", 16384, 64, 0, 0x40000001u, 0, 0, 0, 0, HQQRPC_HELLO_LIMIT},
        {"max_in_flight above B", 16384, 64, 65, 0, 0, 0, 0, 0, HQQRPC_HELLO_LIMIT},
        /* precedence: BAD_HELLO, VERSION, DIGEST, GEOMETRY, LIMIT */
        {"bad magic and wrong digest", 16384, 64, 0, 0, 16, 1, 16 + 32, 0xFFFFFFFFu, HQQRPC_HELLO_BAD},
        {"version and digest", 16384, 64, 0, 0, 20, 1, 16 + 32, 0xFFFFFFFFu, HQQRPC_HELLO_VERSION},
        {"digest and geometry", 16384 + 64, 64, 0, 0, 16 + 32 + 5, 0x40, 0, 0, HQQRPC_HELLO_DIGEST_MISMATCH},
        {"geometry and limit", 16384 + 64, 64, 65, 0, 0, 0, 0, 0, HQQRPC_HELLO_GEOMETRY},
    };
    size_t i;

    for (i = 0; i < sizeof cases / sizeof cases[0]; i++) {
        const hello_case *c = &cases[i];
        hqqrpc_session *s = hqqrpc_test_session_new(&microlib_iface, NULL);
        uint8_t f[RTU_HDR + 64];
        size_t n = 0;
        size_t flen = rtu_hello(f, &microlib_iface, c->S, c->B, c->mif, c->mmsg);
        const uint8_t *r = g_out;

        HQQ_REQUIRE(s != NULL);
        if (c->off != 0) {
            f[c->off] ^= c->xor_;
        }
        if (c->poke_off == 22) { /* the minor version field: a newer client minor is accepted */
            f[RTU_HDR + 6] = 7;
        } else if (c->poke_off == 24) {
            f[RTU_HDR + 8] = 1;
        } else if (c->poke_off == 44) {
            f[RTU_HDR + 28] = 1;
        } else if (c->poke_off == 16 + 32) {
            f[RTU_HDR + 32] ^= 0xFF;
        }
        HQQ_CHECK_EQ_INT(rtu_x(s, f, flen, g_out, sizeof g_out, &n), HQQRPC_OK);
        if (n != RTU_HDR + 64) {
            fprintf(stderr, "case %s: reply of %zu bytes\n", c->name, n);
            HQQ_CHECK(0);
            hqqrpc_test_session_free(s);
            continue;
        }
        if (rtu_le32(r + 12) != c->want_aux) {
            fprintf(stderr, "case %s: aux %u, want %u\n", c->name, (unsigned)rtu_le32(r + 12), (unsigned)c->want_aux);
        }
        HQQ_CHECK_EQ_INT(r[0], HQQRPC_K_HELLO_REPLY);
        HQQ_CHECK_EQ_INT(rtu_le32(r + 12), c->want_aux);
        HQQ_CHECK_EQ_INT(rtu_le32(r + 8), 64);
        HQQ_CHECK(memcmp(r + RTU_HDR, "HQBG", 4) == 0);
        HQQ_CHECK(memcmp(r + RTU_HDR + 32, microlib_iface.digest, 32) == 0); /* the server digest */
        HQQ_CHECK_EQ_INT(rtu_le16(r + RTU_HDR + 6), 0);                      /* min of the minors */
        if (c->want_aux == HQQRPC_HELLO_OK) {
            HQQ_CHECK_EQ_INT(rtu_le32(r + RTU_HDR + 12), 64);
            HQQ_CHECK_EQ_INT(rtu_le32(r + RTU_HDR + 16), 16u << 20);
            HQQ_CHECK_EQ_INT(rtu_le32(r + RTU_HDR + 20), 4096);
            HQQ_CHECK_EQ_INT(hqqrpc_test_ended(s), 0);
        } else {
            HQQ_CHECK_EQ_INT(rtu_le32(r + RTU_HDR + 12), 0);
            HQQ_CHECK_EQ_INT(rtu_le32(r + RTU_HDR + 16), 0);
            HQQ_CHECK_EQ_INT(rtu_le32(r + RTU_HDR + 20), 0);
            HQQ_CHECK(hqqrpc_test_ended(s));
        }
        hqqrpc_test_session_free(s);
    }
}

/* HELLO with a total other than 64 is BAD_HELLO when the framing is consistent; a frame whose
 * length contradicts its total is a violation. */
static void test_hello_total(void) {
    hqqrpc_session *s = hqqrpc_test_session_new(&microlib_iface, NULL);
    uint8_t f[RTU_HDR + 128];
    uint8_t viol[64];
    size_t vn = expected_violation(viol);
    size_t n = 0;

    HQQ_REQUIRE(s != NULL);
    memset(f, 0, sizeof f);
    rtu_hdr(f, HQQRPC_K_HELLO, 0, 0, 0, 32, 0);
    HQQ_CHECK_EQ_INT(rtu_x(s, f, RTU_HDR + 32, g_out, sizeof g_out, &n), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(n, RTU_HDR + 64);
    HQQ_CHECK_EQ_INT(rtu_le32(g_out + 12), HQQRPC_HELLO_BAD);
    hqqrpc_test_session_free(s);

    s = hqqrpc_test_session_new(&microlib_iface, NULL);
    rtu_hello(f, &microlib_iface, 16384, 64, 0, 0);
    HQQ_CHECK_EQ_INT(rtu_x(s, f, RTU_HDR + 65, g_out, sizeof g_out, &n), HQQRPC_E_PROTOCOL);
    RTU_CHECK_BYTES(g_out, n, viol, vn, "HELLO of 65 body bytes");
    hqqrpc_test_session_free(s);

    s = hqqrpc_test_session_new(&microlib_iface, NULL);
    HQQ_CHECK_EQ_INT(rtu_x(s, f, RTU_HDR + 63, g_out, sizeof g_out, &n), HQQRPC_E_PROTOCOL);
    hqqrpc_test_session_free(s);
}

/* Granted limits: min(requested or default, server option) (4.11). */
static void test_hello_grants(void) {
    static const struct {
        uint32_t opt_mif, opt_msg, req_mif, req_msg, want_mif, want_msg;
    } cases[] = {
        {0, 0, 0, 0, 64, 16u << 20},
        {0, 0, 10, 8192, 10, 8192},
        {20, 0, 0, 0, 20, 16u << 20},
        {20, 0, 30, 0, 20, 16u << 20},
        {20, 0, 5, 0, 5, 16u << 20},
        {0, 65536, 0, 0, 64, 65536},
        {0, 65536, 0, 1u << 24, 64, 65536},
        {0, 65536, 0, 5000, 64, 5000},
        {200, 0, 0, 0, 64, 16u << 20}, /* the option cannot exceed B */
    };
    size_t i;

    for (i = 0; i < sizeof cases / sizeof cases[0]; i++) {
        hqqrpc_server_options o;
        hqqrpc_session *s;
        uint8_t f[RTU_HDR + 64];
        size_t n = 0;

        hqqrpc_server_options_init(&o);
        o.max_in_flight = cases[i].opt_mif;
        o.max_message = cases[i].opt_msg;
        s = hqqrpc_test_session_new(&microlib_iface, &o);
        HQQ_REQUIRE(s != NULL);
        HQQ_CHECK_EQ_INT(rtu_x(s, f, rtu_hello(f, &microlib_iface, 16384, 64, cases[i].req_mif, cases[i].req_msg),
                               g_out, sizeof g_out, &n),
                         HQQRPC_OK);
        HQQ_CHECK_EQ_INT(rtu_le32(g_out + 12), 0);
        HQQ_CHECK_EQ_INT(rtu_le32(g_out + RTU_HDR + 12), cases[i].want_mif);
        HQQ_CHECK_EQ_INT(rtu_le32(g_out + RTU_HDR + 16), cases[i].want_msg);
        hqqrpc_test_session_free(s);
    }
}

/* A granted max_message below a request's total is a violation, and above a response bound E_LIMIT. */
static void test_granted_message_limit(void) {
    hqqrpc_session *s = hqqrpc_test_session_new(&microlib_iface, NULL);
    uint8_t f[RTU_HDR + 9000];
    uint8_t body[16];
    size_t n = 0;

    ml_test_reset();
    HQQ_REQUIRE(s != NULL);
    HQQ_CHECK_EQ_INT(rtu_x(s, f, rtu_hello(f, &microlib_iface, 16384, 64, 0, 8192), g_out, sizeof g_out, &n), HQQRPC_OK);
    /* ml_xor with cap 9000: RESP_BOUND = 8 + 11 + 9000 > 8192 -> E_LIMIT, nothing executed */
    body[0] = 1;
    memset(body + 1, 0, 3);
    rtu_put32(body + 4, 9000);
    rtu_put32(body + 8, 0); /* src: empty bytes */
    HQQ_CHECK_EQ_INT(rtu_x(s, f, rtu_req(f, 8, 1, body, 12), g_out, sizeof g_out, &n), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(rtu_le32(g_out + 12), HQQRPC_E_LIMIT);
    HQQ_CHECK_EQ_INT(ml_test_entries(), 0);
    /* A request of 9000 bytes exceeds the granted limit. */
    memset(f, 0, sizeof f);
    rtu_hdr(f, HQQRPC_K_REQUEST, 0, 3, 2, 9000, 0);
    HQQ_CHECK_EQ_INT(rtu_x(s, f, RTU_HDR + 9000, g_out, sizeof g_out, &n), HQQRPC_E_PROTOCOL);
    hqqrpc_test_session_free(s);
}

/* ---- Frame rules (4.4, 4.8) ------------------------------------------------------------------ */

typedef struct vcase {
    const char *name;
    int open;       /* complete the handshake first */
    size_t flen;
    uint8_t f[256];
} vcase;

static void vc_hdr(vcase *c, unsigned kind, unsigned flags, unsigned op, uint32_t id, uint32_t total, uint32_t aux,
                   size_t body) {
    memset(c->f, 0x5A, sizeof c->f);
    rtu_hdr(c->f, kind, flags, op, id, total, aux);
    c->flen = RTU_HDR + body;
}

static void run_violation(const vcase *c, uint32_t max_message) {
    hqqrpc_server_options o;
    hqqrpc_session *s;
    uint8_t viol[64];
    size_t vn = expected_violation(viol);
    size_t n = 0;

    rtu_opts(&o, 16384, 64, max_message);
    s = c->open ? rtu_open(&microlib_iface, &o) : hqqrpc_test_session_new(&microlib_iface, &o);
    HQQ_REQUIRE(s != NULL);
    ml_test_reset();
    if (rtu_x(s, c->f, c->flen, g_out, sizeof g_out, &n) != HQQRPC_E_PROTOCOL) {
        fprintf(stderr, "case %s: not a protocol violation\n", c->name);
        HQQ_CHECK(0);
    } else {
        RTU_CHECK_BYTES(g_out, n, viol, vn, c->name);
    }
    HQQ_CHECK(hqqrpc_test_ended(s));
    HQQ_CHECK_EQ_INT(ml_test_entries(), 0); /* nothing was executed */
    HQQ_CHECK_EQ_INT(rtu_x(s, c->f, c->flen, g_out, sizeof g_out, &n), HQQRPC_E_SHUTTING_DOWN);
    hqqrpc_test_session_free(s);
}

static void test_frame_violations(void) {
    vcase c;
    unsigned k;

    memset(&c, 0, sizeof c);
    c.open = 1;

    c.name = "frame shorter than the header";
    memset(c.f, 0, sizeof c.f);
    c.flen = 15;
    run_violation(&c, 0);

    c.name = "reserved flag bit 2";
    vc_hdr(&c, HQQRPC_K_REQUEST, 0x04, 1, 1, 0, 0, 0);
    run_violation(&c, 0);
    c.name = "reserved flag bit 7";
    vc_hdr(&c, HQQRPC_K_REQUEST, 0x80, 1, 1, 0, 0, 0);
    run_violation(&c, 0);
    c.name = "F_FAIL on a REQUEST";
    vc_hdr(&c, HQQRPC_K_REQUEST, HQQRPC_F_FAIL, 1, 1, 0, 0, 0);
    run_violation(&c, 0);
    c.name = "flags on CLOSE";
    vc_hdr(&c, HQQRPC_K_CLOSE, 0x01, 0, 0, 0, 0, 0);
    run_violation(&c, 0);
    c.name = "F_DETAIL without F_FAIL";
    vc_hdr(&c, HQQRPC_K_RESPONSE, HQQRPC_F_DETAIL, 1, 1, 0, 0, 0);
    run_violation(&c, 0);

    for (k = 0; k < 256; k++) {
        static char names[256][32];

        if (k == HQQRPC_K_REQUEST || k == HQQRPC_K_CONT || k == HQQRPC_K_CLOSE) {
            continue; /* valid kinds in this state; their violations are below */
        }
        snprintf(names[k], sizeof names[k], "kind 0x%02x after HELLO", k);
        c.name = names[k];
        vc_hdr(&c, k, 0, 0, 0, 0, 0, 0);
        run_violation(&c, 0);
    }

    c.name = "second HELLO";
    c.flen = rtu_hello(c.f, &microlib_iface, 16384, 64, 0, 0);
    run_violation(&c, 0);

    c.name = "CONT without a first frame";
    vc_hdr(&c, HQQRPC_K_CONT, 0, 0, 1, 20, 0, 10);
    run_violation(&c, 0);

    c.name = "REQUEST body shorter than total";
    vc_hdr(&c, HQQRPC_K_REQUEST, 0, 3, 1, 8, 0, 7);
    run_violation(&c, 0);
    c.name = "REQUEST body longer than total";
    vc_hdr(&c, HQQRPC_K_REQUEST, 0, 3, 1, 8, 0, 9);
    run_violation(&c, 0);
    c.name = "REQUEST total above CHUNK with a short body";
    vc_hdr(&c, HQQRPC_K_REQUEST, 0, 3, 1, 20000, 0, 100);
    run_violation(&c, 0);
    c.name = "REQUEST total 0xFFFFFFFF";
    vc_hdr(&c, HQQRPC_K_REQUEST, 0, 3, 1, 0xFFFFFFFFu, 0, 100);
    run_violation(&c, 0);
    c.name = "CLOSE with a total";
    vc_hdr(&c, HQQRPC_K_CLOSE, 0, 0, 0, 4, 0, 4);
    run_violation(&c, 0);
    c.name = "CLOSE with a body";
    vc_hdr(&c, HQQRPC_K_CLOSE, 0, 0, 0, 0, 0, 1);
    run_violation(&c, 0);

    c.name = "REQUEST id 5 first";
    vc_hdr(&c, HQQRPC_K_REQUEST, 0, 3, 5, 0, 0, 0);
    run_violation(&c, 0);
    c.name = "REQUEST id 0";
    vc_hdr(&c, HQQRPC_K_REQUEST, 0, 3, 0, 0, 0, 0);
    run_violation(&c, 0);
    c.name = "REQUEST id 0xFFFFFFFF first";
    vc_hdr(&c, HQQRPC_K_REQUEST, 0, 3, 0xFFFFFFFFu, 0, 0, 0);
    run_violation(&c, 0);

    c.open = 0;
    c.name = "REQUEST before HELLO";
    vc_hdr(&c, HQQRPC_K_REQUEST, 0, 3, 1, 0, 0, 0);
    run_violation(&c, 0);
    c.name = "CLOSE before HELLO";
    vc_hdr(&c, HQQRPC_K_CLOSE, 0, 0, 0, 0, 0, 0);
    run_violation(&c, 0);
    c.name = "CONT before HELLO";
    vc_hdr(&c, HQQRPC_K_CONT, 0, 0, 1, 20, 0, 10);
    run_violation(&c, 0);
}

/* A REQUEST above max_message needs a body of that size. */
static void test_total_above_max_message(void) {
    hqqrpc_server_options o;
    hqqrpc_session *s;
    static uint8_t f[RTU_HDR + 5000];
    uint8_t viol[64];
    size_t vn = expected_violation(viol);
    size_t n = 0;

    rtu_opts(&o, 16384, 64, 4096);
    s = rtu_open(&microlib_iface, &o);
    memset(f, 0, sizeof f);
    rtu_hdr(f, HQQRPC_K_REQUEST, 0, 3, 1, 5000, 0);
    HQQ_CHECK_EQ_INT(rtu_x(s, f, RTU_HDR + 5000, g_out, sizeof g_out, &n), HQQRPC_E_PROTOCOL);
    RTU_CHECK_BYTES(g_out, n, viol, vn, "total above max_message");
    hqqrpc_test_session_free(s);

    /* exactly max_message is accepted (and answered E_BAD_REQUEST: the body is not a request) */
    s = rtu_open(&microlib_iface, &o);
    rtu_hdr(f, HQQRPC_K_REQUEST, 0, 3, 1, 4096, 0);
    HQQ_CHECK_EQ_INT(rtu_x(s, f, RTU_HDR + 4096, g_out, sizeof g_out, &n), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(rtu_le32(g_out + 12), HQQRPC_E_BAD_REQUEST);
    hqqrpc_test_session_free(s);
}

/* Multi-frame messages: S = 128, CHUNK = 112. The first frame of a 130-byte message carries 112
 * bytes, the CONT 18. */
static void test_assembly_violations(void) {
    static const struct {
        const char *name;
        int id, total_delta;
        uint32_t aux;
        int body;      /* CONT body bytes */
        unsigned kind; /* frame sent instead of the CONT, 0: CONT */
    } cases[] = {
        {"CONT with another id", 2, 0, 112, 18, 0},
        {"CONT with another total", 1, 1, 112, 18, 0},
        {"CONT with a wrong offset (low)", 1, 0, 111, 18, 0},
        {"CONT with a wrong offset (high)", 1, 0, 113, 18, 0},
        {"CONT too short", 1, 0, 112, 17, 0},
        {"CONT too long", 1, 0, 112, 19, 0},
        {"REQUEST inside a message", 1, 0, 0, 5, HQQRPC_K_REQUEST},
        {"CLOSE inside a message", 1, 0, 0, 0, HQQRPC_K_CLOSE},
        {"HELLO inside a message", 1, 0, 0, 64, HQQRPC_K_HELLO},
    };
    size_t i;

    for (i = 0; i < sizeof cases / sizeof cases[0]; i++) {
        hqqrpc_server_options o;
        hqqrpc_session *s;
        uint8_t f[128];
        uint8_t viol[64];
        size_t vn = expected_violation(viol);
        size_t n = 0;

        rtu_opts(&o, 128, 64, 0);
        s = rtu_open(&microlib_iface, &o);
        ml_test_reset();
        memset(f, 0x33, sizeof f);
        rtu_hdr(f, HQQRPC_K_REQUEST, 0, 3, 1, 130, 0);
        HQQ_CHECK_EQ_INT(rtu_x(s, f, 128, g_out, sizeof g_out, &n), HQQRPC_OK);
        HQQ_CHECK_EQ_INT(n, 0);
        if (cases[i].kind == 0) {
            rtu_hdr(f, HQQRPC_K_CONT, 0, 0, (uint32_t)cases[i].id, (uint32_t)(130 + cases[i].total_delta), cases[i].aux);
        } else if (cases[i].kind == HQQRPC_K_REQUEST) {
            rtu_hdr(f, HQQRPC_K_REQUEST, 0, 3, 2, 5, 0);
        } else if (cases[i].kind == HQQRPC_K_HELLO) {
            rtu_hello(f, &microlib_iface, 128, 64, 0, 0);
        } else {
            rtu_hdr(f, cases[i].kind, 0, 0, 0, 0, 0);
        }
        HQQ_CHECK_EQ_INT(rtu_x(s, f, RTU_HDR + (size_t)cases[i].body, g_out, sizeof g_out, &n), HQQRPC_E_PROTOCOL);
        RTU_CHECK_BYTES(g_out, n, viol, vn, cases[i].name);
        HQQ_CHECK_EQ_INT(ml_test_entries(), 0);
        hqqrpc_test_session_free(s);
    }

    /* A CONT with the right values completes the message; one more CONT is a violation. */
    {
        hqqrpc_server_options o;
        hqqrpc_session *s;
        uint8_t f[128];
        size_t n = 0;

        rtu_opts(&o, 128, 64, 0);
        s = rtu_open(&microlib_iface, &o);
        memset(f, 0x33, sizeof f);
        rtu_hdr(f, HQQRPC_K_REQUEST, 0, 3, 1, 130, 0);
        HQQ_CHECK_EQ_INT(rtu_x(s, f, 128, g_out, sizeof g_out, &n), HQQRPC_OK);
        rtu_hdr(f, HQQRPC_K_CONT, 0, 0, 1, 130, 112);
        HQQ_CHECK_EQ_INT(rtu_x(s, f, RTU_HDR + 18, g_out, sizeof g_out, &n), HQQRPC_OK);
        HQQ_CHECK_EQ_INT(rtu_le32(g_out + 12), HQQRPC_E_BAD_REQUEST); /* ml_counter_add with a 130-byte body */
        HQQ_CHECK_EQ_INT(rtu_x(s, f, RTU_HDR + 18, g_out, sizeof g_out, &n), HQQRPC_E_PROTOCOL);
        hqqrpc_test_session_free(s);
    }
}

/* The id of each request is checked against the last one; a rejected id leaves nothing behind. */
static void test_request_ids(void) {
    hqqrpc_session *s = rtu_open(&microlib_iface, NULL);
    uint8_t f[RTU_HDR + 8];
    uint8_t body[8];
    size_t n = 0;
    uint32_t id;

    rtu_put32(body, 7);
    rtu_put32(body + 4, 35);
    for (id = 1; id <= 5; id++) {
        HQQ_CHECK_EQ_INT(rtu_x(s, f, rtu_req(f, 1, id, body, 8), g_out, sizeof g_out, &n), HQQRPC_OK);
        HQQ_CHECK_EQ_U64(rtu_le32(g_out + 4), id);
        HQQ_CHECK_EQ_U64(rtu_le32(g_out + 16), 42);
    }
    HQQ_CHECK_EQ_INT(rtu_x(s, f, rtu_req(f, 1, 5, body, 8), g_out, sizeof g_out, &n), HQQRPC_E_PROTOCOL); /* repeat */
    hqqrpc_test_session_free(s);

    s = rtu_open(&microlib_iface, NULL);
    HQQ_CHECK_EQ_INT(rtu_x(s, f, rtu_req(f, 1, 1, body, 8), g_out, sizeof g_out, &n), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(rtu_x(s, f, rtu_req(f, 1, 3, body, 8), g_out, sizeof g_out, &n), HQQRPC_E_PROTOCOL); /* skip */
    hqqrpc_test_session_free(s);
}

/* An opcode that names no function is an ordinary error, not a violation (4.4 rule 7, 4.10). */
static void test_unknown_op(void) {
    static const unsigned ops[] = {0, 12, 200, 0xFFFF};
    hqqrpc_session *s = rtu_open(&microlib_iface, NULL);
    uint8_t f[RTU_HDR + 8];
    uint8_t want[64];
    uint8_t body[8];
    size_t n = 0;
    size_t wn;
    size_t i;

    rtu_put32(body, 7);
    rtu_put32(body + 4, 35);
    ml_test_reset();
    for (i = 0; i < sizeof ops / sizeof ops[0]; i++) {
        HQQ_CHECK_EQ_INT(rtu_x(s, f, rtu_req(f, ops[i], (uint32_t)i + 1, body, 8), g_out, sizeof g_out, &n), HQQRPC_OK);
        wn = expected_status(want, ops[i], (uint32_t)i + 1, HQQRPC_E_UNKNOWN_OP, "unknown op");
        RTU_CHECK_BYTES(g_out, n, want, wn, "unknown op response");
    }
    HQQ_CHECK_EQ_INT(ml_test_entries(), 0);
    HQQ_CHECK_EQ_INT(rtu_x(s, f, rtu_req(f, 1, 5, body, 8), g_out, sizeof g_out, &n), HQQRPC_OK);
    HQQ_CHECK_EQ_U64(rtu_le32(g_out + 16), 42);
    HQQ_CHECK_EQ_INT(hqqrpc_test_ended(s), 0);
    hqqrpc_test_session_free(s);
}

/* A malformed body is E_BAD_REQUEST and executes nothing; the session goes on (5.4). */
static void test_bad_bodies(void) {
    hqqrpc_session *s = rtu_open(&microlib_iface, NULL);
    uint8_t f[RTU_HDR + 300];
    uint8_t body[64];
    uint8_t want[64];
    size_t n = 0;
    size_t wn;
    uint32_t id = 1;

    ml_test_reset();
    /* ml_add wants exactly 8 bytes: 7 and 9 are rejected, as are trailing bytes */
    memset(body, 0, sizeof body);
    HQQ_CHECK_EQ_INT(rtu_x(s, f, rtu_req(f, 1, id, body, 7), g_out, sizeof g_out, &n), HQQRPC_OK);
    wn = expected_status(want, 1, id++, HQQRPC_E_BAD_REQUEST, "bad request");
    RTU_CHECK_BYTES(g_out, n, want, wn, "short body");
    HQQ_CHECK_EQ_INT(rtu_x(s, f, rtu_req(f, 1, id, body, 9), g_out, sizeof g_out, &n), HQQRPC_OK);
    wn = expected_status(want, 1, id++, HQQRPC_E_BAD_REQUEST, "bad request");
    RTU_CHECK_BYTES(g_out, n, want, wn, "trailing byte");
    /* ml_hash: str without NUL, with an embedded NUL, over its bound (255), NULL_LEN */
    rtu_put32(body, 3);
    memcpy(body + 4, "abcX", 4);
    HQQ_CHECK_EQ_INT(rtu_x(s, f, rtu_req(f, 9, id, body, 8), g_out, sizeof g_out, &n), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(rtu_le32(g_out + 12), HQQRPC_E_BAD_REQUEST);
    id++;
    memcpy(body + 4, "a\0c\0", 4);
    HQQ_CHECK_EQ_INT(rtu_x(s, f, rtu_req(f, 9, id, body, 8), g_out, sizeof g_out, &n), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(rtu_le32(g_out + 12), HQQRPC_E_BAD_REQUEST);
    id++;
    {
        uint8_t big[4 + 257];

        memset(big, 'x', sizeof big);
        rtu_put32(big, 256);
        big[4 + 256] = 0;
        HQQ_CHECK_EQ_INT(rtu_x(s, f, rtu_req(f, 9, id, big, sizeof big), g_out, sizeof g_out, &n), HQQRPC_OK);
        HQQ_CHECK_EQ_INT(rtu_le32(g_out + 12), HQQRPC_E_BAD_REQUEST);
        id++;
    }
    rtu_put32(body, 0xFFFFFFFFu);
    HQQ_CHECK_EQ_INT(rtu_x(s, f, rtu_req(f, 9, id, body, 4), g_out, sizeof g_out, &n), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(rtu_le32(g_out + 12), HQQRPC_E_BAD_REQUEST);
    id++;
    HQQ_CHECK_EQ_INT(ml_test_entries(), 0);
    /* A valid hash still works; the str of the vector is NUL-terminated and copied. */
    rtu_put32(body, 3);
    memcpy(body + 4, "abc", 4);
    HQQ_CHECK_EQ_INT(rtu_x(s, f, rtu_req(f, 9, id, body, 8), g_out, sizeof g_out, &n), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(rtu_le32(g_out + 12), 0);
    HQQ_CHECK_EQ_U64((uint64_t)rtu_le32(g_out + 16) | ((uint64_t)rtu_le32(g_out + 20) << 32), UINT64_C(0xe71fa2190541574b));
    hqqrpc_test_session_free(s);
}

/* ---- Decoders (4.5) -------------------------------------------------------------------------- */

static void rd_init(hqqrpc_rd *r, const uint8_t *b, size_t n) {
    hqqrpc_call c;

    memset(&c, 0, sizeof c);
    c.body = b;
    c.body_len = n;
    hqqrpc_rd_init(r, &c);
}

static void test_rd_take(void) {
    uint8_t b[64];
    hqqrpc_rd r;
    const uint8_t *p = NULL;
    uint32_t v = 0;

    memset(b, 0, sizeof b);
    rd_init(&r, b, 12);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_take(&r, 1, 1, &p), HQQRPC_OK);
    HQQ_CHECK(p == b);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_take(&r, 4, 4, &p), HQQRPC_OK); /* aligned to 4 */
    HQQ_CHECK(p == b + 4);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_take(&r, 8, 4, &p), HQQRPC_OK); /* aligned to 8, ends exactly at 12 */
    HQQ_CHECK(p == b + 8);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_end(&r), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_take(&r, 1, 1, &p), HQQRPC_E_BAD_REQUEST);

    rd_init(&r, b, 8);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_take(&r, 1, 1, &p), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_take(&r, 8, 8, &p), HQQRPC_E_BAD_REQUEST); /* aligned to 8, 8 bytes remain: no */
    rd_init(&r, b, 3);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_take(&r, 1, 1, &p), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_take(&r, 4, 0, &p), HQQRPC_E_BAD_REQUEST); /* the padding passes the end */
    rd_init(&r, b, 5);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_take(&r, 4, 4, &p), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_end(&r), HQQRPC_E_BAD_REQUEST); /* a trailing byte */
    rd_init(&r, b, 8);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_take(&r, 1, SIZE_MAX, &p), HQQRPC_E_BAD_REQUEST);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_take(&r, 1, (size_t)1 << 62, &p), HQQRPC_E_BAD_REQUEST);

    rtu_put32(b, 0xA1B2C3D4u);
    rd_init(&r, b, 4);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_u32(&r, &v), HQQRPC_OK);
    HQQ_CHECK_EQ_U64(v, 0xA1B2C3D4u);
    rd_init(&r, b, 3);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_u32(&r, &v), HQQRPC_E_BAD_REQUEST);
}

static void test_rd_count(void) {
    uint8_t b[64];
    hqqrpc_rd r;
    uint32_t n = 99;

    memset(b, 0, sizeof b);
    rtu_put32(b, 3);
    rd_init(&r, b, 4 + 24);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_count(&r, 8, 3, &n), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(n, 3);
    rd_init(&r, b, 4 + 24);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_count(&r, 8, 2, &n), HQQRPC_E_BAD_REQUEST); /* above the bound */
    rd_init(&r, b, 4 + 23);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_count(&r, 8, 3, &n), HQQRPC_E_BAD_REQUEST); /* 3 * 8 > 23 */
    rtu_put32(b, 0xFFFFFFFFu);
    rd_init(&r, b, 64);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_count(&r, 0, 0xFFFFFFFFu, &n), HQQRPC_E_BAD_REQUEST); /* NULL_LEN */
    rtu_put32(b, 0x40000000u);
    rd_init(&r, b, 8);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_count(&r, 4, 0xFFFFFFFFu, &n), HQQRPC_E_BAD_REQUEST); /* implausible */
    rtu_put32(b, 1);
    rd_init(&r, b, 64);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_count(&r, SIZE_MAX, 0xFFFFFFFFu, &n), HQQRPC_E_BAD_REQUEST); /* product overflows */
    rtu_put32(b, 5);
    rd_init(&r, b, 4);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_count(&r, 0, 10, &n), HQQRPC_OK); /* no minimum size: any count */
}

static void test_rd_str(void) {
    uint8_t b[64];
    hqqrpc_rd r;
    const char *s = NULL;
    uint32_t n = 99;

    memset(b, 0, sizeof b);
    /* "abc\0" exactly at the end of the body */
    rtu_put32(b, 3);
    memcpy(b + 4, "abc", 4);
    rd_init(&r, b, 8);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_str(&r, 0, 255, &s, &n), HQQRPC_OK);
    HQQ_CHECK(s == (const char *)b + 4);
    HQQ_CHECK_EQ_INT(n, 3);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_end(&r), HQQRPC_OK);
    rd_init(&r, b, 7); /* one byte short: the NUL is missing from the body */
    HQQ_CHECK_EQ_INT(hqqrpc_rd_str(&r, 0, 255, &s, &n), HQQRPC_E_BAD_REQUEST);
    rd_init(&r, b, 8);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_str(&r, 0, 2, &s, &n), HQQRPC_E_BAD_REQUEST); /* above the bound */
    rd_init(&r, b, 8);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_str(&r, 0, 3, &s, &n), HQQRPC_OK); /* exactly at the bound */

    memcpy(b + 4, "abcX", 4); /* the byte after the data is not NUL */
    rd_init(&r, b, 8);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_str(&r, 0, 255, &s, &n), HQQRPC_E_BAD_REQUEST);
    memcpy(b + 4, "a\0c\0", 4); /* embedded NUL */
    rd_init(&r, b, 8);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_str(&r, 0, 255, &s, &n), HQQRPC_E_BAD_REQUEST);

    rtu_put32(b, 0); /* empty string */
    b[4] = 0;
    rd_init(&r, b, 5);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_str(&r, 0, 255, &s, &n), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(n, 0);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_end(&r), HQQRPC_OK);

    rtu_put32(b, 0xFFFFFFFFu); /* NULL_LEN */
    rd_init(&r, b, 4);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_str(&r, 1, 255, &s, &n), HQQRPC_OK);
    HQQ_CHECK(s == NULL);
    HQQ_CHECK_EQ_INT(n, 0);
    rd_init(&r, b, 4);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_str(&r, 0, 255, &s, &n), HQQRPC_E_BAD_REQUEST);

    rtu_put32(b, 0xFFFFFFFEu); /* a length that cannot fit the body, with no 32-bit overflow of len + 1 */
    rd_init(&r, b, 64);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_str(&r, 0, 0xFFFFFFFFu, &s, &n), HQQRPC_E_BAD_REQUEST);

    /* alignment: one byte, padding, then the str */
    b[0] = 'x';
    rtu_put32(b + 4, 1);
    b[8] = 'a';
    b[9] = 0;
    rd_init(&r, b, 10);
    {
        const uint8_t *p = NULL;

        HQQ_CHECK_EQ_INT(hqqrpc_rd_take(&r, 1, 1, &p), HQQRPC_OK);
    }
    HQQ_CHECK_EQ_INT(hqqrpc_rd_str(&r, 0, 255, &s, &n), HQQRPC_OK);
    HQQ_CHECK(s == (const char *)b + 8);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_end(&r), HQQRPC_OK);
}

static void test_rd_bytes_array(void) {
    uint8_t b[96];
    hqqrpc_rd r;
    const void *p = NULL;
    uint32_t n = 99;

    memset(b, 0, sizeof b);
    rtu_put32(b, 3);
    b[4] = 1;
    b[5] = 2;
    b[6] = 3;
    rd_init(&r, b, 7); /* exactly at the end, no NUL for bytes */
    HQQ_CHECK_EQ_INT(hqqrpc_rd_bytes(&r, 0, 3, &p, &n), HQQRPC_OK);
    HQQ_CHECK(p == b + 4);
    HQQ_CHECK_EQ_INT(n, 3);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_end(&r), HQQRPC_OK);
    rd_init(&r, b, 6); /* one above the end */
    HQQ_CHECK_EQ_INT(hqqrpc_rd_bytes(&r, 0, 3, &p, &n), HQQRPC_E_BAD_REQUEST);
    rd_init(&r, b, 7);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_bytes(&r, 0, 2, &p, &n), HQQRPC_E_BAD_REQUEST);
    rtu_put32(b, 0xFFFFFFFFu);
    rd_init(&r, b, 4);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_bytes(&r, 1, 10, &p, &n), HQQRPC_OK);
    HQQ_CHECK(p == NULL && n == 0);
    rd_init(&r, b, 4);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_bytes(&r, 0, 10, &p, &n), HQQRPC_E_BAD_REQUEST);
    rtu_put32(b, 0);
    rd_init(&r, b, 4);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_bytes(&r, 0, 10, &p, &n), HQQRPC_OK); /* empty is fine */
    HQQ_CHECK_EQ_INT(n, 0);
    HQQ_CHECK(p != NULL);

    /* arrays of i64: count at 0, data aligned to 8 -> at 8 */
    rtu_put32(b, 2);
    rd_init(&r, b, 8 + 16);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_array(&r, 8, 8, 0, 2, &p, &n), HQQRPC_OK);
    HQQ_CHECK(p == b + 8);
    HQQ_CHECK_EQ_INT(n, 2);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_end(&r), HQQRPC_OK);
    rd_init(&r, b, 8 + 15);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_array(&r, 8, 8, 0, 2, &p, &n), HQQRPC_E_BAD_REQUEST);
    rd_init(&r, b, 8 + 16);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_array(&r, 8, 8, 0, 1, &p, &n), HQQRPC_E_BAD_REQUEST); /* above the bound */
    rtu_put32(b, 0xFFFFFFFEu);
    rd_init(&r, b, 96);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_array(&r, 8, 8, 0, 0xFFFFFFFFu, &p, &n), HQQRPC_E_BAD_REQUEST); /* count * size overflows 32 bits */
    rtu_put32(b, 2);
    rd_init(&r, b, 96);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_array(&r, SIZE_MAX / 2 + 1, 8, 0, 0xFFFFFFFFu, &p, &n), HQQRPC_E_BAD_REQUEST); /* 64-bit overflow */
    rtu_put32(b, 0xFFFFFFFFu);
    rd_init(&r, b, 4);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_array(&r, 4, 4, 1, 10, &p, &n), HQQRPC_OK);
    HQQ_CHECK(p == NULL && n == 0);
    rd_init(&r, b, 4);
    HQQ_CHECK_EQ_INT(hqqrpc_rd_array(&r, 4, 4, 0, 10, &p, &n), HQQRPC_E_BAD_REQUEST);
}

/* ---- Writers (4.5, 4.6.3, 4.6.4) --------------------------------------------------------------- */

static void wr_init(hqqrpc_wr *w, uint8_t *buf, size_t cap) {
    memset(buf, 0xAA, cap);
    memset(w, 0, sizeof *w);
    w->p = buf;
    w->cap = cap;
}

static void test_wr_fixed_and_padding(void) {
    uint8_t buf[64];
    hqqrpc_wr w;
    uint8_t *o;
    size_t i;

    wr_init(&w, buf, sizeof buf);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_put_u32(&w, 0x01020304u), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(w.off, 4);
    o = hqqrpc_wr_fixed(&w, 8, 16); /* pads 4 zero bytes, then 16 zero bytes */
    HQQ_CHECK(o == buf + 8);
    HQQ_CHECK_EQ_INT(w.off, 24);
    for (i = 4; i < 24; i++) {
        HQQ_CHECK_EQ_INT(buf[i], 0);
    }
    HQQ_CHECK_EQ_INT(buf[24], 0xAA); /* nothing beyond is touched */
    HQQ_CHECK_EQ_INT(buf[0], 4);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_put_u64(&w, UINT64_C(0x1122334455667788)), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(w.off, 32);
    HQQ_CHECK_EQ_INT(buf[24], 0x88);
    HQQ_CHECK_EQ_INT(buf[31], 0x11);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_pad(&w, 16), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(w.off, 32); /* already aligned */
    buf[33] = 0xAA;
    HQQ_CHECK_EQ_INT(hqqrpc_wr_put_null(&w), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(w.off, 36);
    HQQ_CHECK_EQ_U64(rtu_le32(buf + 32), 0xFFFFFFFFu);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_pad(&w, 8), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(w.off, 40);
    HQQ_CHECK_EQ_INT(w.err, 0);
}

static void test_wr_capacity_and_sticky(void) {
    uint8_t buf[16];
    hqqrpc_wr w;

    wr_init(&w, buf, sizeof buf);
    HQQ_CHECK(hqqrpc_wr_fixed(&w, 4, 16) != NULL); /* exactly the capacity */
    HQQ_CHECK_EQ_INT(w.off, 16);
    HQQ_CHECK(hqqrpc_wr_fixed(&w, 1, 1) == NULL);  /* one more byte */
    HQQ_CHECK_EQ_INT(w.err, HQQRPC_E_INTERNAL);
    /* sticky: even a request that would fit fails now, and nothing moves */
    HQQ_CHECK(hqqrpc_wr_fixed(&w, 1, 0) == NULL);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_put_u32(&w, 1), HQQRPC_E_INTERNAL);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_put_str(&w, "", 0, 10), HQQRPC_E_INTERNAL);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_pad(&w, 1), HQQRPC_E_INTERNAL);
    HQQ_CHECK_EQ_INT(w.off, 16);

    /* a 64-bit size that cannot be added without overflow */
    wr_init(&w, buf, sizeof buf);
    HQQ_CHECK(hqqrpc_wr_fixed(&w, 1, SIZE_MAX) == NULL);
    HQQ_CHECK_EQ_INT(w.err, HQQRPC_E_INTERNAL);
    wr_init(&w, buf, sizeof buf);
    HQQ_CHECK(hqqrpc_wr_span(&w, 1, SIZE_MAX - 3) == NULL);
    wr_init(&w, buf, sizeof buf);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_put_u32(&w, 1), HQQRPC_OK);
    HQQ_CHECK(hqqrpc_wr_fixed(&w, 8, 8) != NULL); /* pad 4, 8 bytes: ends at 16 */
    HQQ_CHECK(hqqrpc_wr_fixed(&w, 16, 0) != NULL);
    wr_init(&w, buf, 9);
    HQQ_CHECK(hqqrpc_wr_fixed(&w, 1, 1) != NULL);
    HQQ_CHECK(hqqrpc_wr_fixed(&w, 8, 1) != NULL); /* pad to 8, one byte: ends at 9 */
    HQQ_CHECK(hqqrpc_wr_fixed(&w, 8, 1) == NULL); /* pad to 16 passes the capacity */
}

static void test_wr_str_bytes(void) {
    uint8_t buf[64];
    hqqrpc_wr w;

    wr_init(&w, buf, sizeof buf);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_put_u32(&w, 7), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_put_str(&w, "hi", 2, 16), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(w.off, 4 + 4 + 2 + 1);
    HQQ_CHECK_EQ_U64(rtu_le32(buf + 4), 2);
    HQQ_CHECK(memcmp(buf + 8, "hi\0", 3) == 0);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_put_str(&w, "", 0, 16), HQQRPC_OK); /* aligned to 4 first: 11 -> 12 */
    HQQ_CHECK_EQ_INT(w.off, 12 + 5);
    HQQ_CHECK_EQ_INT(buf[11], 0); /* alignment padding is zero */
    HQQ_CHECK_EQ_INT(buf[16], 0);

    wr_init(&w, buf, sizeof buf);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_put_str(&w, "abcd", 4, 3), HQQRPC_E_INTERNAL); /* above the IDL bound */
    HQQ_CHECK_EQ_INT(w.err, HQQRPC_E_INTERNAL);
    HQQ_CHECK_EQ_INT(w.off, 0);
    wr_init(&w, buf, sizeof buf);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_put_str(&w, "a\0b", 3, 16), HQQRPC_E_INTERNAL); /* an embedded NUL */
    HQQ_CHECK_EQ_INT(w.off, 0);
    wr_init(&w, buf, sizeof buf);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_put_str(&w, "abcd", 4, 16), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_put_str(&w, "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOP", 52, 100),
                     HQQRPC_E_INTERNAL); /* the capacity: 12 + 57 > 64 */
    wr_init(&w, buf, 9);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_put_str(&w, "abcd", 4, 16), HQQRPC_OK); /* 4 + 4 + 1 = 9: exactly full */
    wr_init(&w, buf, 8);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_put_str(&w, "abcd", 4, 16), HQQRPC_E_INTERNAL); /* the NUL does not fit */
    wr_init(&w, buf, 8);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_put_str(&w, NULL, 1, 16), HQQRPC_E_INTERNAL);   /* NULL data with a length */
    wr_init(&w, buf, 8);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_put_str(&w, NULL, 0, UINT32_MAX), HQQRPC_OK);

    wr_init(&w, buf, sizeof buf);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_put_bytes(&w, "a\0b", 3, 3), HQQRPC_OK); /* bytes may contain NUL */
    HQQ_CHECK_EQ_INT(w.off, 7);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_put_bytes(&w, "abcd", 4, 3), HQQRPC_E_INTERNAL);
    wr_init(&w, buf, 6);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_put_bytes(&w, "abc", 3, 100), HQQRPC_E_INTERNAL); /* 4 + 3 > 6 */
    wr_init(&w, buf, 7);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_put_bytes(&w, "abc", 3, 100), HQQRPC_OK);
    wr_init(&w, buf, 64);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_put_bytes(&w, NULL, 0, 4), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_put_bytes(&w, NULL, 1, 4), HQQRPC_E_INTERNAL);
}

static void test_wr_array_and_span(void) {
    uint8_t buf[64];
    hqqrpc_wr w;
    size_t mark = 0;
    uint8_t *v;

    wr_init(&w, buf, sizeof buf);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_put_u32(&w, 5), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_array_begin(&w, &mark), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(mark, 4);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_put_u32(&w, 9), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_array_end(&w, mark, 1, 1), HQQRPC_OK);
    HQQ_CHECK_EQ_U64(rtu_le32(buf + 4), 1);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_array_end(&w, mark, 2, 1), HQQRPC_E_INTERNAL); /* above the bound */
    HQQ_CHECK_EQ_INT(w.err, HQQRPC_E_INTERNAL);

    wr_init(&w, buf, sizeof buf);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_array_begin(&w, &mark), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_array_end(&w, mark, 0xFFFFFFFFu, 0xFFFFFFFFu), HQQRPC_E_INTERNAL); /* NULL_LEN is not a count */
    wr_init(&w, buf, sizeof buf);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_array_end(&w, 8, 0, 1), HQQRPC_E_INTERNAL); /* a mark that was never written */

    /* span: aligns, returns n bytes, advances by what the caller says (standalone writer) */
    wr_init(&w, buf, sizeof buf);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_put_u32(&w, 1), HQQRPC_OK);
    v = hqqrpc_wr_span(&w, 8, 20);
    HQQ_CHECK(v == buf + 8);
    HQQ_CHECK_EQ_INT(w.off, 8); /* only the alignment advanced */
    HQQ_CHECK_EQ_INT(buf[4], 0);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_advance(&w, 20), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(w.off, 28);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_advance(&w, 37), HQQRPC_E_INTERNAL); /* beyond the capacity */
    wr_init(&w, buf, 16);
    HQQ_CHECK(hqqrpc_wr_span(&w, 1, 17) == NULL);
}

static void test_wr_detail(void) {
    uint8_t buf[1100];
    hqqrpc_wr w;
    char big[2000];

    wr_init(&w, buf, sizeof buf);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_put_u32(&w, 1), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_put_detail(&w, NULL), 0); /* nothing, not even the padding */
    HQQ_CHECK_EQ_INT(w.off, 4);
    HQQ_CHECK_EQ_INT(w.err, 0);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_put_detail(&w, "over limit"), HQQRPC_F_DETAIL);
    HQQ_CHECK_EQ_INT(w.off, 8 + 4 + 10 + 1); /* padded to 8, then the str (R12) */
    HQQ_CHECK_EQ_U64(rtu_le32(buf + 8), 10);
    HQQ_CHECK(memcmp(buf + 12, "over limit\0", 11) == 0);
    HQQ_CHECK_EQ_INT(buf[4], 0);

    wr_init(&w, buf, sizeof buf);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_put_detail(&w, ""), HQQRPC_F_DETAIL); /* an empty string is a present D */
    HQQ_CHECK_EQ_INT(w.off, 5);

    memset(big, 'x', sizeof big);
    big[1999] = 0;
    wr_init(&w, buf, sizeof buf);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_put_detail(&w, big), HQQRPC_F_DETAIL);
    HQQ_CHECK_EQ_U64(rtu_le32(buf), 1024); /* truncated at DETAIL_MAX */
    HQQ_CHECK_EQ_INT(w.off, 4 + 1024 + 1);
    big[1024] = 0; /* exactly DETAIL_MAX */
    wr_init(&w, buf, sizeof buf);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_put_detail(&w, big), HQQRPC_F_DETAIL);
    HQQ_CHECK_EQ_U64(rtu_le32(buf), 1024);

    wr_init(&w, buf, 7);
    HQQ_CHECK_EQ_INT(hqqrpc_wr_put_detail(&w, "abc"), 0); /* does not fit: the error is sticky */
    HQQ_CHECK_EQ_INT(w.err, HQQRPC_E_INTERNAL);
}

/* ---- Status helpers -------------------------------------------------------------------------- */

static void test_status_helpers(void) {
    HQQ_CHECK_EQ_INT(hqqrpc_hook_status(HQQRPC_OK), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(hqqrpc_hook_status(HQQRPC_E_DENIED), HQQRPC_E_DENIED);
    HQQ_CHECK_EQ_INT(hqqrpc_hook_status(0x0100), 0x0100);
    HQQ_CHECK_EQ_INT(hqqrpc_hook_status(0xFFFF), 0xFFFF);
    HQQ_CHECK_EQ_INT(hqqrpc_hook_status(HQQRPC_E_BAD_REQUEST), HQQRPC_E_INTERNAL);
    HQQ_CHECK_EQ_INT(hqqrpc_hook_status(0x00FF), HQQRPC_E_INTERNAL);
    HQQ_CHECK_EQ_INT(hqqrpc_hook_status(0x10000), HQQRPC_E_INTERNAL);
    HQQ_CHECK_EQ_INT(hqqrpc_shim_status(HQQRPC_OK), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(hqqrpc_shim_status(HQQRPC_E_DENIED), HQQRPC_E_INTERNAL);
    HQQ_CHECK_EQ_INT(hqqrpc_shim_status(0x0123), 0x0123);
    HQQ_CHECK_EQ_INT(hqqrpc_shim_status(9), HQQRPC_E_INTERNAL);
    HQQ_CHECK(strcmp(hqqrpc_strerror(HQQRPC_E_DENIED), "denied") == 0);
    HQQ_CHECK(strcmp(hqqrpc_strerror(HQQRPC_OK), "ok") == 0);
    HQQ_CHECK(hqqrpc_strerror(0x0150) != NULL && hqqrpc_strerror(77) != NULL && hqqrpc_strerror(0xFFFFFFFFu) != NULL);
}

int main(void) {
    use_unused_vectors();
    test_vector_session();
    test_vector_digest_mismatch();
    test_vector_wrap();
    test_vector_chunking();
    test_hello_statuses();
    test_hello_total();
    test_hello_grants();
    test_granted_message_limit();
    test_frame_violations();
    test_total_above_max_message();
    test_assembly_violations();
    test_request_ids();
    test_unknown_op();
    test_bad_bodies();
    test_rd_take();
    test_rd_count();
    test_rd_str();
    test_rd_bytes_array();
    test_wr_fixed_and_padding();
    test_wr_capacity_and_sticky();
    test_wr_str_bytes();
    test_wr_array_and_span();
    test_wr_detail();
    test_status_helpers();
    return HQQ_TEST_RESULT("rpc");
}
