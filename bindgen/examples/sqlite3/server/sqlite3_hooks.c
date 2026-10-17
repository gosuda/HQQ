/* Hand-written part of the sqlite3 server (BINDGEN.md 7.3, 9.6): the interceptor of sqlite3_open_v2,
 * the shims and the error-detail functions declared in sqlite3.idl, and the per-connection setup.
 *
 * Threading. The service is @serialize(none): sessions run these functions at the same time, each on
 * its own thread, each on its own connections. Nothing here keeps shared mutable state: the only
 * variable of this file that is not constant is sqlite3srv_policy, which main.c fills before the
 * first session and which is read-only afterwards; the message of a failed shim is thread-local.
 *
 * The constants below that repeat a const of sqlite3.idl are marked "IDL"; the Go tests compare the
 * behavior at each limit, so a drift fails a test. */
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "sqlite3_server.h"
#include "sqlite3srv.h"
#include "sqlsrv_proto.h"

/* ---- Constants --------------------------------------------------------------------------------- */

/* The limits of sqlite/PROTOCOL.md section 11 that the hand-written server sets with sqlite3_limit. */
#define SRV_MAX_SQL 1048576 /* IDL SVC_MAX_SQL */
#define SRV_MAX_PARAMS 32766 /* IDL SVC_MAX_PARAMS */
#define SRV_MAX_COLUMNS 2000 /* IDL SVC_MAX_COLUMNS */
#define SRV_MAX_NAME 1024u /* IDL SVC_MAX_NAME */
#define SRV_MAX_FETCH_ROWS 4096 /* IDL SVC_MAX_FETCH_ROWS */
#define SRV_DEFAULT_FETCH_ROWS 256 /* IDL SVC_DEFAULT_FETCH_ROWS */

/* Failure codes that SQLite never returns (IDL SVC_SQL_EMPTY, SVC_SQL_MULTIPLE, SVC_BAD_ARGS). */
#define SRV_SQL_EMPTY 16777217
#define SRV_SQL_MULTIPLE 16777218
#define SRV_BAD_ARGS 16777219

/* Application statuses (IDL SVC_STATUS_*). */
#define SRV_STATUS_SQLITE_BASE 0x0100u
#define SRV_STATUS_META_TOO_LARGE 0x1000u
#define SRV_STATUS_NAME_TOO_LONG 0x1001u

/* Kinds of bind_values and exec_stmt: the SQLite fundamental type codes, and zeroblob. */
#define KIND_INTEGER 1
#define KIND_FLOAT 2
#define KIND_TEXT 3
#define KIND_BLOB 4
#define KIND_NULL 5
#define KIND_ZEROBLOB 6

/* Worst-case bytes of a cell that carries no variable data (a tag, up to 4 bytes of alignment and an
 * 8-byte payload is 19), rounded up, and of the epilogue of a Rows value (an errmsg is a length prefix
 * of at most 7 bytes of padding and 4 bytes, up to SRV_MAX_NAME bytes and a NUL). */
#define CELL_WORST 24u
#define CELL_OVERHEAD 24u /* the same bound for a Text or Blob cell, on top of its bytes */
#define ROWS_EPILOGUE 1040u

/* ---- The message of a failure that SQLite does not know ---------------------------------------- */

/* A shim that fails with an SRV_* code or with SQLITE_RANGE for a reason of its own leaves the text here;
 * the detail function that the glue calls next, on the same thread, hands it out once. A thread-local
 * variable keeps the sessions apart without a lock. */
static _Thread_local char tls_note[96];
static _Thread_local int tls_note_set;

static int note(int code, const char *msg) {
    (void)snprintf(tls_note, sizeof tls_note, "%s", msg);
    tls_note_set = 1;
    return code;
}

static const char *take_note(void) {
    if (tls_note_set) {
        tls_note_set = 0;
        return tls_note;
    }
    return NULL;
}

const char *db_errmsg(sqlite3 *db) {
    return sqlite3_errmsg(db);
}

const char *prep_errmsg(sqlite3 *db) {
    const char *m = take_note();
    return m != NULL ? m : sqlite3_errmsg(db);
}

const char *stmt_errmsg(sqlite3_stmt *stmt) {
    const char *m = take_note();
    return m != NULL ? m : sqlite3_errmsg(sqlite3_db_handle(stmt));
}

/* ---- sqlite3_open_v2: the policy -------------------------------------------------------------- */

sqlsrv_policy sqlite3srv_policy;

