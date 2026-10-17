/*
 * test_rpc_calls: what the runtime does around a call (BINDGEN.md 4.6.6, 4.7), with a hand-written
 * interface whose operations misbehave in every way a glue or hook can:
 *
 *  - resp_begin in place versus in scratch (HDR + body_cap equal to S and S + 1) and the chunking
 *    of a scratch response;
 *  - the held response reservation: every non-success path after resp_begin produces exactly one
 *    error frame and no tombstone; a teardown with a held reservation aborts it;
 *  - the status filter of the dispatch return, a response committed twice, a missing commit;
 *  - handle registration rolled back after a failed call;
 *  - the execution lock released by the runtime when a glue forgets it;
 *  - request memory: slot versus copy_request versus assembly, alignment, scratch;
 *  - the header copy (a header rewritten during the call changes nothing), callbacks and logging.
 */
#include <stdint.h>
#include <string.h>

#include "hqq_test.h"
#include "rpc_test_util.h"

enum {
    OP_BOUND = 1,    /* u64 cap, u32 len: resp_begin(cap), len bytes of 0x77 */
    OP_DENY,         /* resp_begin(16), return E_DENIED */
    OP_WRFAIL,       /* resp_begin(8), write 9 bytes, return w.err */
    OP_SHIM_BAD,     /* resp_begin(8), return hqqrpc_shim_status(0x50) */
    OP_COMMIT_ERR,   /* resp_begin(16), put_str with an embedded NUL, return resp_commit */
    OP_STATUS,       /* u32 status: return it, no response begun */
    OP_NOCOMMIT,     /* resp_begin(8), return OK without commit */
    OP_TWICE,        /* resp_begin twice */
    OP_COMMIT_FAIL,  /* commit, then return E_INTERNAL */
    OP_HANDLES,      /* u32 mode: register two handles; mode 1: return E_INTERNAL */
    OP_LOCK_LEAK,    /* take the execution lock and return holding it */
    OP_SPAN,         /* span of 8, advance 9 */
    OP_END_HELD,     /* resp_begin(8), end the session, return OK uncommitted */
    OP_PEEK,         /* bytes in-argument: records the body pointer */
    OP_HDR_REWRITE,  /* overwrites the request header in the slot, then answers */
    OP_LIMIT,        /* u64 cap: return the status of resp_begin(cap) */
    OP_SCRATCH,      /* u32 n: scratch blocks */
    OP_BIGERR,       /* resp_begin(1000) (scratch for S = 128), return E_DENIED */
    OP_USER,         /* answers whether hqqrpc_session_user is the on_session_start token */
    OP_COUNT = OP_USER
};

static const char *const g_names[OP_COUNT] = {
    "bound", "deny", "wrfail", "shim_bad", "commit_err", "status", "nocommit", "twice", "commit_fail",
    "handles", "lock_leak", "span", "end_held", "peek", "hdr_rewrite", "limit", "scratch", "bigerr", "user",
};

/* ---- Observations of the operations ----------------------------------------------------------- */

static int g_in_place;
static const uint8_t *g_body_ptr;
static size_t g_body_len;
static int g_twice_second;
static uintptr_t g_destroyed[16];
static int g_ndestroyed;
static int g_scratch_ok;
static void *g_token = &g_in_place;
static int g_starts;
static int g_ends;
static void *g_end_user;
static char g_log[8][160];
static int g_nlog;

static int destroy_res(void *p) {
    g_destroyed[g_ndestroyed++] = (uintptr_t)p;
    return 0;
}

static int destroy_sub(void *p) {
    g_destroyed[g_ndestroyed++] = (uintptr_t)p;
    return 0;
}

static const hqqrpc_handle_type g_types[] = {
    {"Res", 0, destroy_res},
    {"Sub", 1, destroy_sub},
};

static uint64_t le64(const uint8_t *p) {
    return (uint64_t)rtu_le32(p) | ((uint64_t)rtu_le32(p + 4) << 32);
}

