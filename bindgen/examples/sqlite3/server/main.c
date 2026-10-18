/* Process entry of the sqlite3 server.
 *
 *   sqlite3_server <listener-name> [--allow-dir DIR] [--alias NAME=PATH]...
 *                  [hqqrpc_main options: --max-sessions N --perm OCTAL --max-handles N --buffer-size N
 *                   --buffer-count N --max-message N --heartbeat-ms N --peer-timeout-ms N
 *                   --shutdown-timeout-ms N --parent-stdin[=force] --lifeline-grace-ms N ...]
 *
 * --allow-dir and --alias configure database path sandboxing (sqlsrv_policy.c); every other option is
 * parsed by hqqrpc_main (c/include/hqq/hqq_rpc.h), which also prints "ready <listener-name>" once the
 * listener exists and shuts down on SIGINT, SIGTERM or end of file on the parent lifeline
 * (--parent-stdin). */
#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include <sqlite3.h>

#include "hqq/hqq_rpc.h"
#include "sqlite3_server.h"
#include "sqlite3srv.h"
#include "sqlsrv_policy.h"
#include "sqlsrv_port.h"

/* hqq-sqlite-server accepts 256 open sessions by default (sqlite/PROTOCOL.md section 11); hqqrpc_main's own
 * default is 64. */
#define DEFAULT_MAX_SESSIONS 256u

/* A connecting client holds one control link for the few milliseconds of its bootstrap (BINDGEN.md 4.11),
 * and a link beyond max_control_links is closed at once: the client sees "peer closed" and no status. The
 * default of 16 is too small for a connection pool that opens many sessions together (the tests of
 * this directory open 20 at the same time), so it follows the session limit. */
#define DEFAULT_MAX_CONTROL_LINKS 256u

/* One line per session on stderr. on_session_end runs after the runtime destroyed every handle the
 * client left behind (BINDGEN.md 5.2), so "session ended" also means that the connections of that
 * session are closed and its open transaction was rolled back. The tests wait for it. */
static void *session_start(void *user, hqqrpc_session *s) {
    (void)user;
    (void)s;
    (void)fputs("sqlite3_server: session started\n", stderr);
    return NULL;
}

static void session_end(void *user, void *session_user, hqqrpc_session *s) {
    (void)user;
    (void)session_user;
    (void)s;
    (void)fputs("sqlite3_server: session ended\n", stderr);
}

static int usage_error(const char *what, const char *value) {
    (void)fprintf(stderr, "sqlite3_server: %s: %s\n", what, value);
    return 2;
}

int main(int argc, char **argv) {
    hqqrpc_server_options o;
    char err[512];
    char **rest;
    int nrest = 0;
    int i;
    int rc;

    if (sqlite3_libversion_number() < 3037000) {
        (void)fputs("sqlite3_server: SQLite 3.37 or later is required (sqlite3_changes64)\n", stderr);
        return 1;
    }
    if (sqlite3_threadsafe() == 0) {
        (void)fputs("sqlite3_server: SQLite must be built thread-safe\n", stderr);
        return 1;
    }
    sqlsrv_policy_init(&sqlite3srv_policy);

    /* Take --allow-dir and --alias out of the command line; hqqrpc_main rejects what it does not know. */
    rest = calloc((size_t)argc + 1u, sizeof *rest);
    if (rest == NULL) {
        return 1;
    }
    rest[nrest++] = argv[0];
    for (i = 1; i < argc; i++) {
        const char *a = argv[i];

        if (strcmp(a, "--allow-dir") == 0 && i + 1 < argc) {
            if (sqlsrv_policy_set_allow_dir(&sqlite3srv_policy, argv[++i], err, sizeof err) != 0) {
                (void)fprintf(stderr, "sqlite3_server: %s: %s\n", err, argv[i]);
                return 2;
            }
        } else if (strcmp(a, "--alias") == 0 && i + 1 < argc) {
            char *eq;
            char *copy = sqlsrv_port_strdup(argv[++i]);

            if (copy == NULL) {
                return 1;
            }
            eq = strchr(copy, '=');
            if (eq == NULL) {
                free(copy);
                return usage_error("invalid alias (want NAME=PATH)", argv[i]);
            }
            *eq = '\0';
            if (sqlsrv_policy_add_alias(&sqlite3srv_policy, copy, eq + 1, err, sizeof err) != 0) {
                (void)fprintf(stderr, "sqlite3_server: alias %s: %s\n", copy, err);
                free(copy);
                return 2;
            }
            free(copy);
        } else {
            rest[nrest++] = argv[i];
        }
    }
    rest[nrest] = NULL;

    /* SQLite initializes itself on the first call, which is thread-safe, but registering the connection
     * setup before the first session thread exists keeps the list of auto extensions out of any race. */
    rc = sqlite3_initialize();
    if (rc == SQLITE_OK) {
        rc = sqlite3srv_init();
    }
    if (rc != SQLITE_OK) {
        (void)fprintf(stderr, "sqlite3_server: cannot initialize SQLite: %d\n", rc);
        return 1;
    }

    hqqrpc_server_options_init(&o);
    o.max_sessions = DEFAULT_MAX_SESSIONS;
    o.max_control_links = DEFAULT_MAX_CONTROL_LINKS;
    o.on_session_start = session_start;
    o.on_session_end = session_end;
    rc = hqqrpc_main(&sqlite3_iface, &o, nrest, rest);
    /* A nonzero result may leave session threads running (BINDGEN.md 5.2): then nothing they read is freed. */
    if (rc == 0) {
        sqlsrv_policy_free(&sqlite3srv_policy);
        free(rest);
    }
    return rc;
}
