/* A stub of the SQLite C API: the declarations that sqlite_lowlevel.idl uses, for compile tests of the generated glue. */
#ifndef STUB_SQLITE3_H
#define STUB_SQLITE3_H

#include <stdint.h>

typedef struct sqlite3 sqlite3;
typedef struct sqlite3_stmt sqlite3_stmt;
typedef int64_t sqlite3_int64;

int sqlite3_open_v2(const char *filename, sqlite3 **ppDb, int flags, const char *zVfs);
int sqlite3_close(sqlite3 *db);
int sqlite3_prepare_v2(sqlite3 *db, const char *zSql, int nByte, sqlite3_stmt **ppStmt, const char **pzTail);
int sqlite3_step(sqlite3_stmt *stmt);
int sqlite3_finalize(sqlite3_stmt *stmt);
int sqlite3_column_count(sqlite3_stmt *stmt);
int sqlite3_bind_int64(sqlite3_stmt *stmt, int index, sqlite3_int64 value);
const char *sqlite3_errmsg(sqlite3 *db);

#endif
