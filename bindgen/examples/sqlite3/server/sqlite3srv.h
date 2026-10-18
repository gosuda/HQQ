/* sqlite3srv.h - what main.c and sqlite3_hooks.c share (hand-written; the generated glue is
 * sqlite3_server.h). */
#ifndef SQLITE3SRV_H
#define SQLITE3SRV_H

#include <sqlite3.h>
#include "sqlsrv_policy.h"

/* Database path policy: aliases and the allow directory. main.c fills it from --alias and --allow-dir
 * before the server starts; afterwards it is read-only, so the interceptor of sqlite3_open_v2 reads it
 * from every session thread without a lock. */
extern sqlsrv_policy sqlite3srv_policy;

int sqlite3srv_init(void);
int sqlite3srv_open_v2(const char *filename, sqlite3 **ppDb, int flags, const char *zVfs);

#endif
