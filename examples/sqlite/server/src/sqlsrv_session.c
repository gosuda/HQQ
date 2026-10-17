#include "sqlsrv_session.h"

#include <limits.h>
#include <math.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

/* How often a waiting handler re-checks its request context (ms). */
#define SQLSRV_POLL_MS 5L
/* Output buffers above this size are released after each request. */
#define SQLSRV_OUT_KEEP_BYTES (UINT64_C(1) << 20)

struct sqlsrv_job {
    uint64_t method;
    const uint8_t *body;
    size_t len;
    hqq_req_ctx *ctx;
    int done;   /* guarded by the session mutex */
    int failed; /* set by the worker */
    sqlsrv_err err;
};

struct sqlsrv_stmt {
    sqlsrv_stmt *next;
    uint32_t id;
    sqlite3_stmt *st;
    sqlsrv_cursor *cur; /* the open cursor of this statement, if any */
};

struct sqlsrv_cursor {
    sqlsrv_cursor *next;
    uint32_t id;
    sqlite3_stmt *st;
    sqlsrv_stmt *owner; /* the prepared statement, NULL when the cursor owns st */
    int owned;
    uint32_t fetch;
    int has_pend;     /* a row was stepped but did not fit the last response */
    sqlsrv_buf pend;  /* its encoded values */
};

typedef struct sqlsrv_param {
    uint32_t ordinal;
    const uint8_t *name;
    uint32_t namelen;
    uint8_t tag;
    int64_t i;
    double d;
    const uint8_t *p;
    uint32_t n;
} sqlsrv_param;

/* ---- Shared state ------------------------------------------------------ */

int sqlsrv_shared_init(sqlsrv_shared *sh) {
    memset(sh, 0, sizeof *sh);
    sqlsrv_policy_init(&sh->policy);
    sh->max_sessions = 256;
    sh->shutdown_timeout_ns = INT64_C(5000000000);
    atomic_init(&sh->shutting, 0);
    if (sqlsrv_mutex_init(&sh->mu) != 0) {
        return -1;
    }
    if (sqlsrv_cond_init(&sh->cv) != 0) {
        sqlsrv_mutex_destroy(&sh->mu);
        return -1;
    }
    return 0;
}

void sqlsrv_shared_destroy(sqlsrv_shared *sh) {
    sqlsrv_policy_free(&sh->policy);
    sqlsrv_cond_destroy(&sh->cv);
    sqlsrv_mutex_destroy(&sh->mu);
}

static int reserve_session(sqlsrv_shared *sh, sqlsrv_err *e) {
    int ok = 0;
    sqlsrv_mutex_lock(&sh->mu);
    if (sh->open_sessions < sh->max_sessions) {
        sh->open_sessions++;
        ok = 1;
    }
    sqlsrv_mutex_unlock(&sh->mu);
    if (!ok) {
        sqlsrv_errf(e, SQLSRV_STATUS_PROTO(SQLSRV_PE_LIMIT), "too many sessions (limit %u)", sh->max_sessions);
        return -1;
    }
    return 0;
}

static void release_session(sqlsrv_shared *sh) {
    sqlsrv_mutex_lock(&sh->mu);
    sh->open_sessions--;
    sqlsrv_mutex_unlock(&sh->mu);
}

/* ---- Helpers ----------------------------------------------------------- */

static int ctx_ended(const hqq_req_ctx *ctx) {
    return hqq_req_ctx_err(ctx) != HQQ_OK;
}

static int fail(sqlsrv_err *e, enum sqlsrv_perr code, const char *msg) {
    sqlsrv_err_proto(e, code, msg);
    return -1;
}

/* Error of a SQLite call: the extended code and message of db (or rc alone). */
static int err_db(sqlite3 *db, int rc, sqlsrv_err *e) {
    int code = rc;
    const char *msg = sqlite3_errstr(rc);
    if (db != NULL) {
        int ext = sqlite3_extended_errcode(db);
        if (ext != SQLITE_OK) {
            code = ext;
        }
        msg = sqlite3_errmsg(db);
        if (msg == NULL) {
            msg = sqlite3_errstr(code);
        }
    }
    sqlsrv_errf(e, SQLSRV_STATUS_SQLITE(code), "%s", msg);
    return -1;
}

static uint32_t clamp_fetch(uint32_t n) {
    if (n == 0) {
        return SQLSRV_DEFAULT_FETCH_ROWS;
    }
    return n > SQLSRV_MAX_FETCH_ROWS ? SQLSRV_MAX_FETCH_ROWS : n;
}

/* Hands out the next id of a counter that starts at 1; 0 is never valid. */
static int alloc_id(uint32_t *counter, uint32_t *out, sqlsrv_err *e, const char *what) {
    if (*counter == 0) {
        sqlsrv_errf(e, SQLSRV_STATUS_PROTO(SQLSRV_PE_LIMIT), "%s ids are exhausted", what);
        return -1;
    }
    *out = *counter;
    *counter = *counter + 1;
    return 0;
}

static const char *method_name(uint64_t m) {
    static const char *const names[] = {
        "", "Open", "Exec", "Query", "Fetch", "CloseCursor", "Prepare", "ExecStmt",
        "QueryStmt", "CloseStmt", "Begin", "Commit", "Rollback", "Ping", "Close",
    };
    if (m >= 1 && m < sizeof names / sizeof names[0]) {
        return names[m];
    }
    return "request";
}

static int authorizer(void *ud, int action, const char *a1, const char *a2, const char *a3, const char *a4) {
    (void)ud;
    (void)a1;
    (void)a2;
    (void)a3;
    (void)a4;
    if (action == SQLITE_ATTACH || action == SQLITE_DETACH) {
        return SQLITE_DENY;
    }
    return SQLITE_OK;
}

/* ---- Statements and cursors -------------------------------------------- */

static sqlsrv_stmt *find_stmt(sqlsrv_session *s, uint32_t id) {
    sqlsrv_stmt *st;
    for (st = s->stmts; st != NULL; st = st->next) {
        if (st->id == id) {
            return st;
        }
    }
    return NULL;
}

static sqlsrv_cursor *find_cursor(sqlsrv_session *s, uint32_t id) {
    sqlsrv_cursor *c;
    for (c = s->cursors; c != NULL; c = c->next) {
        if (c->id == id) {
            return c;
        }
    }
    return NULL;
}

/* Releases a cursor that is linked into the session (or not). */
static void cursor_free(sqlsrv_cursor *c) {
    if (c->owned) {
        sqlite3_finalize(c->st);
    } else {
        sqlite3_reset(c->st);
        if (c->owner != NULL) {
            c->owner->cur = NULL;
        }
    }
    sqlsrv_buf_free(&c->pend);
    free(c);
}

static void drop_cursor(sqlsrv_session *s, sqlsrv_cursor *c) {
    sqlsrv_cursor **link = &s->cursors;
    while (*link != NULL && *link != c) {
        link = &(*link)->next;
    }
    if (*link == c) {
        *link = c->next;
        s->ncursors--;
    }
    cursor_free(c);
}

static void drop_stmt(sqlsrv_session *s, sqlsrv_stmt *st) {
    sqlsrv_stmt **link = &s->stmts;
    while (*link != NULL && *link != st) {
        link = &(*link)->next;
    }
    if (*link == st) {
        *link = st->next;
        s->nstmts--;
    }
    if (st->cur != NULL) {
        drop_cursor(s, st->cur);
    }
    sqlite3_finalize(st->st);
    free(st);
}

