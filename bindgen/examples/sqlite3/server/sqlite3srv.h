/* sqlite3srv.h - what main.c and sqlite3_hooks.c share (hand-written; the generated glue is
 * sqlite3_server.h). */
#ifndef SQLITE3SRV_H
#define SQLITE3SRV_H

#include "sqlsrv_policy.h"

/* The database policy of the hand-written hqq-sqlite-server (examples/sqlite/server/src): aliases
 * and the allow directory. main.c fills it from --alias and --allow-dir before the server starts;
 * afterwards it is read-only, so the interceptor of sqlite3_open_v2 reads it from every session thread
 * without a lock. */
extern sqlsrv_policy sqlite3srv_policy;

/* Registers the setup that every SQLite connection of this process gets when it is opened
 * (sqlite3_auto_extension). Call once, before the first session. Returns 0, or the SQLite result code
 * of the failed registration. */
int sqlite3srv_init(void);

#endif
