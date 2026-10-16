#define _POSIX_C_SOURCE 200809L

#include "sqlsrv_policy.h"

#include <errno.h>
#include <limits.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

#include <sqlite3.h>

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
 * Canonical absolute form of cand. An existing file is resolved with
 * realpath(). A missing file keeps its final name, and its parent must exist
 * (the parent is canonicalized). A dangling symbolic link is refused.
 */
static int canon_path(const char *cand, char **out, sqlsrv_err *e) {
    char *real = realpath(cand, NULL);
    const char *slash;
    char *parent;
    char *rp;
    char *full;
    size_t plen;
    size_t rl;
    size_t bl;
    size_t need_sep;
    struct stat sb;

    if (real != NULL) {
        *out = real;
        return 0;
    }
    if (errno != ENOENT && errno != ENOTDIR) {
        sqlsrv_err_proto(e, SQLSRV_PE_FORBIDDEN, "database path cannot be resolved");
        return -1;
    }
    if (lstat(cand, &sb) == 0) {
        sqlsrv_err_proto(e, SQLSRV_PE_FORBIDDEN, "database path is a dangling symbolic link");
        return -1;
    }
    slash = strrchr(cand, '/');
    if (slash == NULL || slash[1] == '\0' || strcmp(slash + 1, ".") == 0 || strcmp(slash + 1, "..") == 0) {
        sqlsrv_err_proto(e, SQLSRV_PE_FORBIDDEN, "invalid database path");
        return -1;
    }
    plen = slash == cand ? 1 : (size_t)(slash - cand);
    parent = malloc(plen + 1);
    if (parent == NULL) {
        sqlsrv_err_proto(e, SQLSRV_PE_INTERNAL, "out of memory");
        return -1;
    }
    memcpy(parent, cand, plen);
    parent[plen] = '\0';
    rp = realpath(parent, NULL);
    free(parent);
    if (rp == NULL) {
        sqlsrv_errf(e, SQLSRV_STATUS_SQLITE(14), "unable to open database file");
        return -1;
    }
    rl = strlen(rp);
    bl = strlen(slash + 1);
    need_sep = rp[rl - 1] == '/' ? 0 : 1;
    full = malloc(rl + need_sep + bl + 1);
    if (full == NULL) {
        free(rp);
        sqlsrv_err_proto(e, SQLSRV_PE_INTERNAL, "out of memory");
        return -1;
    }
    memcpy(full, rp, rl);
    if (need_sep) {
        full[rl] = '/';
    }
    memcpy(full + rl + need_sep, slash + 1, bl + 1);
    free(rp);
    *out = full;
    return 0;
}

int sqlsrv_policy_add_alias(sqlsrv_policy *p, const char *name, const char *path, char *err, size_t errcap) {
    char *cand = NULL;
    char *canon = NULL;
    sqlsrv_err e;
    size_t i;
    size_t nlen = strlen(name);

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
    if (path[0] == '/') {
        cand = strdup(path);
    } else {
        char cwd[PATH_MAX];
        if (getcwd(cwd, sizeof cwd) == NULL) {
            set_err(err, errcap, "cannot read the working directory");
            return -1;
        }
        size_t cl = strlen(cwd);
        size_t pl = strlen(path);
        cand = malloc(cl + 1 + pl + 1);
        if (cand != NULL) {
            memcpy(cand, cwd, cl);
            cand[cl] = '/';
            memcpy(cand + cl + 1, path, pl + 1);
        }
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
    p->aliases[p->naliases].name = strdup(name);
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
    char *real = realpath(dir, NULL);
    struct stat sb;
    size_t rl;
    char *prefix;

    if (real == NULL) {
        set_err(err, errcap, "--allow-dir does not exist");
        return -1;
    }
    if (stat(real, &sb) != 0 || !S_ISDIR(sb.st_mode)) {
        free(real);
        set_err(err, errcap, "--allow-dir is not a directory");
        return -1;
    }
    rl = strlen(real);
    prefix = malloc(rl + 2);
    if (prefix == NULL) {
        free(real);
        set_err(err, errcap, "out of memory");
        return -1;
    }
    memcpy(prefix, real, rl);
    if (rl == 0 || real[rl - 1] != '/') {
        prefix[rl] = '/';
        prefix[rl + 1] = '\0';
    } else {
        prefix[rl] = '\0';
    }
    free(real);
    free(p->allow_prefix);
    p->allow_prefix = prefix;
    return 0;
}

int sqlsrv_policy_resolve(const sqlsrv_policy *p, const uint8_t *db, size_t n, char **out, sqlsrv_err *e) {
    size_t i;
    char *cand;
    char *canon = NULL;
    size_t plen;
    size_t cl;

    for (i = 0; i < p->naliases; i++) {
        const char *name = p->aliases[i].name;
        if (strlen(name) == n && memcmp(name, db, n) == 0) {
            *out = strdup(p->aliases[i].path);
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
    /* Traversal is refused by name, so it does not depend on which parts exist. */
    for (i = 0; i + 1 < n; i++) {
        int starts = i == 0 || db[i - 1] == '/';
        int ends = i + 2 == n || db[i + 2] == '/';
        if (starts && ends && db[i] == '.' && db[i + 1] == '.') {
            sqlsrv_err_proto(e, SQLSRV_PE_FORBIDDEN, "database path must not contain '..'");
            return -1;
        }
    }
    plen = strlen(p->allow_prefix);
    if (n > 0 && db[0] == '/') {
        cand = malloc(n + 1);
        if (cand != NULL) {
            memcpy(cand, db, n);
            cand[n] = '\0';
        }
    } else {
        cand = malloc(plen + n + 1);
        if (cand != NULL) {
            memcpy(cand, p->allow_prefix, plen);
            if (n > 0) {
                memcpy(cand + plen, db, n);
            }
            cand[plen + n] = '\0';
        }
    }
    if (cand == NULL) {
        sqlsrv_err_proto(e, SQLSRV_PE_INTERNAL, "out of memory");
        return -1;
    }
    if (canon_path(cand, &canon, e) != 0) {
        free(cand);
        return -1;
    }
    free(cand);
    cl = strlen(canon);
    /* Containment: the canonical path must lie strictly inside the allow directory. */
    if (cl <= plen || strncmp(canon, p->allow_prefix, plen) != 0) {
        free(canon);
        sqlsrv_err_proto(e, SQLSRV_PE_FORBIDDEN, "database path is outside the allowed directory");
        return -1;
    }
    *out = canon;
    return 0;
}