/* Mirrors do_open of examples/sqlite/server/src/sqlsrv_session.c, in the same order: the flags, the
 * database name (an alias, or a raw path inside the allow directory), then the open itself. The
 * policy functions are those of the hand-written server (sqlsrv_policy.c, sqlsrv_pathrules.c), compiled
 * into this server; nothing is copied. What is not here: the busy timeout and the pragmas (the client
 * runs them as calls and SQL), and the connection setup (sqlite3srv_init). */
hqqrpc_status sqlite3_intercept_sqlite3_open_v2(hqqrpc_session *s, sqlite3_args_sqlite3_open_v2 *args) {
    sqlsrv_err e;
    char *path = NULL;
    char *copy;
    size_t n;

    /* A VFS name selects another way to touch the files (no locking, a different directory layout). */
    if (args->zvfs != NULL) {
        return HQQRPC_E_DENIED;
    }
    /* Exactly one of READONLY and READWRITE, optionally CREATE, and no other bit: no URI filenames,
     * no cache or mutex mode, no in-memory or temporary database. The bits of SQLSRV_OPEN_* are those
     * of SQLITE_OPEN_*. */
    if (args->flags < 0 || !sqlsrv_valid_open_flags((uint32_t)args->flags)) {
        return HQQRPC_E_DENIED;
    }
    /* The wire str has no NUL byte inside, so strlen is the length (the hand-written server refuses
     * a NUL in the name as BAD_REQUEST; here the codec already did). */
    n = strlen(args->filename);
    if (n == 0) {
        return HQQRPC_E_DENIED;
    }
    if (sqlsrv_policy_resolve(&sqlite3srv_policy, (const uint8_t *)args->filename, n, &path, &e) != 0) {
        if (e.status == SQLSRV_STATUS_SQLITE(SQLITE_CANTOPEN)) {
            return SRV_STATUS_SQLITE_BASE + (uint32_t)SQLITE_CANTOPEN;
        }
        if (e.status == SQLSRV_STATUS_PROTO(SQLSRV_PE_INTERNAL)) {
            return HQQRPC_E_INTERNAL;
        }
        return HQQRPC_E_DENIED; /* FORBIDDEN: not an alias and raw paths are off, or outside the directory */
    }
    /* SQLite opens the canonical path that the policy approved. The copy lives until the end of the call. */
    copy = hqqrpc_scratch(s, strlen(path) + 1);
    if (copy == NULL) {
        free(path);
        return HQQRPC_E_INTERNAL;
    }
    memcpy(copy, path, strlen(path) + 1);
    free(path);
    args->filename = copy;
    return HQQRPC_OK;
}

/* ---- The setup of every connection ------------------------------------------------------------- */

static int deny_attach(void *ud, int action, const char *a1, const char *a2, const char *a3, const char *a4) {
    (void)ud;
    (void)a1;
    (void)a2;
    (void)a3;
    (void)a4;
    return action == SQLITE_ATTACH || action == SQLITE_DETACH ? SQLITE_DENY : SQLITE_OK;
}

/* Runs inside sqlite3_open_v2, before it returns (sqlite3_auto_extension), so a connection is never
 * visible to a client without it. It is what do_open does after sqlite3_open_v2 in the hand-written
 * server, except the busy timeout (sqlite3_busy_timeout is a call of this service). */
static int conn_setup(sqlite3 *db, char **errmsg, const sqlite3_api_routines *api) {
    int rc;

    (void)api;
    (void)sqlite3_extended_result_codes(db, 1);
    (void)sqlite3_limit(db, SQLITE_LIMIT_SQL_LENGTH, SRV_MAX_SQL);
    (void)sqlite3_limit(db, SQLITE_LIMIT_VARIABLE_NUMBER, SRV_MAX_PARAMS);
    (void)sqlite3_limit(db, SQLITE_LIMIT_COLUMN, SRV_MAX_COLUMNS);
    (void)sqlite3_limit(db, SQLITE_LIMIT_ATTACHED, 0);
    /* A build without the extension loader reports the option as unsupported: that is the goal. */
    rc = sqlite3_db_config(db, SQLITE_DBCONFIG_ENABLE_LOAD_EXTENSION, 0, NULL);
    if (rc != SQLITE_OK && sqlite3_compileoption_used("OMIT_LOAD_EXTENSION") == 0) {
        *errmsg = sqlite3_mprintf("cannot disable extension loading");
        return rc;
    }
    rc = sqlite3_set_authorizer(db, deny_attach, NULL);
    if (rc != SQLITE_OK) {
        *errmsg = sqlite3_mprintf("cannot set the authorizer");
        return rc;
    }
    return SQLITE_OK;
}

