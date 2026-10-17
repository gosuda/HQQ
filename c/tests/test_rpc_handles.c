/*
 * test_rpc_handles: the handle table of BINDGEN.md 4.9 and 5.1: ids, generations, reuse, stale ids,
 * limits, destroy semantics (failed destroy consumed, @retain_on_fail kept, cascade, rollback,
 * teardown order) and a randomized comparison with a reference model.
 *
 * The destroy glue below is the one of 9.6 (hqqrpc_handle_destroy_children, the adapter, then
 * hqqrpc_handle_consume unless the function has @retain_on_fail and failed), written as a function.
 */
#include <stdint.h>
#include <string.h>

#include "hqq_test.h"
#include "rpc_test_util.h"

#define TA 1u /* no parent */
#define TB 2u /* owned by A */
#define TC 3u /* owned by B */

#define MAX_OBJECTS 262144u

static uint8_t g_fail[MAX_OBJECTS];      /* the adapter of object n fails */
static uint8_t g_dcount[MAX_OBJECTS];    /* how often the adapter of object n ran */
static uint32_t g_log[MAX_OBJECTS];      /* object numbers in destroy order */
static size_t g_nlog;
static uint32_t g_next_obj = 1;

static uint32_t obj_number(void *p) {
    return (uint32_t)((uintptr_t)p >> 4);
}

static void *obj_ptr(uint32_t n) {
    return (void *)((uintptr_t)n << 4);
}

static int destroy_any(void *p) {
    uint32_t n = obj_number(p);

    HQQ_CHECK(n != 0 && n < MAX_OBJECTS);
    g_dcount[n]++;
    HQQ_CHECK(g_dcount[n] == 1); /* no pointer is destroyed twice */
    g_log[g_nlog++] = n;
    return g_fail[n];
}

static const hqqrpc_handle_type g_types[] = {
    {"A", 0, destroy_any},
    {"B", TA, destroy_any},
    {"C", TB, destroy_any},
};

static hqqrpc_status unused_dispatch(hqqrpc_call *c) {
    (void)c;
    return HQQRPC_E_UNKNOWN_OP;
}

static const hqqrpc_iface g_iface = {
    "handles", {0}, 0, NULL, 3, g_types, unused_dispatch, HQQRPC_SERIALIZE_SESSION,
};

static hqqrpc_session *new_session(uint32_t max_handles) {
    hqqrpc_server_options o;
    hqqrpc_session *s;

    hqqrpc_server_options_init(&o);
    o.max_handles = max_handles;
    s = hqqrpc_test_session_new(&g_iface, &o);
    HQQ_REQUIRE(s != NULL);
    return s;
}

/* Registers one object of `type` (a call of its own). *id is 0 unless the status is OK. */
static hqqrpc_status put1(hqqrpc_session *s, uint16_t type, uint32_t parent_id, int fail, uint32_t *id, uint32_t *num) {
    hqqrpc_status st;
    uint32_t n = g_next_obj;

    *id = 0;
    hqqrpc_test_call_begin(s);
    st = hqqrpc_handle_reserve(s, 1);
    if (st == HQQRPC_OK) {
        g_next_obj++;
        g_fail[n] = (uint8_t)fail;
        st = hqqrpc_handle_put(s, type, obj_ptr(n), parent_id, id);
        *num = n;
    }
    hqqrpc_test_call_end(s, 1);
    return st;
}

/* What the generated destroy function does (9.6, 5.1). *failed is the adapter's verdict. */
static hqqrpc_status glue_destroy(hqqrpc_session *s, uint32_t id, uint16_t type, int retain_on_fail, int *failed) {
    void *p = NULL;
    hqqrpc_status st = hqqrpc_handle_get(s, id, type, 0, &p);
    int fail;

    if (st != HQQRPC_OK) {
        return st;
    }
    hqqrpc_exec_begin(s);
    hqqrpc_handle_destroy_children(s, id);
    fail = g_types[type - 1u].destroy(p) != 0;
    if (!(retain_on_fail && fail)) {
        hqqrpc_handle_consume(s, id);
    }
    hqqrpc_exec_end(s);
    *failed = fail;
    return HQQRPC_OK;
}

static hqqrpc_status get(hqqrpc_session *s, uint32_t id, uint16_t type, void **p) {
    return hqqrpc_handle_get(s, id, type, 0, p);
}

/* ---- Ids, generations, reuse ------------------------------------------------------------------ */