/* Reads the sql of a request: a string, checked for length and NUL. */
static int read_sql(sqlsrv_rd *r, const uint8_t **sql, uint32_t *n, sqlsrv_err *e) {
    if (sqlsrv_rd_str(r, sql, n) != 0) {
        return fail(e, SQLSRV_PE_BAD_REQUEST, "truncated request");
    }
    if (*n > SQLSRV_MAX_SQL_BYTES) {
        sqlsrv_err_proto(e, SQLSRV_PE_LIMIT, "SQL text exceeds 1048576 bytes");
        return -1;
    }
    return 0;
}

/*
 * Prepares exactly one statement (PROTOCOL.md section 6.2). Trailing
 * whitespace, comments and ';' are allowed; anything else is "multiple
 * statements", and no text at all is "empty statement".
 */
static int prepare_one(sqlsrv_session *s, const uint8_t *sql, size_t n, sqlite3_stmt **out, sqlsrv_err *e) {
    sqlite3_stmt *st = NULL;
    const char *tail = NULL;
    const char *text = n > 0 ? (const char *)sql : "";
    size_t rest;
    int rc;

    *out = NULL;
    if (n > 0 && memchr(sql, '\0', n) != NULL) {
        return fail(e, SQLSRV_PE_BAD_REQUEST, "SQL text contains a NUL byte");
    }
    rc = sqlite3_prepare_v2(s->db, text, (int)n, &st, &tail);
    if (rc != SQLITE_OK) {
        return err_db(s->db, rc, e);
    }
    if (st == NULL) {
        return fail(e, SQLSRV_PE_BAD_REQUEST, "empty statement");
    }
    rest = n - (size_t)(tail - text);
    while (rest > 0) {
        sqlite3_stmt *more = NULL;
        const char *next = NULL;
        size_t adv;
        rc = sqlite3_prepare_v2(s->db, tail, (int)rest, &more, &next);
        if (more != NULL || rc != SQLITE_OK || next == NULL) {
            sqlite3_finalize(more);
            sqlite3_finalize(st);
            return fail(e, SQLSRV_PE_BAD_REQUEST, "multiple statements");
        }
        adv = (size_t)(next - tail);
        if (adv == 0) {
            break;
        }
        tail = next;
        rest -= adv;
    }
    *out = st;
    return 0;
}

/* ---- Parameters -------------------------------------------------------- */

static int parse_params(sqlsrv_rd *r, sqlsrv_param **out, uint32_t *count, sqlsrv_err *e) {
    /* One bit per ordinal: duplicates are a bad request. */
    uint8_t seen[(SQLSRV_MAX_PARAMS + 8u) / 8u];
    sqlsrv_param *ps;
    uint32_t n;
    uint32_t i;

    *out = NULL;
    *count = 0;
    if (sqlsrv_rd_u32(r, &n) != 0) {
        return fail(e, SQLSRV_PE_BAD_REQUEST, "truncated request");
    }
    if (n > SQLSRV_MAX_PARAMS) {
        sqlsrv_err_proto(e, SQLSRV_PE_LIMIT, "too many parameters (limit 32766)");
        return -1;
    }
    if (n == 0) {
        return 0;
    }
    ps = calloc(n, sizeof *ps);
    if (ps == NULL) {
        return fail(e, SQLSRV_PE_INTERNAL, "out of memory");
    }
    memset(seen, 0, sizeof seen);
    for (i = 0; i < n; i++) {
        sqlsrv_param *p = &ps[i];
        uint32_t bit;
        if (sqlsrv_rd_u32(r, &p->ordinal) != 0 || sqlsrv_rd_str(r, &p->name, &p->namelen) != 0) {
            goto truncated;
        }
        if (p->ordinal < 1 || p->ordinal > SQLSRV_MAX_PARAMS) {
            free(ps);
            return fail(e, SQLSRV_PE_BAD_REQUEST, "parameter ordinal out of range");
        }
        bit = p->ordinal;
        if ((seen[bit / 8u] & (1u << (bit % 8u))) != 0) {
            free(ps);
            return fail(e, SQLSRV_PE_BAD_REQUEST, "duplicate parameter ordinal");
        }
        seen[bit / 8u] |= (uint8_t)(1u << (bit % 8u));
        if (p->namelen > SQLSRV_MAX_NAME_BYTES) {
            free(ps);
            sqlsrv_err_proto(e, SQLSRV_PE_LIMIT, "parameter name exceeds 1024 bytes");
            return -1;
        }
        if (p->namelen > 0 && memchr(p->name, '\0', p->namelen) != NULL) {
            free(ps);
            return fail(e, SQLSRV_PE_BAD_REQUEST, "parameter name contains a NUL byte");
        }
        if (sqlsrv_rd_u8(r, &p->tag) != 0) {
            goto truncated;
        }
        switch (p->tag) {
        case SQLSRV_TAG_INTEGER:
            if (sqlsrv_rd_i64(r, &p->i) != 0) {
                goto truncated;
            }
            break;
        case SQLSRV_TAG_FLOAT:
            if (sqlsrv_rd_f64(r, &p->d) != 0) {
                goto truncated;
            }
            break;
        case SQLSRV_TAG_TEXT:
        case SQLSRV_TAG_BLOB:
            if (sqlsrv_rd_str(r, &p->p, &p->n) != 0) {
                goto truncated;
            }
            break;
        case SQLSRV_TAG_NULL:
            break;
        default:
            free(ps);
            return fail(e, SQLSRV_PE_BAD_REQUEST, "invalid value type");
        }
    }
    *out = ps;
    *count = n;
    return 0;

truncated:
    free(ps);
    return fail(e, SQLSRV_PE_BAD_REQUEST, "truncated request");
}

static int bind_value(sqlite3_stmt *st, int idx, const sqlsrv_param *p) {
    switch (p->tag) {
    case SQLSRV_TAG_INTEGER:
        return sqlite3_bind_int64(st, idx, p->i);
    case SQLSRV_TAG_FLOAT:
        return sqlite3_bind_double(st, idx, p->d);
    case SQLSRV_TAG_TEXT:
        return sqlite3_bind_text64(st, idx, p->n > 0 ? (const char *)p->p : "", p->n, SQLITE_TRANSIENT,
                                   SQLITE_UTF8);
    case SQLSRV_TAG_BLOB:
        if (p->n == 0) {
            return sqlite3_bind_zeroblob64(st, idx, 0);
        }
        return sqlite3_bind_blob64(st, idx, p->p, p->n, SQLITE_TRANSIENT);
    default:
        return sqlite3_bind_null(st, idx);
    }
}

