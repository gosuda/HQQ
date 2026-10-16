#define _POSIX_C_SOURCE 200809L

#include "sqlsrv_proto.h"

#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

void sqlsrv_errf(sqlsrv_err *e, uint64_t status, const char *fmt, ...) {
    va_list ap;
    e->status = status;
    va_start(ap, fmt);
    (void)vsnprintf(e->msg, sizeof e->msg, fmt, ap);
    va_end(ap);
}

void sqlsrv_err_proto(sqlsrv_err *e, enum sqlsrv_perr code, const char *msg) {
    sqlsrv_errf(e, SQLSRV_STATUS_PROTO(code), "%s", msg);
}

/* ---- Output buffer ----------------------------------------------------- */

static void buf_reserve(sqlsrv_buf *b, size_t extra) {
    size_t need;
    size_t cap;
    uint8_t *p;

    if (b->oom || extra > SIZE_MAX - b->len) {
        b->oom = 1;
        return;
    }
    need = b->len + extra;
    if (need <= b->cap) {
        return;
    }
    cap = b->cap != 0 ? b->cap : 256;
    while (cap < need) {
        if (cap > SIZE_MAX / 2) {
            b->oom = 1;
            return;
        }
        cap *= 2;
    }
    p = realloc(b->p, cap);
    if (p == NULL) {
        b->oom = 1;
        return;
    }
    b->p = p;
    b->cap = cap;
}

void sqlsrv_buf_free(sqlsrv_buf *b) {
    free(b->p);
    b->p = NULL;
    b->len = 0;
    b->cap = 0;
    b->oom = 0;
}

void sqlsrv_buf_u8(sqlsrv_buf *b, uint8_t v) {
    buf_reserve(b, 1);
    if (!b->oom) {
        b->p[b->len++] = v;
    }
}

void sqlsrv_buf_u16(sqlsrv_buf *b, uint16_t v) {
    sqlsrv_buf_u8(b, (uint8_t)(v & 0xffu));
    sqlsrv_buf_u8(b, (uint8_t)(v >> 8));
}

void sqlsrv_buf_u32(sqlsrv_buf *b, uint32_t v) {
    int i;
    buf_reserve(b, 4);
    if (b->oom) {
        return;
    }
    for (i = 0; i < 4; i++) {
        b->p[b->len++] = (uint8_t)(v >> (8 * i));
    }
}

void sqlsrv_buf_i64(sqlsrv_buf *b, int64_t v) {
    uint64_t u = (uint64_t)v;
    int i;
    buf_reserve(b, 8);
    if (b->oom) {
        return;
    }
    for (i = 0; i < 8; i++) {
        b->p[b->len++] = (uint8_t)(u >> (8 * i));
    }
}

void sqlsrv_buf_f64(sqlsrv_buf *b, double v) {
    uint64_t bits;
    memcpy(&bits, &v, sizeof bits);
    sqlsrv_buf_i64(b, (int64_t)bits);
}

void sqlsrv_buf_bytes(sqlsrv_buf *b, const void *p, size_t n) {
    buf_reserve(b, n);
    if (!b->oom && n > 0) {
        memcpy(b->p + b->len, p, n);
        b->len += n;
    }
}

void sqlsrv_buf_str(sqlsrv_buf *b, const void *p, size_t n) {
    sqlsrv_buf_u32(b, (uint32_t)n);
    sqlsrv_buf_bytes(b, p, n);
}

void sqlsrv_buf_put_u32(sqlsrv_buf *b, size_t pos, uint32_t v) {
    int i;
    for (i = 0; i < 4; i++) {
        b->p[pos + (size_t)i] = (uint8_t)(v >> (8 * i));
    }
}

void sqlsrv_buf_put_u8(sqlsrv_buf *b, size_t pos, uint8_t v) {
    b->p[pos] = v;
}

/* ---- Reader ------------------------------------------------------------ */

void sqlsrv_rd_init(sqlsrv_rd *r, const void *p, size_t n) {
    r->p = (const uint8_t *)p;
    r->n = n;
    r->off = 0;
}