static hqqrpc_status calls_dispatch(hqqrpc_call *c) {
    hqqrpc_session *s = c->session;
    hqqrpc_rd r;
    hqqrpc_wr w;
    const uint8_t *g = NULL;
    uint8_t *o;
    hqqrpc_status st;

    hqqrpc_rd_init(&r, c);
    switch (c->op) {
    case OP_BOUND: {
        uint64_t cap;
        uint32_t len;

        if ((st = hqqrpc_rd_take(&r, 8, 16, &g)) != HQQRPC_OK || (st = hqqrpc_rd_end(&r)) != HQQRPC_OK) {
            return st;
        }
        cap = le64(g);
        len = rtu_le32(g + 8);
        if ((st = hqqrpc_resp_begin(c, cap, &w)) != HQQRPC_OK) {
            return st;
        }
        g_in_place = w.p == hqqrpc_test_tx_buffer(s) + RTU_HDR;
        o = hqqrpc_wr_span(&w, 1, len);
        if (o == NULL) {
            return w.err;
        }
        memset(o, 0x77, len);
        if (hqqrpc_wr_advance(&w, len) != HQQRPC_OK) {
            return w.err;
        }
        return hqqrpc_resp_commit(c, &w, 0);
    }
    case OP_DENY:
        if ((st = hqqrpc_resp_begin(c, 16, &w)) != HQQRPC_OK) {
            return st;
        }
        return HQQRPC_E_DENIED;
    case OP_WRFAIL:
        if ((st = hqqrpc_resp_begin(c, 8, &w)) != HQQRPC_OK) {
            return st;
        }
        if (hqqrpc_wr_fixed(&w, 1, 9) == NULL) {
            return w.err;
        }
        return hqqrpc_resp_commit(c, &w, 0);
    case OP_SHIM_BAD:
        if ((st = hqqrpc_resp_begin(c, 8, &w)) != HQQRPC_OK) {
            return st;
        }
        return hqqrpc_shim_status(0x50);
    case OP_COMMIT_ERR:
        if ((st = hqqrpc_resp_begin(c, 16, &w)) != HQQRPC_OK) {
            return st;
        }
        (void)hqqrpc_wr_put_str(&w, "a\0b", 3, 10);
        return hqqrpc_resp_commit(c, &w, 0);
    case OP_STATUS:
        if ((st = hqqrpc_rd_take(&r, 4, 4, &g)) != HQQRPC_OK) {
            return st;
        }
        return rtu_le32(g);
    case OP_NOCOMMIT:
        if ((st = hqqrpc_resp_begin(c, 8, &w)) != HQQRPC_OK) {
            return st;
        }
        return HQQRPC_OK;
    case OP_TWICE: {
        hqqrpc_wr w2;

        if ((st = hqqrpc_resp_begin(c, 8, &w)) != HQQRPC_OK) {
            return st;
        }
        g_twice_second = (int)hqqrpc_resp_begin(c, 8, &w2);
        o = hqqrpc_wr_fixed(&w, 4, 4);
        if (o == NULL) {
            return w.err;
        }
        rtu_put32(o, 0xC0FFEEu);
        return hqqrpc_resp_commit(c, &w, 0);
    }
    case OP_COMMIT_FAIL:
        if ((st = hqqrpc_resp_begin(c, 8, &w)) != HQQRPC_OK) {
            return st;
        }
        if ((st = hqqrpc_resp_commit(c, &w, 0)) != HQQRPC_OK) {
            return st;
        }
        return HQQRPC_E_INTERNAL;
    case OP_HANDLES: {
        uint32_t id1 = 0;
        uint32_t id2 = 0;

        if ((st = hqqrpc_rd_take(&r, 4, 4, &g)) != HQQRPC_OK) {
            return st;
        }
        if ((st = hqqrpc_handle_reserve(s, 2)) != HQQRPC_OK || (st = hqqrpc_resp_begin(c, 8, &w)) != HQQRPC_OK) {
            return st;
        }
        hqqrpc_exec_begin(s);
        st = hqqrpc_handle_put(s, 1, (void *)(uintptr_t)0x1000, 0, &id1);
        if (st == HQQRPC_OK) {
            st = hqqrpc_handle_put(s, 2, (void *)(uintptr_t)0x2000, id1, &id2);
        }
        hqqrpc_exec_end(s);
        if (st != HQQRPC_OK || rtu_le32(g) == 1) {
            return HQQRPC_E_INTERNAL;
        }
        o = hqqrpc_wr_fixed(&w, 4, 8);
        if (o == NULL) {
            return w.err;
        }
        rtu_put32(o, id1);
        rtu_put32(o + 4, id2);
        return hqqrpc_resp_commit(c, &w, 0);
    }
    case OP_LOCK_LEAK:
        if ((st = hqqrpc_resp_begin(c, 8, &w)) != HQQRPC_OK) {
            return st;
        }
        hqqrpc_exec_begin(s);
        return hqqrpc_resp_commit(c, &w, 0); /* and no hqqrpc_exec_end */
    case OP_SPAN:
        if ((st = hqqrpc_resp_begin(c, 32, &w)) != HQQRPC_OK) {
            return st;
        }
        if (hqqrpc_wr_span(&w, 4, 8) == NULL) {
            return w.err;
        }
        if (hqqrpc_wr_advance(&w, 9) != HQQRPC_OK) { /* more than the span */
            return w.err;
        }
        return hqqrpc_resp_commit(c, &w, 0);
    case OP_END_HELD:
        if ((st = hqqrpc_resp_begin(c, 8, &w)) != HQQRPC_OK) {
            return st;
        }
        hqqrpc_session_set_end(s, HQQRPC_END_PEER); /* what a link failure in the middle of a call does */
        return HQQRPC_OK;
    case OP_PEEK: {
        const void *p;
        uint32_t n;

        g_body_ptr = c->body;
        g_body_len = c->body_len;
        if ((st = hqqrpc_rd_bytes(&r, 0, UINT32_MAX, &p, &n)) != HQQRPC_OK || (st = hqqrpc_rd_end(&r)) != HQQRPC_OK) {
            return st;
        }
        if ((st = hqqrpc_resp_begin(c, 0, &w)) != HQQRPC_OK) {
            return st;
        }
        return hqqrpc_resp_commit(c, &w, 0);
    }
    case OP_HDR_REWRITE: {
        uint8_t *hdr = (uint8_t *)(uintptr_t)c->body - RTU_HDR;

        if ((st = hqqrpc_resp_begin(c, 4, &w)) != HQQRPC_OK) {
            return st;
        }
        memset(hdr, 0xFF, RTU_HDR); /* a client rewriting its header while the call runs */
        o = hqqrpc_wr_fixed(&w, 4, 4);
        if (o == NULL) {
            return w.err;
        }
        rtu_put32(o, 0xABCDEF01u);
        return hqqrpc_resp_commit(c, &w, 0);
    }
    case OP_LIMIT:
        if ((st = hqqrpc_rd_take(&r, 8, 8, &g)) != HQQRPC_OK) {
            return st;
        }
        if ((st = hqqrpc_resp_begin(c, le64(g), &w)) != HQQRPC_OK) {
            return st;
        }
        return hqqrpc_resp_commit(c, &w, 0);
    case OP_SCRATCH: {
        static const size_t sizes[] = {0, 1, 15, 17, 100000};
        uint8_t *blk[5];
        size_t i;
        size_t j;

        g_scratch_ok = 1;
        for (i = 0; i < 5; i++) {
            blk[i] = (uint8_t *)hqqrpc_scratch(s, sizes[i]);
            if (blk[i] == NULL || ((uintptr_t)blk[i] % HQQRPC_ALIGN) != 0) {
                g_scratch_ok = 0;
                return HQQRPC_E_INTERNAL;
            }
            memset(blk[i], (int)(0x10 + i), sizes[i] == 0 ? 1 : sizes[i]);
        }
        for (i = 0; i < 5; i++) { /* the blocks stay distinct and intact until the call ends */
            for (j = 0; j < (sizes[i] == 0 ? 1 : sizes[i]); j++) {
                if (blk[i][j] != 0x10 + i) {
                    g_scratch_ok = 0;
                }
            }
        }
        if ((st = hqqrpc_resp_begin(c, 0, &w)) != HQQRPC_OK) {
            return st;
        }
        return hqqrpc_resp_commit(c, &w, 0);
    }
    case OP_BIGERR:
        if ((st = hqqrpc_resp_begin(c, 1000, &w)) != HQQRPC_OK) {
            return st;
        }
        return HQQRPC_E_DENIED;
    case OP_USER:
        if ((st = hqqrpc_resp_begin(c, 4, &w)) != HQQRPC_OK) {
            return st;
        }
        o = hqqrpc_wr_fixed(&w, 4, 4);
        if (o == NULL) {
            return w.err;
        }
        rtu_put32(o, hqqrpc_session_user(s) == g_token ? 1u : 0u);
        return hqqrpc_resp_commit(c, &w, 0);
    default:
        return HQQRPC_E_UNKNOWN_OP;
    }
}

