/*
 * test_rpc_fuzz: a deterministic mutational fuzz of the frame receiver, the handshake and the
 * body decoders of the C runtime (BINDGEN.md 10.2, "test_rpc_fuzz").
 *
 * Seeds are the golden request vectors of 4.14 (the whole session, HELLO to CLOSE, and the
 * oversize pair at S = 128) plus structured random requests. Each iteration applies one to three
 * mutations (bit flips, byte and field overwrites, truncation and extension, dropped, duplicated,
 * swapped and random frames) and feeds the frames through hqqrpc_test_exchange until the session
 * ends. Properties:
 *
 *  - no ASan or UBSan report, no leak, every handle destroyed when the session is freed;
 *  - every outcome is a response, a protocol-violation teardown or a session end; a protocol
 *    violation answers exactly the E_PROTOCOL final frame of 5.4;
 *  - the frames published are well formed: header, flags, status ranges, total against the frame
 *    lengths, CONT chunks, the id and op of the request they answer;
 *  - a request rejected before the call (E_UNKNOWN_OP, E_BAD_REQUEST, E_BAD_HANDLE, E_LIMIT,
 *    E_DENIED) never enters the library;
 *  - the handle table never exceeds max_handles.
 *
 * HQQ_FUZZ_ITERS overrides the iteration count (default 200000, the figure of 10.2);
 * HQQ_FUZZ_SEED the seed.
 */
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

#include "hqq_test.h"
#include "rpc_ml.h"
#include "rpc_ml_server.h"
#include "rpc_test_util.h"
#include "rpc_vectors.h"

#define MAX_FRAMES 24u
#define FRAME_CAP (16384u + 64u)
#define MAX_MESSAGE 65536u
#define OUT_CAP 260000u

static uint8_t g_frames[MAX_FRAMES][FRAME_CAP];
static size_t g_lens[MAX_FRAMES];
static unsigned g_nframes;
static uint8_t g_out[OUT_CAP];

static uint64_t g_rng = UINT64_C(0x2545F4914F6CDD1D);

static uint32_t rnd(void) {
    uint64_t x = g_rng;

    x ^= x << 13;
    x ^= x >> 7;
    x ^= x << 17;
    g_rng = x;
    return (uint32_t)(x >> 24);
}

