/* Verbatim copy of the generated microlib server of BINDGEN.md 9.6 (interface digest 06a738744a1ea9f11fb08cc41afbe48e1891c76e60830a276e98cf8c15944e5c), kept here as a
 * test fixture of the C runtime until the emitter output of bindgen/e2e/microlib is available. */
#include <string.h>
#include "rpc_ml_server.h"

static int destroy_Counter(void *p) { ml_counter_free((ml_counter *)p); return 0; }
static int destroy_Cursor(void *p) { return ml_cursor_free((ml_cursor *)p) != ML_OK; }

/* op 1: ml_add. G1: a i32 @0, b i32 @4. G2: ret i32. RESP_BOUND 4. */
static hqqrpc_status op_ml_add(hqqrpc_call *c) {
    hqqrpc_session *s = c->session;
    hqqrpc_rd r;
    const uint8_t *g;
    uint8_t *o;
    hqqrpc_status st;
    hqqrpc_wr w;
    int32_t a, b, ret;

    hqqrpc_rd_init(&r, c);
    if ((st = hqqrpc_rd_take(&r, 4, 8, &g)) != HQQRPC_OK || (st = hqqrpc_rd_end(&r)) != HQQRPC_OK)
        return st;
    memcpy(&a, g + 0, 4);
    memcpy(&b, g + 4, 4);
    if ((st = hqqrpc_resp_begin(c, 4, &w)) != HQQRPC_OK)
        return st;
    hqqrpc_exec_begin(s);
    ret = ml_add(a, b);
    hqqrpc_exec_end(s);
    if ((o = hqqrpc_wr_fixed(&w, 4, 4)) == NULL)
        return w.err;
    memcpy(o, &ret, 4);
    return hqqrpc_resp_commit(c, &w, 0);
}

/* op 2: ml_counter_new. @error(null). G1: start i64 @0. G2: ret u32. */
static hqqrpc_status op_ml_counter_new(hqqrpc_call *c) {
    hqqrpc_session *s = c->session;
    hqqrpc_rd r;
    const uint8_t *g;
    uint8_t *o;
    hqqrpc_status st;
    hqqrpc_wr w;
    int64_t start;
    ml_counter *ret;
    uint32_t id = 0;
    uint8_t flags = 0;

    hqqrpc_rd_init(&r, c);
    if ((st = hqqrpc_rd_take(&r, 8, 8, &g)) != HQQRPC_OK || (st = hqqrpc_rd_end(&r)) != HQQRPC_OK)
        return st;
    memcpy(&start, g + 0, 8);
    if ((st = hqqrpc_handle_reserve(s, 1)) != HQQRPC_OK || (st = hqqrpc_resp_begin(c, 4, &w)) != HQQRPC_OK)
        return st;
    hqqrpc_exec_begin(s);
    ret = ml_counter_new(start);
    if (ret == NULL) {
        flags = HQQRPC_F_FAIL;
    } else if (hqqrpc_handle_put(s, MICROLIB_H_COUNTER, ret, 0, &id) != HQQRPC_OK) {
        destroy_Counter(ret);
        hqqrpc_exec_end(s);
        return HQQRPC_E_INTERNAL;
    }
    hqqrpc_exec_end(s);
    if ((o = hqqrpc_wr_fixed(&w, 4, 4)) == NULL)
        return w.err;
    memcpy(o, &id, 4);
    return hqqrpc_resp_commit(c, &w, flags);
}

