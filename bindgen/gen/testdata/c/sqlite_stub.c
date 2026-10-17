/* Stubs of the SQLite functions, the interceptor and the shims of sqlite_lowlevel.idl, so that the generated
 * glue links. Nothing here behaves like SQLite. */
#include "sqlite_lowlevel_server.h"

struct sqlite3 { int unused; };
struct sqlite3_stmt { int unused; };

int sqlite3_open_v2(const char *filename, sqlite3 **ppDb, int flags, const char *zVfs) {
    static sqlite3 db;
    (void)filename; (void)flags; (void)zVfs;
    *ppDb = &db;
    return 0;
}
int sqlite3_close(sqlite3 *db) { (void)db; return 0; }
int sqlite3_prepare_v2(sqlite3 *db, const char *zSql, int nByte, sqlite3_stmt **ppStmt, const char **pzTail) {
    static sqlite3_stmt stmt;
    (void)db;
    *ppStmt = &stmt;
    *pzTail = zSql + (nByte < 0 ? 0 : nByte);
    return 0;
}
int sqlite3_step(sqlite3_stmt *stmt) { (void)stmt; return 101; }
int sqlite3_finalize(sqlite3_stmt *stmt) { (void)stmt; return 0; }
int sqlite3_column_count(sqlite3_stmt *stmt) { (void)stmt; return 0; }
int sqlite3_bind_int64(sqlite3_stmt *stmt, int index, sqlite3_int64 value) { (void)stmt; (void)index; (void)value; return 0; }
const char *sqlite3_errmsg(sqlite3 *db) { (void)db; return "stub"; }

hqqrpc_status sqlite_lowlevel_intercept_sqlite3_open_v2(hqqrpc_session *s, sqlite_lowlevel_args_sqlite3_open_v2 *args) {
    (void)s; (void)args;
    return HQQRPC_OK;
}
int32_t bind_text(sqlite3_stmt *stmt, int32_t index, const char *text, int32_t n) {
    (void)stmt; (void)index; (void)text; (void)n;
    return 0;
}
hqqrpc_status fetch_rows(sqlite3_stmt *stmt, int32_t max, hqqrpc_wr *ret) {
    size_t mark;
    hqqrpc_status st = hqqrpc_wr_put_u32(ret, 1);
    (void)stmt; (void)max;
    if (st != HQQRPC_OK)
        return st;
    if ((st = hqqrpc_wr_array_begin(ret, &mark)) != HQQRPC_OK)
        return st;
    st = sqlite_lowlevel_Value_put_Null(ret);
    return st != HQQRPC_OK ? st : hqqrpc_wr_array_end(ret, mark, 1, UINT32_MAX);
}
const char *stmt_errmsg(sqlite3_stmt *stmt) { (void)stmt; return "stub"; }
