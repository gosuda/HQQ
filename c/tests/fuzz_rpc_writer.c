/*
 * fuzz_rpc_writer: libFuzzer target (BINDGEN.md 10.2). The input is a capacity and a sequence of
 * writer calls with arbitrary bounds, lengths and embedded NUL bytes, applied to a heap buffer of
 * exactly that capacity (ASan reports any write past it).
 *
 * Properties (abort on violation): the offset never exceeds the capacity; the first rejection sets
 * the sticky error and every later call fails without moving the offset; a body written without
 * error decodes with hqqrpc_rd_* to exactly the values that were written and ends exactly at the
 * offset (BINDGEN.md 4.5).
 */
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

#include "rpc_fuzz_driver.h"
#include "rpc_test_util.h"

#define PROP(cond)                                                              \
    do {                                                                        \
        if (!(cond)) {                                                          \
            fprintf(stderr, "%s:%d: property failed: %s\n", __FILE__, __LINE__, #cond); \
            abort();                                                            \
        }                                                                       \
    } while (0)

enum { T_PAD, T_U32, T_U64, T_STR, T_BYTES, T_NULL, T_FIXED, T_RAW, T_ARR, T_DETAIL };

typedef struct tok {
    int kind;
    size_t align;
    size_t n;          /* length of the data, or the size of a fixed or raw span */
    uint64_t v;        /* the value, or the count of an array */
    size_t data;       /* offset of the copy in the side buffer */
} tok;

#define MAX_TOK 256u

int LLVMFuzzerTestOneInput(const uint8_t *in, size_t size) {
    static const size_t aligns[] = {1, 2, 4, 8, 16};
    static uint8_t side[8192];
    tok toks[MAX_TOK];
    size_t nt = 0;
    size_t side_len = 0;
    size_t pos;
    size_t cap;
    uint8_t *buf;
    hqqrpc_wr w;
    hqqrpc_cstate cs; /* what hqqrpc_resp_begin attaches: it tracks the last span */
    size_t last_arr = (size_t)-1;
    size_t last_mark = 0;

    if (size < 1) {
        return 0;
    }
    cap = in[0];
    buf = (uint8_t *)malloc(cap != 0 ? cap : 1); /* exactly cap bytes: ASan sees an overflow */
    PROP(buf != NULL);
    memset(&w, 0, sizeof w);
    memset(&cs, 0, sizeof cs);
    w.priv = &cs;
    w.p = buf;
    w.cap = cap;
    pos = 1;

    while (pos < size && nt + 2 < MAX_TOK) {
        unsigned op = in[pos++] % 11u;
        size_t before = w.off;
        int was_err = w.err != 0;
        size_t n = 0;
        size_t a = aligns[pos < size ? in[pos] % 5u : 0];
        uint32_t max = pos + 1 < size ? in[pos + 1] : 255u;
        tok *t = &toks[nt];

        if (pos < size) {
            n = in[pos] % 24u;
        }
        switch (op) {
        case 0: { /* put_u32 */
            uint32_t v = pos + 4 <= size ? rtu_le32(in + pos) : 7u;

            if (hqqrpc_wr_put_u32(&w, v) == HQQRPC_OK) {
                t->kind = T_U32;
                t->v = v;
                nt++;
            }
            break;
        }
        case 1: { /* put_u64 */
            uint64_t v = pos + 8 <= size ? ((uint64_t)rtu_le32(in + pos) << 32 | rtu_le32(in + pos + 4)) : 9u;

            if (hqqrpc_wr_put_u64(&w, v) == HQQRPC_OK) {
                t->kind = T_U64;
                t->v = v;
                nt++;
            }
            break;
        }
        case 2: /* put_str: the bytes may contain NUL */
        case 3: { /* put_bytes */
            size_t avail = size > pos + 2 ? size - pos - 2 : 0;
            size_t len = n < avail ? n : avail;
            const uint8_t *d = in + (size > pos + 2 ? pos + 2 : size);
            hqqrpc_status st = op == 2 ? hqqrpc_wr_put_str(&w, (const char *)d, (uint32_t)len, max)
                                       : hqqrpc_wr_put_bytes(&w, d, (uint32_t)len, max);

            if (st == HQQRPC_OK) {
                PROP(len <= max);
                PROP(op == 3 || (len == 0 || memchr(d, 0, len) == NULL));
                PROP(side_len + len <= sizeof side);
                memcpy(side + side_len, d, len);
                t->kind = op == 2 ? T_STR : T_BYTES;
                t->n = len;
                t->data = side_len;
                side_len += len;
                nt++;
            }
            break;
        }
        case 4:
            if (hqqrpc_wr_put_null(&w) == HQQRPC_OK) {
                t->kind = T_NULL;
                nt++;
            }
            break;
        case 5:
            if (hqqrpc_wr_pad(&w, a) == HQQRPC_OK) {
                t->kind = T_PAD;
                t->align = a;
                nt++;
            }
            break;
        case 6: { /* fixed */
            size_t sz = n * 3u;
            uint8_t *o = hqqrpc_wr_fixed(&w, a, sz);

            if (o != NULL) {
                PROP(o >= buf && o + sz <= buf + cap);
                t->kind = T_FIXED;
                t->align = a;
                t->n = sz;
                nt++;
            }
            break;
        }
        case 7: { /* span, then advance by up to a little more than the span */
            size_t sz = n * 2u;
            uint8_t *o = hqqrpc_wr_span(&w, a, sz);

            if (o != NULL) {
                size_t m = pos + 1 < size ? in[pos + 1] % (sz + 2u) : sz;

                PROP(o >= buf && o + sz <= buf + cap);
                t->kind = T_PAD; /* the alignment of the span moved the offset */
                t->align = a;
                nt++;
                memset(o, 0xC3, sz);
                if (hqqrpc_wr_advance(&w, m) == HQQRPC_OK) {
                    PROP(m <= sz);
                    toks[nt].kind = T_RAW;
                    toks[nt].n = m;
                    nt++;
                }
            }
            break;
        }
        case 8:
            if (hqqrpc_wr_array_begin(&w, &last_mark) == HQQRPC_OK) {
                t->kind = T_ARR;
                t->v = 0; /* the placeholder */
                last_arr = nt;
                nt++;
            }
            break;
        case 9:
            if (last_arr != (size_t)-1) {
                uint32_t count = pos < size ? in[pos] : 0u;

                if (hqqrpc_wr_array_end(&w, last_mark, count, max) == HQQRPC_OK) {
                    PROP(count <= max);
                    toks[last_arr].v = count;
                }
            }
            break;
        default: { /* detail from the input bytes, NUL-terminated by the copy */
            char msg[40];
            size_t len = n < sizeof msg - 1 ? n : sizeof msg - 1;
            size_t k;
            uint8_t fl;

            for (k = 0; k < len; k++) {
                msg[k] = (char)(in[pos + k < size ? pos + k : size - 1] | 1u);
            }
            msg[len] = '\0';
            fl = hqqrpc_wr_put_detail(&w, msg);
            if (fl == HQQRPC_F_DETAIL) {
                t->kind = T_DETAIL;
                t->n = len;
                PROP(side_len + len <= sizeof side);
                memcpy(side + side_len, msg, len);
                t->data = side_len;
                side_len += len;
                nt++;
            }
            break;
        }
        }
        PROP(w.off <= w.cap);
        if (was_err) {
            PROP(w.err != 0 && w.off == before); /* sticky: nothing moved after the error */
        }
        pos += 1;
    }

    PROP(w.off <= cap);
    if (w.err != 0) {
        size_t off = w.off;

        PROP(hqqrpc_wr_put_u32(&w, 1) != HQQRPC_OK);
        PROP(hqqrpc_wr_put_str(&w, "", 0, 10) != HQQRPC_OK);
        PROP(hqqrpc_wr_fixed(&w, 1, 0) == NULL);
        PROP(w.off == off);
    } else {
        /* the body decodes to what was written, and ends exactly at the offset */
        hqqrpc_call c;
        hqqrpc_rd r;
        size_t i;

        memset(&c, 0, sizeof c);
        c.body = buf;
        c.body_len = w.off;
        hqqrpc_rd_init(&r, &c);
        for (i = 0; i < nt; i++) {
            const uint8_t *p = NULL;
            const char *s = NULL;
            const void *bp = NULL;
            uint32_t n32 = 0;
            uint32_t v32 = 0;
            size_t k;

            switch (toks[i].kind) {
            case T_PAD:
                PROP(hqqrpc_rd_take(&r, toks[i].align, 0, &p) == HQQRPC_OK);
                break;
            case T_U32:
            case T_ARR:
                PROP(hqqrpc_rd_u32(&r, &v32) == HQQRPC_OK && v32 == (uint32_t)toks[i].v);
                break;
            case T_U64:
                PROP(hqqrpc_rd_take(&r, 8, 8, &p) == HQQRPC_OK);
                PROP(((uint64_t)rtu_le32(p + 4) << 32 | rtu_le32(p)) == toks[i].v);
                break;
            case T_NULL:
                PROP(hqqrpc_rd_u32(&r, &v32) == HQQRPC_OK && v32 == HQQRPC_NULL_LEN);
                break;
            case T_STR:
                PROP(hqqrpc_rd_str(&r, 0, UINT32_MAX, &s, &n32) == HQQRPC_OK && n32 == toks[i].n);
                PROP(n32 == 0 || memcmp(s, side + toks[i].data, n32) == 0);
                break;
            case T_BYTES:
                PROP(hqqrpc_rd_bytes(&r, 0, UINT32_MAX, &bp, &n32) == HQQRPC_OK && n32 == toks[i].n);
                PROP(n32 == 0 || memcmp(bp, side + toks[i].data, n32) == 0);
                break;
            case T_FIXED:
                PROP(hqqrpc_rd_take(&r, toks[i].align, toks[i].n, &p) == HQQRPC_OK);
                for (k = 0; k < toks[i].n; k++) {
                    PROP(p[k] == 0); /* fixed groups are zero-filled */
                }
                break;
            case T_RAW:
                PROP(hqqrpc_rd_take(&r, 1, toks[i].n, &p) == HQQRPC_OK);
                break;
            case T_DETAIL:
                PROP(hqqrpc_rd_take(&r, 8, 0, &p) == HQQRPC_OK);
                PROP(hqqrpc_rd_str(&r, 0, UINT32_MAX, &s, &n32) == HQQRPC_OK && n32 == toks[i].n);
                PROP(n32 == 0 || memcmp(s, side + toks[i].data, n32) == 0);
                break;
            default:
                abort();
            }
        }
        PROP(hqqrpc_rd_end(&r) == HQQRPC_OK);
    }
    free(buf);
    return 0;
}