static const hqqrpc_iface g_iface = {
    "calls",
    {0x42, 0x42, 0x42, 0x42, 0x42, 0x42, 0x42, 0x42, 0x42, 0x42, 0x42, 0x42, 0x42, 0x42, 0x42, 0x42,
     0x42, 0x42, 0x42, 0x42, 0x42, 0x42, 0x42, 0x42, 0x42, 0x42, 0x42, 0x42, 0x42, 0x42, 0x42, 0x42},
    OP_COUNT,
    g_names,
    2,
    g_types,
    calls_dispatch,
    HQQRPC_SERIALIZE_SESSION,
};

static void *on_start(void *user, hqqrpc_session *s) {
    (void)user;
    (void)s;
    g_starts++;
    return g_token;
}

static void on_end(void *user, void *session_user, hqqrpc_session *s) {
    (void)user;
    (void)s;
    g_ends++;
    g_end_user = session_user;
}

static void on_log(void *user, int level, const char *msg) {
    (void)user;
    (void)level;
    snprintf(g_log[g_nlog % 8], sizeof g_log[0], "%s", msg);
    g_nlog++;
}

/* ---- Helpers ------------------------------------------------------------------------------------ */

#define OUTCAP 70000u
static uint8_t g_out[OUTCAP];

typedef struct sess {
    hqqrpc_session *s;
    uint32_t id;
} sess;

static void options(hqqrpc_server_options *o, uint32_t S, uint32_t max_message) {
    rtu_opts(o, S, 64, max_message);
    o->on_session_start = on_start;
    o->on_session_end = on_end;
    o->on_log = on_log;
}

