/*
 * sqlsrv_pathrules.c - the lexical path rules of the database policy (see
 * sqlsrv_pathrules.h). No platform calls: this file is tested on every host.
 */
#include "sqlsrv_pathrules.h"

#include <stdlib.h>
#include <string.h>

sqlsrv_path_style sqlsrv_path_host_style(void) {
#if defined(_WIN32)
    return SQLSRV_PATH_WINDOWS;
#else
    return SQLSRV_PATH_POSIX;
#endif
}

char sqlsrv_path_sep(sqlsrv_path_style style) {
    return style == SQLSRV_PATH_WINDOWS ? '\\' : '/';
}

int sqlsrv_path_is_sep(unsigned char c, sqlsrv_path_style style) {
    return c == '/' || (style == SQLSRV_PATH_WINDOWS && c == '\\');
}

static int is_alpha(unsigned char c) {
    return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z');
}

static int upper(unsigned char c) {
    return (c >= 'a' && c <= 'z') ? c - 'a' + 'A' : c;
}

static int lower(unsigned char c) {
    return (c >= 'A' && c <= 'Z') ? c - 'A' + 'a' : c;
}

sqlsrv_path_kind sqlsrv_path_classify(const uint8_t *p, size_t n, sqlsrv_path_style style) {
    if (style == SQLSRV_PATH_POSIX) {
        return n > 0 && p[0] == '/' ? SQLSRV_PATHK_ABSOLUTE : SQLSRV_PATHK_RELATIVE;
    }
    if (n >= 2 && sqlsrv_path_is_sep(p[0], style) && sqlsrv_path_is_sep(p[1], style)) {
        return SQLSRV_PATHK_UNC;
    }
    if (n >= 1 && sqlsrv_path_is_sep(p[0], style)) {
        return SQLSRV_PATHK_ROOTED;
    }
    if (n >= 2 && is_alpha(p[0]) && p[1] == ':') {
        return n >= 3 && sqlsrv_path_is_sep(p[2], style) ? SQLSRV_PATHK_ABSOLUTE : SQLSRV_PATHK_DRIVE_RELATIVE;
    }
    return SQLSRV_PATHK_RELATIVE;
}

/* A ".." component, with '/' as the only separator (POSIX). */
static int has_dotdot_posix(const uint8_t *p, size_t n) {
    size_t i;
    for (i = 0; i + 1 < n; i++) {
        int starts = i == 0 || p[i - 1] == '/';
        int ends = i + 2 == n || p[i + 2] == '/';
        if (starts && ends && p[i] == '.' && p[i + 1] == '.') {
            return 1;
        }
    }
    return 0;
}

/* Case-insensitive comparison of the n bytes of s with the ASCII name. */
static int name_is(const uint8_t *s, size_t n, const char *name) {
    size_t i;
    if (strlen(name) != n) {
        return 0;
    }
    for (i = 0; i < n; i++) {
        if (upper(s[i]) != (unsigned char)name[i]) {
            return 0;
        }
    }
    return 1;
}

/* A reserved device name: the part of the component before its first dot. */
static int is_reserved_device(const uint8_t *s, size_t n) {
    static const char *const fixed[] = {"CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$"};
    size_t base = 0;
    size_t k;

    while (base < n && s[base] != '.') {
        base++;
    }
    for (k = 0; k < sizeof fixed / sizeof fixed[0]; k++) {
        if (name_is(s, base, fixed[k])) {
            return 1;
        }
    }
    /* COM0..COM9 and LPT0..LPT9 */
    if (base == 4 && s[3] >= '0' && s[3] <= '9') {
        if (name_is(s, 3, "COM") || name_is(s, 3, "LPT")) {
            return 1;
        }
    }
    return 0;
}

static const char *check_component(const uint8_t *s, size_t len) {
    if (len == 0) {
        return "invalid database path";
    }
    if (len == 1 && s[0] == '.') {
        return NULL;
    }
    if (len == 2 && s[0] == '.' && s[1] == '.') {
        return "database path must not contain '..'";
    }
    /* Windows strips trailing dots and spaces, so two names would be one file. */
    if (s[len - 1] == '.' || s[len - 1] == ' ') {
        return "invalid database path";
    }
    if (is_reserved_device(s, len)) {
        return "reserved device name in database path";
    }
    return NULL;
}

static int bad_windows_char(unsigned char c) {
    if (c < 0x20 || c == 0x7f) {
        return 1;
    }
    return c == '<' || c == '>' || c == '"' || c == '|' || c == '?' || c == '*' || c == ':';
}