/* op 3: ml_counter_add. @intercept, detail. G1: c u32 @0, delta i64 @8. G2: ret i32 @0, total i64 @8 (16). */
static hqqrpc_status op_ml_counter_add(hqqrpc_call *c) {
    hqqrpc_session *s = c->session;
    hqqrpc_rd r;
    const uint8_t *g;
    uint8_t *o;
    hqqrpc_status st;
    hqqrpc_wr w;
    microlib_args_ml_counter_add a;
    uint32_t h_c;
    void *p;
    int64_t delta, total = 0;
    int32_t ret;
    uint8_t flags = 0;

    hqqrpc_rd_init(&r, c);
    if ((st = hqqrpc_rd_take(&r, 8, 16, &g)) != HQQRPC_OK || (st = hqqrpc_rd_end(&r)) != HQQRPC_OK)
        return st;
    memcpy(&h_c, g + 0, 4);
    memcpy(&delta, g + 8, 8);
    if ((st = hqqrpc_handle_get(s, h_c, MICROLIB_H_COUNTER, 0, &p)) != HQQRPC_OK)
        return st;
    if ((st = hqqrpc_resp_begin(c, (uint64_t)16 + HQQRPC_DETAIL_RESERVE, &w)) != HQQRPC_OK)
        return st;
    a.c = (ml_counter *)p;
    a.delta = delta;
    hqqrpc_exec_begin(s);
    if ((st = microlib_intercept_ml_counter_add(s, &a)) != HQQRPC_OK) {
        hqqrpc_exec_end(s);
        return hqqrpc_hook_status(st);
    }
    ret = ml_counter_add(a.c, a.delta, &total);
    if ((o = hqqrpc_wr_fixed(&w, 8, 16)) == NULL) {
        hqqrpc_exec_end(s);
        return w.err;
    }
    memcpy(o + 0, &ret, 4);
    if (ret == ML_OK)
        memcpy(o + 8, &total, 8);
    else /* borrowed message: copied before the lock is released */
        flags = HQQRPC_F_FAIL | hqqrpc_wr_put_detail(&w, ml_counter_errmsg(a.c));
    hqqrpc_exec_end(s);
    return hqqrpc_resp_commit(c, &w, flags);
}

/* op 4: ml_counter_free, destroy of Counter. G1: c u32 @0. Empty response. Step 9: cascade. */
static hqqrpc_status op_ml_counter_free(hqqrpc_call *c) {
    hqqrpc_session *s = c->session;
    hqqrpc_rd r;
    const uint8_t *g;
    hqqrpc_status st;
    hqqrpc_wr w;
    uint32_t h_c;
    void *p;

    hqqrpc_rd_init(&r, c);
    if ((st = hqqrpc_rd_take(&r, 4, 4, &g)) != HQQRPC_OK || (st = hqqrpc_rd_end(&r)) != HQQRPC_OK)
        return st;
    memcpy(&h_c, g + 0, 4);
    if ((st = hqqrpc_handle_get(s, h_c, MICROLIB_H_COUNTER, 0, &p)) != HQQRPC_OK)
        return st;
    if ((st = hqqrpc_resp_begin(c, 0, &w)) != HQQRPC_OK)
        return st;
    hqqrpc_exec_begin(s);
    hqqrpc_handle_destroy_children(s, h_c); /* the lock is held; Cursors go first */
    ml_counter_free((ml_counter *)p);
    hqqrpc_handle_consume(s, h_c);          /* void destroy: always consumed */
    hqqrpc_exec_end(s);
    return hqqrpc_resp_commit(c, &w, 0);
}

/* op 5: ml_cursor_open. owned_by Counter, @error(null). G1: c u32 @0, step i32 @4. G2: ret u32. */
static hqqrpc_status op_ml_cursor_open(hqqrpc_call *c) {
    hqqrpc_session *s = c->session;
    hqqrpc_rd r;
    const uint8_t *g;
    uint8_t *o;
    hqqrpc_status st;
    hqqrpc_wr w;
    uint32_t h_c, id = 0;
    int32_t step;
    void *p;
    ml_cursor *ret;
    uint8_t flags = 0;

    hqqrpc_rd_init(&r, c);
    if ((st = hqqrpc_rd_take(&r, 4, 8, &g)) != HQQRPC_OK || (st = hqqrpc_rd_end(&r)) != HQQRPC_OK)
        return st;
    memcpy(&h_c, g + 0, 4);
    memcpy(&step, g + 4, 4);
    if ((st = hqqrpc_handle_get(s, h_c, MICROLIB_H_COUNTER, 0, &p)) != HQQRPC_OK ||
        (st = hqqrpc_handle_reserve(s, 1)) != HQQRPC_OK ||
        (st = hqqrpc_resp_begin(c, 4, &w)) != HQQRPC_OK)
        return st;
    hqqrpc_exec_begin(s);
    ret = ml_cursor_open((ml_counter *)p, step);
    if (ret == NULL) {
        flags = HQQRPC_F_FAIL;
    } else if (hqqrpc_handle_put(s, MICROLIB_H_CURSOR, ret, h_c, &id) != HQQRPC_OK) {
        destroy_Cursor(ret);
        hqqrpc_exec_end(s);
        return HQQRPC_E_INTERNAL;
    }
    hqqrpc_exec_end(s);
    if ((o = hqqrpc_wr_fixed(&w, 4, 4)) == NULL)
        return w.err;
    memcpy(o, &id, 4);
    return hqqrpc_resp_commit(c, &w, flags);
}