static void test_ids_and_reuse(void) {
    hqqrpc_session *s = new_session(0);
    uint32_t a;
    uint32_t b;
    uint32_t c;
    uint32_t n;
    void *p = NULL;

    HQQ_CHECK_EQ_INT(put1(s, TA, 0, 0, &a, &n), HQQRPC_OK);
    HQQ_CHECK_EQ_U64(a, 0x00010001u); /* generation 1, index 1 */
    HQQ_CHECK_EQ_INT(put1(s, TA, 0, 0, &b, &n), HQQRPC_OK);
    HQQ_CHECK_EQ_U64(b, 0x00010002u);
    HQQ_CHECK_EQ_INT(put1(s, TA, 0, 0, &c, &n), HQQRPC_OK);
    HQQ_CHECK_EQ_U64(c, 0x00010003u);

    /* LIFO: the entry freed last is reused first, with generation + 1 */
    hqqrpc_handle_consume(s, a);
    hqqrpc_handle_consume(s, b);
    HQQ_CHECK_EQ_INT(put1(s, TA, 0, 0, &a, &n), HQQRPC_OK);
    HQQ_CHECK_EQ_U64(a, 0x00020002u);
    HQQ_CHECK_EQ_INT(put1(s, TA, 0, 0, &b, &n), HQQRPC_OK);
    HQQ_CHECK_EQ_U64(b, 0x00020001u);
    HQQ_CHECK_EQ_INT(put1(s, TA, 0, 0, &a, &n), HQQRPC_OK); /* free list empty: the next never-used index */
    HQQ_CHECK_EQ_U64(a, 0x00010004u);

    /* the validation order of 4.9: every failure is E_BAD_HANDLE */
    HQQ_CHECK_EQ_INT(get(s, 0, TA, &p), HQQRPC_E_BAD_HANDLE);                           /* NULL, not nullable */
    HQQ_CHECK_EQ_INT(hqqrpc_handle_get(s, 0, TA, 1, &p), HQQRPC_OK);                    /* NULL, nullable */
    HQQ_CHECK(p == NULL);
    HQQ_CHECK_EQ_INT(get(s, 0x00010000u, TA, &p), HQQRPC_E_BAD_HANDLE);                 /* index 0 */
    HQQ_CHECK_EQ_INT(get(s, 0x00010005u, TA, &p), HQQRPC_E_BAD_HANDLE);                 /* above the high water mark */
    HQQ_CHECK_EQ_INT(get(s, 0x0001FFFFu, TA, &p), HQQRPC_E_BAD_HANDLE);
    HQQ_CHECK_EQ_INT(get(s, 0x00010001u, TA, &p), HQQRPC_E_BAD_HANDLE);                 /* stale generation */
    HQQ_CHECK_EQ_INT(get(s, 0x00030001u, TA, &p), HQQRPC_E_BAD_HANDLE);                 /* future generation */
    HQQ_CHECK_EQ_INT(get(s, 0x00000001u, TA, &p), HQQRPC_E_BAD_HANDLE);                 /* generation 0 */
    HQQ_CHECK_EQ_INT(get(s, 0x00020001u, TB, &p), HQQRPC_E_BAD_HANDLE);                 /* wrong type */
    HQQ_CHECK_EQ_INT(get(s, 0x00020001u, TA, &p), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(obj_number(p), 5);
    hqqrpc_handle_consume(s, 0x00020001u);
    HQQ_CHECK_EQ_INT(get(s, 0x00020001u, TA, &p), HQQRPC_E_BAD_HANDLE);                 /* dead entry */
    hqqrpc_handle_consume(s, 0x00020001u);                                              /* consuming twice is a no-op */
    hqqrpc_handle_consume(s, 0);
    hqqrpc_handle_consume(s, 0x7FFF0001u);
    HQQ_CHECK_EQ_INT(put1(s, TA, 0, 0, &a, &n), HQQRPC_OK);
    HQQ_CHECK_EQ_U64(a, 0x00030001u);                                                   /* freed once, not twice */
    hqqrpc_test_session_free(s);
}

/* The generation counts 1..65535 and wraps to 1; a stale id is detected unless the index was
 * recycled a multiple of 65535 times (4.9, accepted residual). */
static void test_generation_wrap(void) {
    hqqrpc_session *s = new_session(0);
    uint32_t first = 0;
    uint32_t id = 0;
    uint32_t n;
    uint32_t i;
    void *p = NULL;

    for (i = 1; i <= 65535u + 5u; i++) {
        uint32_t gen_want = (i - 1u) % 65535u + 1u;

        HQQ_REQUIRE(put1(s, TA, 0, 0, &id, &n) == HQQRPC_OK);
        HQQ_REQUIRE((id & 0xFFFFu) == 1u);
        HQQ_REQUIRE((id >> 16) == gen_want); /* never 0 */
        if (i == 1) {
            first = id;
        }
        if (i == 65535u) {
            HQQ_CHECK_EQ_U64(id >> 16, 65535u);
        }
        if (i == 65535u + 1u) {
            HQQ_CHECK_EQ_U64(id >> 16, 1u); /* wrapped */
            /* the residual: the id of cycle 1 is valid again after 65535 recycles */
            HQQ_CHECK_EQ_U64(id, first);
            HQQ_CHECK_EQ_INT(get(s, first, TA, &p), HQQRPC_OK);
        }
        if (i == 65535u + 2u) {
            HQQ_CHECK_EQ_INT(get(s, first, TA, &p), HQQRPC_E_BAD_HANDLE); /* an id one recycle old is stale */
        }
        hqqrpc_handle_consume(s, id);
    }
    HQQ_CHECK_EQ_INT(hqqrpc_test_live_handles(s), 0);
    hqqrpc_test_session_free(s);
}

/* ---- Limits ------------------------------------------------------------------------------------ */

static void test_limits(void) {
    hqqrpc_session *s = new_session(3);
    uint32_t id[4];
    uint32_t n;
    uint32_t i;

    /* a reservation counts against the table before anything is registered */
    hqqrpc_test_call_begin(s);
    HQQ_CHECK_EQ_INT(hqqrpc_handle_reserve(s, 2), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(hqqrpc_handle_reserve(s, 2), HQQRPC_E_LIMIT); /* 2 + 2 > 3 */
    HQQ_CHECK_EQ_INT(hqqrpc_handle_reserve(s, 1), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(hqqrpc_handle_reserve(s, 1), HQQRPC_E_LIMIT);
    HQQ_CHECK_EQ_INT(hqqrpc_handle_reserve(s, 0), HQQRPC_OK);
    hqqrpc_test_call_end(s, 1); /* the unused reservation ends with the call */
    HQQ_CHECK_EQ_INT(hqqrpc_test_live_handles(s), 0);

    for (i = 0; i < 3; i++) {
        HQQ_CHECK_EQ_INT(put1(s, TA, 0, 0, &id[i], &n), HQQRPC_OK);
    }
    HQQ_CHECK_EQ_INT(put1(s, TA, 0, 0, &id[3], &n), HQQRPC_E_LIMIT); /* full */
    HQQ_CHECK_EQ_U64(id[3], 0);
    hqqrpc_handle_consume(s, id[1]);
    HQQ_CHECK_EQ_INT(put1(s, TA, 0, 0, &id[3], &n), HQQRPC_OK); /* one entry free again */
    HQQ_CHECK_EQ_U64(id[3], 0x00020002u);
    HQQ_CHECK_EQ_INT(put1(s, TA, 0, 0, &id[1], &n), HQQRPC_E_LIMIT);

    /* put without a reservation, a bad type, a NULL pointer, a dead parent */
    hqqrpc_test_call_begin(s);
    {
        uint32_t x = 0;

        HQQ_CHECK_EQ_INT(hqqrpc_handle_put(s, TA, obj_ptr(MAX_OBJECTS - 1u), 0, &x), HQQRPC_E_INTERNAL);
    }
    hqqrpc_test_call_end(s, 1);
    hqqrpc_handle_consume(s, id[0]);
    hqqrpc_test_call_begin(s);
    {
        uint32_t x = 0;

        HQQ_CHECK_EQ_INT(hqqrpc_handle_reserve(s, 1), HQQRPC_OK);
        HQQ_CHECK_EQ_INT(hqqrpc_handle_put(s, 0, obj_ptr(MAX_OBJECTS - 2u), 0, &x), HQQRPC_E_INTERNAL);
        HQQ_CHECK_EQ_INT(hqqrpc_handle_put(s, 4, obj_ptr(MAX_OBJECTS - 2u), 0, &x), HQQRPC_E_INTERNAL);
        HQQ_CHECK_EQ_INT(hqqrpc_handle_put(s, TA, NULL, 0, &x), HQQRPC_E_INTERNAL);
        HQQ_CHECK_EQ_INT(hqqrpc_handle_put(s, TB, obj_ptr(MAX_OBJECTS - 2u), id[0], &x), HQQRPC_E_INTERNAL); /* id[0] was consumed */
        HQQ_CHECK_EQ_INT(hqqrpc_handle_put(s, TA, obj_ptr(MAX_OBJECTS - 2u), 0, NULL), HQQRPC_E_INTERNAL);
        HQQ_CHECK_EQ_INT(hqqrpc_handle_put(s, TA, obj_ptr(MAX_OBJECTS - 2u), 0, &x), HQQRPC_OK); /* the reservation is intact */
    }
    hqqrpc_test_call_end(s, 1);
    hqqrpc_test_session_free(s);
}

/* The largest table: 65535 handles, the table grows on demand. */
static void test_table_65535(void) {
    hqqrpc_session *s = new_session(65535);
    uint32_t id = 0;
    uint32_t n;
    uint32_t i;
    uint32_t last = 0;

    for (i = 1; i <= 65535u; i++) {
        HQQ_REQUIRE(put1(s, TA, 0, 0, &id, &n) == HQQRPC_OK);
        HQQ_REQUIRE((id & 0xFFFFu) == i && (id >> 16) == 1u);
        last = id;
    }
    HQQ_CHECK_EQ_U64(last, 0x0001FFFFu);
    HQQ_CHECK_EQ_INT(put1(s, TA, 0, 0, &id, &n), HQQRPC_E_LIMIT);
    HQQ_CHECK_EQ_INT(hqqrpc_test_live_handles(s), 65535);
    hqqrpc_test_session_free(s); /* teardown destroys all of them, newest first */
    HQQ_CHECK_EQ_INT(g_dcount[n], 1);
}

/* ---- Destroy semantics (5.1) ------------------------------------------------------------------------ */

static void log_reset(void) {
    g_nlog = 0;
}

static void check_log(const char *what, const uint32_t *want, size_t n) {
    size_t i;
    int same = g_nlog == n;

    for (i = 0; same && i < n; i++) {
        same = g_log[i] == want[i];
    }
    HQQ_CHECK(same);
    if (!same) {
        fprintf(stderr, "%s: destroy order:", what);
        for (i = 0; i < g_nlog; i++) {
            fprintf(stderr, " %u", (unsigned)g_log[i]);
        }
        fprintf(stderr, "\n   want:");
        for (i = 0; i < n; i++) {
            fprintf(stderr, " %u", (unsigned)want[i]);
        }
        fprintf(stderr, "\n");
    }
}

/* A failing destroy consumes the entry; with @retain_on_fail the glue keeps it (5.1 step 4). */
static void test_failed_destroy(void) {
    hqqrpc_session *s = new_session(0);
    uint32_t a;
    uint32_t parent;
    uint32_t child;
    uint32_t n_a;
    uint32_t n_p;
    uint32_t n_c;
    int failed = 0;
    void *p = NULL;

    HQQ_CHECK_EQ_INT(put1(s, TA, 0, 1, &a, &n_a), HQQRPC_OK); /* its destroy will fail */
    HQQ_CHECK_EQ_INT(glue_destroy(s, a, TA, 0, &failed), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(failed, 1);
    HQQ_CHECK_EQ_INT(g_dcount[n_a], 1);
    HQQ_CHECK_EQ_INT(get(s, a, TA, &p), HQQRPC_E_BAD_HANDLE);                    /* consumed whatever the status */
    HQQ_CHECK_EQ_INT(glue_destroy(s, a, TA, 0, &failed), HQQRPC_E_BAD_HANDLE);   /* vector R11 */
    HQQ_CHECK_EQ_INT(g_dcount[n_a], 1);                                          /* the library is not called again */
    HQQ_CHECK_EQ_INT(hqqrpc_test_live_handles(s), 0);

    /* retain_on_fail: the entry stays live and its children are gone (a failed parent destroy is not
     * rolled back); the object survived, so a second attempt may succeed */
    HQQ_CHECK_EQ_INT(put1(s, TA, 0, 1, &parent, &n_p), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(put1(s, TB, parent, 0, &child, &n_c), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(glue_destroy(s, parent, TA, 1, &failed), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(failed, 1);
    HQQ_CHECK_EQ_INT(g_dcount[n_p], 1);
    HQQ_CHECK_EQ_INT(get(s, parent, TA, &p), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(obj_number(p), n_p);
    HQQ_CHECK_EQ_INT(get(s, child, TB, &p), HQQRPC_E_BAD_HANDLE);
    HQQ_CHECK_EQ_INT(g_dcount[n_c], 1);
    HQQ_CHECK_EQ_INT(hqqrpc_test_live_handles(s), 1);
    g_fail[n_p] = 0;
    g_dcount[n_p] = 0;
    HQQ_CHECK_EQ_INT(glue_destroy(s, parent, TA, 1, &failed), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(failed, 0);
    HQQ_CHECK_EQ_INT(get(s, parent, TA, &p), HQQRPC_E_BAD_HANDLE);
    HQQ_CHECK_EQ_INT(hqqrpc_test_live_handles(s), 0);
    hqqrpc_test_session_free(s);
}

/* A cascade destroys the descendants newest first, consumes every entry, counts a failing child,
 * and the parent's own destroy still runs (5.1 step 2). */
static void test_cascade(void) {
    hqqrpc_session *s = new_session(0);
    uint32_t a;
    uint32_t a2;
    uint32_t b1;
    uint32_t b2;
    uint32_t c1;
    uint32_t b3;
    uint32_t n[6];
    int failed = 0;
    void *p = NULL;
    uint32_t want[8];

    HQQ_CHECK_EQ_INT(put1(s, TA, 0, 0, &a, &n[0]), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(put1(s, TB, a, 0, &b1, &n[1]), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(put1(s, TA, 0, 0, &a2, &n[2]), HQQRPC_OK); /* unrelated */
    HQQ_CHECK_EQ_INT(put1(s, TB, a, 1, &b2, &n[3]), HQQRPC_OK); /* its destroy fails */
    HQQ_CHECK_EQ_INT(put1(s, TC, b1, 0, &c1, &n[4]), HQQRPC_OK); /* grandchild, newer than b2 */
    HQQ_CHECK_EQ_INT(put1(s, TB, a2, 0, &b3, &n[5]), HQQRPC_OK); /* child of the unrelated A */

    log_reset();
    HQQ_CHECK_EQ_INT(glue_destroy(s, a, TA, 0, &failed), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(failed, 0);
    want[0] = n[4]; /* c1 */
    want[1] = n[3]; /* b2 (fails, consumed, counted) */
    want[2] = n[1]; /* b1 */
    want[3] = n[0]; /* a */
    check_log("cascade", want, 4);
    HQQ_CHECK_EQ_INT(hqqrpc_test_destroy_failures(s), 1);
    HQQ_CHECK_EQ_INT(get(s, b1, TB, &p), HQQRPC_E_BAD_HANDLE);
    HQQ_CHECK_EQ_INT(get(s, b2, TB, &p), HQQRPC_E_BAD_HANDLE); /* consumed although its destroy failed */
    HQQ_CHECK_EQ_INT(get(s, c1, TC, &p), HQQRPC_E_BAD_HANDLE);
    HQQ_CHECK_EQ_INT(get(s, a, TA, &p), HQQRPC_E_BAD_HANDLE);
    HQQ_CHECK_EQ_INT(get(s, a2, TA, &p), HQQRPC_OK); /* the other tree is untouched */
    HQQ_CHECK_EQ_INT(get(s, b3, TB, &p), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(hqqrpc_test_live_handles(s), 2);

    /* destroy_children of a dead or unknown id does nothing */
    log_reset();
    hqqrpc_exec_begin(s);
    hqqrpc_handle_destroy_children(s, a);
    hqqrpc_handle_destroy_children(s, 0);
    hqqrpc_handle_destroy_children(s, 0x7FFF0007u);
    hqqrpc_exec_end(s);
    HQQ_CHECK_EQ_INT(g_nlog, 0);

    /* the leaves of the other tree go with their parent; the failure counter keeps its value */
    HQQ_CHECK_EQ_INT(glue_destroy(s, a2, TA, 0, &failed), HQQRPC_OK);
    want[0] = n[5];
    want[1] = n[2];
    check_log("second cascade", want, 2);
    HQQ_CHECK_EQ_INT(hqqrpc_test_destroy_failures(s), 1);
    HQQ_CHECK_EQ_INT(hqqrpc_test_live_handles(s), 0);
    hqqrpc_test_session_free(s);
}

/* A parent link that went stale (the parent entry was consumed without a cascade and its index
 * reused) does not make the entry a descendant of the new owner of that index. */
static void test_stale_parent_link(void) {
    hqqrpc_session *s = new_session(0);
    uint32_t a;
    uint32_t b;
    uint32_t a2;
    uint32_t n[3];

    HQQ_CHECK_EQ_INT(put1(s, TA, 0, 0, &a, &n[0]), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(put1(s, TB, a, 0, &b, &n[1]), HQQRPC_OK);
    hqqrpc_handle_consume(s, a); /* a glue that skipped the cascade */
    HQQ_CHECK_EQ_INT(put1(s, TA, 0, 0, &a2, &n[2]), HQQRPC_OK);
    HQQ_CHECK_EQ_U64(a2 & 0xFFFFu, a & 0xFFFFu); /* the index was reused */
    log_reset();
    hqqrpc_exec_begin(s);
    hqqrpc_handle_destroy_children(s, a2);
    hqqrpc_exec_end(s);
    HQQ_CHECK_EQ_INT(g_nlog, 0); /* b is not a child of a2 */
    hqqrpc_test_session_free(s);
}

/* A call that fails after it registered handles destroys them (newest first) and frees the entries
 * (4.6.6); a call that succeeds keeps them. */
static void test_rollback(void) {
    hqqrpc_session *s = new_session(0);
    uint32_t id1 = 0;
    uint32_t id2 = 0;
    uint32_t keep = 0;
    uint32_t n[3];
    uint32_t want[2];
    void *p = NULL;

    hqqrpc_test_call_begin(s);
    HQQ_CHECK_EQ_INT(hqqrpc_handle_reserve(s, 2), HQQRPC_OK);
    n[0] = g_next_obj++;
    n[1] = g_next_obj++;
    g_fail[n[0]] = 0;
    g_fail[n[1]] = 1; /* a failing destroy during rollback is counted, not fatal */
    HQQ_CHECK_EQ_INT(hqqrpc_handle_put(s, TA, obj_ptr(n[0]), 0, &id1), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(hqqrpc_handle_put(s, TB, obj_ptr(n[1]), id1, &id2), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(hqqrpc_handle_put(s, TA, obj_ptr(MAX_OBJECTS - 3u), 0, &keep), HQQRPC_E_INTERNAL); /* only two were reserved */
    log_reset();
    hqqrpc_test_call_end(s, 0);
    want[0] = n[1];
    want[1] = n[0];
    check_log("rollback", want, 2);
    HQQ_CHECK_EQ_INT(hqqrpc_test_live_handles(s), 0);
    HQQ_CHECK_EQ_INT(hqqrpc_test_destroy_failures(s), 1);
    HQQ_CHECK_EQ_INT(get(s, id1, TA, &p), HQQRPC_E_BAD_HANDLE);
    HQQ_CHECK_EQ_INT(get(s, id2, TB, &p), HQQRPC_E_BAD_HANDLE);

    HQQ_CHECK_EQ_INT(put1(s, TA, 0, 0, &keep, &n[2]), HQQRPC_OK); /* a successful call keeps its handle */
    HQQ_CHECK_EQ_INT(get(s, keep, TA, &p), HQQRPC_OK);
    hqqrpc_test_session_free(s);
}

/* Teardown destroys every live handle newest first, ignores retain_on_fail, destroys nothing twice,
 * and CLOSE_ACK counts the failures of the cascades before it and of the teardown itself. */
static void test_teardown_order(void) {
    hqqrpc_session *s = new_session(0);
    uint8_t close[RTU_HDR];
    uint8_t out[128];
    size_t out_len = 0;
    uint32_t id[6];
    uint32_t n[6];
    uint32_t want[6];
    uint8_t hello[RTU_HDR + 64];
    int failed;

    HQQ_REQUIRE(rtu_x(s, hello, rtu_hello(hello, &g_iface, 16384, 64, 0, 0), out, sizeof out, &out_len) == HQQRPC_OK);
    HQQ_CHECK_EQ_INT(put1(s, TA, 0, 0, &id[0], &n[0]), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(put1(s, TB, id[0], 1, &id[1], &n[1]), HQQRPC_OK);  /* fails in the cascade below */
    HQQ_CHECK_EQ_INT(put1(s, TA, 0, 1, &id[2], &n[2]), HQQRPC_OK);      /* fails in the teardown */
    HQQ_CHECK_EQ_INT(put1(s, TB, id[2], 0, &id[3], &n[3]), HQQRPC_OK);
    HQQ_CHECK_EQ_INT(put1(s, TC, id[3], 1, &id[4], &n[4]), HQQRPC_OK);  /* fails in the teardown */
    HQQ_CHECK_EQ_INT(put1(s, TA, 0, 0, &id[5], &n[5]), HQQRPC_OK);

    HQQ_CHECK_EQ_INT(glue_destroy(s, id[0], TA, 0, &failed), HQQRPC_OK); /* cascade: n1 fails (1 failure) */
    HQQ_CHECK_EQ_INT(hqqrpc_test_destroy_failures(s), 1);
    log_reset();

    rtu_hdr(close, HQQRPC_K_CLOSE, 0, 0, 0, 0, 0);
    HQQ_CHECK_EQ_INT(rtu_x(s, close, RTU_HDR, out, sizeof out, &out_len), HQQRPC_OK);
    want[0] = n[5];
    want[1] = n[4];
    want[2] = n[3];
    want[3] = n[2];
    check_log("teardown", want, 4);
    HQQ_CHECK_EQ_INT(out_len, RTU_HDR);
    HQQ_CHECK_EQ_INT(out[0], HQQRPC_K_CLOSE_ACK);
    HQQ_CHECK_EQ_U64(rtu_le32(out + 12), 3); /* 1 from the cascade, 2 from the teardown */
    HQQ_CHECK_EQ_INT(hqqrpc_test_live_handles(s), 0);
    hqqrpc_test_session_free(s);
    HQQ_CHECK_EQ_INT(g_nlog, 4); /* the final free destroys nothing again */
}

/* ---- Randomized comparison with a reference model ------------------------------------------------------- */

typedef struct mobj {
    uint32_t num;      /* object number (the pointer) */
    uint32_t id;
    uint16_t type;
    int parent;        /* model index of the parent, -1 for none */
    int live;
    int fail;
} mobj;

static uint64_t rng_state = UINT64_C(0x9E3779B97F4A7C15);

static uint32_t rnd(void) {
    uint64_t x = rng_state;

    x ^= x << 13;
    x ^= x >> 7;
    x ^= x << 17;
    rng_state = x;
    return (uint32_t)(x >> 16);
}

static mobj M[60000];
static int g_nm;

static int is_desc(int idx, int root) {
    int p = M[idx].parent;

    while (p >= 0) {
        if (p == root) {
            return 1;
        }
        p = M[p].parent;
    }
    return 0;
}

static int pick_live(int type) {
    int cand[64];
    int nc = 0;
    int i;
    int start = g_nm > 200 ? g_nm - 200 : 0;

    for (i = g_nm - 1; i >= start && nc < 64; i--) {
        if (M[i].live && (type == 0 || M[i].type == type)) {
            cand[nc++] = i;
        }
    }
    return nc == 0 ? -1 : cand[rnd() % (uint32_t)nc];
}

/* Expected destroy order of a cascade below `root`: live descendants, newest creation first. */
static size_t model_cascade(int root, uint32_t *order, unsigned *failures) {
    size_t n = 0;
    int i;

    for (i = g_nm - 1; i >= 0; i--) {
        if (M[i].live && i != root && is_desc(i, root)) {
            order[n++] = M[i].num;
            M[i].live = 0;
            if (M[i].fail) {
                (*failures)++;
            }
        }
    }
    return n;
}

static void model_verify(hqqrpc_session *s, int live_count_only) {
    int i;
    int live = 0;
    int start = live_count_only ? (g_nm > 300 ? g_nm - 300 : 0) : 0;
    void *p = NULL;

    for (i = 0; i < g_nm; i++) {
        live += M[i].live;
    }
    HQQ_CHECK_EQ_INT(hqqrpc_test_live_handles(s), live);
    for (i = start; i < g_nm; i++) {
        hqqrpc_status st = hqqrpc_handle_get(s, M[i].id, M[i].type, 0, &p);

        if (M[i].live) {
            HQQ_CHECK_EQ_INT(st, HQQRPC_OK);
            if (st == HQQRPC_OK) {
                HQQ_CHECK_EQ_INT(obj_number(p), M[i].num);
            }
        } else {
            HQQ_CHECK_EQ_INT(st, HQQRPC_E_BAD_HANDLE); /* a stale id is never accepted here */
        }
        HQQ_CHECK_EQ_INT(hqqrpc_handle_get(s, M[i].id, (uint16_t)(M[i].type % 3u + 1u), 0, &p), HQQRPC_E_BAD_HANDLE); /* wrong type */
    }
}

static void test_model(uint32_t max_handles, unsigned ops) {
    hqqrpc_session *s = new_session(max_handles);
    uint8_t hello[RTU_HDR + 64];
    uint8_t out[128];
    size_t out_len = 0;
    unsigned failures = 0;
    unsigned op;
    uint32_t order[4096];
    size_t log_start;

    g_nm = 0;
    log_reset();
    HQQ_REQUIRE(rtu_x(s, hello, rtu_hello(hello, &g_iface, 16384, 64, 0, 0), out, sizeof out, &out_len) == HQQRPC_OK);

    for (op = 0; op < ops; op++) {
        uint32_t r = rnd() % 100u;
        int i;
        if (r < 40) { /* create */
            uint16_t type = (uint16_t)(1u + rnd() % 3u);
            int parent = type == TA ? -1 : pick_live(type - 1);
            uint32_t id = 0;
            uint32_t num = 0;
            int total_live = 0;
            hqqrpc_status st;

            if (type != TA && parent < 0) {
                continue;
            }
            for (i = 0; i < g_nm; i++) {
                total_live += M[i].live;
            }
            st = put1(s, type, parent < 0 ? 0 : M[parent].id, (int)(rnd() % 4u == 0), &id, &num);
            if ((uint32_t)total_live >= max_handles) {
                HQQ_CHECK_EQ_INT(st, HQQRPC_E_LIMIT);
                HQQ_CHECK_EQ_U64(id, 0);
                continue;
            }
            HQQ_REQUIRE(st == HQQRPC_OK);
            HQQ_REQUIRE(g_nm < (int)(sizeof M / sizeof M[0]));
            HQQ_CHECK(id != 0 && (id & 0xFFFFu) >= 1u && (id & 0xFFFFu) <= max_handles && (id >> 16) >= 1u);
            for (i = 0; i < g_nm; i++) {
                if (M[i].live && M[i].id == id) {
                    HQQ_CHECK(0); /* an id of a live handle was issued again */
                }
            }
            M[g_nm].num = num;
            M[g_nm].id = id;
            M[g_nm].type = type;
            M[g_nm].parent = parent;
            M[g_nm].live = 1;
            M[g_nm].fail = g_fail[num];
            g_nm++;
        } else if (r < 70) { /* explicit destroy through the glue; type C has @retain_on_fail */
            int t = pick_live(0);
            size_t n;
            int failed = 0;
            int retain;
            uint32_t want[4097];
            size_t wn;

            if (t < 0) {
                continue;
            }
            retain = M[t].type == TC;
            log_start = g_nlog;
            n = model_cascade(t, order, &failures);
            memcpy(want, order, n * sizeof order[0]);
            want[n] = M[t].num;
            wn = n + 1;
            HQQ_REQUIRE(glue_destroy(s, M[t].id, M[t].type, retain, &failed) == HQQRPC_OK);
            HQQ_CHECK_EQ_INT(failed, M[t].fail);
            if (!(retain && M[t].fail)) {
                M[t].live = 0;
            }
            HQQ_CHECK_EQ_INT(g_nlog - log_start, wn);
            for (i = 0; (size_t)i < wn && log_start + (size_t)i < g_nlog; i++) {
                HQQ_CHECK_EQ_U64(g_log[log_start + (size_t)i], want[i]);
            }
            if (retain && M[t].fail) {
                /* the object survived: it can be destroyed again, now successfully */
                g_fail[M[t].num] = 0;
                M[t].fail = 0;
                g_dcount[M[t].num] = 0;
            }
        } else if (r < 85) { /* lookups of live and stale ids */
            model_verify(s, 1);
        } else if (r < 92) { /* cascade only */
            int t = pick_live(0);
            size_t n;

            if (t < 0) {
                continue;
            }
            log_start = g_nlog;
            n = model_cascade(t, order, &failures);
            hqqrpc_exec_begin(s);
            hqqrpc_handle_destroy_children(s, M[t].id);
            hqqrpc_exec_end(s);
            HQQ_CHECK_EQ_INT(g_nlog - log_start, n);
            for (i = 0; (size_t)i < n && log_start + (size_t)i < g_nlog; i++) {
                HQQ_CHECK_EQ_U64(g_log[log_start + (size_t)i], order[i]);
            }
        } else { /* a call that fails after registering two handles */
            uint32_t a = 0;
            uint32_t b = 0;
            uint32_t na;
            uint32_t nb;
            uint32_t total_live = 0;

            for (i = 0; i < g_nm; i++) {
                total_live += (uint32_t)M[i].live;
            }
            if (total_live + 2u > max_handles) {
                continue;
            }
            hqqrpc_test_call_begin(s);
            HQQ_REQUIRE(hqqrpc_handle_reserve(s, 2) == HQQRPC_OK);
            na = g_next_obj++;
            nb = g_next_obj++;
            g_fail[na] = (uint8_t)(rnd() % 3u == 0);
            g_fail[nb] = 0;
            HQQ_REQUIRE(hqqrpc_handle_put(s, TA, obj_ptr(na), 0, &a) == HQQRPC_OK);
            HQQ_REQUIRE(hqqrpc_handle_put(s, TB, obj_ptr(nb), a, &b) == HQQRPC_OK);
            log_start = g_nlog;
            hqqrpc_test_call_end(s, 0);
            HQQ_CHECK_EQ_INT(g_nlog - log_start, 2);
            if (g_nlog - log_start == 2) {
                HQQ_CHECK_EQ_U64(g_log[log_start], nb);
                HQQ_CHECK_EQ_U64(g_log[log_start + 1], na);
            }
            failures += g_fail[na];
        }
        HQQ_CHECK_EQ_INT(hqqrpc_test_destroy_failures(s), failures);
    }
    model_verify(s, 0);

    /* teardown: every live handle, newest first */
    {
        uint8_t close[RTU_HDR];
        uint32_t expect[70000];
        size_t ne = 0;
        int i;
        unsigned fails_before = failures;

        for (i = g_nm - 1; i >= 0; i--) {
            if (M[i].live) {
                expect[ne++] = M[i].num;
                failures += (unsigned)M[i].fail;
            }
        }
        log_start = g_nlog;
        rtu_hdr(close, HQQRPC_K_CLOSE, 0, 0, 0, 0, 0);
        HQQ_CHECK_EQ_INT(rtu_x(s, close, RTU_HDR, out, sizeof out, &out_len), HQQRPC_OK);
        HQQ_CHECK_EQ_INT(g_nlog - log_start, ne);
        for (i = 0; (size_t)i < ne && log_start + (size_t)i < g_nlog; i++) {
            HQQ_CHECK_EQ_U64(g_log[log_start + (size_t)i], expect[i]);
        }
        HQQ_CHECK_EQ_U64(rtu_le32(out + 12), failures);
        HQQ_CHECK(failures >= fails_before);
        HQQ_CHECK_EQ_INT(hqqrpc_test_live_handles(s), 0);
    }
    hqqrpc_test_session_free(s);
}

int main(void) {
    test_ids_and_reuse();
    test_generation_wrap();
    test_limits();
    test_table_65535();
    test_failed_destroy();
    test_cascade();
    test_stale_parent_link();
    test_rollback();
    test_teardown_order();
    test_model(8, 20000);
    test_model(40, 20000);
    test_model(4096, 8000);
    return HQQ_TEST_RESULT("rpc_handles");
}