int sqlite3srv_init(void) {
    return sqlite3_auto_extension((void (*)(void))conn_setup);
}

/* ---- Statements ------------------------------------------------------------------------------- */

/* sqlite3_prepare_v2 for exactly one statement (PROTOCOL.md 6.2), the algorithm of prepare_one in
 * sqlsrv_session.c: what follows the first statement may be white space, comments and ';' and nothing
 * else (it must prepare to no statement, without error). The glue passes the length, so the text in the
 * request slot needs no NUL. */
int32_t prepare_one(sqlite3 *db, const char *sql, int32_t nbyte, sqlite3_stmt **stmt) {
    sqlite3_stmt *st = NULL;
    const char *tail = NULL;
    size_t rest;
    int rc;

    tls_note_set = 0;
    *stmt = NULL;
    rc = sqlite3_prepare_v2(db, sql, nbyte, &st, &tail);
    if (rc != SQLITE_OK) {
        return rc;
    }
    if (st == NULL) {
        return note(SRV_SQL_EMPTY, "empty statement");
    }
    rest = (size_t)nbyte - (size_t)(tail - sql);
    while (rest > 0) {
        sqlite3_stmt *more = NULL;
        const char *next = NULL;
        size_t adv;

        rc = sqlite3_prepare_v2(db, tail, (int)rest, &more, &next);
        if (more != NULL || rc != SQLITE_OK || next == NULL) {
            (void)sqlite3_finalize(more);
            (void)sqlite3_finalize(st);
            return note(SRV_SQL_MULTIPLE, "multiple statements");
        }
        adv = (size_t)(next - tail);
        if (adv == 0) {
            break;
        }
        tail = next;
        rest -= adv;
    }
#ifdef SQLITE3GEN_TEST_HOOKS
    /* Test build only (cmake -DSQLITE3GEN_TEST_HOOKS=ON, build.sh -T). With SQLITE3GEN_TEST_LEAK_STMT set, every
     * successful prepare also leaves a statement that the session table does not know and nobody finalizes,
     * so that sqlite3_close answers SQLITE_BUSY: the only way for a test to reach the @retain_on_fail path of
     * the destroy function of Db, which the cascade of BINDGEN.md 5.1 otherwise makes unreachable. */
    if (getenv("SQLITE3GEN_TEST_LEAK_STMT") != NULL) {
        sqlite3_stmt *leaked = NULL;

        (void)sqlite3_prepare_v2(db, "SELECT 1", -1, &leaked, NULL);
    }
#endif
    *stmt = st;
    return SQLITE_OK;
}

/* ---- Binding ---------------------------------------------------------------------------------- */

int32_t bind_text(sqlite3_stmt *stmt, int32_t index, const void *text, size_t n) {
    tls_note_set = 0;
    return sqlite3_bind_text64(stmt, index, n > 0 ? (const char *)text : "", n, SQLITE_TRANSIENT, SQLITE_UTF8);
}

int32_t bind_blob(sqlite3_stmt *stmt, int32_t index, const void *data, size_t n) {
    tls_note_set = 0;
    if (n == 0) {
        return sqlite3_bind_zeroblob64(stmt, index, 0);
    }
    return sqlite3_bind_blob64(stmt, index, data, n, SQLITE_TRANSIENT);
}

/* Checks the three arrays of bind_values and exec_stmt, without touching the statement: kinds and words
 * have the same length, every kind is known, and the lengths of the text and blob values add up to the
 * data. Returns 0, or SRV_BAD_ARGS. */
static int check_values(const uint8_t *kinds, int32_t nkinds, const uint64_t *words, int32_t nwords, size_t ndata) {
    uint64_t used = 0;
    int32_t i;

    if (nkinds < 0 || nkinds != nwords || nkinds > SRV_MAX_PARAMS) {
        return note(SRV_BAD_ARGS, "kinds and words differ in length");
    }
    for (i = 0; i < nkinds; i++) {
        switch (kinds[i]) {
        case KIND_TEXT:
        case KIND_BLOB:
            if (words[i] > (uint64_t)ndata - used) {
                return note(SRV_BAD_ARGS, "value lengths exceed the data");
            }
            used += words[i];
            break;
        case KIND_INTEGER:
        case KIND_FLOAT:
        case KIND_NULL:
        case KIND_ZEROBLOB:
            break;
        default:
            return note(SRV_BAD_ARGS, "unknown value kind");
        }
    }
    if (used != (uint64_t)ndata) {
        return note(SRV_BAD_ARGS, "value lengths do not add up to the data");
    }
    return 0;
}