/* Binds the parameters in request order (PROTOCOL.md section 4). */
static int bind_params(sqlsrv_session *s, sqlite3_stmt *st, const sqlsrv_param *ps, uint32_t n, sqlsrv_err *e) {
    static const char prefixes[] = ":@$";
    int nparams = sqlite3_bind_parameter_count(st);
    uint32_t i;

    for (i = 0; i < n; i++) {
        const sqlsrv_param *p = &ps[i];
        int idx = 0;
        int rc;
        if (p->namelen == 0) {
            if (p->ordinal > (uint32_t)nparams) {
                sqlsrv_errf(e, SQLSRV_STATUS_SQLITE(SQLITE_RANGE), "parameter index out of range");
                return -1;
            }
            idx = (int)p->ordinal;
        } else {
            char name[SQLSRV_MAX_NAME_BYTES + 2];
            int k;
            for (k = 0; k < 3 && idx == 0; k++) {
                name[0] = prefixes[k];
                memcpy(name + 1, p->name, p->namelen);
                name[p->namelen + 1] = '\0';
                idx = sqlite3_bind_parameter_index(st, name);
            }
            if (idx == 0) {
                sqlsrv_errf(e, SQLSRV_STATUS_SQLITE(SQLITE_RANGE), "no such parameter: %.*s", (int)p->namelen,
                            (const char *)p->name);
                return -1;
            }
        }
        rc = bind_value(st, idx, p);
        if (rc != SQLITE_OK) {
            return err_db(s->db, rc, e);
        }
    }
    return 0;
}

/* ---- Rows and batches -------------------------------------------------- */

static int write_columns(sqlsrv_buf *b, sqlite3_stmt *st, sqlsrv_err *e) {
    int ncols = sqlite3_column_count(st);
    int i;

    if (ncols < 0 || (unsigned)ncols > SQLSRV_MAX_COLUMNS) {
        sqlsrv_err_proto(e, SQLSRV_PE_LIMIT, "too many columns (limit 2000)");
        return -1;
    }
    sqlsrv_buf_u32(b, (uint32_t)ncols);
    for (i = 0; i < ncols; i++) {
        const char *name = sqlite3_column_name(st, i);
        const char *decl = sqlite3_column_decltype(st, i);
        size_t nl;
        if (name == NULL) {
            return fail(e, SQLSRV_PE_INTERNAL, "column name unavailable");
        }
        nl = strlen(name);
        if (nl > SQLSRV_MAX_NAME_BYTES) {
            sqlsrv_err_proto(e, SQLSRV_PE_LIMIT, "column name exceeds 1024 bytes");
            return -1;
        }
        sqlsrv_buf_str(b, name, nl);
        if (decl == NULL) {
            sqlsrv_buf_u32(b, 0);
        } else {
            sqlsrv_buf_str(b, decl, strlen(decl));
        }
    }
    return 0;
}

/*
 * Encoded size of the current row, measured without copying a value. The
 * type is read before any accessor converts the value, and the same accessor
 * is used for the copy, so the sizes agree.
 */
static int row_size(sqlsrv_session *s, sqlite3_stmt *st, int ncols, uint64_t *size, sqlsrv_err *e) {
    uint64_t total = 0;
    int i;
    for (i = 0; i < ncols; i++) {
        switch (sqlite3_column_type(st, i)) {
        case SQLITE_INTEGER:
            total += 9;
            break;
        case SQLITE_FLOAT: {
            double d = sqlite3_column_double(st, i);
            total += d != d ? 1 : 9; /* NaN is sent as NULL */
            break;
        }
        case SQLITE_TEXT: {
            const void *p = sqlite3_column_text(st, i);
            int n = sqlite3_column_bytes(st, i);
            if (p == NULL && sqlite3_errcode(s->db) == SQLITE_NOMEM) {
                return err_db(s->db, SQLITE_NOMEM, e);
            }
            total += 5 + (uint64_t)n;
            break;
        }
        case SQLITE_BLOB: {
            const void *p = sqlite3_column_blob(st, i);
            int n = sqlite3_column_bytes(st, i);
            if (p == NULL && sqlite3_errcode(s->db) == SQLITE_NOMEM) {
                return err_db(s->db, SQLITE_NOMEM, e);
            }
            total += 5 + (uint64_t)n;
            break;
        }
        default:
            total += 1;
            break;
        }
    }
    *size = total;
    return 0;
}

static void encode_row(sqlite3_stmt *st, int ncols, sqlsrv_buf *b) {
    int i;
    for (i = 0; i < ncols; i++) {
        switch (sqlite3_column_type(st, i)) {
        case SQLITE_INTEGER:
            sqlsrv_buf_u8(b, SQLSRV_TAG_INTEGER);
            sqlsrv_buf_i64(b, sqlite3_column_int64(st, i));
            break;
        case SQLITE_FLOAT: {
            double d = sqlite3_column_double(st, i);
            if (d != d) {
                sqlsrv_buf_u8(b, SQLSRV_TAG_NULL);
            } else {
                sqlsrv_buf_u8(b, SQLSRV_TAG_FLOAT);
                sqlsrv_buf_f64(b, d);
            }
            break;
        }
        case SQLITE_TEXT: {
            const void *p = sqlite3_column_text(st, i);
            int n = sqlite3_column_bytes(st, i);
            sqlsrv_buf_u8(b, SQLSRV_TAG_TEXT);
            sqlsrv_buf_str(b, p, (size_t)n);
            break;
        }
        case SQLITE_BLOB: {
            const void *p = sqlite3_column_blob(st, i);
            int n = sqlite3_column_bytes(st, i);
            sqlsrv_buf_u8(b, SQLSRV_TAG_BLOB);
            sqlsrv_buf_str(b, p, (size_t)n);
            break;
        }
        default:
            sqlsrv_buf_u8(b, SQLSRV_TAG_NULL);
            break;
        }
    }
}

/*
 * Writes one batch (PROTOCOL.md section 5) into b: a u32 row count and the
 * rows. Stops at n rows, at the byte budget, at the message size, or at the
 * end of the statement. *more is 1 when the cursor is still open.
 */
static int fill_batch(sqlsrv_session *s, sqlsrv_job *j, sqlsrv_cursor *c, sqlsrv_buf *b, uint32_t n, int *more,
                      sqlsrv_err *e) {
    size_t pos = b->len;
    uint32_t nrows = 0;
    int ncols = sqlite3_column_count(c->st);

    sqlsrv_buf_u32(b, 0);
    *more = 0;
    for (;;) {
        int rc;
        uint64_t sz;
        if (nrows >= n) {
            *more = 1;
            break;
        }
        if (nrows > 0 && b->len - pos >= SQLSRV_MAX_BATCH_BYTES) {
            *more = 1;
            break;
        }
        if (c->has_pend) {
            if (b->len + c->pend.len > SQLSRV_MAX_MESSAGE_BYTES) {
                *more = 1;
                break;
            }
            sqlsrv_buf_bytes(b, c->pend.p, c->pend.len);
            c->has_pend = 0;
            c->pend.len = 0;
            nrows++;
            continue;
        }
        if (ctx_ended(j->ctx)) {
            sqlsrv_errf(e, SQLSRV_STATUS_SQLITE(SQLITE_INTERRUPT), "request canceled");
            return -1;
        }
        rc = sqlite3_step(c->st);
        if (rc == SQLITE_DONE) {
            *more = 0;
            break;
        }
        if (rc != SQLITE_ROW) {
            return err_db(s->db, rc, e);
        }
        if (row_size(s, c->st, ncols, &sz, e) != 0) {
            return -1;
        }
        if (sz + 5 > SQLSRV_MAX_MESSAGE_BYTES) {
            sqlsrv_errf(e, SQLSRV_STATUS_SQLITE(SQLITE_TOOBIG), "row exceeds message size limit");
            return -1;
        }
        if (b->len + sz > SQLSRV_MAX_MESSAGE_BYTES) {
            /* The row fits a message of its own, not this one: keep it for the next batch. */
            c->pend.len = 0;
            encode_row(c->st, ncols, &c->pend);
            if (c->pend.oom) {
                return fail(e, SQLSRV_PE_INTERNAL, "out of memory");
            }
            c->has_pend = 1;
            *more = 1;
            break;
        }
        encode_row(c->st, ncols, b);
        nrows++;
    }
    if (b->oom) {
        return fail(e, SQLSRV_PE_INTERNAL, "out of memory");
    }
    sqlsrv_buf_put_u32(b, pos, nrows);
    return 0;
}