/* op 6: ml_cursor_next. ok(0, 1). G1: cur u32 @0. G2: ret i32 @0, value i64 @8 (16). value is zero-initialized. */
static hqqrpc_status op_ml_cursor_next(hqqrpc_call *c) {
    hqqrpc_session *s = c->session;
    hqqrpc_rd r;
    const uint8_t *g;
    uint8_t *o;
    hqqrpc_status st;
    hqqrpc_wr w;
    uint32_t h_cur;
    void *p;
    int64_t value = 0;
    int32_t ret;
    uint8_t flags = 0;

    hqqrpc_rd_init(&r, c);
    if ((st = hqqrpc_rd_take(&r, 4, 4, &g)) != HQQRPC_OK || (st = hqqrpc_rd_end(&r)) != HQQRPC_OK)
        return st;
    memcpy(&h_cur, g + 0, 4);
    if ((st = hqqrpc_handle_get(s, h_cur, MICROLIB_H_CURSOR, 0, &p)) != HQQRPC_OK ||
        (st = hqqrpc_resp_begin(c, 16, &w)) != HQQRPC_OK)
        return st;
    hqqrpc_exec_begin(s);
    ret = ml_cursor_next((ml_cursor *)p, &value);
    hqqrpc_exec_end(s);
    if (ret != ML_OK && ret != ML_EMPTY)
        flags = HQQRPC_F_FAIL;
    if ((o = hqqrpc_wr_fixed(&w, 8, 16)) == NULL)
        return w.err;
    memcpy(o + 0, &ret, 4);
    memcpy(o + 8, &value, 8);
    return hqqrpc_resp_commit(c, &w, flags);
}

/* op 7: ml_cursor_free, destroy of Cursor, @error(status, ok(ML_OK)), no @retain_on_fail. G2: ret i32. */
static hqqrpc_status op_ml_cursor_free(hqqrpc_call *c) {
    hqqrpc_session *s = c->session;
    hqqrpc_rd r;
    const uint8_t *g;
    uint8_t *o;
    hqqrpc_status st;
    hqqrpc_wr w;
    uint32_t h_cur;
    void *p;
    int32_t ret;
    uint8_t flags = 0;

    hqqrpc_rd_init(&r, c);
    if ((st = hqqrpc_rd_take(&r, 4, 4, &g)) != HQQRPC_OK || (st = hqqrpc_rd_end(&r)) != HQQRPC_OK)
        return st;
    memcpy(&h_cur, g + 0, 4);
    if ((st = hqqrpc_handle_get(s, h_cur, MICROLIB_H_CURSOR, 0, &p)) != HQQRPC_OK ||
        (st = hqqrpc_resp_begin(c, 4, &w)) != HQQRPC_OK)
        return st;
    hqqrpc_exec_begin(s);
    ret = ml_cursor_free((ml_cursor *)p);
    hqqrpc_handle_consume(s, h_cur); /* always: the object is gone whatever ret says (5.1) */
    hqqrpc_exec_end(s);
    if (ret != ML_OK)
        flags = HQQRPC_F_FAIL;
    if ((o = hqqrpc_wr_fixed(&w, 4, 4)) == NULL)
        return w.err;
    memcpy(o, &ret, 4);
    return hqqrpc_resp_commit(c, &w, flags);
}

