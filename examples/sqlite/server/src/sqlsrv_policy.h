/*
 * sqlsrv_policy.h - the database policy of the server (PROTOCOL.md section 12):
 * aliases from the command line, and raw paths confined to one directory.
 *
 * Configured once at start-up, read-only afterwards (thread-safe to read).
 */
#ifndef SQLSRV_POLICY_H
#define SQLSRV_POLICY_H

#include <stddef.h>
#include <stdint.h>

#include "sqlsrv_proto.h"

typedef struct sqlsrv_alias {
    char *name;
    char *path; /* canonical absolute path */
} sqlsrv_alias;

typedef struct sqlsrv_policy {
    sqlsrv_alias *aliases;
    size_t naliases;
    size_t capaliases;
    char *allow_prefix; /* canonical --allow-dir with a trailing '/'; NULL: raw paths disabled */
} sqlsrv_policy;

void sqlsrv_policy_init(sqlsrv_policy *p);
void sqlsrv_policy_free(sqlsrv_policy *p);

/* Both return 0, or -1 with a message in err. */
int sqlsrv_policy_add_alias(sqlsrv_policy *p, const char *name, const char *path, char *err, size_t errcap);
int sqlsrv_policy_set_allow_dir(sqlsrv_policy *p, const char *dir, char *err, size_t errcap);

/*
 * Maps the `db` of Open to a canonical path (a new string, free() it). An alias
 * wins over a raw path. Raw paths are relative to the allow directory (or
 * absolute, and then must still lie inside it). Symbolic links are resolved
 * before the containment check. Returns 0, or -1 with a status in e:
 * FORBIDDEN (not an alias and raw paths disabled, or outside the allow
 * directory), BAD_REQUEST (embedded NUL), or CANTOPEN (missing parent).
 */
int sqlsrv_policy_resolve(const sqlsrv_policy *p, const uint8_t *db, size_t n, char **out, sqlsrv_err *e);

#endif /* SQLSRV_POLICY_H */