/* Steps a statement to SQLITE_DONE, discarding rows and checking the request between steps. */
static int step_done(sqlsrv_session *s, sqlsrv_job *j, sqlite3_stmt *st, sqlsrv_err *e) {
    for (;;) {
        int rc;
        if (ctx_ended(j->ctx)) {
            sqlsrv_errf(e, SQLSRV_STATUS_SQLITE(SQLITE_INTERRUPT), "request canceled");
            return -1;
        }
        rc = sqlite3_step(st);
        if (rc == SQLITE_DONE) {
            return 0;
        }
        if (rc != SQLITE_ROW) {
            return err_db(s->db, rc, e);
        }
    }
}

/*
 * Runs a statement with a result. A statement without columns runs to the end
 * here. Otherwise it becomes a cursor (owned by the statement, or by the
 * prepared statement `owner`) and its first batch is written to s->out. The
 * function takes ownership of st: on failure it is released.
 */
static int start_query(sqlsrv_session *s, sqlsrv_job *j, sqlite3_stmt *st, sqlsrv_stmt *owner, uint32_t fetch,
                       sqlsrv_err *e) {
    int ncols = sqlite3_column_count(st);
    sqlsrv_cursor *c;
    size_t pos_id;
    size_t pos_more;
    int more = 0;
    uint32_t id = 0;

    if (ncols == 0) {
        int rc = step_done(s, j, st, e);
        if (owner != NULL) {
            sqlite3_reset(st);
        } else {
            sqlite3_finalize(st);
        }
        if (rc != 0) {
            return -1;
        }
        /* cursor = 0, Columns = empty, more = 0, empty batch. */
        sqlsrv_buf_u32(&s->out, 0);
        sqlsrv_buf_u32(&s->out, 0);
        sqlsrv_buf_u8(&s->out, 0);
        sqlsrv_buf_u32(&s->out, 0);
        return 0;
    }
    if (ncols > (int)SQLSRV_MAX_COLUMNS) {
        goto limit_fail;
    }
    if (s->ncursors >= SQLSRV_MAX_CURSORS) {
        sqlsrv_errf(e, SQLSRV_STATUS_PROTO(SQLSRV_PE_LIMIT), "too many open cursors (limit %u)", SQLSRV_MAX_CURSORS);
        goto release_fail;
    }
    c = calloc(1, sizeof *c);
    if (c == NULL) {
        (void)fail(e, SQLSRV_PE_INTERNAL, "out of memory");
        goto release_fail;
    }
    c->st = st;
    c->owner = owner;
    c->owned = owner == NULL;
    c->fetch = clamp_fetch(fetch);

    pos_id = s->out.len;
    sqlsrv_buf_u32(&s->out, 0); /* cursor id, patched below */
    if (write_columns(&s->out, st, e) != 0) {
        cursor_free(c);
        return -1;
    }
    pos_more = s->out.len;
    sqlsrv_buf_u8(&s->out, 0);
    if (fill_batch(s, j, c, &s->out, c->fetch, &more, e) != 0) {
        cursor_free(c);
        return -1;
    }
    if (more) {
        if (alloc_id(&s->next_cursor, &id, e, "cursor") != 0) {
            cursor_free(c);
            return -1;
        }
        c->id = id;
        c->next = s->cursors;
        s->cursors = c;
        s->ncursors++;
        if (owner != NULL) {
            owner->cur = c;
        }
        sqlsrv_buf_put_u32(&s->out, pos_id, id);
        sqlsrv_buf_put_u8(&s->out, pos_more, 1);
    } else {
        /* The statement is done: no cursor id (PROTOCOL.md section 5). */
        sqlsrv_buf_put_u32(&s->out, pos_id, 0);
        cursor_free(c);
    }
    return 0;

limit_fail:
    sqlsrv_err_proto(e, SQLSRV_PE_LIMIT, "too many columns (limit 2000)");
release_fail:
    if (owner != NULL) {
        sqlite3_reset(st);
    } else {
        sqlite3_finalize(st);
    }
    return -1;
}

/* Releases the open cursor of a prepared statement, if any. */
static void release_cursor_of(sqlsrv_session *s, sqlsrv_stmt *st) {
    if (st->cur != NULL) {
        drop_cursor(s, st->cur);
    }
}

/* Next batch of a cursor (PROTOCOL.md section 6.4). */
static int do_fetch(sqlsrv_session *s, sqlsrv_job *j, sqlsrv_err *e) {
    sqlsrv_rd r;
    uint32_t id;
    uint32_t nrows;
    sqlsrv_cursor *c;
    uint32_t n;
    size_t pos_more;
    int more = 0;

    sqlsrv_rd_init(&r, j->body, j->len);
    if (sqlsrv_rd_u32(&r, &id) != 0 || sqlsrv_rd_u32(&r, &nrows) != 0) {
        return fail(e, SQLSRV_PE_BAD_REQUEST, "truncated request");
    }
    if (sqlsrv_rd_end(&r) != 0) {
        return fail(e, SQLSRV_PE_BAD_REQUEST, "trailing bytes in request");
    }
    c = find_cursor(s, id);
    if (c == NULL) {
        sqlsrv_errf(e, SQLSRV_STATUS_PROTO(SQLSRV_PE_NO_CURSOR), "no open cursor %u", id);
        return -1;
    }
    n = nrows == 0 ? c->fetch : (nrows > SQLSRV_MAX_FETCH_ROWS ? SQLSRV_MAX_FETCH_ROWS : nrows);
    pos_more = s->out.len;
    sqlsrv_buf_u8(&s->out, 0);
    if (fill_batch(s, j, c, &s->out, n, &more, e) != 0) {
        drop_cursor(s, c);
        return -1;
    }
    if (more) {
        sqlsrv_buf_put_u8(&s->out, pos_more, 1);
    } else {
        drop_cursor(s, c);
    }
    return 0;
}

/* ---- Methods ----------------------------------------------------------- */

static void write_rows_changed(sqlsrv_session *s) {
    sqlsrv_buf_i64(&s->out, (int64_t)sqlite3_changes64(s->db));
    sqlsrv_buf_i64(&s->out, (int64_t)sqlite3_last_insert_rowid(s->db));
}

static int do_exec(sqlsrv_session *s, sqlsrv_job *j, sqlsrv_err *e) {
    sqlsrv_rd r;
    const uint8_t *sql = NULL;
    uint32_t sqln = 0;
    sqlsrv_param *ps = NULL;
    uint32_t np = 0;
    sqlite3_stmt *st = NULL;
    int rc = -1;

    sqlsrv_rd_init(&r, j->body, j->len);
    if (read_sql(&r, &sql, &sqln, e) != 0 || parse_params(&r, &ps, &np, e) != 0) {
        goto out;
    }
    if (sqlsrv_rd_end(&r) != 0) {
        (void)fail(e, SQLSRV_PE_BAD_REQUEST, "trailing bytes in request");
        goto out;
    }
    if (prepare_one(s, sql, sqln, &st, e) != 0) {
        goto out;
    }
    if (bind_params(s, st, ps, np, e) != 0 || step_done(s, j, st, e) != 0) {
        goto out;
    }
    write_rows_changed(s);
    rc = 0;
out:
    sqlite3_finalize(st);
    free(ps);
    return rc;
}