/* op 8: ml_xor. G1: key u8 @0, dst.cap u32 @4. V1: src. G2: ret u64 @0. V2: dst, length at @8, data at @12. */
static hqqrpc_status op_ml_xor(hqqrpc_call *c) {
    hqqrpc_session *s = c->session;
    hqqrpc_rd r;
    const uint8_t *g;
    uint8_t *o, *v;
    hqqrpc_status st;
    hqqrpc_wr w;
    const void *src;
    uint32_t cap, srcn;
    uint8_t key, flags = 0;
    size_t ret;
    uint64_t ret64;
    int failed;

    hqqrpc_rd_init(&r, c);
    if ((st = hqqrpc_rd_take(&r, 4, 8, &g)) != HQQRPC_OK)
        return st;
    key = g[0];
    memcpy(&cap, g + 4, 4);
    if ((st = hqqrpc_rd_bytes(&r, 0, 65536, &src, &srcn)) != HQQRPC_OK || (st = hqqrpc_rd_end(&r)) != HQQRPC_OK)
        return st;
    if ((st = hqqrpc_resp_begin(c, (uint64_t)8 + HQQRPC_VAR_SLACK + cap, &w)) != HQQRPC_OK)
        return st; /* HQQRPC_E_LIMIT if RESP_BOUND exceeds max_message */
    hqqrpc_exec_begin(s);
    o = hqqrpc_wr_fixed(&w, 8, 8);                  /* G2 */
    v = hqqrpc_wr_span(&w, 4, (size_t)4 + cap);     /* V2: length, then the buffer the library fills */
    if (o == NULL || v == NULL) {
        hqqrpc_exec_end(s);
        return w.err;
    }
    ret = ml_xor((const uint8_t *)src, srcn, key, v + 4, cap); /* writes straight into the response */
    failed = ml_xor_failed(ret);
    hqqrpc_exec_end(s);
    ret64 = (uint64_t)ret;
    memcpy(o, &ret64, 8);
    if (failed) {
        flags = HQQRPC_F_FAIL; /* V2 omitted; the body is G2 */
    } else {
        uint32_t n32;
        if (ret > cap)
            return HQQRPC_E_INTERNAL; /* library result contradicts the IDL */
        n32 = (uint32_t)ret;
        memcpy(v, &n32, 4);
        if (hqqrpc_wr_advance(&w, (size_t)4 + n32) != HQQRPC_OK)
            return w.err;
    }
    return hqqrpc_resp_commit(c, &w, flags);
}

/* op 9: ml_hash. V1: s, NUL-reliant, so copied (7.2). G2: ret u64. */
static hqqrpc_status op_ml_hash(hqqrpc_call *c) {
    hqqrpc_session *s = c->session;
    hqqrpc_rd r;
    uint8_t *o;
    hqqrpc_status st;
    hqqrpc_wr w;
    const char *str;
    char *sc;
    uint32_t n;
    uint64_t ret;

    hqqrpc_rd_init(&r, c);
    if ((st = hqqrpc_rd_str(&r, 0, 255, &str, &n)) != HQQRPC_OK || (st = hqqrpc_rd_end(&r)) != HQQRPC_OK)
        return st;
    if ((sc = (char *)hqqrpc_scratch(s, (size_t)n + 1)) == NULL)
        return HQQRPC_E_INTERNAL;
    memcpy(sc, str, n);
    sc[n] = '\0';
    if ((st = hqqrpc_resp_begin(c, 8, &w)) != HQQRPC_OK)
        return st;
    hqqrpc_exec_begin(s);
    ret = ml_hash(sc);
    hqqrpc_exec_end(s);
    if ((o = hqqrpc_wr_fixed(&w, 8, 8)) == NULL)
        return w.err;
    memcpy(o, &ret, 8);
    return hqqrpc_resp_commit(c, &w, 0);
}

/* op 10: ml_cells (shim). G1: c u32 @0, max i32 @4. V2: Cell[]<=256, max_out 8192. */
static hqqrpc_status op_ml_cells(hqqrpc_call *c) {
    hqqrpc_session *s = c->session;
    hqqrpc_rd r;
    const uint8_t *g;
    hqqrpc_status st;
    hqqrpc_wr w;
    uint32_t h_c;
    int32_t max;
    void *p;

    hqqrpc_rd_init(&r, c);
    if ((st = hqqrpc_rd_take(&r, 4, 8, &g)) != HQQRPC_OK || (st = hqqrpc_rd_end(&r)) != HQQRPC_OK)
        return st;
    memcpy(&h_c, g + 0, 4);
    memcpy(&max, g + 4, 4);
    if ((st = hqqrpc_handle_get(s, h_c, MICROLIB_H_COUNTER, 0, &p)) != HQQRPC_OK ||
        (st = hqqrpc_resp_begin(c, 8192, &w)) != HQQRPC_OK)
        return st;
    hqqrpc_exec_begin(s);
    st = ml_cells((ml_counter *)p, max, &w);
    hqqrpc_exec_end(s);
    if ((st = hqqrpc_shim_status(st)) != HQQRPC_OK)
        return st;
    return hqqrpc_resp_commit(c, &w, 0); /* returns w.err if the shim ignored a writer failure */
}

