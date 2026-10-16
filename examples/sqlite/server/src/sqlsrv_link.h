/*
 * sqlsrv_link.h - the HQQ side of the server: the listener (one acceptor),
 * one link per accepted client, one session per stream (PROTOCOL.md section 2).
 */
#ifndef SQLSRV_LINK_H
#define SQLSRV_LINK_H

#include <stddef.h>
#include <stdint.h>

#include "sqlsrv_session.h"

typedef struct sqlsrv_listen_options {
    unsigned perm;             /* permission of the listener segment */
    int64_t heartbeat_ns;      /* 0: the libhqq default */
    int64_t peer_timeout_ns;   /* 0: the libhqq default */
} sqlsrv_listen_options;

typedef struct sqlsrv_listener sqlsrv_listener;

/* Creates the listener and starts accepting links. Returns 0, or -1 with a message in err. */
int sqlsrv_listener_start(sqlsrv_shared *sh, const char *name, const sqlsrv_listen_options *o,
                          sqlsrv_listener **out, char *err, size_t errcap);
/*
 * Starts the graceful shutdown (sets sh->shutting, closes the acceptor). The
 * links finish their in-flight requests, then close. Call
 * sqlsrv_shared_wait_links to wait for them.
 */
void sqlsrv_listener_stop(sqlsrv_listener *ls);
void sqlsrv_shared_wait_links(sqlsrv_shared *sh);

#endif /* SQLSRV_LINK_H */