static int do_query(sqlsrv_session *s, sqlsrv_job *j, sqlsrv_err *e) {
    sqlsrv_rd r;
    const uint8_t *sql = NULL;
    uint32_t sqln = 0;
    uint32_t fetch = 0;
    sqlsrv_param *ps = NULL;
    uint32_t np = 0;
    sqlite3_stmt *st = NULL;
    int rc = -1;

    sqlsrv_rd_init(&r, j->body, j->len);
    if (read_sql(&r, &sql, &sqln, e) != 0 || parse_params(&r, &ps, &np, e) != 0) {
        goto out;
    }
    if (sqlsrv_rd_u32(&r, &fetch) != 0) {
        (void)fail(e, SQLSRV_PE_BAD_REQUEST, "truncated request");
        goto out;
    }
    if (sqlsrv_rd_end(&r) != 0) {
        (void)fail(e, SQLSRV_PE_BAD_REQUEST, "trailing bytes in request");
        goto out;
    }
    if (prepare_one(s, sql, sqln, &st, e) != 0) {
        goto out;
    }
    if (bind_params(s, st, ps, np, e) != 0) {
        sqlite3_finalize(st);
        goto out;
    }
    rc = start_query(s, j, st, NULL, fetch, e);
out:
    free(ps);
    return rc;
}

/* Reads "u32 stmt, Params[, u32 fetch]" of ExecStmt and QueryStmt and binds the statement. */
static int stmt_prepare_bind(sqlsrv_session *s, sqlsrv_rd *r, sqlsrv_stmt **out, uint32_t *fetch,
                             sqlsrv_param **ps, uint32_t *np, sqlsrv_err *e) {
    uint32_t id;
    sqlsrv_stmt *stmt;

    if (sqlsrv_rd_u32(r, &id) != 0) {
        return fail(e, SQLSRV_PE_BAD_REQUEST, "truncated request");
    }
    if (parse_params(r, ps, np, e) != 0) {
        return -1;
    }
    if (fetch != NULL && sqlsrv_rd_u32(r, fetch) != 0) {
        return fail(e, SQLSRV_PE_BAD_REQUEST, "truncated request");
    }
    if (sqlsrv_rd_end(r) != 0) {
        return fail(e, SQLSRV_PE_BAD_REQUEST, "trailing bytes in request");
    }
    stmt = find_stmt(s, id);
    if (stmt == NULL) {
        sqlsrv_errf(e, SQLSRV_STATUS_PROTO(SQLSRV_PE_NO_STATEMENT), "no statement %u", id);
        return -1;
    }
    release_cursor_of(s, stmt);
    sqlite3_reset(stmt->st);
    sqlite3_clear_bindings(stmt->st);
    *out = stmt;
    return 0;
}

static int do_exec_stmt(sqlsrv_session *s, sqlsrv_job *j, sqlsrv_err *e) {
    sqlsrv_rd r;
    sqlsrv_stmt *stmt = NULL;
    sqlsrv_param *ps = NULL;
    uint32_t np = 0;
    int rc = -1;

    sqlsrv_rd_init(&r, j->body, j->len);
    if (stmt_prepare_bind(s, &r, &stmt, NULL, &ps, &np, e) != 0) {
        return -1;
    }
    if (bind_params(s, stmt->st, ps, np, e) == 0 && step_done(s, j, stmt->st, e) == 0) {
        write_rows_changed(s);
        rc = 0;
    }
    free(ps);
    return rc;
}

static int do_query_stmt(sqlsrv_session *s, sqlsrv_job *j, sqlsrv_err *e) {
    sqlsrv_rd r;
    sqlsrv_stmt *stmt = NULL;
    sqlsrv_param *ps = NULL;
    uint32_t np = 0;
    uint32_t fetch = 0;
    int rc = -1;

    sqlsrv_rd_init(&r, j->body, j->len);
    if (stmt_prepare_bind(s, &r, &stmt, &fetch, &ps, &np, e) != 0) {
        return -1;
    }
    if (bind_params(s, stmt->st, ps, np, e) == 0) {
        rc = start_query(s, j, stmt->st, stmt, fetch, e);
    }
    free(ps);
    return rc;
}

static int do_prepare(sqlsrv_session *s, sqlsrv_job *j, sqlsrv_err *e) {
    sqlsrv_rd r;
    const uint8_t *sql = NULL;
    uint32_t sqln = 0;
    sqlite3_stmt *st = NULL;
    sqlsrv_stmt *stmt;
    uint32_t id = 0;

    sqlsrv_rd_init(&r, j->body, j->len);
    if (read_sql(&r, &sql, &sqln, e) != 0) {
        return -1;
    }
    if (sqlsrv_rd_end(&r) != 0) {
        return fail(e, SQLSRV_PE_BAD_REQUEST, "trailing bytes in request");
    }
    if (s->nstmts >= SQLSRV_MAX_STATEMENTS) {
        sqlsrv_errf(e, SQLSRV_STATUS_PROTO(SQLSRV_PE_LIMIT), "too many statements (limit %u)", SQLSRV_MAX_STATEMENTS);
        return -1;
    }
    if (prepare_one(s, sql, sqln, &st, e) != 0) {
        return -1;
    }
    if (alloc_id(&s->next_stmt, &id, e, "statement") != 0) {
        sqlite3_finalize(st);
        return -1;
    }
    sqlsrv_buf_u32(&s->out, id);
    sqlsrv_buf_u32(&s->out, (uint32_t)sqlite3_bind_parameter_count(st));
    if (write_columns(&s->out, st, e) != 0) {
        sqlite3_finalize(st);
        return -1;
    }
    stmt = calloc(1, sizeof *stmt);
    if (stmt == NULL) {
        sqlite3_finalize(st);
        return fail(e, SQLSRV_PE_INTERNAL, "out of memory");
    }
    stmt->id = id;
    stmt->st = st;
    stmt->next = s->stmts;
    s->stmts = stmt;
    s->nstmts++;
    return 0;
}

static int do_close_stmt(sqlsrv_session *s, sqlsrv_job *j, sqlsrv_err *e) {
    sqlsrv_rd r;
    uint32_t id;
    sqlsrv_stmt *stmt;

    sqlsrv_rd_init(&r, j->body, j->len);
    if (sqlsrv_rd_u32(&r, &id) != 0) {
        return fail(e, SQLSRV_PE_BAD_REQUEST, "truncated request");
    }
    if (sqlsrv_rd_end(&r) != 0) {
        return fail(e, SQLSRV_PE_BAD_REQUEST, "trailing bytes in request");
    }
    stmt = find_stmt(s, id);
    if (stmt == NULL) {
        sqlsrv_errf(e, SQLSRV_STATUS_PROTO(SQLSRV_PE_NO_STATEMENT), "no statement %u", id);
        return -1;
    }
    drop_stmt(s, stmt);
    return 0;
}