/* op 11: ml_counter_check. detail. G1: c u32 @0, limit i32 @4. G2: ret i32 (4). D starts at 8 on failure. */
static hqqrpc_status op_ml_counter_check(hqqrpc_call *c) {
    hqqrpc_session *s = c->session;
    hqqrpc_rd r;
    const uint8_t *g;
    uint8_t *o;
    hqqrpc_status st;
    hqqrpc_wr w;
    uint32_t h_c;
    int32_t limit, ret;
    void *p;
    uint8_t flags = 0;

    hqqrpc_rd_init(&r, c);
    if ((st = hqqrpc_rd_take(&r, 4, 8, &g)) != HQQRPC_OK || (st = hqqrpc_rd_end(&r)) != HQQRPC_OK)
        return st;
    memcpy(&h_c, g + 0, 4);
    memcpy(&limit, g + 4, 4);
    if ((st = hqqrpc_handle_get(s, h_c, MICROLIB_H_COUNTER, 0, &p)) != HQQRPC_OK ||
        (st = hqqrpc_resp_begin(c, (uint64_t)8 + HQQRPC_DETAIL_RESERVE, &w)) != HQQRPC_OK)
        return st;
    hqqrpc_exec_begin(s);
    ret = ml_counter_check((ml_counter *)p, limit);
    if ((o = hqqrpc_wr_fixed(&w, 4, 4)) == NULL) {
        hqqrpc_exec_end(s);
        return w.err;
    }
    memcpy(o, &ret, 4);
    if (ret != ML_OK) /* put_detail pads to 8 only when D is written */
        flags = HQQRPC_F_FAIL | hqqrpc_wr_put_detail(&w, ml_counter_errmsg((ml_counter *)p));
    hqqrpc_exec_end(s);
    return hqqrpc_resp_commit(c, &w, flags);
}

static hqqrpc_status microlib_dispatch(hqqrpc_call *c) {
    switch (c->op) {
    case 1: return op_ml_add(c);
    case 2: return op_ml_counter_new(c);
    case 3: return op_ml_counter_add(c);
    case 4: return op_ml_counter_free(c);
    case 5: return op_ml_cursor_open(c);
    case 6: return op_ml_cursor_next(c);
    case 7: return op_ml_cursor_free(c);
    case 8: return op_ml_xor(c);
    case 9: return op_ml_hash(c);
    case 10: return op_ml_cells(c);
    case 11: return op_ml_counter_check(c);
    default: return HQQRPC_E_UNKNOWN_OP; /* default deny (7.1) */
    }
}

static const hqqrpc_handle_type microlib_handle_types[] = {
    { "Counter", 0, destroy_Counter },                  /* MICROLIB_H_COUNTER */
    { "Cursor", MICROLIB_H_COUNTER, destroy_Cursor },   /* MICROLIB_H_CURSOR, owned_by Counter */
};

static const char *const microlib_op_names[] = {
    "ml_add", "ml_counter_new", "ml_counter_add", "ml_counter_free", "ml_cursor_open",
    "ml_cursor_next", "ml_cursor_free", "ml_xor", "ml_hash", "ml_cells", "ml_counter_check",
};

const hqqrpc_iface microlib_iface = {
    "microlib",
    {
        0x06, 0xa7, 0x38, 0x74, 0x4a, 0x1e, 0xa9, 0xf1,
        0x1f, 0xb0, 0x8c, 0xc4, 0x1a, 0xfb, 0xe4, 0x8e,
        0x18, 0x91, 0xc7, 0x6e, 0x60, 0x83, 0x0a, 0x27,
        0x6e, 0x98, 0xcf, 0x8c, 0x15, 0x94, 0x4e, 0x5c,
    },
    11, microlib_op_names,
    2, microlib_handle_types,
    microlib_dispatch,
    HQQRPC_SERIALIZE_SESSION,
};

/* Writers for union Cell: tag u32 (aligned 4), then the payload. */
hqqrpc_status microlib_Cell_put_Int(hqqrpc_wr *w, int64_t v) {
    hqqrpc_status st = hqqrpc_wr_put_u32(w, 1u);
    return st != HQQRPC_OK ? st : hqqrpc_wr_put_u64(w, (uint64_t)v);
}
hqqrpc_status microlib_Cell_put_Text(hqqrpc_wr *w, const char *s, uint32_t n) {
    hqqrpc_status st = hqqrpc_wr_put_u32(w, 2u);
    return st != HQQRPC_OK ? st : hqqrpc_wr_put_str(w, s, n, 16u);
}
hqqrpc_status microlib_Cell_put_Null(hqqrpc_wr *w) { return hqqrpc_wr_put_u32(w, 3u); }
