/* microlib: see microlib.h for the contract and the trace format. */
#include "microlib.h"

#include <inttypes.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define CURSOR_LEN 3
#define WORK_SEED UINT64_C(0x9e3779b97f4a7c15)
#define WORK_MUL UINT64_C(0xbf58476d1ce4e5b9)
#define FNV_BASIS UINT64_C(0xcbf29ce484222325)
#define FNV_PRIME UINT64_C(0x00000100000001b3)

enum { ERR_NONE, ERR_OVERFLOW, ERR_OVER_LIMIT };

struct ml_counter {
    int64_t total;
    uint64_t id;
    int last_err;
};

struct ml_cursor {
    int64_t next;
    int64_t step;
    int32_t left;
    uint64_t id;
};

static uint64_t next_counter_id;
static uint64_t next_cursor_id;
static int trace_state; /* 0 not read yet, 1 off, 2 on */

#if defined(__GNUC__)
__attribute__((format(printf, 1, 2)))
#endif
static void trace_write(const char *fmt, ...) {
    char line[256];
    const char *path = getenv("ML_TRACE");
    va_list ap;
    int n;
    FILE *f;

    if (path == NULL || *path == '\0')
        return;
    va_start(ap, fmt);
    n = vsnprintf(line, sizeof line - 1, fmt, ap);
    va_end(ap);
    if (n < 0)
        return;
    if ((size_t)n > sizeof line - 2)
        n = (int)(sizeof line - 2);
    line[n++] = '\n';
    f = fopen(path, "ab");
    if (f == NULL)
        return;
    /* One fwrite and one close give one write(2) in O_APPEND mode: lines from
     * several processes do not interleave. */
    (void)fwrite(line, 1, (size_t)n, f);
    (void)fclose(f);
}

static int trace_enabled(void) {
    if (trace_state == 0) {
        const char *p = getenv("ML_TRACE");
        trace_state = (p != NULL && *p != '\0') ? 2 : 1;
    }
    return trace_state == 2;
}

/* The check is outside the call so that a disabled trace costs a load and a
 * branch: the benchmarks of 10.5 time these functions. */
#define TRACE(...)                                                                                 \
    do {                                                                                           \
        if (trace_enabled())                                                                       \
            trace_write(__VA_ARGS__);                                                              \
    } while (0)

static int64_t wrap_add(int64_t a, int64_t b) { return (int64_t)((uint64_t)a + (uint64_t)b); }

int32_t ml_add(int32_t a, int32_t b) {
    int32_t r = (int32_t)((uint32_t)a + (uint32_t)b);

    TRACE("ml_add a=%" PRId32 " b=%" PRId32 " ret=%" PRId32, a, b, r);
    return r;
}

ml_counter *ml_counter_new(int64_t start) {
    ml_counter *c = malloc(sizeof *c);

    if (c != NULL) {
        c->total = start;
        c->id = ++next_counter_id;
        c->last_err = ERR_NONE;
    }
    TRACE("ml_counter_new start=%" PRId64 " counter=%" PRIu64, start, c != NULL ? c->id : 0);
    return c;
}

int32_t ml_counter_add(ml_counter *c, int64_t delta, int64_t *total) {
    if ((delta > 0 && c->total > INT64_MAX - delta) || (delta < 0 && c->total < INT64_MIN - delta)) {
        c->last_err = ERR_OVERFLOW;
        TRACE("ml_counter_add counter=%" PRIu64 " delta=%" PRId64 " ret=-1", c->id, delta);
        return -1;
    }
    c->total += delta;
    c->last_err = ERR_NONE;
    *total = c->total;
    TRACE("ml_counter_add counter=%" PRIu64 " delta=%" PRId64 " ret=0 total=%" PRId64, c->id, delta, c->total);
    return 0;
}

int32_t ml_counter_check(ml_counter *c, int32_t limit) {
    int32_t r = c->total <= limit ? 0 : -1;

    c->last_err = r == 0 ? ERR_NONE : ERR_OVER_LIMIT;
    TRACE("ml_counter_check counter=%" PRIu64 " limit=%" PRId32 " ret=%" PRId32, c->id, limit, r);
    return r;
}

const char *ml_counter_errmsg(ml_counter *c) {
    const char *m = "";

    if (c->last_err == ERR_OVERFLOW)
        m = "overflow";
    else if (c->last_err == ERR_OVER_LIMIT)
        m = "over limit";
    TRACE("ml_counter_errmsg counter=%" PRIu64 " msg=\"%s\"", c->id, m);
    return m;
}

void ml_counter_free(ml_counter *c) {
    TRACE("ml_counter_free counter=%" PRIu64, c != NULL ? c->id : 0);
    free(c);
}

ml_cursor *ml_cursor_open(ml_counter *c, int32_t step) {
    ml_cursor *cur = malloc(sizeof *cur);

    if (cur != NULL) {
        cur->next = c->total;
        cur->step = step;
        cur->left = CURSOR_LEN;
        cur->id = ++next_cursor_id;
    }
    TRACE("ml_cursor_open counter=%" PRIu64 " step=%" PRId32 " cursor=%" PRIu64, c->id, step,
          cur != NULL ? cur->id : 0);
    return cur;
}

int32_t ml_cursor_next(ml_cursor *cur, int64_t *value) {
    if (cur->left == 0) {
        TRACE("ml_cursor_next cursor=%" PRIu64 " ret=%d", cur->id, ML_EMPTY);
        return ML_EMPTY;
    }
    *value = cur->next;
    cur->next = wrap_add(cur->next, cur->step);
    cur->left--;
    TRACE("ml_cursor_next cursor=%" PRIu64 " ret=%d value=%" PRId64, cur->id, ML_OK, *value);
    return ML_OK;
}

int32_t ml_cursor_free(ml_cursor *cur) {
    int32_t r = (cur != NULL && cur->left > 0) ? ML_PARTIAL : ML_OK;

    TRACE("ml_cursor_free cursor=%" PRIu64 " ret=%" PRId32, cur != NULL ? cur->id : 0, r);
    free(cur);
    return r;
}

size_t ml_xor(const uint8_t *src, size_t n, uint8_t key, uint8_t *dst, size_t cap) {
    size_t r = n;
    size_t i;

    if (cap < n)
        r = SIZE_MAX;
    else
        for (i = 0; i < n; i++)
            dst[i] = (uint8_t)(src[i] ^ key);
    TRACE("ml_xor n=%" PRIu64 " key=%u cap=%" PRIu64 " ret=%" PRIu64, (uint64_t)n, (unsigned)key, (uint64_t)cap,
          (uint64_t)r);
    return r;
}

int ml_xor_failed(size_t result) {
    int r = result == SIZE_MAX;

    TRACE("ml_xor_failed result=%" PRIu64 " ret=%d", (uint64_t)result, r);
    return r;
}

uint64_t ml_hash(const char *s) {
    uint64_t h = FNV_BASIS;
    const unsigned char *p;

    for (p = (const unsigned char *)s; *p != '\0'; p++) {
        h ^= *p;
        h *= FNV_PRIME;
    }
    TRACE("ml_hash len=%" PRIu64 " ret=%" PRIu64, (uint64_t)strlen(s), h);
    return h;
}

uint64_t ml_work(uint32_t iters) {
    uint64_t x = WORK_SEED;
    uint32_t i;

    for (i = 0; i < iters; i++)
        x = (x ^ (x >> 29)) * WORK_MUL + 1;
    TRACE("ml_work iters=%" PRIu32 " ret=%" PRIu64, iters, x);
    return x;
}