static int do_close_cursor(sqlsrv_session *s, sqlsrv_job *j, sqlsrv_err *e) {
    sqlsrv_rd r;
    uint32_t id;
    sqlsrv_cursor *c;

    sqlsrv_rd_init(&r, j->body, j->len);
    if (sqlsrv_rd_u32(&r, &id) != 0) {
        return fail(e, SQLSRV_PE_BAD_REQUEST, "truncated request");
    }
    if (sqlsrv_rd_end(&r) != 0) {
        return fail(e, SQLSRV_PE_BAD_REQUEST, "trailing bytes in request");
    }
    c = find_cursor(s, id);
    if (c == NULL) {
        sqlsrv_errf(e, SQLSRV_STATUS_PROTO(SQLSRV_PE_NO_CURSOR), "no open cursor %u", id);
        return -1;
    }
    drop_cursor(s, c);
    return 0;
}

/* Runs a single statement of SQL text, with the error mapped. */
static int exec_sql(sqlsrv_session *s, const char *sql, sqlsrv_err *e) {
    int rc = sqlite3_exec(s->db, sql, NULL, NULL, NULL);
    if (rc != SQLITE_OK) {
        return err_db(s->db, rc, e);
    }
    return 0;
}

/* Restores query_only once no transaction is active (PROTOCOL.md section 6.9). */
static void restore_read_only(sqlsrv_session *s, sqlite3 *db) {
    char sql[64];
    if (!s->ro_active || !sqlite3_get_autocommit(db)) {
        return;
    }
    (void)snprintf(sql, sizeof sql, "PRAGMA query_only = %d", s->ro_saved != 0 ? 1 : 0);
    (void)sqlite3_exec(db, sql, NULL, NULL, NULL);
    s->ro_active = 0;
}

static int get_query_only(sqlite3 *db, int *val, sqlsrv_err *e) {
    sqlite3_stmt *st = NULL;
    int rc = sqlite3_prepare_v2(db, "PRAGMA query_only", -1, &st, NULL);
    if (rc != SQLITE_OK) {
        return err_db(db, rc, e);
    }
    *val = 0;
    rc = sqlite3_step(st);
    if (rc == SQLITE_ROW) {
        *val = sqlite3_column_int(st, 0);
    }
    if (rc != SQLITE_ROW && rc != SQLITE_DONE) {
        (void)err_db(db, rc, e);
        sqlite3_finalize(st);
        return -1;
    }
    sqlite3_finalize(st);
    return 0;
}

static int do_begin(sqlsrv_session *s, sqlsrv_job *j, sqlsrv_err *e) {
    static const char *const begins[] = {"BEGIN DEFERRED", "BEGIN IMMEDIATE", "BEGIN EXCLUSIVE"};
    sqlsrv_rd r;
    uint8_t mode = 0;
    uint8_t ro = 0;
    int saved = 0;

    sqlsrv_rd_init(&r, j->body, j->len);
    if (sqlsrv_rd_u8(&r, &mode) != 0 || sqlsrv_rd_u8(&r, &ro) != 0) {
        return fail(e, SQLSRV_PE_BAD_REQUEST, "truncated request");
    }
    if (sqlsrv_rd_end(&r) != 0) {
        return fail(e, SQLSRV_PE_BAD_REQUEST, "trailing bytes in request");
    }
    if (mode > 2) {
        return fail(e, SQLSRV_PE_BAD_REQUEST, "invalid transaction mode");
    }
    if (ro > 1) {
        return fail(e, SQLSRV_PE_BAD_REQUEST, "invalid read_only flag");
    }
    if (ro && mode != 0) {
        return fail(e, SQLSRV_PE_BAD_REQUEST, "read-only transactions must use DEFERRED mode");
    }
    if (!ro) {
        return exec_sql(s, begins[mode], e);
    }
    if (get_query_only(s->db, &saved, e) != 0) {
        return -1;
    }
    if (exec_sql(s, begins[0], e) != 0) {
        return -1;
    }
    s->ro_saved = saved;
    s->ro_active = 1;
    return exec_sql(s, "PRAGMA query_only = 1", e);
}

static int do_commit(sqlsrv_session *s, sqlsrv_job *j, sqlsrv_err *e) {
    int rc;
    if (j->len != 0) {
        return fail(e, SQLSRV_PE_BAD_REQUEST, "request body must be empty");
    }
    if (sqlite3_get_autocommit(s->db)) {
        restore_read_only(s, s->db);
        sqlsrv_errf(e, SQLSRV_STATUS_SQLITE(SQLITE_ERROR), "cannot commit - no transaction is active");
        return -1;
    }
    rc = exec_sql(s, "COMMIT", e);
    restore_read_only(s, s->db);
    return rc;
}

static int do_rollback(sqlsrv_session *s, sqlsrv_job *j, sqlsrv_err *e) {
    int rc = 0;
    if (j->len != 0) {
        return fail(e, SQLSRV_PE_BAD_REQUEST, "request body must be empty");
    }
    if (!sqlite3_get_autocommit(s->db)) {
        rc = exec_sql(s, "ROLLBACK", e);
    }
    restore_read_only(s, s->db);
    return rc;
}

static int do_empty(sqlsrv_job *j, sqlsrv_err *e) {
    if (j->len != 0) {
        return fail(e, SQLSRV_PE_BAD_REQUEST, "request body must be empty");
    }
    return 0;
}

/* Rolls back, finalizes, and closes the connection (PROTOCOL.md section 8.3). */
static void teardown(sqlsrv_session *s) {
    sqlite3 *db;

    sqlsrv_mutex_lock(&s->mu);
    db = s->db;
    s->db = NULL;
    sqlsrv_mutex_unlock(&s->mu);
    if (db == NULL) {
        return;
    }
    while (s->cursors != NULL) {
        drop_cursor(s, s->cursors);
    }
    if (!sqlite3_get_autocommit(db)) {
        (void)sqlite3_exec(db, "ROLLBACK", NULL, NULL, NULL);
    }
    restore_read_only(s, db);
    while (s->stmts != NULL) {
        drop_stmt(s, s->stmts);
    }
    (void)sqlite3_close_v2(db);
    release_session(s->sh);
}

