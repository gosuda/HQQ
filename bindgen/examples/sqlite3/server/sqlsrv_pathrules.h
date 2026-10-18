/*
 * sqlsrv_pathrules.h - the lexical part of the database path policy: pure
 * string functions, the same on every host. The canonical form (symbolic
 * links, the real case of names) is the platform's job (sqlsrv_port_realpath).
 *
 * The rules of both styles are compiled and tested on every host: every
 * function takes the style, so the Windows rules run on a POSIX machine and
 * the other way round. The server itself uses sqlsrv_path_host_style().
 */
#ifndef SQLSRV_PATHRULES_H
#define SQLSRV_PATHRULES_H

#include <stddef.h>
#include <stdint.h>

typedef enum sqlsrv_path_style {
    SQLSRV_PATH_POSIX = 0,   /* '/' only; a name may contain a backslash */
    SQLSRV_PATH_WINDOWS = 1  /* '\' or '/'; drive letters; reserved names; no ':' except a drive */
} sqlsrv_path_style;

/* The style of the host the server is built for. */
sqlsrv_path_style sqlsrv_path_host_style(void);
/* The separator written into new paths: '/' or '\'. */
char sqlsrv_path_sep(sqlsrv_path_style style);
int sqlsrv_path_is_sep(unsigned char c, sqlsrv_path_style style);

typedef enum sqlsrv_path_kind {
    SQLSRV_PATHK_RELATIVE = 0,   /* joined to the allow directory */
    SQLSRV_PATHK_ABSOLUTE,       /* POSIX: leading '/'. Windows: "X:\" or "X:/" */
    SQLSRV_PATHK_ROOTED,         /* Windows only: a separator without a drive ("\x") */
    SQLSRV_PATHK_DRIVE_RELATIVE, /* Windows only: "X:x" */
    SQLSRV_PATHK_UNC             /* Windows only: "\\server\share", "\\?\...", "\\.\..." */
} sqlsrv_path_kind;

sqlsrv_path_kind sqlsrv_path_classify(const uint8_t *p, size_t n, sqlsrv_path_style style);

/*
 * Checks a raw database path (not an alias) before it is joined to the allow
 * directory. Returns NULL when it is acceptable, otherwise the reason (a static
 * string). POSIX: a ".." component is refused. Windows: UNC, device, rooted and
 * drive-relative forms are refused; ':' is allowed only in the drive letter (no
 * alternate data streams); < > " | ? * and control characters are refused; a
 * component may not be empty, end in '.' or ' ', or be a reserved device name
 * (CON, PRN, AUX, NUL, COMn, LPTn, CONIN$, CONOUT$, with or without an extension).
 */
const char *sqlsrv_path_check_raw(const uint8_t *p, size_t n, sqlsrv_path_style style);

/*
 * The candidate path: p itself when it is absolute, otherwise base + separator
 * + p. base must not be NULL for a relative path. Windows: every '/' becomes
 * '\'. Returns a new string (free() it), or NULL.
 */
char *sqlsrv_path_join(const char *base, const uint8_t *p, size_t n, sqlsrv_path_style style);

/*
 * Splits a path at its last separator: *dir_len is the length of the parent
 * ("/" or "C:\" keep their root), *leaf_at the index of the final name. Returns
 * 0, or -1 when the path has no separator.
 */
int sqlsrv_path_split(const char *path, sqlsrv_path_style style, size_t *dir_len, size_t *leaf_at);

/* A directory with exactly one trailing separator (a new string, free() it). */
char *sqlsrv_path_dir_prefix(const char *dir, sqlsrv_path_style style);

/*
 * 1 when path lies strictly inside prefix (prefix ends with a separator), 0
 * otherwise. Windows compares ASCII letters case-insensitively; a non-ASCII
 * case difference is a mismatch, which fails closed.
 */
int sqlsrv_path_inside(const char *path, const char *prefix, sqlsrv_path_style style);

#endif /* SQLSRV_PATHRULES_H */
