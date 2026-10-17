/*
 * sqlsrv_session.h - one session per HQQ stream (PROTOCOL.md section 8): one
 * SQLite connection, its statements and cursors, and its transaction state.
 *
 * Threads: each session has a worker thread (sqlsrv_sess_new starts it). The
 * worker runs every request, so a session never runs two requests at once and
 * a long query blocks only its own session. The Serve handler of the request
 * waits for the worker, and it alone calls sqlite3_interrupt() when the
 * request context ends (sqlsrv_sess_request). The stream watcher of the link
 * layer ends the session with sqlsrv_sess_stop().
 */
#ifndef SQLSRV_SESSION_H
#define SQLSRV_SESSION_H

#include <stdatomic.h>
#include <stdint.h>
#include "sqlsrv_port.h"

#include <sqlite3.h>

#include "hqq/hqq_advanced.h"
#include "sqlsrv_policy.h"
#include "sqlsrv_proto.h"

/* State shared by all links and sessions of the server. */
typedef struct sqlsrv_shared {
    sqlsrv_policy policy;
    unsigned max_sessions;
    int64_t shutdown_timeout_ns;
    atomic_int shutting;
    sqlsrv_mutex mu; /* guards open_sessions and live_links */
    sqlsrv_cond cv;  /* signalled when live_links drops */
    unsigned open_sessions;
    unsigned live_links;
} sqlsrv_shared;

int sqlsrv_shared_init(sqlsrv_shared *sh);
void sqlsrv_shared_destroy(sqlsrv_shared *sh);

typedef struct sqlsrv_session sqlsrv_session;
typedef struct sqlsrv_job sqlsrv_job;
typedef struct sqlsrv_stmt sqlsrv_stmt;
typedef struct sqlsrv_cursor sqlsrv_cursor;

struct sqlsrv_session {
    /* Owned by the link layer; guarded by the link's mutex. */
    sqlsrv_session *next;
    void *owner;
    hqq_stream *stream;
    uint64_t id;

    sqlsrv_shared *sh;
    sqlsrv_thread worker;

    /* Guarded by mu. refs counts the Serve handlers that use the session. */
    sqlsrv_mutex mu;
    sqlsrv_cond cv;
    int stop;
    int refs;
    sqlsrv_job *job;
    int claimed;
    sqlite3 *db; /* NULL: the session is CLOSED */

    /* Worker state (the watcher touches it only after joining the worker). */
    int ro_active;
    int ro_saved;
    sqlsrv_stmt *stmts;
    unsigned nstmts;
    uint32_t next_stmt;
    sqlsrv_cursor *cursors;
    unsigned ncursors;
    uint32_t next_cursor;
    sqlsrv_buf out;
};

/* Starts the session and its worker thread. NULL on failure (nothing is kept). */
sqlsrv_session *sqlsrv_sess_new(sqlsrv_shared *sh, hqq_stream *st, uint64_t id, void *owner);
/* Stops the worker (interrupting a running request), joins it, and tears the session down. */
void sqlsrv_sess_stop(sqlsrv_session *s);
/* Waits until no Serve handler uses the session. */
void sqlsrv_sess_wait_refs(sqlsrv_session *s);
void sqlsrv_sess_free(sqlsrv_session *s);

/* A handler takes a reference before it calls sqlsrv_sess_request. */
void sqlsrv_sess_ref(sqlsrv_session *s);
void sqlsrv_sess_unref(sqlsrv_session *s);

/*
 * Runs one request on the session and writes its response to w. Returns when
 * the response is written, or without writing when the request context ended.
 */
void sqlsrv_sess_request(sqlsrv_session *s, hqq_req_ctx *ctx, hqq_response *w, uint64_t method,
                         const void *body, size_t len);

#endif /* SQLSRV_SESSION_H */