const char *sqlsrv_path_check_raw(const uint8_t *p, size_t n, sqlsrv_path_style style) {
    sqlsrv_path_kind kind = sqlsrv_path_classify(p, n, style);
    size_t start = 0;
    size_t comp;
    size_t i;

    if (style == SQLSRV_PATH_POSIX) {
        return has_dotdot_posix(p, n) ? "database path must not contain '..'" : NULL;
    }
    switch (kind) {
    case SQLSRV_PATHK_ABSOLUTE:
        start = 3; /* "X:\" is checked as its drive and the rest */
        break;
    case SQLSRV_PATHK_RELATIVE:
        break;
    case SQLSRV_PATHK_ROOTED:
        return "database path must start with a drive letter or be relative";
    case SQLSRV_PATHK_DRIVE_RELATIVE:
        return "database path must not be drive-relative";
    case SQLSRV_PATHK_UNC:
    default:
        return "UNC, device and extended-length paths are not allowed";
    }
    for (i = start; i < n; i++) {
        if (bad_windows_char(p[i])) {
            return "invalid character in database path";
        }
    }
    comp = start;
    for (i = start; i <= n; i++) {
        if (i == n || sqlsrv_path_is_sep(p[i], style)) {
            const char *why = check_component(p + comp, i - comp);
            if (why != NULL) {
                return why;
            }
            comp = i + 1;
        }
    }
    return NULL;
}

char *sqlsrv_path_join(const char *base, const uint8_t *p, size_t n, sqlsrv_path_style style) {
    int absolute = sqlsrv_path_classify(p, n, style) == SQLSRV_PATHK_ABSOLUTE;
    size_t bl = 0;
    size_t off = 0;
    size_t i;
    int need_sep = 0;
    char *out;

    if (!absolute) {
        if (base == NULL || base[0] == '\0') {
            return NULL;
        }
        bl = strlen(base);
        need_sep = !sqlsrv_path_is_sep((unsigned char)base[bl - 1], style);
    }
    out = malloc(bl + (size_t)need_sep + n + 1);
    if (out == NULL) {
        return NULL;
    }
    if (!absolute) {
        memcpy(out, base, bl);
        off = bl;
        if (need_sep) {
            out[off++] = sqlsrv_path_sep(style);
        }
    }
    for (i = 0; i < n; i++) {
        unsigned char c = p[i];
        out[off + i] = (char)(style == SQLSRV_PATH_WINDOWS && c == '/' ? '\\' : c);
    }
    out[off + n] = '\0';
    return out;
}

int sqlsrv_path_split(const char *path, sqlsrv_path_style style, size_t *dir_len, size_t *leaf_at) {
    size_t n = strlen(path);
    size_t i;
    for (i = n; i > 0; i--) {
        if (sqlsrv_path_is_sep((unsigned char)path[i - 1], style)) {
            size_t k = i - 1;
            *leaf_at = k + 1;
            if (k == 0) {
                *dir_len = 1; /* "/x" and "\x": the root */
            } else if (style == SQLSRV_PATH_WINDOWS && k == 2 && path[1] == ':') {
                *dir_len = 3; /* "C:\x": the drive root */
            } else {
                *dir_len = k;
            }
            return 0;
        }
    }
    return -1;
}

char *sqlsrv_path_dir_prefix(const char *dir, sqlsrv_path_style style) {
    size_t n = strlen(dir);
    int need = n == 0 || !sqlsrv_path_is_sep((unsigned char)dir[n - 1], style);
    char *out = malloc(n + (size_t)need + 1);
    if (out == NULL) {
        return NULL;
    }
    memcpy(out, dir, n);
    if (need) {
        out[n++] = sqlsrv_path_sep(style);
    }
    out[n] = '\0';
    return out;
}

int sqlsrv_path_inside(const char *path, const char *prefix, sqlsrv_path_style style) {
    size_t pl = strlen(prefix);
    size_t n = strlen(path);
    size_t i;
    if (n <= pl) {
        return 0;
    }
    for (i = 0; i < pl; i++) {
        unsigned char a = (unsigned char)path[i];
        unsigned char b = (unsigned char)prefix[i];
        if (style == SQLSRV_PATH_WINDOWS) {
            a = (unsigned char)lower(a);
            b = (unsigned char)lower(b);
        }
        if (a != b) {
            return 0;
        }
    }
    return 1;
}
