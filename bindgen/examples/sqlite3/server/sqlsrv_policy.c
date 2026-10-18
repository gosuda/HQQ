#include "sqlsrv_policy.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "sqlsrv_pathrules.h"
#include "sqlsrv_port.h"

void sqlsrv_policy_init(sqlsrv_policy *p) {
    memset(p, 0, sizeof *p);
}

void sqlsrv_policy_free(sqlsrv_policy *p) {
    size_t i;
    for (i = 0; i < p->naliases; i++) {
        free(p->aliases[i].name);
        free(p->aliases[i].path);
    }
    free(p->aliases);
    free(p->allow_prefix);
    sqlsrv_policy_init(p);
}

static void set_err(char *err, size_t cap, const char *msg) {
    if (err != NULL && cap > 0) {
        (void)snprintf(err, cap, "%s", msg);
    }
}

/*
 * Canonical absolute form of cand. An existing path is resolved by the
 * platform (symbolic links followed). A missing file keeps its final name, and
 * its parent must exist (the parent is canonicalized). A dangling symbolic link
 * is refused.
 */
static int canon_path(const char *cand, char **out, sqlsrv_err *e) {
    sqlsrv_path_style style = sqlsrv_path_host_style();
    int missing = 0;
    int parent_missing = 0;
    size_t dlen = 0;
    size_t leaf_at = 0;
    size_t rl;
    size_t bl;
    size_t need_sep;
    char *parent;
    char *rp = NULL;
    char *full;
    const char *leaf;

    if (sqlsrv_port_realpath(cand, out, &missing) == 0) {
        return 0;
    }
    if (!missing) {
        sqlsrv_err_proto(e, SQLSRV_PE_FORBIDDEN, "database path cannot be resolved");
        return -1;
    }
    if (sqlsrv_port_entry_exists(cand)) {
        sqlsrv_err_proto(e, SQLSRV_PE_FORBIDDEN, "database path is a dangling symbolic link");
        return -1;
    }
    if (sqlsrv_path_split(cand, style, &dlen, &leaf_at) != 0) {
        sqlsrv_err_proto(e, SQLSRV_PE_FORBIDDEN, "invalid database path");
        return -1;
    }
    leaf = cand + leaf_at;
    if (leaf[0] == '\0' || strcmp(leaf, ".") == 0 || strcmp(leaf, "..") == 0) {
        sqlsrv_err_proto(e, SQLSRV_PE_FORBIDDEN, "invalid database path");
        return -1;
    }
    parent = malloc(dlen + 1);
    if (parent == NULL) {
        sqlsrv_err_proto(e, SQLSRV_PE_INTERNAL, "out of memory");
        return -1;
    }
    memcpy(parent, cand, dlen);
    parent[dlen] = '\0';
    if (sqlsrv_port_realpath(parent, &rp, &parent_missing) != 0) {
        free(parent);
        sqlsrv_errf(e, SQLSRV_STATUS_SQLITE(14), "unable to open database file");
        return -1;
    }
    free(parent);
    rl = strlen(rp);
    bl = strlen(leaf);
    need_sep = rl > 0 && sqlsrv_path_is_sep((unsigned char)rp[rl - 1], style) ? 0 : 1;
    full = malloc(rl + need_sep + bl + 1);
    if (full == NULL) {
        free(rp);
        sqlsrv_err_proto(e, SQLSRV_PE_INTERNAL, "out of memory");
        return -1;
    }
    memcpy(full, rp, rl);
    if (need_sep) {
        full[rl] = sqlsrv_path_sep(style);
    }
    memcpy(full + rl + need_sep, leaf, bl + 1);
    free(rp);
    *out = full;
    return 0;
}