static uint32_t rnd_below(uint32_t n) {
    return n == 0 ? 0 : rnd() % n;
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

static void add_frame(const uint8_t *f, size_t n) {
    HQQ_REQUIRE(g_nframes < MAX_FRAMES && n <= FRAME_CAP);
    memcpy(g_frames[g_nframes], f, n);
    g_lens[g_nframes++] = n;
}

static void add_vec(const char *name) {
    const hqq_rpcvec_frame *v = vec(name);

    add_frame(v->data, v->len);
}

/* ---- Scripts (the unmutated frame sequences) ---------------------------------------------------- */

enum { SCRIPT_SESSION, SCRIPT_CHUNKED, SCRIPT_RANDOM, SCRIPT_COUNT };

static uint32_t script_S(int script) {
    return script == SCRIPT_CHUNKED ? 128u : 16384u;
}

static void build_session_script(void) {
    char name[16];
    int i;

    g_nframes = 0;
    add_vec("HELLO");
    for (i = 1; i <= 13; i++) {
        snprintf(name, sizeof name, "R%d_REQ", i);
        add_vec(name);
    }
    add_vec("CLOSE");
}

static void build_chunked_script(void) {
    uint8_t f[RTU_HDR + 64];
    uint8_t body[8];

    g_nframes = 0;
    add_frame(f, rtu_hello(f, &microlib_iface, 128, 64, 0, 0));
    add_vec("C_REQ_1");
    add_vec("C_REQ_2");
    rtu_put32(body, 9);
    rtu_put32(body + 4, 10);
    add_frame(f, rtu_req(f, 1, 2, body, 8));
    add_frame(f, rtu_req(f, 1, 3, body, 8));
    rtu_hdr(f, HQQRPC_K_CLOSE, 0, 0, 0, 0, 0);
    add_frame(f, RTU_HDR);
}

/* HELLO, then requests with random operations and bodies. Handle ids that exist after the first
 * requests (0x00010001, 0x00010002) are woven into the bodies so that the lookups are reached. */
static void build_random_script(void) {
    uint8_t f[RTU_HDR + 128];
    uint8_t body[128];
    unsigned i;
    uint32_t id = 1;

    g_nframes = 0;
    add_frame(f, rtu_hello(f, &microlib_iface, 16384, 64, 0, 0));
    /* ml_counter_new(7), ml_cursor_open(c, 1) so that two handles exist */
    memset(body, 0, sizeof body);
    body[0] = 7;
    add_frame(f, rtu_req(f, 2, id++, body, 8));
    rtu_put32(body, 0x00010001u);
    rtu_put32(body + 4, 1);
    add_frame(f, rtu_req(f, 5, id++, body, 8));
    for (i = 0; i < 14; i++) {
        size_t n = rnd_below(48);
        size_t j;
        unsigned op = rnd_below(14);

        for (j = 0; j < n; j++) {
            body[j] = (uint8_t)rnd();
        }
        if (n >= 8 && rnd_below(3) != 0) {
            rtu_put32(body + (rnd_below(2) ? 0 : 4), rnd_below(2) ? 0x00010001u : 0x00010002u);
        }
        if (n >= 4 && rnd_below(3) == 0) {
            rtu_put32(body, rnd_below(6)); /* small counts and lengths */
        }
        add_frame(f, rtu_req(f, op, id++, body, n));
    }
    rtu_hdr(f, HQQRPC_K_CLOSE, 0, 0, 0, 0, 0);
    add_frame(f, RTU_HDR);
}

/* ---- Mutations ---------------------------------------------------------------------------------- */

static const uint32_t k_interesting[] = {0, 1, 2, 3, 4, 7, 8, 15, 16, 17, 63, 64, 65, 111, 112, 113, 127, 128,
                                          255, 256, 4095, 4096, 16367, 16368, 16369, 65535, 65536, 65537,
                                          0x7FFFFFFFu, 0x80000000u, 0xFFFFFFFEu, 0xFFFFFFFFu};

static void mutate_frame(unsigned fi, uint32_t cap_len) {
    uint8_t *f = g_frames[fi];
    size_t *n = &g_lens[fi];

    switch (rnd_below(10)) {
    case 0: /* flip a bit */
        if (*n != 0) {
            f[rnd_below((uint32_t)*n)] ^= (uint8_t)(1u << rnd_below(8));
        }
        break;
    case 1: /* random byte */
        if (*n != 0) {
            f[rnd_below((uint32_t)*n)] = (uint8_t)rnd();
        }
        break;
    case 2: /* 0 or 0xFF byte */
        if (*n != 0) {
            f[rnd_below((uint32_t)*n)] = rnd_below(2) ? 0 : 0xFF;
        }
        break;
    case 3: /* a header byte */
        if (*n >= RTU_HDR) {
            f[rnd_below(RTU_HDR)] = (uint8_t)rnd();
        }
        break;
    case 4: /* a 32-bit header field */
        if (*n >= RTU_HDR) {
            rtu_put32(f + 4 + 4 * rnd_below(3), k_interesting[rnd_below(sizeof k_interesting / sizeof k_interesting[0])]);
        }
        break;
    case 5: /* a 32-bit value anywhere */
        if (*n >= 4) {
            rtu_put32(f + rnd_below((uint32_t)*n - 3), k_interesting[rnd_below(sizeof k_interesting / sizeof k_interesting[0])]);
        }
        break;
    case 6: /* truncate */
        *n = rnd_below((uint32_t)*n + 1);
        break;
    case 7: { /* extend with random bytes */
        size_t extra = 1 + rnd_below(64);
        size_t i;

        if (*n + extra > cap_len) {
            extra = cap_len - *n;
        }
        for (i = 0; i < extra; i++) {
            f[*n + i] = (uint8_t)rnd();
        }
        *n += extra;
        break;
    }
    case 8: /* kind and flags */
        if (*n >= 2) {
            f[0] = rnd_below(2) ? (uint8_t)rnd_below(12) : (uint8_t)rnd();
            f[1] = rnd_below(2) ? (uint8_t)rnd_below(4) : (uint8_t)rnd();
        }
        break;
    default: /* duplicate a chunk of the body over another place */
        if (*n > RTU_HDR + 2) {
            size_t len = 1 + rnd_below((uint32_t)(*n - RTU_HDR) / 2);
            size_t from = RTU_HDR + rnd_below((uint32_t)(*n - RTU_HDR - len + 1));
            size_t to = RTU_HDR + rnd_below((uint32_t)(*n - RTU_HDR - len + 1));

            memmove(f + to, f + from, len);
        }
        break;
    }
}

static void mutate_script(void) {
    unsigned k = 1 + rnd_below(3);

    while (k-- > 0 && g_nframes != 0) {
        unsigned fi = rnd_below(g_nframes);

        switch (rnd_below(8)) {
        case 0: /* drop a frame */
            memmove(g_frames[fi], g_frames[fi + 1], (size_t)(g_nframes - fi - 1) * FRAME_CAP);
            memmove(&g_lens[fi], &g_lens[fi + 1], (size_t)(g_nframes - fi - 1) * sizeof g_lens[0]);
            g_nframes--;
            break;
        case 1: /* duplicate a frame */
            if (g_nframes < MAX_FRAMES) {
                memmove(g_frames[fi + 1], g_frames[fi], (size_t)(g_nframes - fi) * FRAME_CAP);
                memmove(&g_lens[fi + 1], &g_lens[fi], (size_t)(g_nframes - fi) * sizeof g_lens[0]);
                g_nframes++;
            }
            break;
        case 2: { /* swap two frames */
            static uint8_t tmp[FRAME_CAP];
            unsigned fj = rnd_below(g_nframes);
            size_t t = g_lens[fi];

            memcpy(tmp, g_frames[fi], FRAME_CAP);
            memcpy(g_frames[fi], g_frames[fj], FRAME_CAP);
            memcpy(g_frames[fj], tmp, FRAME_CAP);
            g_lens[fi] = g_lens[fj];
            g_lens[fj] = t;
            break;
        }
        case 3: /* a random frame inserted */
            if (g_nframes < MAX_FRAMES) {
                size_t n = rnd_below(80);
                size_t i;

                memmove(g_frames[fi + 1], g_frames[fi], (size_t)(g_nframes - fi) * FRAME_CAP);
                memmove(&g_lens[fi + 1], &g_lens[fi], (size_t)(g_nframes - fi) * sizeof g_lens[0]);
                for (i = 0; i < n; i++) {
                    g_frames[fi][i] = (uint8_t)rnd();
                }
                g_lens[fi] = n;
                g_nframes++;
            }
            break;
        default:
            mutate_frame(fi, FRAME_CAP);
            break;
        }
    }
}

/* ---- Output checks ---------------------------------------------------------------------------- */

typedef struct outcheck {
    uint32_t S;
    uint32_t max_message;
    unsigned messages;     /* complete messages in the output */
    uint32_t first_aux;
    uint32_t first_id;
    unsigned first_kind;
    int ok;
} outcheck;

static void out_fail(const char *why, const uint8_t *out, size_t n) {
    fprintf(stderr, "fuzz: bad output: %s\n", why);
    rtu_dump("output", out, n);
    HQQ_CHECK(0);
}

/* Parses the frames in out and checks every rule of 4.2 to 4.4 and 4.10 that concerns the server's
 * own output. */
static void check_output(outcheck *c, const uint8_t *out, size_t n) {
    size_t off = 0;
    uint32_t chunk = c->S - RTU_HDR;

    c->messages = 0;
    c->ok = 1;
    while (off < n) {
        const uint8_t *f = out + off;
        unsigned kind;
        unsigned flags;
        uint32_t total;
        uint32_t aux;
        uint32_t id;
        uint32_t first_body;
        uint32_t sent;

        if (n - off < RTU_HDR) {
            out_fail("short header", out, n);
            return;
        }
        kind = f[0];
        flags = f[1];
        id = rtu_le32(f + 4);
        total = rtu_le32(f + 8);
        aux = rtu_le32(f + 12);
        if (kind != HQQRPC_K_RESPONSE && kind != HQQRPC_K_HELLO_REPLY && kind != HQQRPC_K_CLOSE_ACK) {
            out_fail("unexpected kind", out, n);
            return;
        }
        if (kind == HQQRPC_K_RESPONSE) {
            if ((flags & ~3u) != 0 || ((flags & 1u) != 0 && (flags & 2u) == 0) || total > c->max_message) {
                out_fail("flags or total", out, n);
                return;
            }
            if (id == 0 ? (aux != HQQRPC_E_PROTOCOL && aux != HQQRPC_E_SHUTTING_DOWN)
                        : (aux > HQQRPC_E_INTERNAL && (aux < HQQRPC_E_APP_MIN || aux > HQQRPC_E_APP_MAX))) {
                out_fail("status out of range", out, n);
                return;
            }
            if (aux != 0 && (flags != 0 || total > HQQRPC_ERRBODY_MAX)) {
                out_fail("error response with flags or a long body", out, n);
                return;
            }
        } else {
            if (flags != 0) {
                out_fail("flags on a session frame", out, n);
                return;
            }
        }
        first_body = total < chunk ? total : chunk;
        if (kind == HQQRPC_K_HELLO_REPLY && total != 64) {
            out_fail("HELLO_REPLY length", out, n);
            return;
        }
        if (kind == HQQRPC_K_CLOSE_ACK && total != 0) {
            out_fail("CLOSE_ACK length", out, n);
            return;
        }
        if (n - off < (size_t)RTU_HDR + first_body) {
            out_fail("frame cut", out, n);
            return;
        }
        if (c->messages == 0) {
            c->first_aux = aux;
            c->first_id = id;
            c->first_kind = kind;
        }
        off += RTU_HDR + first_body;
        sent = first_body;
        while (sent < total) { /* CONT frames of the same message */
            uint32_t want = total - sent < chunk ? total - sent : chunk;
            const uint8_t *g = out + off;

            if (n - off < (size_t)RTU_HDR + want || g[0] != HQQRPC_K_CONT || g[1] != 0 || rtu_le32(g + 4) != id ||
                rtu_le32(g + 8) != total || rtu_le32(g + 12) != sent) {
                out_fail("CONT frame", out, n);
                return;
            }
            off += RTU_HDR + want;
            sent += want;
        }
        c->messages++;
    }
}

/* ---- The loop ---------------------------------------------------------------------------------- */

/* Outcome counters: they show that the mutations reach every kind of answer. */
static unsigned long g_stat_violation;
static unsigned long g_stat_ok;
static unsigned long g_stat_error;
static unsigned long g_stat_app;
static unsigned long g_stat_silent; /* first frame or CONT of a multi-frame message */
static unsigned long g_stat_handshake_fail;

static void run_one(int script) {
    hqqrpc_server_options o;
    hqqrpc_session *s;
    outcheck oc;
    uint8_t viol[64];
    unsigned i;
    uint32_t S = script_S(script);

    rtu_opts(&o, S, 64, MAX_MESSAGE);
    o.max_handles = 8; /* small, so that E_LIMIT and reuse are reached */
    s = hqqrpc_test_session_new(&microlib_iface, &o);
    HQQ_REQUIRE(s != NULL);
    ml_test_reset();
    oc.S = S;
    oc.max_message = MAX_MESSAGE;
    rtu_hdr(viol, HQQRPC_K_RESPONSE, 0, 0, 0, 23, HQQRPC_E_PROTOCOL);
    rtu_put32(viol + 16, 18);
    memcpy(viol + 20, "protocol violation", 18);
    viol[38] = 0;

    for (i = 0; i < g_nframes; i++) {
        size_t out_len = 0;
        hqqrpc_status st;
        long entries_before = ml_test_entries();
        const uint8_t *f = g_frames[i];
        size_t n = g_lens[i];
        unsigned in_kind = n >= 1 ? f[0] : 0;
        uint32_t in_id = n >= RTU_HDR ? rtu_le32(f + 4) : 0;
        unsigned in_op = n >= RTU_HDR ? rtu_le16(f + 2) : 0;
        uint32_t in_total = n >= RTU_HDR ? rtu_le32(f + 8) : 0;

        st = hqqrpc_test_exchange(s, f, n, g_out, sizeof g_out, &out_len);
        if (st != HQQRPC_OK && st != HQQRPC_E_PROTOCOL && !(st == HQQRPC_E_LIMIT && n > S)) {
            fprintf(stderr, "fuzz: unexpected status %u for frame %u (%zu bytes)\n", (unsigned)st, i, n);
            HQQ_CHECK(0);
            break;
        }
        if (st == HQQRPC_E_LIMIT) {
            HQQ_CHECK_EQ_INT(out_len, 0);
            continue; /* the frame did not fit a slot: it never reaches the server */
        }
        check_output(&oc, g_out, out_len);
        if (st == HQQRPC_E_PROTOCOL) {
            g_stat_violation++;
            HQQ_CHECK_EQ_INT(out_len, 39);
            HQQ_CHECK(out_len == 39 && memcmp(g_out, viol, 39) == 0);
            HQQ_CHECK(hqqrpc_test_ended(s));
            break;
        }
        HQQ_CHECK(oc.messages <= 1);
        if (oc.messages == 1) {
            if (oc.first_kind == HQQRPC_K_HELLO_REPLY) {
                g_stat_handshake_fail += oc.first_aux != 0;
            } else if (oc.first_kind == HQQRPC_K_RESPONSE) {
                if (oc.first_aux == 0) {
                    g_stat_ok++;
                } else if (oc.first_aux >= HQQRPC_E_APP_MIN) {
                    g_stat_app++;
                } else {
                    g_stat_error++;
                }
            }
            if (in_kind == HQQRPC_K_REQUEST || in_kind == HQQRPC_K_CONT) {
                HQQ_CHECK_EQ_INT(oc.first_kind, HQQRPC_K_RESPONSE);
                if (in_kind == HQQRPC_K_REQUEST) {
                    HQQ_CHECK_EQ_U64(oc.first_id, in_id);
                    HQQ_CHECK_EQ_INT(rtu_le16(g_out + 2), in_op);
                }
            } else if (in_kind == HQQRPC_K_HELLO) {
                HQQ_CHECK_EQ_INT(oc.first_kind, HQQRPC_K_HELLO_REPLY);
            } else if (in_kind == HQQRPC_K_CLOSE) {
                HQQ_CHECK_EQ_INT(oc.first_kind, HQQRPC_K_CLOSE_ACK);
            }
            /* rejected before the call: the library was not entered */
            if (oc.first_kind == HQQRPC_K_RESPONSE && oc.first_aux >= HQQRPC_E_UNKNOWN_OP &&
                oc.first_aux <= HQQRPC_E_DENIED) {
                HQQ_CHECK_EQ_INT(ml_test_entries() - entries_before, 0);
            }
        } else if (in_kind == HQQRPC_K_REQUEST && in_total <= S - RTU_HDR) {
            /* a complete single-frame request is always answered */
            HQQ_CHECK(0);
        } else {
            g_stat_silent++;
        }
        HQQ_CHECK(hqqrpc_test_live_handles(s) <= 8);
        if (hqqrpc_test_ended(s)) {
            break;
        }
    }
    hqqrpc_test_session_free(s);
    HQQ_CHECK_EQ_INT(ml_test_live_objects(), 0); /* every library object was destroyed */
}

int main(void) {
    unsigned long iters = 200000;
    unsigned long it;
    const char *env = getenv("HQQ_FUZZ_ITERS");

    if (env != NULL && *env != '\0') {
        iters = strtoul(env, NULL, 10);
    }
    env = getenv("HQQ_FUZZ_SEED");
    if (env != NULL && *env != '\0') {
        g_rng ^= strtoull(env, NULL, 10) * UINT64_C(0x9E3779B97F4A7C15);
        if (g_rng == 0) {
            g_rng = 1;
        }
    }
    (void)hqq_rpcvec_sqlite_lowlevel_digest;
    (void)hqq_rpcvec_sqlite_lowlevel_canonical;
    (void)hqq_rpcvec_microlib_canonical;
    (void)hqq_rpcvec_microlib_digest;

    for (it = 0; it < iters; it++) {
        int script = (int)(it % 10 < 5 ? SCRIPT_SESSION : it % 10 < 7 ? SCRIPT_CHUNKED : SCRIPT_RANDOM);

        if (script == SCRIPT_SESSION) {
            build_session_script();
        } else if (script == SCRIPT_CHUNKED) {
            build_chunked_script();
        } else {
            build_random_script();
        }
        if (it % 50 != 49) { /* every 50th iteration runs the script unmutated */
            mutate_script();
        }
        run_one(script);
    }
    printf("rpc_fuzz: %lu iterations: %lu violations, %lu results, %lu errors, %lu application statuses, "
           "%lu silent frames, %lu failed handshakes\n",
           iters, g_stat_violation, g_stat_ok, g_stat_error, g_stat_app, g_stat_silent, g_stat_handshake_fail);
    return HQQ_TEST_RESULT("rpc_fuzz");
}
