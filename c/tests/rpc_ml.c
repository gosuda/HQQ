/* rpc_ml.c - see rpc_ml.h. */
#include "rpc_ml.h"

#include <stdatomic.h>
#include <stdlib.h>
#include <string.h>

struct ml_counter {
    int64_t total;
    const char *msg;
};

struct ml_cursor {
    int64_t next;
    int64_t step;
    int remaining;
    int exhausted;
};

static _Atomic long g_live;
static _Atomic long g_inside;
static _Atomic long g_max_inside;
static _Atomic long g_entries;
static _Atomic(ml_probe_fn) g_probe;
static void *_Atomic g_probe_ctx;

static void enter(void) {
    long now = atomic_fetch_add(&g_inside, 1) + 1;
    long max = atomic_load(&g_max_inside);

    atomic_fetch_add(&g_entries, 1);
    while (now > max && !atomic_compare_exchange_weak(&g_max_inside, &max, now)) {
    }
}

static void leave(void) {
    atomic_fetch_sub(&g_inside, 1);
}

void ml_test_set_probe(ml_probe_fn fn, void *ctx) {
    atomic_store(&g_probe_ctx, ctx);
    atomic_store(&g_probe, fn);
}

long ml_test_live_objects(void) {
    return atomic_load(&g_live);
}

long ml_test_max_overlap(void) {
    return atomic_load(&g_max_inside);
}

long ml_test_entries(void) {
    return atomic_load(&g_entries);
}

void ml_test_reset(void) {
    atomic_store(&g_max_inside, 0);
    atomic_store(&g_entries, 0);
}

int32_t ml_add(int32_t a, int32_t b) {
    ml_probe_fn probe;
    int32_t r;

    enter();
    probe = atomic_load(&g_probe);
    if (probe != NULL) {
        probe(atomic_load(&g_probe_ctx));
    }
    r = (int32_t)((uint32_t)a + (uint32_t)b);
    leave();
    return r;
}

ml_counter *ml_counter_new(int64_t start) {
    ml_counter *c;

    enter();
    c = (ml_counter *)malloc(sizeof *c);
    if (c != NULL) {
        c->total = start;
        c->msg = "";
        atomic_fetch_add(&g_live, 1);
    }
    leave();
    return c;
}

int32_t ml_counter_add(ml_counter *c, int64_t delta, int64_t *total) {
    int32_t r = ML_OK;

    enter();
    if ((delta > 0 && c->total > INT64_MAX - delta) || (delta < 0 && c->total < INT64_MIN - delta)) {
        c->msg = "overflow";
        r = -1;
    } else {
        c->total += delta;
        *total = c->total;
        c->msg = "";
    }
    leave();
    return r;
}

int32_t ml_counter_check(ml_counter *c, int32_t limit) {
    int32_t r = ML_OK;

    enter();
    if (c->total > limit) {
        c->msg = "over limit";
        r = -1;
    } else {
        c->msg = "";
    }
    leave();
    return r;
}

const char *ml_counter_errmsg(ml_counter *c) {
    return c->msg;
}

void ml_counter_free(ml_counter *c) {
    enter();
    free(c);
    atomic_fetch_sub(&g_live, 1);
    leave();
}

ml_cursor *ml_cursor_open(ml_counter *c, int32_t step) {
    ml_cursor *cur;

    enter();
    cur = (ml_cursor *)malloc(sizeof *cur);
    if (cur != NULL) {
        cur->next = c->total;
        cur->step = step;
        cur->remaining = 3;
        cur->exhausted = 0;
        atomic_fetch_add(&g_live, 1);
    }
    leave();
    return cur;
}

int32_t ml_cursor_next(ml_cursor *cur, int64_t *value) {
    int32_t r = ML_OK;

    enter();
    if (cur->remaining == 0) {
        cur->exhausted = 1;
        r = ML_EMPTY;
    } else {
        *value = cur->next;
        cur->next += cur->step;
        cur->remaining--;
    }
    leave();
    return r;
}

int32_t ml_cursor_free(ml_cursor *cur) {
    int32_t r;

    enter();
    r = cur->exhausted ? ML_OK : ML_PARTIAL;
    free(cur);
    atomic_fetch_sub(&g_live, 1);
    leave();
    return r;
}

size_t ml_xor(const uint8_t *src, size_t n, uint8_t key, uint8_t *dst, size_t cap) {
    size_t i;

    enter();
    if (cap < n) {
        leave();
        return SIZE_MAX;
    }
    for (i = 0; i < n; i++) {
        dst[i] = (uint8_t)(src[i] ^ key);
    }
    leave();
    return n;
}

int ml_xor_failed(size_t result) {
    return result == SIZE_MAX;
}

uint64_t ml_hash(const char *s) {
    uint64_t h = UINT64_C(14695981039346656037);

    enter();
    for (; *s != '\0'; s++) {
        h ^= (uint8_t)*s;
        h *= UINT64_C(1099511628211);
    }
    leave();
    return h;
}