int sqlsrv_policy_add_alias(sqlsrv_policy *p, const char *name, const char *path, char *err, size_t errcap) {
    sqlsrv_path_style style = sqlsrv_path_host_style();
    sqlsrv_path_kind kind;
    char *cand = NULL;
    char *canon = NULL;
    sqlsrv_err e;
    size_t i;
    size_t nlen = strlen(name);
    size_t plen = strlen(path);

    if (nlen == 0 || nlen > SQLSRV_MAX_PATH_BYTES || strchr(name, '=') != NULL) {
        set_err(err, errcap, "alias name must be non-empty and must not contain '='");
        return -1;
    }
    for (i = 0; i < p->naliases; i++) {
        if (strcmp(p->aliases[i].name, name) == 0) {
            set_err(err, errcap, "duplicate alias");
            return -1;
        }
    }
    /* An alias is written by the operator: absolute or relative to the working directory. */
    kind = sqlsrv_path_classify((const uint8_t *)path, plen, style);
    if (kind == SQLSRV_PATHK_ABSOLUTE) {
        cand = sqlsrv_path_join(NULL, (const uint8_t *)path, plen, style);
    } else if (kind == SQLSRV_PATHK_RELATIVE) {
        char *cwd = sqlsrv_port_getcwd();
        if (cwd == NULL) {
            set_err(err, errcap, "cannot read the working directory");
            return -1;
        }
        cand = sqlsrv_path_join(cwd, (const uint8_t *)path, plen, style);
        free(cwd);
    } else {
        set_err(err, errcap, "alias path must be absolute or relative (no UNC, device or drive-relative paths)");
        return -1;
    }
    if (cand == NULL) {
        set_err(err, errcap, "out of memory");
        return -1;
    }
    if (canon_path(cand, &canon, &e) != 0) {
        free(cand);
        set_err(err, errcap, e.msg);
        return -1;
    }
    free(cand);
    if (p->naliases == p->capaliases) {
        size_t cap = p->capaliases == 0 ? 8 : p->capaliases * 2;
        sqlsrv_alias *na = realloc(p->aliases, cap * sizeof *na);
        if (na == NULL) {
            free(canon);
            set_err(err, errcap, "out of memory");
            return -1;
        }
        p->aliases = na;
        p->capaliases = cap;
    }
    p->aliases[p->naliases].name = sqlsrv_port_strdup(name);
    p->aliases[p->naliases].path = canon;
    if (p->aliases[p->naliases].name == NULL) {
        free(canon);
        set_err(err, errcap, "out of memory");
        return -1;
    }
    p->naliases++;
    return 0;
}

int sqlsrv_policy_set_allow_dir(sqlsrv_policy *p, const char *dir, char *err, size_t errcap) {
    char *real = NULL;
    char *prefix;
    int missing = 0;

    if (sqlsrv_port_realpath(dir, &real, &missing) != 0) {
        set_err(err, errcap, "--allow-dir does not exist");
        return -1;
    }
    if (!sqlsrv_port_is_directory(real)) {
        free(real);
        set_err(err, errcap, "--allow-dir is not a directory");
        return -1;
    }
    prefix = sqlsrv_path_dir_prefix(real, sqlsrv_path_host_style());
    free(real);
    if (prefix == NULL) {
        set_err(err, errcap, "out of memory");
        return -1;
    }
    free(p->allow_prefix);
    p->allow_prefix = prefix;
    return 0;
}

int sqlsrv_policy_resolve(const sqlsrv_policy *p, const uint8_t *db, size_t n, char **out, sqlsrv_err *e) {
    sqlsrv_path_style style = sqlsrv_path_host_style();
    size_t i;
    const char *why;
    char *cand;
    char *canon = NULL;

    for (i = 0; i < p->naliases; i++) {
        const char *name = p->aliases[i].name;
        if (strlen(name) == n && memcmp(name, db, n) == 0) {
            *out = sqlsrv_port_strdup(p->aliases[i].path);
            if (*out == NULL) {
                sqlsrv_err_proto(e, SQLSRV_PE_INTERNAL, "out of memory");
                return -1;
            }
            return 0;
        }
    }
    if (memchr(db, '\0', n) != NULL) {
        sqlsrv_err_proto(e, SQLSRV_PE_BAD_REQUEST, "database path contains a NUL byte");
        return -1;
    }
    if (p->allow_prefix == NULL) {
        sqlsrv_err_proto(e, SQLSRV_PE_FORBIDDEN, "database is not an alias and raw paths are disabled");
        return -1;
    }
    /* Refused by name (traversal, device names, drives), so it does not depend on which parts exist. */
    why = sqlsrv_path_check_raw(db, n, style);
    if (why != NULL) {
        sqlsrv_err_proto(e, SQLSRV_PE_FORBIDDEN, why);
        return -1;
    }
    cand = sqlsrv_path_join(p->allow_prefix, db, n, style);
    if (cand == NULL) {
        sqlsrv_err_proto(e, SQLSRV_PE_INTERNAL, "out of memory");
        return -1;
    }
    if (canon_path(cand, &canon, e) != 0) {
        free(cand);
        return -1;
    }
    free(cand);
    /* Containment: the canonical path must lie strictly inside the allow directory. */
    if (!sqlsrv_path_inside(canon, p->allow_prefix, style)) {
        free(canon);
        sqlsrv_err_proto(e, SQLSRV_PE_FORBIDDEN, "database path is outside the allowed directory");
        return -1;
    }
    *out = canon;
    return 0;
}