/* Open (PROTOCOL.md section 6.1): checks in the documented order. */
static int do_open(sqlsrv_session *s, const uint8_t *body, size_t len, sqlsrv_err *e) {
    struct {
        const uint8_t *name;
        uint32_t nlen;
        const uint8_t *value;
        uint32_t vlen;
    } pr[SQLSRV_MAX_PRAGMAS];
    sqlsrv_rd r;
    uint16_t major = 0;
    uint16_t minor = 0;
    const uint8_t *dbp = NULL;
    uint32_t dbn = 0;
    uint32_t flags = 0;
    uint32_t busy_ms = 0;
    uint32_t npragmas = 0;
    uint32_t i;
    char *path = NULL;
    sqlite3 *db = NULL;
    int sflags;
    int rc;
    int reserved = 0;

    if (s->db != NULL) {
        return fail(e, SQLSRV_PE_ALREADY_OPEN, "session is already open");
    }
    sqlsrv_rd_init(&r, body, len);
    if (sqlsrv_rd_u16(&r, &major) != 0) {
        return fail(e, SQLSRV_PE_BAD_REQUEST, "truncated request");
    }
    if (major != SQLSRV_PROTO_MAJOR) {
        return fail(e, SQLSRV_PE_VERSION, "unsupported protocol major version");
    }
    if (sqlsrv_rd_u16(&r, &minor) != 0 || sqlsrv_rd_str(&r, &dbp, &dbn) != 0 || sqlsrv_rd_u32(&r, &flags) != 0 ||
        sqlsrv_rd_u32(&r, &busy_ms) != 0 || sqlsrv_rd_u32(&r, &npragmas) != 0) {
        return fail(e, SQLSRV_PE_BAD_REQUEST, "truncated request");
    }
    (void)minor; /* the effective minor is 0 for every 1.x client */
    if (dbn == 0) {
        return fail(e, SQLSRV_PE_BAD_REQUEST, "empty database name");
    }
    if (dbn > SQLSRV_MAX_PATH_BYTES) {
        return fail(e, SQLSRV_PE_LIMIT, "database name exceeds 4096 bytes");
    }
    if (!sqlsrv_valid_open_flags(flags)) {
        return fail(e, SQLSRV_PE_BAD_REQUEST, "invalid open flags");
    }
    if (npragmas > SQLSRV_MAX_PRAGMAS) {
        return fail(e, SQLSRV_PE_LIMIT, "too many pragmas (limit 32)");
    }
    for (i = 0; i < npragmas; i++) {
        if (sqlsrv_rd_str(&r, &pr[i].name, &pr[i].nlen) != 0 || sqlsrv_rd_str(&r, &pr[i].value, &pr[i].vlen) != 0) {
            return fail(e, SQLSRV_PE_BAD_REQUEST, "truncated request");
        }
        if (!sqlsrv_valid_pragma_name(pr[i].name, pr[i].nlen)) {
            return fail(e, SQLSRV_PE_BAD_REQUEST, "invalid pragma name");
        }
        if (!sqlsrv_valid_pragma_value(pr[i].value, pr[i].vlen)) {
            return fail(e, SQLSRV_PE_BAD_REQUEST, "invalid pragma value");
        }
    }
    if (sqlsrv_rd_end(&r) != 0) {
        return fail(e, SQLSRV_PE_BAD_REQUEST, "trailing bytes in request");
    }

    if (sqlsrv_policy_resolve(&s->sh->policy, dbp, dbn, &path, e) != 0) {
        return -1;
    }
    if (reserve_session(s->sh, e) != 0) {
        free(path);
        return -1;
    }
    reserved = 1;
    if ((flags & SQLSRV_OPEN_CREATE) == 0 && sqlsrv_port_file_exists(path) == 0) {
        sqlsrv_errf(e, SQLSRV_STATUS_SQLITE(SQLITE_CANTOPEN), "unable to open database file");
        goto abort_open;
    }
    sflags = (flags & SQLSRV_OPEN_READONLY) ? SQLITE_OPEN_READONLY : SQLITE_OPEN_READWRITE;
    if (flags & SQLSRV_OPEN_CREATE) {
        sflags |= SQLITE_OPEN_CREATE;
    }
    /* No SQLITE_OPEN_URI: the path is a file name, never a URI. */
    rc = sqlite3_open_v2(path, &db, sflags, NULL);
    if (rc != SQLITE_OK) {
        (void)err_db(db, rc, e);
        goto abort_open;
    }
    sqlite3_extended_result_codes(db, 1);
    sqlite3_busy_timeout(db, busy_ms > (uint32_t)INT_MAX ? INT_MAX : (int)busy_ms);
    (void)sqlite3_limit(db, SQLITE_LIMIT_SQL_LENGTH, (int)SQLSRV_MAX_SQL_BYTES);
    (void)sqlite3_limit(db, SQLITE_LIMIT_VARIABLE_NUMBER, (int)SQLSRV_MAX_PARAMS);
    (void)sqlite3_limit(db, SQLITE_LIMIT_COLUMN, (int)SQLSRV_MAX_COLUMNS);
    (void)sqlite3_limit(db, SQLITE_LIMIT_ATTACHED, 0);
    /*
     * Loadable extensions off. A build compiled with OMIT_LOAD_EXTENSION has
     * no extension loader to disable, and then reports the option unsupported.
     */
    rc = sqlite3_db_config(db, SQLITE_DBCONFIG_ENABLE_LOAD_EXTENSION, 0, NULL);
    if (rc != SQLITE_OK && sqlite3_compileoption_used("OMIT_LOAD_EXTENSION") == 0) {
        (void)err_db(db, rc, e);
        goto abort_open;
    }
    if (sqlite3_set_authorizer(db, authorizer, NULL) != SQLITE_OK) {
        (void)err_db(db, SQLITE_ERROR, e);
        goto abort_open;
    }
    for (i = 0; i < npragmas; i++) {
        char sql[SQLSRV_MAX_PRAGMA_NAME + SQLSRV_MAX_PRAGMA_VALUE + 64];
        (void)snprintf(sql, sizeof sql, "PRAGMA %.*s = %.*s;", (int)pr[i].nlen, (const char *)pr[i].name,
                       (int)pr[i].vlen, (const char *)pr[i].value);
        if (sqlite3_exec(db, sql, NULL, NULL, NULL) != SQLITE_OK) {
            (void)err_db(db, sqlite3_errcode(db), e);
            goto abort_open;
        }
    }

    sqlsrv_mutex_lock(&s->mu);
    s->db = db;
    sqlsrv_mutex_unlock(&s->mu);
    s->ro_active = 0;
    free(path);
    sqlsrv_buf_u16(&s->out, SQLSRV_PROTO_MAJOR);
    sqlsrv_buf_u16(&s->out, SQLSRV_PROTO_MINOR);
    sqlsrv_buf_u32(&s->out, SQLSRV_MAX_FETCH_ROWS);
    sqlsrv_buf_u32(&s->out, SQLSRV_MAX_PARAMS);
    return 0;

abort_open:
    if (db != NULL) {
        (void)sqlite3_close_v2(db);
    }
    free(path);
    if (reserved) {
        release_session(s->sh);
    }
    return -1;
}

static int dispatch(sqlsrv_session *s, sqlsrv_job *j, sqlsrv_err *e) {
    if (j->method < SQLSRV_M_OPEN || j->method > SQLSRV_M_CLOSE) {
        sqlsrv_errf(e, SQLSRV_STATUS_PROTO(SQLSRV_PE_UNKNOWN_METHOD), "unknown method %llu",
                    (unsigned long long)j->method);
        return -1;
    }
    if (j->method == SQLSRV_M_OPEN) {
        return do_open(s, j->body, j->len, e);
    }
    if (j->method == SQLSRV_M_CLOSE) {
        if (j->len != 0) {
            return fail(e, SQLSRV_PE_BAD_REQUEST, "request body must be empty");
        }
        teardown(s);
        return 0;
    }
    if (s->db == NULL) {
        sqlsrv_errf(e, SQLSRV_STATUS_PROTO(SQLSRV_PE_NO_SESSION), "%s requires an open session",
                    method_name(j->method));
        return -1;
    }
    switch (j->method) {
    case SQLSRV_M_EXEC:
        return do_exec(s, j, e);
    case SQLSRV_M_QUERY:
        return do_query(s, j, e);
    case SQLSRV_M_FETCH:
        return do_fetch(s, j, e);
    case SQLSRV_M_CLOSE_CURSOR:
        return do_close_cursor(s, j, e);
    case SQLSRV_M_PREPARE:
        return do_prepare(s, j, e);
    case SQLSRV_M_EXEC_STMT:
        return do_exec_stmt(s, j, e);
    case SQLSRV_M_QUERY_STMT:
        return do_query_stmt(s, j, e);
    case SQLSRV_M_CLOSE_STMT:
        return do_close_stmt(s, j, e);
    case SQLSRV_M_BEGIN:
        return do_begin(s, j, e);
    case SQLSRV_M_COMMIT:
        return do_commit(s, j, e);
    case SQLSRV_M_ROLLBACK:
        return do_rollback(s, j, e);
    case SQLSRV_M_PING:
        return do_empty(j, e);
    default:
        return fail(e, SQLSRV_PE_UNKNOWN_METHOD, "unknown method");
    }
}