static void open_session(sess *x, uint32_t S, uint32_t max_message, int copy_request) {
    hqqrpc_server_options o;

    options(&o, S, max_message);
    o.copy_request = copy_request;
    x->s = rtu_open(&g_iface, &o);
    x->id = 0;
}

/* Sends a request (single frame) and returns the status of the exchange. */
static hqqrpc_status call(sess *x, unsigned op, const void *body, size_t n, size_t *out_len) {
    uint8_t f[RTU_HDR + 512];

    HQQ_REQUIRE(n <= 512);
    x->id++;
    return rtu_x(x->s, f, rtu_req(f, op, x->id, body, n) , g_out, sizeof g_out, out_len);
}

static size_t expected_status(uint8_t *f, unsigned op, uint32_t id, hqqrpc_status st, const char *msg) {
    size_t n = strlen(msg);

    rtu_hdr(f, HQQRPC_K_RESPONSE, 0, op, id, (uint32_t)(n + 5), st);
    rtu_put32(f + 16, (uint32_t)n);
    memcpy(f + 20, msg, n);
    f[20 + n] = 0;
    return 20 + n + 1;
}

static void bound_body(uint8_t *b, uint64_t cap, uint32_t len) {
    rtu_put64(b, cap);
    rtu_put32(b + 8, len);
    rtu_put32(b + 12, 0);
}

/* ---- resp_begin: in place versus scratch (4.7.3) ------------------------------------------------------ */

static void check_response_frames(const char *what, const uint8_t *out, size_t out_len, uint32_t id, unsigned op,
                                  uint32_t total, uint32_t chunk, unsigned want_frames) {
    size_t off = 0;
    uint32_t sent = 0;
    unsigned frames = 0;

    while (off < out_len) {
        uint32_t n = total - sent < chunk ? total - sent : chunk;
        const uint8_t *f = out + off;
        uint32_t i;

        if (off + RTU_HDR + n > out_len) {
            fprintf(stderr, "%s: frame %u is cut\n", what, frames);
            HQQ_CHECK(0);
            return;
        }
        HQQ_CHECK_EQ_INT(f[0], frames == 0 ? HQQRPC_K_RESPONSE : HQQRPC_K_CONT);
        HQQ_CHECK_EQ_INT(f[1], 0);
        HQQ_CHECK_EQ_INT(rtu_le16(f + 2), frames == 0 ? op : 0);
        HQQ_CHECK_EQ_U64(rtu_le32(f + 4), id);
        HQQ_CHECK_EQ_U64(rtu_le32(f + 8), total);
        HQQ_CHECK_EQ_U64(rtu_le32(f + 12), frames == 0 ? 0 : sent);
        for (i = 0; i < n; i++) {
            if (f[RTU_HDR + i] != 0x77) {
                HQQ_CHECK(0);
                break;
            }
        }
        off += RTU_HDR + n;
        sent += n;
        frames++;
    }
    HQQ_CHECK_EQ_INT(sent, total);
    HQQ_CHECK_EQ_INT(frames, want_frames);
}

static void test_bound_in_place_vs_scratch(void) {
    static const struct {
        uint32_t S;
        uint64_t cap;
        uint32_t len;
        int in_place;
        unsigned frames;
    } cases[] = {
        {128, 112, 10, 1, 1},   /* HDR + body_cap == S: in place */
        {128, 113, 10, 0, 1},   /* S + 1: scratch, one frame of the 10 bytes written */
        {128, 0, 0, 1, 1},      /* an empty response */
        {128, 300, 0, 0, 1},    /* scratch, empty */
        {128, 300, 112, 0, 1},  /* exactly CHUNK */
        {128, 300, 113, 0, 2},  /* CHUNK + 1 */
        {128, 300, 224, 0, 2},  /* 2 * CHUNK */
        {128, 300, 225, 0, 3},  /* 2 * CHUNK + 1 */
        {128, 112, 112, 1, 1},  /* in place, full slot */
        {16384, 16368, 16368, 1, 1},
        {16384, 16369, 16368, 0, 1},
        {16384, 40000, 40000, 0, 3},
    };
    size_t i;

    for (i = 0; i < sizeof cases / sizeof cases[0]; i++) {
        sess x;
        uint8_t body[16];
        size_t n = 0;
        uint64_t frames_before;

        open_session(&x, cases[i].S, 0, 0);
        bound_body(body, cases[i].cap, cases[i].len);
        frames_before = hqqrpc_test_frames(x.s);
        g_in_place = -1;
        HQQ_CHECK_EQ_INT(call(&x, OP_BOUND, body, sizeof body, &n), HQQRPC_OK);
        HQQ_CHECK_EQ_INT(g_in_place, cases[i].in_place);
        check_response_frames("bound", g_out, n, x.id, OP_BOUND, cases[i].len, cases[i].S - RTU_HDR, cases[i].frames);
        HQQ_CHECK_EQ_INT(hqqrpc_test_frames(x.s) - frames_before, cases[i].frames);
        HQQ_CHECK_EQ_INT(hqqrpc_test_tombstones(x.s), 0);
        hqqrpc_test_session_free(x.s);
    }
}