/* The bindings of PROTOCOL.md section 4, by position: clear, then bind 1..n. */
static int bind_all(sqlite3_stmt *st, const uint8_t *kinds, int32_t nkinds, const uint64_t *words,
                    const uint8_t *data) {
    size_t off = 0;
    int32_t i;
    int rc = sqlite3_clear_bindings(st);

    for (i = 0; rc == SQLITE_OK && i < nkinds; i++) {
        int idx = (int)i + 1;
        sqlite3_uint64 w = words[i];

        switch (kinds[i]) {
        case KIND_INTEGER:
            rc = sqlite3_bind_int64(st, idx, (sqlite3_int64)w);
            break;
        case KIND_FLOAT: {
            double d;

            memcpy(&d, &w, sizeof d);
            rc = sqlite3_bind_double(st, idx, d);
            break;
        }
        case KIND_TEXT:
            rc = sqlite3_bind_text64(st, idx, w > 0 ? (const char *)(data + off) : "", w, SQLITE_TRANSIENT,
                                     SQLITE_UTF8);
            off += (size_t)w;
            break;
        case KIND_BLOB:
            rc = w == 0 ? sqlite3_bind_zeroblob64(st, idx, 0)
                        : sqlite3_bind_blob64(st, idx, data + off, w, SQLITE_TRANSIENT);
            off += (size_t)w;
            break;
        case KIND_ZEROBLOB:
            rc = sqlite3_bind_zeroblob64(st, idx, w);
            break;
        default:
            rc = sqlite3_bind_null(st, idx);
            break;
        }
    }
    return rc;
}

int32_t bind_values(sqlite3_stmt *stmt, const uint8_t *kinds, int32_t nkinds, const uint64_t *words, int32_t nwords,
                    const void *data, size_t ndata) {
    int rc;

    tls_note_set = 0;
    rc = check_values(kinds, nkinds, words, nwords, ndata);
    if (rc != 0) {
        return rc;
    }
    /* The result of the reset is the error of the previous run, which is not this call's business. */
    (void)sqlite3_reset(stmt);
    return bind_all(stmt, kinds, nkinds, words, data);
}

/* Exec and ExecStmt of the hand-written protocol in one call (PROTOCOL.md 6.2, 6.7): reset, bind, step to
 * the end, discard rows, report changes64 and last_insert_rowid. Nothing runs unless every parameter bound. */
int32_t exec_stmt(sqlite3_stmt *stmt, const uint8_t *kinds, int32_t nkinds, const uint64_t *words, int32_t nwords,
                  const void *data, size_t ndata, int64_t *changes, int64_t *last_rowid) {
    sqlite3 *db;
    int rc;

    tls_note_set = 0;
    rc = check_values(kinds, nkinds, words, nwords, ndata);
    if (rc != 0) {
        return rc;
    }
    /* The result of the reset is the error of the previous run, which is not this call's business. */
    (void)sqlite3_reset(stmt);
    rc = bind_all(stmt, kinds, nkinds, words, data);
    if (rc != SQLITE_OK) {
        return rc;
    }
    do {
        rc = sqlite3_step(stmt);
    } while (rc == SQLITE_ROW);
    if (rc != SQLITE_DONE) {
        return rc;
    }
    db = sqlite3_db_handle(stmt);
    *changes = sqlite3_changes64(db);
    *last_rowid = sqlite3_last_insert_rowid(db);
    return SQLITE_DONE;
}

/* ---- Metadata --------------------------------------------------------------------------------- */

static uint64_t room(const hqqrpc_wr *w) {
    return w->cap - w->off;
}

/* Writes a str, or says that the response bound would be exceeded. 0, or a status. */
static hqqrpc_status put_meta_str(hqqrpc_wr *w, const char *s) {
    size_t n = s != NULL ? strlen(s) : 0;

    if (n > SRV_MAX_NAME) {
        return SRV_STATUS_NAME_TOO_LONG;
    }
    /* A length prefix with up to 3 bytes of padding, the bytes and the NUL. */
    if (room(w) < n + 8u) {
        return SRV_STATUS_META_TOO_LARGE;
    }
    return hqqrpc_wr_put_str(w, s != NULL ? s : "", (uint32_t)n, UINT32_MAX);
}