static int rd_take(sqlsrv_rd *r, size_t n, const uint8_t **at) {
    if (n > r->n - r->off) {
        return -1;
    }
    *at = r->p + r->off;
    r->off += n;
    return 0;
}

static int rd_uint(sqlsrv_rd *r, size_t width, uint64_t *v) {
    const uint8_t *at = NULL;
    uint64_t out = 0;
    size_t i;
    if (rd_take(r, width, &at) != 0) {
        return -1;
    }
    for (i = 0; i < width; i++) {
        out |= (uint64_t)at[i] << (8 * i);
    }
    *v = out;
    return 0;
}

int sqlsrv_rd_u8(sqlsrv_rd *r, uint8_t *v) {
    uint64_t x;
    if (rd_uint(r, 1, &x) != 0) {
        return -1;
    }
    *v = (uint8_t)x;
    return 0;
}

int sqlsrv_rd_u16(sqlsrv_rd *r, uint16_t *v) {
    uint64_t x;
    if (rd_uint(r, 2, &x) != 0) {
        return -1;
    }
    *v = (uint16_t)x;
    return 0;
}

int sqlsrv_rd_u32(sqlsrv_rd *r, uint32_t *v) {
    uint64_t x;
    if (rd_uint(r, 4, &x) != 0) {
        return -1;
    }
    *v = (uint32_t)x;
    return 0;
}

int sqlsrv_rd_i64(sqlsrv_rd *r, int64_t *v) {
    uint64_t x;
    if (rd_uint(r, 8, &x) != 0) {
        return -1;
    }
    *v = (int64_t)x;
    return 0;
}

int sqlsrv_rd_f64(sqlsrv_rd *r, double *v) {
    uint64_t bits;
    if (rd_uint(r, 8, &bits) != 0) {
        return -1;
    }
    memcpy(v, &bits, sizeof bits);
    return 0;
}

int sqlsrv_rd_str(sqlsrv_rd *r, const uint8_t **p, uint32_t *n) {
    uint32_t len;
    const uint8_t *at = NULL;
    if (sqlsrv_rd_u32(r, &len) != 0) {
        return -1;
    }
    if (rd_take(r, len, &at) != 0) {
        return -1;
    }
    *p = at;
    *n = len;
    return 0;
}

int sqlsrv_rd_end(const sqlsrv_rd *r) {
    return r->off == r->n ? 0 : -1;
}

/* ---- Validation -------------------------------------------------------- */

int sqlsrv_valid_open_flags(uint32_t flags) {
    const uint32_t known = SQLSRV_OPEN_READONLY | SQLSRV_OPEN_READWRITE | SQLSRV_OPEN_CREATE;
    uint32_t access;
    if ((flags & ~known) != 0) {
        return 0;
    }
    access = flags & (SQLSRV_OPEN_READONLY | SQLSRV_OPEN_READWRITE);
    return access == SQLSRV_OPEN_READONLY || access == SQLSRV_OPEN_READWRITE;
}

/* [a-z_][a-z0-9_]{0,63} */
int sqlsrv_valid_pragma_name(const uint8_t *s, size_t n) {
    size_t i;
    if (n == 0 || n > SQLSRV_MAX_PRAGMA_NAME) {
        return 0;
    }
    for (i = 0; i < n; i++) {
        uint8_t c = s[i];
        int lower = c >= 'a' && c <= 'z';
        int digit = c >= '0' && c <= '9';
        if (lower || c == '_') {
            continue;
        }
        if (digit && i > 0) {
            continue;
        }
        return 0;
    }
    return 1;
}

/* [A-Za-z0-9_.+-]{0,255}; the empty value is allowed. */
int sqlsrv_valid_pragma_value(const uint8_t *s, size_t n) {
    size_t i;
    if (n > SQLSRV_MAX_PRAGMA_VALUE) {
        return 0;
    }
    for (i = 0; i < n; i++) {
        uint8_t c = s[i];
        int alnum = (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9');
        if (alnum || c == '_' || c == '.' || c == '+' || c == '-') {
            continue;
        }
        return 0;
    }
    return 1;
}