/* A writer that is asked for more than the bound it was created with fails the call. */
static void test_bound_exceeded(void) {
    sess x;
    uint8_t body[16];
    uint8_t want[64];
    size_t n = 0;
    size_t wn;

    open_session(&x, 128, 0, 0);
    bound_body(body, 20, 21); /* 21 bytes into a 20-byte bound */
    HQQ_CHECK_EQ_INT(call(&x, OP_BOUND, body, sizeof body, &n), HQQRPC_OK);
    wn = expected_status(want, OP_BOUND, x.id, HQQRPC_E_INTERNAL, "internal error");
    RTU_CHECK_BYTES(g_out, n, want, wn, "writer past its bound");
    HQQ_CHECK_EQ_INT(hqqrpc_test_tombstones(x.s), 0);
    hqqrpc_test_session_free(x.s);
}

/* resp_begin above max_message is E_LIMIT, and holds nothing. */
static void test_resp_limit(void) {
    sess x;
    uint8_t body[8];
    uint8_t want[64];
    size_t n = 0;
    size_t wn;

    open_session(&x, 128, 4096, 0);
    rtu_put64(body, 4097);
    HQQ_CHECK_EQ_INT(call(&x, OP_LIMIT, body, 8, &n), HQQRPC_OK);
    wn = expected_status(want, OP_LIMIT, x.id, HQQRPC_E_LIMIT, "limit exceeded");
    RTU_CHECK_BYTES(g_out, n, want, wn, "body_cap above max_message");
    HQQ_CHECK_EQ_INT(hqqrpc_test_frames(x.s), 2); /* HELLO_REPLY and the error */
    rtu_put64(body, 4096); /* exactly max_message: scratch, empty response */
    HQQ_CHECK_EQ_INT(call(&x, OP_LIMIT, body, 8, &n), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(n, RTU_HDR);
    HQQ_CHECK_EQ_U64(rtu_le32(g_out + 12), 0);
    rtu_put64(body, UINT64_MAX); /* no 64-bit wrap in HDR + body_cap */
    HQQ_CHECK_EQ_INT(call(&x, OP_LIMIT, body, 8, &n), HQQRPC_OK);
    HQQ_CHECK_EQ_U64(rtu_le32(g_out + 12), HQQRPC_E_LIMIT);
    HQQ_CHECK_EQ_INT(hqqrpc_test_tombstones(x.s), 0);
    hqqrpc_test_session_free(x.s);
}

/* ---- Held reservation (4.7.3) --------------------------------------------------------------------- */

static void test_held_paths(void) {
    static const struct {
        unsigned op;
        hqqrpc_status st;
        const char *msg;
    } cases[] = {
        {OP_DENY, HQQRPC_E_DENIED, "denied"},
        {OP_WRFAIL, HQQRPC_E_INTERNAL, "internal error"},
        {OP_SHIM_BAD, HQQRPC_E_INTERNAL, "internal error"},
        {OP_COMMIT_ERR, HQQRPC_E_INTERNAL, "internal error"},
        {OP_NOCOMMIT, HQQRPC_E_INTERNAL, "internal error"},
        {OP_SPAN, HQQRPC_E_INTERNAL, "internal error"},
        {OP_BIGERR, HQQRPC_E_DENIED, "denied"},
    };
    size_t i;
    int round;

    for (round = 0; round < 3; round++) {
        sess x;

        open_session(&x, 128, 0, 0);
        for (i = 0; i < sizeof cases / sizeof cases[0]; i++) {
            uint8_t want[64];
            size_t n = 0;
            size_t wn;
            uint64_t before = hqqrpc_test_frames(x.s);

            HQQ_CHECK_EQ_INT(call(&x, cases[i].op, NULL, 0, &n), HQQRPC_OK);
            wn = expected_status(want, cases[i].op, x.id, cases[i].st, cases[i].msg);
            RTU_CHECK_BYTES(g_out, n, want, wn, g_names[cases[i].op - 1]);
            HQQ_CHECK_EQ_INT(hqqrpc_test_frames(x.s) - before, 1); /* exactly one frame ... */
            HQQ_CHECK_EQ_INT(hqqrpc_test_tombstones(x.s), 0);      /* ... and no tombstone */
        }
        HQQ_CHECK_EQ_INT(hqqrpc_test_ended(x.s), 0);
        hqqrpc_test_session_free(x.s);
    }
}

static void test_status_filter(void) {
    static const struct {
        uint32_t in;
        uint32_t out;
    } cases[] = {
        {1, 1}, {2, 2}, {3, 3}, {4, 4}, {5, 5}, {6, 6},
        {7, 6}, {8, 6}, {9, 6}, {0xFF, 6},
        {0x100, 0x100}, {0x1234, 0x1234}, {0xFFFF, 0xFFFF},
        {0x10000, 6}, {0xFFFFFFFFu, 6},
    };
    sess x;
    size_t i;

    open_session(&x, 16384, 0, 0);
    for (i = 0; i < sizeof cases / sizeof cases[0]; i++) {
        uint8_t body[4];
        uint8_t want[64];
        size_t n = 0;
        size_t wn;
        const char *msg = cases[i].out >= 0x100 ? "" : hqqrpc_strerror(cases[i].out);

        rtu_put32(body, cases[i].in);
        HQQ_CHECK_EQ_INT(call(&x, OP_STATUS, body, 4, &n), HQQRPC_OK);
        wn = expected_status(want, OP_STATUS, x.id, cases[i].out, msg);
        RTU_CHECK_BYTES(g_out, n, want, wn, "status filter");
    }
    HQQ_CHECK_EQ_INT(hqqrpc_test_tombstones(x.s), 0);
    hqqrpc_test_session_free(x.s);
}

/* One response per request, whatever the dispatch return says after it committed. */
static void test_commit_then_failure(void) {
    sess x;
    size_t n = 0;
    uint64_t before;

    open_session(&x, 16384, 0, 0);
    before = hqqrpc_test_frames(x.s);
    HQQ_CHECK_EQ_INT(call(&x, OP_COMMIT_FAIL, NULL, 0, &n), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(n, RTU_HDR); /* the committed (empty, successful) response only */
    HQQ_CHECK_EQ_U64(rtu_le32(g_out + 12), 0);
    HQQ_CHECK_EQ_INT(hqqrpc_test_frames(x.s) - before, 1);
    HQQ_CHECK_EQ_INT(hqqrpc_test_tombstones(x.s), 0);
    hqqrpc_test_session_free(x.s);
}

/* A second resp_begin of the same call fails and does not disturb the first. */
static void test_begin_twice(void) {
    sess x;
    size_t n = 0;

    open_session(&x, 16384, 0, 0);
    g_twice_second = 0;
    HQQ_CHECK_EQ_INT(call(&x, OP_TWICE, NULL, 0, &n), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(g_twice_second, HQQRPC_E_INTERNAL);
    HQQ_CHECK_EQ_INT(n, RTU_HDR + 4);
    HQQ_CHECK_EQ_U64(rtu_le32(g_out + 16), 0xC0FFEEu);
    HQQ_CHECK_EQ_INT(hqqrpc_test_tombstones(x.s), 0);
    hqqrpc_test_session_free(x.s);
}

/* A link failure in the middle of a call aborts the held reservation (a tombstone) and sends nothing. */
static void test_teardown_with_held_reservation(void) {
    sess x;
    size_t n = 99;
    uint64_t before;

    open_session(&x, 16384, 0, 0);
    before = hqqrpc_test_frames(x.s);
    HQQ_CHECK_EQ_INT(call(&x, OP_END_HELD, NULL, 0, &n), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(n, 0);
    HQQ_CHECK_EQ_INT(hqqrpc_test_frames(x.s) - before, 0);
    HQQ_CHECK_EQ_INT(hqqrpc_test_tombstones(x.s), 1);
    HQQ_CHECK(hqqrpc_test_ended(x.s));
    hqqrpc_test_session_free(x.s);
}

/* ---- Handle registration of a failed call (4.6.6, 5.1) ------------------------------------------------- */

static void test_handles_rollback(void) {
    sess x;
    uint8_t body[4];
    uint8_t want[64];
    size_t n = 0;
    size_t wn;

    open_session(&x, 16384, 0, 0);
    g_ndestroyed = 0;
    rtu_put32(body, 1); /* mode 1: fail after both handles were registered */
    HQQ_CHECK_EQ_INT(call(&x, OP_HANDLES, body, 4, &n), HQQRPC_OK);
    wn = expected_status(want, OP_HANDLES, x.id, HQQRPC_E_INTERNAL, "internal error");
    RTU_CHECK_BYTES(g_out, n, want, wn, "failed registration");
    HQQ_CHECK_EQ_INT(g_ndestroyed, 2);
    HQQ_CHECK_EQ_U64(g_destroyed[0], 0x2000); /* the child first */
    HQQ_CHECK_EQ_U64(g_destroyed[1], 0x1000);
    HQQ_CHECK_EQ_INT(hqqrpc_test_live_handles(x.s), 0);

    rtu_put32(body, 0); /* mode 0: success; the freed indices are reused with generation 2 */
    HQQ_CHECK_EQ_INT(call(&x, OP_HANDLES, body, 4, &n), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(n, RTU_HDR + 8);
    HQQ_CHECK_EQ_U64(rtu_le32(g_out + 16), 0x00020001u);
    HQQ_CHECK_EQ_U64(rtu_le32(g_out + 20), 0x00020002u);
    HQQ_CHECK_EQ_INT(hqqrpc_test_live_handles(x.s), 2);
    HQQ_CHECK_EQ_INT(g_ndestroyed, 2);

    /* teardown destroys what a successful call registered, newest first */
    {
        uint8_t close[RTU_HDR];

        rtu_hdr(close, HQQRPC_K_CLOSE, 0, 0, 0, 0, 0);
        HQQ_CHECK_EQ_INT(rtu_x(x.s, close, RTU_HDR, g_out, sizeof g_out, &n), HQQRPC_OK);
        HQQ_CHECK_EQ_INT(n, RTU_HDR);
        HQQ_CHECK_EQ_INT(g_out[0], HQQRPC_K_CLOSE_ACK);
        HQQ_CHECK_EQ_U64(rtu_le32(g_out + 12), 0);
    }
    HQQ_CHECK_EQ_INT(g_ndestroyed, 4);
    HQQ_CHECK_EQ_U64(g_destroyed[2], 0x2000);
    HQQ_CHECK_EQ_U64(g_destroyed[3], 0x1000);
    hqqrpc_test_session_free(x.s);
    HQQ_CHECK_EQ_INT(g_ndestroyed, 4); /* nothing is destroyed twice */
}

/* ---- Execution lock (8.1) ------------------------------------------------------------------------------ */

static void test_lock_released_by_runtime(void) {
    sess x;
    size_t n = 0;
    int logs_before;

    open_session(&x, 16384, 0, 0);
    logs_before = g_nlog;
    HQQ_CHECK_EQ_INT(call(&x, OP_LOCK_LEAK, NULL, 0, &n), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(n, RTU_HDR);
    HQQ_CHECK_EQ_INT(g_nlog - logs_before, 1);
    HQQ_CHECK(strstr(g_log[(g_nlog - 1) % 8], "execution lock") != NULL);
    /* the lock is free again: another call enters and leaves it */
    HQQ_CHECK_EQ_INT(call(&x, OP_HANDLES, (uint8_t[]){0, 0, 0, 0}, 4, &n), HQQRPC_OK);
    hqqrpc_test_session_free(x.s);
}

/* ---- Request memory (4.7.2) ----------------------------------------------------------------------------- */

static void test_request_memory(void) {
    uint8_t body[4 + 64];
    sess x;
    size_t n = 0;
    const uint8_t *rx;

    memset(body, 0xEE, sizeof body);
    rtu_put32(body, 64);

    /* single frame, no copy: the decoder works on the slot */
    open_session(&x, 16384, 0, 0);
    rx = hqqrpc_test_rx_buffer(x.s);
    HQQ_CHECK_EQ_INT(call(&x, OP_PEEK, body, sizeof body, &n), HQQRPC_OK);
    HQQ_CHECK(g_body_ptr == rx + RTU_HDR);
    HQQ_CHECK_EQ_INT(((uintptr_t)g_body_ptr % HQQRPC_ALIGN), 0);
    HQQ_CHECK_EQ_INT(g_body_len, sizeof body);
    hqqrpc_test_session_free(x.s);

    /* copy_request: a private, ALIGN-aligned copy */
    open_session(&x, 16384, 0, 1);
    rx = hqqrpc_test_rx_buffer(x.s);
    HQQ_CHECK_EQ_INT(call(&x, OP_PEEK, body, sizeof body, &n), HQQRPC_OK);
    HQQ_CHECK(g_body_ptr < rx || g_body_ptr >= rx + 16384);
    HQQ_CHECK_EQ_INT(((uintptr_t)g_body_ptr % HQQRPC_ALIGN), 0);
    HQQ_CHECK_EQ_INT(g_body_len, sizeof body);
    hqqrpc_test_session_free(x.s);

    /* a multi-frame request is assembled into ALIGN-aligned scratch: S = 128, 200 body bytes */
    {
        uint8_t f[128];
        uint8_t big[204];

        open_session(&x, 128, 0, 0);
        rx = hqqrpc_test_rx_buffer(x.s);
        memset(big, 0x11, sizeof big);
        rtu_put32(big, 200);
        rtu_hdr(f, HQQRPC_K_REQUEST, 0, OP_PEEK, 1, 204, 0);
        memcpy(f + RTU_HDR, big, 112);
        HQQ_CHECK_EQ_INT(rtu_x(x.s, f, 128, g_out, sizeof g_out, &n), HQQRPC_OK);
        HQQ_CHECK_EQ_INT(n, 0);
        rtu_hdr(f, HQQRPC_K_CONT, 0, 0, 1, 204, 112);
        memcpy(f + RTU_HDR, big + 112, 92);
        HQQ_CHECK_EQ_INT(rtu_x(x.s, f, RTU_HDR + 92, g_out, sizeof g_out, &n), HQQRPC_OK);
        HQQ_CHECK_EQ_INT(n, RTU_HDR);
        HQQ_CHECK_EQ_U64(rtu_le32(g_out + 12), 0);
        HQQ_CHECK(g_body_ptr < rx || g_body_ptr >= rx + 128);
        HQQ_CHECK_EQ_INT(((uintptr_t)g_body_ptr % HQQRPC_ALIGN), 0);
        HQQ_CHECK_EQ_INT(g_body_len, 204);
        hqqrpc_test_session_free(x.s);
    }
}

static void test_scratch(void) {
    sess x;
    size_t n = 0;
    int i;

    open_session(&x, 16384, 0, 0);
    for (i = 0; i < 3; i++) { /* the spare block is reused and released again */
        g_scratch_ok = 0;
        HQQ_CHECK_EQ_INT(call(&x, OP_SCRATCH, NULL, 0, &n), HQQRPC_OK);
        HQQ_CHECK_EQ_INT(g_scratch_ok, 1);
        HQQ_CHECK_EQ_INT(n, RTU_HDR);
    }
    hqqrpc_test_session_free(x.s);
}

/* The runtime keeps its own copy of the header: a client that rewrites the header in its slot while the
 * call runs changes neither the id and op of the response nor the length (4.4 rule 8). */
static void test_header_copy(void) {
    sess x;
    size_t n = 0;

    open_session(&x, 16384, 0, 0);
    HQQ_CHECK_EQ_INT(call(&x, OP_HDR_REWRITE, NULL, 0, &n), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(n, RTU_HDR + 4);
    HQQ_CHECK_EQ_INT(g_out[0], HQQRPC_K_RESPONSE);
    HQQ_CHECK_EQ_INT(rtu_le16(g_out + 2), OP_HDR_REWRITE);
    HQQ_CHECK_EQ_U64(rtu_le32(g_out + 4), x.id);
    HQQ_CHECK_EQ_U64(rtu_le32(g_out + 8), 4);
    HQQ_CHECK_EQ_U64(rtu_le32(g_out + 12), 0);
    HQQ_CHECK_EQ_U64(rtu_le32(g_out + 16), 0xABCDEF01u);
    /* the next request is judged by the id of the copy as well */
    HQQ_CHECK_EQ_INT(call(&x, OP_HDR_REWRITE, NULL, 0, &n), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(hqqrpc_test_ended(x.s), 0);
    hqqrpc_test_session_free(x.s);
}

/* ---- Callbacks and logging (5.5) -------------------------------------------------------------------------- */

static void test_callbacks(void) {
    sess x;
    size_t n = 0;
    uint8_t close[RTU_HDR];

    g_starts = 0;
    g_ends = 0;
    g_end_user = NULL;
    open_session(&x, 16384, 0, 0);
    HQQ_CHECK_EQ_INT(g_starts, 1);
    HQQ_CHECK_EQ_INT(g_ends, 0);
    HQQ_CHECK_EQ_INT(call(&x, OP_USER, NULL, 0, &n), HQQRPC_OK);
    HQQ_CHECK_EQ_U64(rtu_le32(g_out + 16), 1); /* hqqrpc_session_user returns what on_session_start returned */
    rtu_hdr(close, HQQRPC_K_CLOSE, 0, 0, 0, 0, 0);
    HQQ_CHECK_EQ_INT(rtu_x(x.s, close, RTU_HDR, g_out, sizeof g_out, &n), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(g_ends, 1); /* before the session is freed */
    HQQ_CHECK(g_end_user == g_token);
    hqqrpc_test_session_free(x.s);
    HQQ_CHECK_EQ_INT(g_ends, 1);

    /* a session that is freed while running ends once too */
    open_session(&x, 16384, 0, 0);
    hqqrpc_test_session_free(x.s);
    HQQ_CHECK_EQ_INT(g_starts, 2);
    HQQ_CHECK_EQ_INT(g_ends, 2);

    /* a violation is logged */
    g_nlog = 0;
    open_session(&x, 16384, 0, 0);
    {
        uint8_t f[RTU_HDR];

        rtu_hdr(f, 0x7F, 0, 0, 0, 0, 0);
        HQQ_CHECK_EQ_INT(rtu_x(x.s, f, RTU_HDR, g_out, sizeof g_out, &n), HQQRPC_E_PROTOCOL);
    }
    HQQ_CHECK(g_nlog >= 1 && strstr(g_log[0], "protocol violation") != NULL);
    hqqrpc_test_session_free(x.s);
}

int main(void) {
    test_bound_in_place_vs_scratch();
    test_bound_exceeded();
    test_resp_limit();
    test_held_paths();
    test_status_filter();
    test_commit_then_failure();
    test_begin_twice();
    test_teardown_with_held_reservation();
    test_handles_rollback();
    test_lock_released_by_runtime();
    test_request_memory();
    test_scratch();
    test_header_copy();
    test_callbacks();
    return HQQ_TEST_RESULT("rpc_calls");
}