/* StmtMeta: nparams, readonly, param_names, columns. Nothing is written after a refusal that matters:
 * the runtime discards the partial body and sends the status. */
hqqrpc_status stmt_info(sqlite3_stmt *stmt, hqqrpc_wr *w) {
    size_t mark;
    int nparams = sqlite3_bind_parameter_count(stmt);
    int ncols = sqlite3_column_count(stmt);
    hqqrpc_status st;
    uint8_t *ro;
    int i;

    tls_note_set = 0;
    st = hqqrpc_wr_put_u32(w, (uint32_t)nparams);
    if (st != HQQRPC_OK) {
        return st;
    }
    ro = hqqrpc_wr_fixed(w, 1, 1);
    if (ro == NULL) {
        return HQQRPC_E_INTERNAL;
    }
    ro[0] = sqlite3_stmt_readonly(stmt) != 0 ? 1u : 0u;
    st = hqqrpc_wr_array_begin(w, &mark);
    for (i = 1; st == HQQRPC_OK && i <= nparams; i++) {
        st = put_meta_str(w, sqlite3_bind_parameter_name(stmt, i));
    }
    if (st == HQQRPC_OK) {
        st = hqqrpc_wr_array_end(w, mark, (uint32_t)nparams, UINT32_MAX);
    }
    if (st == HQQRPC_OK) {
        st = hqqrpc_wr_array_begin(w, &mark);
    }
    for (i = 0; st == HQQRPC_OK && i < ncols; i++) {
        st = put_meta_str(w, sqlite3_column_name(stmt, i));
        if (st == HQQRPC_OK) {
            st = put_meta_str(w, sqlite3_column_decltype(stmt, i));
        }
    }
    if (st == HQQRPC_OK) {
        st = hqqrpc_wr_array_end(w, mark, (uint32_t)ncols, UINT32_MAX);
    }
    return st;
}

/* ---- Rows ------------------------------------------------------------------------------------- */

static void put_le32(uint8_t *p, uint32_t v) {
    p[0] = (uint8_t)v;
    p[1] = (uint8_t)(v >> 8);
    p[2] = (uint8_t)(v >> 16);
    p[3] = (uint8_t)(v >> 24);
}

/* Writes the current row of stmt as ncols Value cells. A TEXT or BLOB value is written in place while
 * enough of the bound is left for it and for the markers of the cells that follow; otherwise the cell is a
 * BigText or BigBlob marker that carries its length, and *big is set. The reserve that fetch_rows checks
 * before it steps (CELL_WORST per column, ROWS_EPILOGUE) guarantees that every row fits as markers. */
static hqqrpc_status put_row(hqqrpc_wr *w, sqlite3_stmt *st, int ncols, int *big) {
    int i;

    for (i = 0; i < ncols; i++) {
        uint64_t keep = (uint64_t)(ncols - i - 1) * CELL_WORST + ROWS_EPILOGUE;
        hqqrpc_status r;

        switch (sqlite3_column_type(st, i)) {
        case SQLITE_INTEGER:
            r = sqlite3_Value_put_Int(w, sqlite3_column_int64(st, i));
            break;
        case SQLITE_FLOAT:
            r = sqlite3_Value_put_Float(w, sqlite3_column_double(st, i));
            break;
        case SQLITE_TEXT: {
            const char *p = (const char *)sqlite3_column_text(st, i);
            uint64_t n = (uint64_t)sqlite3_column_bytes(st, i);

            if (room(w) >= keep + CELL_OVERHEAD && room(w) - keep - CELL_OVERHEAD >= n) {
                if (n > 0 && memchr(p, 0, (size_t)n) != NULL) {
                    r = sqlite3_Value_put_TextRaw(w, p, (uint32_t)n);
                } else {
                    r = sqlite3_Value_put_Text(w, n > 0 ? p : "", (uint32_t)n);
                }
            } else {
                *big = 1;
                r = sqlite3_Value_put_BigText(w, n);
            }
            break;
        }
        case SQLITE_BLOB: {
            const void *p = sqlite3_column_blob(st, i);
            uint64_t n = (uint64_t)sqlite3_column_bytes(st, i);

            if (room(w) >= keep + CELL_OVERHEAD && room(w) - keep - CELL_OVERHEAD >= n) {
                r = sqlite3_Value_put_Blob(w, n > 0 ? p : "", (uint32_t)n);
            } else {
                *big = 1;
                r = sqlite3_Value_put_BigBlob(w, n);
            }
            break;
        }
        default:
            r = sqlite3_Value_put_Null(w);
            break;
        }
        if (r != HQQRPC_OK) {
            return r;
        }
    }
    return HQQRPC_OK;
}

