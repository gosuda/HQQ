/* Portable name validation (SPEC 11.1); no OS calls. */
#define HQQ_BUILDING 1
#include "hqq_internal.h"

#define HQQ_MAX_BASE_BYTES 255u
#define HQQ_WIN_MAX_OBJECT_UTF16 259u
#define HQQ_WIN_SEM_SUFFIX_LEN 9u /* ".hqq-sem0" */

hqq_status hqq_name_base(const char *name, size_t max_len, const char **base, size_t *base_len) {
    size_t n, i;
    const char *b;

    if (name == NULL) {
        return HQQ_ERR_INVALID_NAME;
    }
    n = strlen(name);
    if (max_len > 0 && n > max_len) {
        return HQQ_ERR_INVALID_NAME;
    }
    b = name;
    if (n > 0 && b[0] == '/') {
        b++;
        n--;
    }
    if (n == 0 || (n == 1 && b[0] == '.') || (n == 2 && b[0] == '.' && b[1] == '.')) {
        return HQQ_ERR_INVALID_NAME;
    }
    for (i = 0; i < n; i++) {
        if (b[i] == '/' || b[i] == '\\') {
            return HQQ_ERR_INVALID_NAME;
        }
    }
    if (n > HQQ_MAX_BASE_BYTES) {
        return HQQ_ERR_INVALID_NAME;
    }
    if (base != NULL) {
        *base = b;
    }
    if (base_len != NULL) {
        *base_len = n;
    }
    return HQQ_OK;
}

/* Number of UTF-16 code units of a UTF-8 string (invalid bytes count as one). */
static size_t utf16_units(const char *s, size_t n) {
    size_t i = 0, units = 0;
    while (i < n) {
        unsigned char c = (unsigned char)s[i];
        size_t len = 1;
        if (c >= 0xF0 && c <= 0xF4) {
            len = 4;
        } else if (c >= 0xE0 && c < 0xF0) {
            len = 3;
        } else if (c >= 0xC2 && c < 0xE0) {
            len = 2;
        }
        if (len > n - i) {
            len = 1;
        } else {
            size_t k;
            for (k = 1; k < len; k++) {
                if (((unsigned char)s[i + k] & 0xC0u) != 0x80u) {
                    len = 1;
                    break;
                }
            }
        }
        units += (len == 4) ? 2 : 1;
        i += len;
    }
    return units;
}

static int ascii_prefix_ci(const char *s, size_t n, const char *prefix, size_t plen) {
    size_t i;
    if (n <= plen) { /* the prefix must be followed by at least one more character */
        return 0;
    }
    for (i = 0; i < plen; i++) {
        char a = s[i], b = prefix[i];
        if (a >= 'A' && a <= 'Z') {
            a = (char)(a - 'A' + 'a');
        }
        if (b >= 'A' && b <= 'Z') {
            b = (char)(b - 'A' + 'a');
        }
        if (a != b) {
            return 0;
        }
    }
    return 1;
}

hqq_status hqq_windows_object_name(const char *name, char *out, size_t cap, size_t *out_len) {
    static const char global_prefix[] = "Global\\";
    static const char local_prefix[] = "Local\\";
    const char *prefix = local_prefix;
    size_t plen = sizeof local_prefix - 1;
    const char *rest = name;
    const char *base;
    size_t base_len, total;
    hqq_status st;

    if (name == NULL) {
        return HQQ_ERR_INVALID_NAME;
    }
    {
        size_t n = strlen(name);
        if (ascii_prefix_ci(name, n, global_prefix, sizeof global_prefix - 1)) {
            prefix = global_prefix;
            plen = sizeof global_prefix - 1;
            rest = name + plen;
        } else if (ascii_prefix_ci(name, n, local_prefix, sizeof local_prefix - 1)) {
            prefix = local_prefix;
            plen = sizeof local_prefix - 1;
            rest = name + plen;
        }
    }
    st = hqq_name_base(rest, 0, &base, &base_len);
    if (st != HQQ_OK) {
        return st;
    }
    total = plen + base_len;
    if (utf16_units(prefix, plen) + utf16_units(base, base_len) + HQQ_WIN_SEM_SUFFIX_LEN >
        HQQ_WIN_MAX_OBJECT_UTF16) {
        return HQQ_ERR_INVALID_NAME;
    }
    if (out != NULL) {
        if (cap < total + 1) {
            return HQQ_ERR_INVALID_NAME;
        }
        memcpy(out, prefix, plen);
        memcpy(out + plen, base, base_len);
        out[total] = '\0';
    }
    if (out_len != NULL) {
        *out_len = total;
    }
    return HQQ_OK;
}