/* Runs one job on the worker thread. The job's request body is valid until the handler returns. */
static void run_job(sqlsrv_session *s, sqlsrv_job *j) {
    s->out.len = 0;
    s->out.oom = 0;
    j->failed = 0;
    if (ctx_ended(j->ctx)) {
        /* Canceled before it started: no response is sent for it (PROTOCOL.md section 9). */
        j->failed = 1;
        sqlsrv_errf(&j->err, SQLSRV_STATUS_SQLITE(SQLITE_INTERRUPT), "request canceled");
        return;
    }
    if (dispatch(s, j, &j->err) != 0) {
        j->failed = 1;
        return;
    }
    if (s->out.oom) {
        j->failed = 1;
        sqlsrv_err_proto(&j->err, SQLSRV_PE_INTERNAL, "out of memory");
    }
}

static void *worker_main(void *arg) {
    sqlsrv_session *s = arg;

    sqlsrv_mutex_lock(&s->mu);
    for (;;) {
        if (s->job != NULL && !s->claimed) {
            sqlsrv_job *j = s->job;
            s->claimed = 1;
            sqlsrv_mutex_unlock(&s->mu);
            run_job(s, j);
            sqlsrv_mutex_lock(&s->mu);
            j->done = 1;
            sqlsrv_cond_broadcast(&s->cv);
            continue;
        }
        if (s->stop && s->job == NULL) {
            break;
        }
        sqlsrv_cond_wait(&s->cv, &s->mu);
    }
    sqlsrv_mutex_unlock(&s->mu);
    return NULL;
}

sqlsrv_session *sqlsrv_sess_new(sqlsrv_shared *sh, hqq_stream *st, uint64_t id, void *owner) {
    sqlsrv_session *s = calloc(1, sizeof *s);
    if (s == NULL) {
        return NULL;
    }
    s->sh = sh;
    s->stream = st;
    s->id = id;
    s->owner = owner;
    s->next_stmt = 1;
    s->next_cursor = 1;
    if (sqlsrv_mutex_init(&s->mu) != 0) {
        free(s);
        return NULL;
    }
    if (sqlsrv_cond_init(&s->cv) != 0) {
        sqlsrv_mutex_destroy(&s->mu);
        free(s);
        return NULL;
    }
    if (sqlsrv_thread_create(&s->worker, worker_main, s) != 0) {
        sqlsrv_cond_destroy(&s->cv);
        sqlsrv_mutex_destroy(&s->mu);
        free(s);
        return NULL;
    }
    return s;
}

void sqlsrv_sess_stop(sqlsrv_session *s) {
    sqlsrv_mutex_lock(&s->mu);
    s->stop = 1;
    if (s->db != NULL) {
        /* Ends a running request; a request that starts later sees the stop flag. */
        sqlite3_interrupt(s->db);
    }
    sqlsrv_cond_broadcast(&s->cv);
    sqlsrv_mutex_unlock(&s->mu);
    sqlsrv_thread_join(s->worker);
    teardown(s);
}

void sqlsrv_sess_wait_refs(sqlsrv_session *s) {
    sqlsrv_mutex_lock(&s->mu);
    while (s->refs > 0) {
        sqlsrv_cond_wait(&s->cv, &s->mu);
    }
    sqlsrv_mutex_unlock(&s->mu);
}

void sqlsrv_sess_free(sqlsrv_session *s) {
    sqlsrv_buf_free(&s->out);
    sqlsrv_cond_destroy(&s->cv);
    sqlsrv_mutex_destroy(&s->mu);
    free(s);
}

void sqlsrv_sess_ref(sqlsrv_session *s) {
    sqlsrv_mutex_lock(&s->mu);
    s->refs++;
    sqlsrv_mutex_unlock(&s->mu);
}

void sqlsrv_sess_unref(sqlsrv_session *s) {
    sqlsrv_mutex_lock(&s->mu);
    s->refs--;
    if (s->refs == 0) {
        sqlsrv_cond_broadcast(&s->cv);
    }
    sqlsrv_mutex_unlock(&s->mu);
}

static void send_error(hqq_response *w, const sqlsrv_err *err) {
    (void)hqq_response_error(w, err->status, err->msg, strlen(err->msg));
}

void sqlsrv_sess_request(sqlsrv_session *s, hqq_req_ctx *ctx, hqq_response *w, uint64_t method, const void *body,
                         size_t len) {
    sqlsrv_job j;
    int canceled;

    memset(&j, 0, sizeof j);
    j.method = method;
    j.body = (const uint8_t *)body;
    j.len = len;
    j.ctx = ctx;

    sqlsrv_mutex_lock(&s->mu);
    /* One request at a time: a pipelined request waits for the previous one to be answered. */
    while (s->job != NULL && !s->stop && !ctx_ended(ctx)) {
        sqlsrv_cond_timedwait_ms(&s->cv, &s->mu, SQLSRV_POLL_MS);
    }
    if (ctx_ended(ctx)) {
        sqlsrv_mutex_unlock(&s->mu);
        return;
    }
    if (s->stop) {
        sqlsrv_mutex_unlock(&s->mu);
        sqlsrv_err pe;
        sqlsrv_err_proto(&pe, SQLSRV_PE_NO_SESSION, "no session on this stream");
        send_error(w, &pe);
        return;
    }
    s->job = &j;
    s->claimed = 0;
    sqlsrv_cond_broadcast(&s->cv);
    while (!j.done) {
        sqlsrv_cond_timedwait_ms(&s->cv, &s->mu, SQLSRV_POLL_MS);
        /* Re-interrupt until the statement stops: a lost interrupt cannot run the request to its end. */
        if (!j.done && s->db != NULL && ctx_ended(ctx)) {
            sqlite3_interrupt(s->db);
        }
    }
    /* The job is still ours until it is cleared below, so the worker stays idle. */
    sqlsrv_mutex_unlock(&s->mu);

    canceled = ctx_ended(ctx);
    if (!canceled) {
        if (j.failed) {
            send_error(w, &j.err);
        } else if (s->out.len > 0) {
            (void)hqq_response_write(w, s->out.p, s->out.len);
        }
    }

    sqlsrv_mutex_lock(&s->mu);
    s->job = NULL;
    s->claimed = 0;
    if (s->out.cap > SQLSRV_OUT_KEEP_BYTES) {
        sqlsrv_buf_free(&s->out);
    }
    sqlsrv_cond_broadcast(&s->cv);
    sqlsrv_mutex_unlock(&s->mu);
}