/* Rows: rc, ncols, cells, errmsg. rc and ncols come first on the wire and are known last, so they are
 * claimed first and filled in at the end. */
hqqrpc_status fetch_rows(sqlite3_stmt *stmt, int32_t max, hqqrpc_wr *w) {
    int ncols = sqlite3_column_count(stmt);
    uint32_t limit = max <= 0 ? (uint32_t)SRV_DEFAULT_FETCH_ROWS
                              : (max > SRV_MAX_FETCH_ROWS ? (uint32_t)SRV_MAX_FETCH_ROWS : (uint32_t)max);
    uint64_t reserve = (uint64_t)ncols * CELL_WORST + ROWS_EPILOGUE;
    uint32_t nrows = 0;
    int rc = SQLITE_ROW;
    int big = 0;
    uint8_t *head;
    size_t mark;
    hqqrpc_status st;
    const char *msg = "";
    size_t mlen = 0;

    tls_note_set = 0;
    head = hqqrpc_wr_fixed(w, 4, 8);
    if (head == NULL) {
        return HQQRPC_E_INTERNAL;
    }
    st = hqqrpc_wr_array_begin(w, &mark);
    if (st != HQQRPC_OK) {
        return st;
    }
    /* Step only while a whole row is certain to fit: a row that was stepped and does not fit would
     * be lost (the next step moves on), and there is no state between calls. */
    while (nrows < limit && !big) {
        if (room(w) < reserve) {
            if (nrows == 0) {
                return SRV_STATUS_META_TOO_LARGE; /* ncols so large that even a row of markers cannot fit */
            }
            break;
        }
        rc = sqlite3_step(stmt);
        if (rc != SQLITE_ROW) {
            break;
        }
        st = put_row(w, stmt, ncols, &big);
        if (st != HQQRPC_OK) {
            return st;
        }
        nrows++;
    }
    st = hqqrpc_wr_array_end(w, mark, nrows * (uint32_t)ncols, UINT32_MAX);
    if (st != HQQRPC_OK) {
        return st;
    }
    if (rc != SQLITE_ROW && rc != SQLITE_DONE) {
        msg = sqlite3_errmsg(sqlite3_db_handle(stmt));
        mlen = strlen(msg);
        if (mlen > SRV_MAX_NAME) {
            mlen = SRV_MAX_NAME;
        }
    }
    st = hqqrpc_wr_put_str(w, msg, (uint32_t)mlen, SRV_MAX_NAME);
    if (st != HQQRPC_OK) {
        return st;
    }
    put_le32(head, (uint32_t)rc);
    put_le32(head + 4, (uint32_t)ncols);
    return HQQRPC_OK;
}

/* The bytes of a TEXT or BLOB value of the current row, from offset on, written straight into dst (the
 * response slot, or scratch). Idempotent: it does not move the statement. */
int32_t column_chunk(sqlite3_stmt *stmt, int32_t col, uint64_t offset, void *dst, size_t cap, size_t *n) {
    const void *p;
    uint64_t len;
    size_t take;

    tls_note_set = 0;
    if (col < 0 || col >= sqlite3_data_count(stmt)) {
        return note(SQLITE_RANGE, "no such column, or the statement is not on a row");
    }
    switch (sqlite3_column_type(stmt, col)) {
    case SQLITE_TEXT:
        p = sqlite3_column_text(stmt, col);
        break;
    case SQLITE_BLOB:
        p = sqlite3_column_blob(stmt, col);
        break;
    default:
        return note(SQLITE_RANGE, "the column is not text or blob");
    }
    len = (uint64_t)sqlite3_column_bytes(stmt, col);
    if (offset > len) {
        return note(SQLITE_RANGE, "offset is beyond the end of the value");
    }
    take = (size_t)(len - offset < (uint64_t)cap ? len - offset : (uint64_t)cap);
    if (take > 0) {
        memcpy(dst, (const uint8_t *)p + offset, take);
    }
    *n = take;
    return SQLITE_OK;
}
