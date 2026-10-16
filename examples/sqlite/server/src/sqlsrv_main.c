/*
 * hqq-sqlite-server: the SQLite service of examples/sqlite (PROTOCOL.md).
 *
 *   hqq-sqlite-server <listener-name> [--allow-dir DIR] [--alias NAME=PATH]...
 *                     [--max-sessions N] [--perm OCTAL] [--heartbeat-ms N]
 *                     [--peer-timeout-ms N] [--shutdown-timeout-ms N]
 *
 * Prints "ready <listener-name>" on stdout when the listener is published.
 * SIGINT and SIGTERM start a graceful shutdown; SIGKILL needs no cleanup
 * (SQLite recovers its files on the next open).
 */
#define _POSIX_C_SOURCE 200809L

#include <errno.h>
#include <limits.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include <pthread.h>
#include <sqlite3.h>

#include "sqlsrv_link.h"

#define SERVER_VERSION "hqq-sqlite-server, protocol 1.0"

static void usage(FILE *f) {
    fprintf(f,
            "usage: hqq-sqlite-server <listener-name> [options]\n"
            "  --allow-dir DIR          allow raw database paths inside DIR (default: aliases only)\n"
            "  --alias NAME=PATH        database alias (repeatable)\n"
            "  --max-sessions N         open sessions allowed at once (1..65536, default 256)\n"
            "  --perm OCTAL             listener permission (default 0600)\n"
            "  --heartbeat-ms N         link heartbeat interval (default: libhqq's)\n"
            "  --peer-timeout-ms N      peer timeout (default: libhqq's)\n"
            "  --shutdown-timeout-ms N  wait for running requests on shutdown (default 5000)\n"
            "  --version, --help\n");
}

static int parse_uint(const char *s, unsigned long min, unsigned long max, int base, unsigned long *out) {
    char *end = NULL;
    unsigned long v;
    errno = 0;
    v = strtoul(s, &end, base);
    if (errno != 0 || end == s || *end != '\0' || v < min || v > max) {
        return -1;
    }
    *out = v;
    return 0;
}

static int bad_arg(const char *what, const char *value) {
    fprintf(stderr, "hqq-sqlite-server: invalid %s: %s\n", what, value);
    return 2;
}

int main(int argc, char **argv) {
    sqlsrv_shared sh;
    sqlsrv_listen_options lo;
    sqlsrv_listener *lst = NULL;
    const char *name = NULL;
    char err[512];
    sigset_t set;
    int sig = 0;
    int i;

    if (sqlite3_libversion_number() < 3024000) {
        fprintf(stderr, "hqq-sqlite-server: SQLite 3.24 or later is required\n");
        return 1;
    }
    if (sqlite3_threadsafe() == 0) {
        fprintf(stderr, "hqq-sqlite-server: SQLite must be built thread-safe\n");
        return 1;
    }
    if (sqlsrv_shared_init(&sh) != 0) {
        fprintf(stderr, "hqq-sqlite-server: out of resources\n");
        return 1;
    }
    memset(&lo, 0, sizeof lo);
    lo.perm = 0600;

    for (i = 1; i < argc; i++) {
        const char *a = argv[i];
        unsigned long v = 0;
        int has_next = i + 1 < argc;

        if (strcmp(a, "--help") == 0 || strcmp(a, "-h") == 0) {
            usage(stdout);
            return 0;
        }
        if (strcmp(a, "--version") == 0) {
            printf("%s\n", SERVER_VERSION);
            return 0;
        }
        if (strcmp(a, "--allow-dir") == 0 && has_next) {
            if (sqlsrv_policy_set_allow_dir(&sh.policy, argv[++i], err, sizeof err) != 0) {
                fprintf(stderr, "hqq-sqlite-server: %s: %s\n", err, argv[i]);
                return 2;
            }
        } else if (strcmp(a, "--alias") == 0 && has_next) {
            char *eq;
            char *copy = strdup(argv[++i]);
            if (copy == NULL) {
                return 1;
            }
            eq = strchr(copy, '=');
            if (eq == NULL) {
                free(copy);
                return bad_arg("alias (want NAME=PATH)", argv[i]);
            }
            *eq = '\0';
            if (sqlsrv_policy_add_alias(&sh.policy, copy, eq + 1, err, sizeof err) != 0) {
                fprintf(stderr, "hqq-sqlite-server: alias %s: %s\n", copy, err);
                free(copy);
                return 2;
            }
            free(copy);
        } else if (strcmp(a, "--max-sessions") == 0 && has_next) {
            if (parse_uint(argv[++i], 1, 65536, 10, &v) != 0) {
                return bad_arg("--max-sessions", argv[i]);
            }
            sh.max_sessions = (unsigned)v;
        } else if (strcmp(a, "--perm") == 0 && has_next) {
            if (parse_uint(argv[++i], 0, 0777, 8, &v) != 0) {
                return bad_arg("--perm", argv[i]);
            }
            lo.perm = (unsigned)v;
        } else if (strcmp(a, "--heartbeat-ms") == 0 && has_next) {
            if (parse_uint(argv[++i], 1, 60000, 10, &v) != 0) {
                return bad_arg("--heartbeat-ms", argv[i]);
            }
            lo.heartbeat_ns = (int64_t)v * 1000000;
        } else if (strcmp(a, "--peer-timeout-ms") == 0 && has_next) {
            if (parse_uint(argv[++i], 1, 3600000, 10, &v) != 0) {
                return bad_arg("--peer-timeout-ms", argv[i]);
            }
            lo.peer_timeout_ns = (int64_t)v * 1000000;
        } else if (strcmp(a, "--shutdown-timeout-ms") == 0 && has_next) {
            if (parse_uint(argv[++i], 0, 3600000, 10, &v) != 0) {
                return bad_arg("--shutdown-timeout-ms", argv[i]);
            }
            sh.shutdown_timeout_ns = (int64_t)v * 1000000;
        } else if (a[0] == '-') {
            fprintf(stderr, "hqq-sqlite-server: unknown or incomplete option: %s\n", a);
            usage(stderr);
            return 2;
        } else if (name == NULL) {
            name = a;
        } else {
            fprintf(stderr, "hqq-sqlite-server: unexpected argument: %s\n", a);
            return 2;
        }
    }
    if (name == NULL || name[0] == '\0') {
        usage(stderr);
        return 2;
    }

    /* Signals are handled by sigwait on this thread: block them before any thread starts. */
    sigemptyset(&set);
    sigaddset(&set, SIGINT);
    sigaddset(&set, SIGTERM);
    (void)pthread_sigmask(SIG_BLOCK, &set, NULL);
    (void)signal(SIGPIPE, SIG_IGN);

    if (sqlsrv_listener_start(&sh, name, &lo, &lst, err, sizeof err) != 0) {
        fprintf(stderr, "hqq-sqlite-server: %s\n", err);
        sqlsrv_shared_destroy(&sh);
        return 1;
    }
    printf("ready %s\n", name);
    fflush(stdout);

    while (sigwait(&set, &sig) != 0) {
        /* Retry on a spurious error. */
    }
    fprintf(stderr, "hqq-sqlite-server: signal %d: shutting down\n", sig);
    sqlsrv_listener_stop(lst);
    sqlsrv_shared_wait_links(&sh);
    sqlsrv_shared_destroy(&sh);
    return 0;
}
