/*
 * hqq_rpc.c - request readers, response writers, scratch memory, the handle table and the
 * execution lock of the hqq-bindgen C runtime (public API: include/hqq/hqq_rpc.h; contract:
 * spec/toolchain/BINDGEN.md 4.5, 4.9, 5.1, 8.1, 9.5).
 *
 * Everything here runs on the session thread of one session, except the status helpers.
 * Portable C11: no POSIX headers, no threads (the platform layer provides the mutex).
 */
#if defined(_WIN32)
#  ifndef WIN32_LEAN_AND_MEAN
#    define WIN32_LEAN_AND_MEAN 1
#  endif
#  ifndef NOMINMAX
#    define NOMINMAX 1
#  endif
#  ifndef _WIN32_WINNT
#    define _WIN32_WINNT 0x0A00
#  endif
#elif defined(__linux__)
#  ifndef _GNU_SOURCE
#    define _GNU_SOURCE 1
#  endif
#elif defined(__APPLE__)
#  ifndef _DARWIN_C_SOURCE
#    define _DARWIN_C_SOURCE 1
#  endif
#else
#  ifndef _POSIX_C_SOURCE
#    define _POSIX_C_SOURCE 200809L
#  endif
#endif

#include <stdarg.h>
#include <stdio.h>
#include <string.h>

#define HQQ_BUILDING 1
#include "hqq_rpc_internal.h"

/* ---- Little-endian helpers ------------------------------------------------------------- */

uint16_t hqqrpc_le16(const uint8_t *p) {
    return (uint16_t)((uint16_t)p[0] | (uint16_t)((uint16_t)p[1] << 8));
}

uint32_t hqqrpc_le32(const uint8_t *p) {
    return (uint32_t)p[0] | ((uint32_t)p[1] << 8) | ((uint32_t)p[2] << 16) | ((uint32_t)p[3] << 24);
}

void hqqrpc_put_le16(uint8_t *p, uint16_t v) {
    p[0] = (uint8_t)v;
    p[1] = (uint8_t)(v >> 8);
}

void hqqrpc_put_le32(uint8_t *p, uint32_t v) {
    p[0] = (uint8_t)v;
    p[1] = (uint8_t)(v >> 8);
    p[2] = (uint8_t)(v >> 16);
    p[3] = (uint8_t)(v >> 24);
}

void hqqrpc_put_le64(uint8_t *p, uint64_t v) {
    hqqrpc_put_le32(p, (uint32_t)v);
    hqqrpc_put_le32(p + 4, (uint32_t)(v >> 32));
}

/* ---- Status (4.10) --------------------------------------------------------------------- */

const char *hqqrpc_status_text(hqqrpc_status st) {
    switch (st) {
    case HQQRPC_E_UNKNOWN_OP: return "unknown op";
    case HQQRPC_E_BAD_REQUEST: return "bad request";
    case HQQRPC_E_BAD_HANDLE: return "bad handle";
    case HQQRPC_E_LIMIT: return "limit exceeded";
    case HQQRPC_E_DENIED: return "denied";
    case HQQRPC_E_INTERNAL: return "internal error";
    case HQQRPC_E_PROTOCOL: return "protocol violation";
    case HQQRPC_E_SHUTTING_DOWN: return "shutting down";
    default: return "";
    }
}

const char *hqqrpc_strerror(hqqrpc_status status) {
    if (status == HQQRPC_OK) {
        return "ok";
    }
    if (status >= HQQRPC_E_UNKNOWN_OP && status <= HQQRPC_E_SHUTTING_DOWN) {
        return hqqrpc_status_text(status);
    }
    if (status >= HQQRPC_E_APP_MIN && status <= HQQRPC_E_APP_MAX) {
        return "application status";
    }
    return "reserved status";
}

hqqrpc_status hqqrpc_request_status(hqqrpc_status st) {
    if (st >= HQQRPC_E_UNKNOWN_OP && st <= HQQRPC_E_INTERNAL) {
        return st;
    }
    if (st >= HQQRPC_E_APP_MIN && st <= HQQRPC_E_APP_MAX) {
        return st;
    }
    return HQQRPC_E_INTERNAL;
}

hqqrpc_status hqqrpc_hook_status(hqqrpc_status st) {
    if (st == HQQRPC_OK || st == HQQRPC_E_DENIED || (st >= HQQRPC_E_APP_MIN && st <= HQQRPC_E_APP_MAX)) {
        return st;
    }
    return HQQRPC_E_INTERNAL;
}

hqqrpc_status hqqrpc_shim_status(hqqrpc_status st) {
    if (st == HQQRPC_OK || (st >= HQQRPC_E_APP_MIN && st <= HQQRPC_E_APP_MAX)) {
        return st;
    }
    return HQQRPC_E_INTERNAL;
}

/* ---- Log ------------------------------------------------------------------------------- */

void hqqrpc_log(hqqrpc_session *s, int level, const char *fmt, ...) {
    char buf[256];
    va_list ap;

    if (s == NULL || s->on_log == NULL) {
        return;
    }
    va_start(ap, fmt);
    (void)vsnprintf(buf, sizeof buf, fmt, ap);
    va_end(ap);
    s->on_log(s->cb_user, level, buf);
}

/* ---- Configuration and interface checks ----------------------------------------------- */

static int is_pow2_u32(uint32_t v) {
    return v != 0 && (v & (v - 1u)) == 0;
}

hqq_status hqqrpc_cfg_resolve(const hqqrpc_server_options *in, hqqrpc_cfg *out) {
    hqqrpc_cfg c;

    if (in == NULL || out == NULL || in->struct_size < sizeof *in) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    memset(&c, 0, sizeof c);
    c.perm = in->perm != 0 ? in->perm : 0600u;
    c.buffer_count = in->default_buffer_count != 0 ? in->default_buffer_count : HQQRPC_DEF_BUFFER_COUNT;
    c.buffer_size = in->default_buffer_size != 0 ? in->default_buffer_size : HQQRPC_DEF_BUFFER_SIZE;
    c.max_buffer_count = in->max_buffer_count != 0 ? in->max_buffer_count : HQQRPC_MAX_BUFFER_COUNT;
    c.max_buffer_size = in->max_buffer_size != 0 ? in->max_buffer_size : HQQRPC_MAX_BUFFER_SIZE;
    c.max_sessions = in->max_sessions != 0 ? in->max_sessions : HQQRPC_DEF_MAX_SESSIONS;
    c.max_handles = in->max_handles != 0 ? in->max_handles : HQQRPC_DEF_MAX_HANDLES;
    c.max_control_links = in->max_control_links != 0 ? in->max_control_links : HQQRPC_DEF_MAX_CONTROL_LINKS;
    c.max_segment_bytes = in->max_segment_bytes != 0 ? in->max_segment_bytes : HQQRPC_DEF_MAX_SEGMENT_BYTES;
    c.max_in_flight = in->max_in_flight;
    c.max_message = in->max_message != 0 ? in->max_message : HQQRPC_DEF_MAX_MESSAGE;
    c.open_timeout_ns = in->open_timeout_ns != 0 ? in->open_timeout_ns : HQQRPC_DEF_OPEN_TIMEOUT_NS;
    c.shutdown_timeout_ns = in->shutdown_timeout_ns != 0 ? in->shutdown_timeout_ns : HQQRPC_DEF_SHUTDOWN_TIMEOUT_NS;
    c.write_stall_timeout_ns =
        in->write_stall_timeout_ns != 0 ? in->write_stall_timeout_ns : HQQRPC_DEF_WRITE_STALL_NS;
    c.idle_timeout_ns = in->idle_timeout_ns > 0 ? in->idle_timeout_ns : 0;
    c.scratch_keep = in->scratch_keep != 0 ? in->scratch_keep : HQQRPC_DEF_SCRATCH_KEEP;
    c.copy_request = in->copy_request != 0;

    if (c.max_buffer_count > HQQRPC_MAX_BUFFER_COUNT || c.max_buffer_size > HQQRPC_MAX_BUFFER_SIZE ||
        c.max_handles > HQQRPC_MAX_HANDLES || c.max_message > HQQRPC_HARD_MAX_MESSAGE ||
        c.max_message < HQQRPC_MIN_MAX_MESSAGE) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    if (c.open_timeout_ns < 0 || c.shutdown_timeout_ns < 0 || c.write_stall_timeout_ns < 0) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    if (!is_pow2_u32(c.buffer_count) || c.buffer_count < HQQ_MIN_BUFFER_COUNT || c.buffer_count > c.max_buffer_count ||
        (c.buffer_size % 64u) != 0 || c.buffer_size < HQQRPC_MIN_BUFFER_SIZE || c.buffer_size > c.max_buffer_size ||
        2u * (uint64_t)c.buffer_count * c.buffer_size > c.max_segment_bytes) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    *out = c;
    return HQQ_OK;
}

hqq_status hqqrpc_iface_check(const hqqrpc_iface *iface) {
    unsigned i;

    if (iface == NULL || iface->dispatch == NULL || iface->serialize > HQQRPC_SERIALIZE_NONE) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    if (iface->handle_type_count != 0 && iface->handle_types == NULL) {
        return HQQ_ERR_INVALID_ARGUMENT;
    }
    for (i = 0; i < iface->handle_type_count; i++) {
        uint16_t parent = iface->handle_types[i].parent;
        if (parent > iface->handle_type_count || parent == i + 1u) {
            return HQQ_ERR_INVALID_ARGUMENT;
        }
    }
    return HQQ_OK;
}

/* ---- Memory blocks (ALIGN-aligned, one allocation each) ---------------------------------- */

hqqrpc_blk *hqqrpc_blk_new(hqqrpc_session *s, size_t n) {
    const size_t hdr = (sizeof(hqqrpc_blk) + (HQQRPC_ALIGN - 1u)) & ~(size_t)(HQQRPC_ALIGN - 1u);
    size_t total;
    uintptr_t addr;
    uint8_t *raw;
    hqqrpc_blk *b;

    if (n == 0) {
        n = 1;
    }
    if (n > SIZE_MAX - hdr - HQQRPC_ALIGN) {
        return NULL;
    }
    total = hdr + n + HQQRPC_ALIGN;
    raw = (uint8_t *)hqq_alloc(s->alloc, total);
    if (raw == NULL) {
        return NULL;
    }
    b = (hqqrpc_blk *)(void *)raw;
    addr = ((uintptr_t)raw + hdr + (HQQRPC_ALIGN - 1u)) & ~(uintptr_t)(HQQRPC_ALIGN - 1u);
    b->next = NULL;
    b->total = total;
    b->p = (uint8_t *)addr;
    b->cap = total - (size_t)(addr - (uintptr_t)raw);
    return b;
}

void hqqrpc_blk_free(hqqrpc_session *s, hqqrpc_blk *b) {
    if (b != NULL) {
        hqq_free(s->alloc, b, b->total);
    }
}

/* Gives a block back: the largest block of at most scratch_keep bytes stays as the spare. */
void hqqrpc_blk_release(hqqrpc_session *s, hqqrpc_blk *b) {
    if (b == NULL) {
        return;
    }
    b->next = NULL;
    if (b->cap <= s->cfg.scratch_keep && (s->spare == NULL || s->spare->cap < b->cap)) {
        hqqrpc_blk_free(s, s->spare);
        s->spare = b;
        return;
    }
    hqqrpc_blk_free(s, b);
}

void hqqrpc_scratch_release(hqqrpc_session *s) {
    hqqrpc_blk *b = s->cs.scratch;
    s->cs.scratch = NULL;
    while (b != NULL) {
        hqqrpc_blk *next = b->next;
        hqqrpc_blk_release(s, b);
        b = next;
    }
}

void *hqqrpc_scratch(hqqrpc_session *s, size_t n) {
    hqqrpc_blk *b;

    if (s == NULL) {
        return NULL;
    }
    if (n == 0) {
        n = 1;
    }
    if (s->spare != NULL && s->spare->cap >= n) {
        b = s->spare;
        s->spare = NULL;
    } else {
        b = hqqrpc_blk_new(s, n);
        if (b == NULL) {
            return NULL;
        }
    }
    b->next = s->cs.scratch;
    s->cs.scratch = b;
    return b->p;
}

void *hqqrpc_session_user(hqqrpc_session *s) {
    return s != NULL ? s->user : NULL;
}

/* ---- Execution lock (8.1) ---------------------------------------------------------------- */

void hqqrpc_exec_begin(hqqrpc_session *s) {
    if (s->exec_mu != NULL && !s->lock_held) {
        hqq_plat_mutex_lock(s->exec_mu);
        s->lock_held = 1;
    }
}

void hqqrpc_exec_end(hqqrpc_session *s) {
    if (s->lock_held) {
        s->lock_held = 0;
        hqq_plat_mutex_unlock(s->exec_mu);
    }
}

/* ---- Request decoding (4.5) ---------------------------------------------------------------- */

void hqqrpc_rd_init(hqqrpc_rd *r, const hqqrpc_call *call) {
    r->p = call->body;
    r->len = call->body_len;
    r->off = 0;
}

/* Moves r->off up to a multiple of align; 0 if that passes the end of the body. */
static int rd_align(hqqrpc_rd *r, size_t align) {
    size_t rem;

    if (align > 1) {
        rem = r->off % align;
        if (rem != 0) {
            size_t pad = align - rem;
            if (pad > r->len - r->off) {
                return 0;
            }
            r->off += pad;
        }
    }
    return 1;
}

hqqrpc_status hqqrpc_rd_take(hqqrpc_rd *r, size_t align, size_t size, const uint8_t **out) {
    if (!rd_align(r, align) || size > r->len - r->off) {
        return HQQRPC_E_BAD_REQUEST;
    }
    *out = r->p + r->off;
    r->off += size;
    return HQQRPC_OK;
}

hqqrpc_status hqqrpc_rd_u32(hqqrpc_rd *r, uint32_t *v) {
    const uint8_t *p;
    hqqrpc_status st = hqqrpc_rd_take(r, 4, 4, &p);

    if (st != HQQRPC_OK) {
        return st;
    }
    *v = hqqrpc_le32(p);
    return HQQRPC_OK;
}

hqqrpc_status hqqrpc_rd_count(hqqrpc_rd *r, size_t minsize, uint32_t max, uint32_t *count) {
    uint32_t n;
    hqqrpc_status st = hqqrpc_rd_u32(r, &n);

    if (st != HQQRPC_OK) {
        return st;
    }
    if (n == HQQRPC_NULL_LEN || n > max || (minsize != 0 && n > (r->len - r->off) / minsize)) {
        return HQQRPC_E_BAD_REQUEST;
    }
    *count = n;
    return HQQRPC_OK;
}

hqqrpc_status hqqrpc_rd_str(hqqrpc_rd *r, int nullable, uint32_t max, const char **s, uint32_t *n) {
    const uint8_t *data;
    uint32_t len;
    hqqrpc_status st = hqqrpc_rd_u32(r, &len);

    if (st != HQQRPC_OK) {
        return st;
    }
    if (len == HQQRPC_NULL_LEN) {
        if (!nullable) {
            return HQQRPC_E_BAD_REQUEST;
        }
        *s = NULL;
        *n = 0;
        return HQQRPC_OK;
    }
    /* len + 1 cannot overflow in 64 bits. */
    if (len > max || (uint64_t)len + 1u > r->len - r->off) {
        return HQQRPC_E_BAD_REQUEST;
    }
    data = r->p + r->off;
    if ((len != 0 && memchr(data, 0, len) != NULL) || data[len] != 0) {
        return HQQRPC_E_BAD_REQUEST;
    }
    r->off += (size_t)len + 1u;
    *s = (const char *)data;
    *n = len;
    return HQQRPC_OK;
}

hqqrpc_status hqqrpc_rd_bytes(hqqrpc_rd *r, int nullable, uint32_t max, const void **p, uint32_t *n) {
    uint32_t len;
    hqqrpc_status st = hqqrpc_rd_u32(r, &len);

    if (st != HQQRPC_OK) {
        return st;
    }
    if (len == HQQRPC_NULL_LEN) {
        if (!nullable) {
            return HQQRPC_E_BAD_REQUEST;
        }
        *p = NULL;
        *n = 0;
        return HQQRPC_OK;
    }
    if (len > max || len > r->len - r->off) {
        return HQQRPC_E_BAD_REQUEST;
    }
    *p = r->p + r->off;
    *n = len;
    r->off += len;
    return HQQRPC_OK;
}

hqqrpc_status hqqrpc_rd_array(hqqrpc_rd *r, size_t elem_size, size_t elem_align, int nullable, uint32_t max,
                              const void **p, uint32_t *count) {
    uint32_t n;
    size_t remaining;
    hqqrpc_status st = hqqrpc_rd_u32(r, &n);

    if (st != HQQRPC_OK) {
        return st;
    }
    if (n == HQQRPC_NULL_LEN) {
        if (!nullable) {
            return HQQRPC_E_BAD_REQUEST;
        }
        *p = NULL;
        *count = 0;
        return HQQRPC_OK;
    }
    if (n > max || (n != 0 && !rd_align(r, elem_align))) { /* an empty array has no element to align (4.5) */
        return HQQRPC_E_BAD_REQUEST;
    }
    remaining = r->len - r->off;
    if (elem_size != 0 && n > remaining / elem_size) {
        return HQQRPC_E_BAD_REQUEST;
    }
    *p = r->p + r->off;
    *count = n;
    r->off += (size_t)n * elem_size;
    return HQQRPC_OK;
}

hqqrpc_status hqqrpc_rd_end(const hqqrpc_rd *r) {
    return r->off == r->len ? HQQRPC_OK : HQQRPC_E_BAD_REQUEST;
}

/* ---- Response writer (4.5, 4.6.3) ----------------------------------------------------------- */

/*
 * Claims `size` bytes after padding w->off to a multiple of align. The padding is zero. advance == 0
 * leaves w->off at the start of the claimed bytes (a span). Sets the sticky error and returns NULL
 * if the capacity (a uint64_t comparison) or an earlier error forbids it.
 */
static uint8_t *wr_claim(hqqrpc_wr *w, size_t align, uint64_t size, int zero, int advance) {
    hqqrpc_cstate *cs = (hqqrpc_cstate *)w->priv;
    uint64_t off = w->off;
    uint64_t pad = 0;

    if (w->err != 0) {
        return NULL;
    }
    if (align > 1) {
        uint64_t rem = off % align;
        pad = rem != 0 ? align - rem : 0;
    }
    if (off > w->cap || pad > w->cap - off || size > w->cap - off - pad) {
        w->err = HQQRPC_E_INTERNAL;
        return NULL;
    }
    if (pad != 0) {
        memset(w->p + off, 0, (size_t)pad);
    }
    off += pad;
    if (zero && size != 0) {
        memset(w->p + off, 0, (size_t)size);
    }
    w->off = advance ? off + size : off;
    if (cs != NULL) {
        cs->span = advance ? 0 : size;
    }
    return w->p + off;
}

uint8_t *hqqrpc_wr_fixed(hqqrpc_wr *w, size_t align, size_t size) {
    return wr_claim(w, align, size, 1, 1);
}

uint8_t *hqqrpc_wr_span(hqqrpc_wr *w, size_t align, size_t n) {
    return wr_claim(w, align, n, 0, 0);
}

hqqrpc_status hqqrpc_wr_advance(hqqrpc_wr *w, size_t n) {
    hqqrpc_cstate *cs = (hqqrpc_cstate *)w->priv;
    uint64_t limit;

    if (w->err != 0) {
        return HQQRPC_E_INTERNAL;
    }
    limit = cs != NULL ? cs->span : (w->off <= w->cap ? w->cap - w->off : 0);
    if (n > limit) {
        w->err = HQQRPC_E_INTERNAL;
        return HQQRPC_E_INTERNAL;
    }
    w->off += n;
    if (cs != NULL) {
        cs->span -= n;
    }
    return HQQRPC_OK;
}

hqqrpc_status hqqrpc_wr_pad(hqqrpc_wr *w, size_t align) {
    return wr_claim(w, align, 0, 0, 1) != NULL ? HQQRPC_OK : HQQRPC_E_INTERNAL;
}

hqqrpc_status hqqrpc_wr_put_u32(hqqrpc_wr *w, uint32_t v) {
    uint8_t *o = wr_claim(w, 4, 4, 0, 1);

    if (o == NULL) {
        return HQQRPC_E_INTERNAL;
    }
    hqqrpc_put_le32(o, v);
    return HQQRPC_OK;
}

hqqrpc_status hqqrpc_wr_put_u64(hqqrpc_wr *w, uint64_t v) {
    uint8_t *o = wr_claim(w, 8, 8, 0, 1);

    if (o == NULL) {
        return HQQRPC_E_INTERNAL;
    }
    hqqrpc_put_le64(o, v);
    return HQQRPC_OK;
}

hqqrpc_status hqqrpc_wr_put_str(hqqrpc_wr *w, const char *s, uint32_t n, uint32_t max) {
    uint8_t *o;

    if (w->err != 0) {
        return HQQRPC_E_INTERNAL;
    }
    if (n > max || (n != 0 && (s == NULL || memchr(s, 0, n) != NULL))) {
        w->err = HQQRPC_E_INTERNAL;
        return HQQRPC_E_INTERNAL;
    }
    o = wr_claim(w, 4, (uint64_t)n + 5u, 0, 1);
    if (o == NULL) {
        return HQQRPC_E_INTERNAL;
    }
    hqqrpc_put_le32(o, n);
    if (n != 0) {
        memcpy(o + 4, s, n);
    }
    o[4u + n] = 0;
    return HQQRPC_OK;
}

hqqrpc_status hqqrpc_wr_put_bytes(hqqrpc_wr *w, const void *p, uint32_t n, uint32_t max) {
    uint8_t *o;

    if (w->err != 0) {
        return HQQRPC_E_INTERNAL;
    }
    if (n > max || (n != 0 && p == NULL)) {
        w->err = HQQRPC_E_INTERNAL;
        return HQQRPC_E_INTERNAL;
    }
    o = wr_claim(w, 4, (uint64_t)n + 4u, 0, 1);
    if (o == NULL) {
        return HQQRPC_E_INTERNAL;
    }
    hqqrpc_put_le32(o, n);
    if (n != 0) {
        memcpy(o + 4, p, n);
    }
    return HQQRPC_OK;
}

hqqrpc_status hqqrpc_wr_put_null(hqqrpc_wr *w) {
    return hqqrpc_wr_put_u32(w, HQQRPC_NULL_LEN);
}

hqqrpc_status hqqrpc_wr_array_begin(hqqrpc_wr *w, size_t *mark) {
    uint8_t *o = wr_claim(w, 4, 4, 1, 1);

    if (o == NULL) {
        return HQQRPC_E_INTERNAL;
    }
    *mark = (size_t)(o - w->p);
    return HQQRPC_OK;
}

hqqrpc_status hqqrpc_wr_array_end(hqqrpc_wr *w, size_t mark, uint32_t count, uint32_t max) {
    if (w->err != 0) {
        return HQQRPC_E_INTERNAL;
    }
    if (count > max || count == HQQRPC_NULL_LEN || mark > w->off || w->off - mark < 4u) {
        w->err = HQQRPC_E_INTERNAL;
        return HQQRPC_E_INTERNAL;
    }
    hqqrpc_put_le32(w->p + mark, count);
    return HQQRPC_OK;
}

uint8_t hqqrpc_wr_put_detail(hqqrpc_wr *w, const char *msg) {
    const char *nul;
    uint32_t n;
    uint8_t *o;

    if (msg == NULL || w->err != 0) {
        return 0;
    }
    nul = (const char *)memchr(msg, 0, HQQRPC_DETAIL_MAX + 1u);
    n = nul != NULL ? (uint32_t)(nul - msg) : HQQRPC_DETAIL_MAX;
    if (wr_claim(w, 8, 0, 0, 1) == NULL) {
        return 0;
    }
    o = wr_claim(w, 4, (uint64_t)n + 5u, 0, 1);
    if (o == NULL) {
        return 0;
    }
    hqqrpc_put_le32(o, n);
    if (n != 0) {
        memcpy(o + 4, msg, n);
    }
    o[4u + n] = 0;
    return HQQRPC_F_DETAIL;
}

/* ---- Handle table (4.9, 5.1) --------------------------------------------------------------- */

static hqqrpc_hentry *tab_lookup(hqqrpc_session *s, uint32_t id) {
    uint32_t idx = id & 0xFFFFu;
    uint32_t gen = id >> 16;
    hqqrpc_hentry *e;

    if (idx == 0 || idx > s->high_water) {
        return NULL;
    }
    e = &s->tab[idx];
    if (!e->live || e->gen != gen) {
        return NULL;
    }
    return e;
}

/* Makes index `top` addressable. 0 on success. */
static int tab_grow(hqqrpc_session *s, uint32_t top) {
    uint32_t cap;
    hqqrpc_hentry *nt;

    if (top < s->tab_cap) {
        return 0;
    }
    cap = s->tab_cap != 0 ? s->tab_cap : 32u;
    while (cap <= top) {
        cap *= 2u;
    }
    if (cap > s->cfg.max_handles + 1u) {
        cap = s->cfg.max_handles + 1u;
    }
    nt = (hqqrpc_hentry *)hqq_alloc(s->alloc, (size_t)cap * sizeof *nt);
    if (nt == NULL) {
        return -1;
    }
    memset(nt, 0, (size_t)cap * sizeof *nt);
    if (s->tab != NULL) {
        memcpy(nt, s->tab, (size_t)s->tab_cap * sizeof *nt);
        hqq_free(s->alloc, s->tab, (size_t)s->tab_cap * sizeof *nt);
    }
    s->tab = nt;
    s->tab_cap = cap;
    return 0;
}

void hqqrpc_tab_free(hqqrpc_session *s) {
    hqq_free(s->alloc, s->tab, (size_t)s->tab_cap * sizeof *s->tab);
    s->tab = NULL;
    s->tab_cap = 0;
    hqq_free(s->alloc, s->cs.reg, (size_t)s->cs.regcap * sizeof *s->cs.reg);
    s->cs.reg = NULL;
    s->cs.regcap = 0;
}

hqqrpc_status hqqrpc_handle_get(hqqrpc_session *s, uint32_t id, uint16_t type, int nullable, void **ptr) {
    hqqrpc_hentry *e;

    if (id == 0) {
        if (!nullable) {
            return HQQRPC_E_BAD_HANDLE;
        }
        *ptr = NULL;
        return HQQRPC_OK;
    }
    e = tab_lookup(s, id);
    if (e == NULL || e->type != type) {
        return HQQRPC_E_BAD_HANDLE;
    }
    *ptr = e->ptr;
    return HQQRPC_OK;
}

hqqrpc_status hqqrpc_handle_reserve(hqqrpc_session *s, unsigned n) {
    uint64_t avail = (uint64_t)s->free_count + (s->cfg.max_handles - s->high_water);
    uint64_t want = (uint64_t)s->cs.hreserved + n;
    uint64_t fresh;

    if (want > avail) {
        return HQQRPC_E_LIMIT;
    }
    fresh = want > s->free_count ? want - s->free_count : 0;
    if (fresh != 0 && tab_grow(s, (uint32_t)(s->high_water + fresh)) != 0) {
        return HQQRPC_E_INTERNAL;
    }
    s->cs.hreserved += n;
    return HQQRPC_OK;
}

static void entry_unlink(hqqrpc_session *s, uint32_t idx) {
    hqqrpc_hentry *e = &s->tab[idx];

    if (e->prev != 0) {
        s->tab[e->prev].next = e->next;
    } else {
        s->live_head = e->next;
    }
    if (e->next != 0) {
        s->tab[e->next].prev = e->prev;
    } else {
        s->live_tail = e->prev;
    }
    e->prev = 0;
    e->next = 0;
}

/* Frees the entry: generation + 1, index to the free list. */
static void entry_free(hqqrpc_session *s, uint32_t idx) {
    hqqrpc_hentry *e = &s->tab[idx];

    entry_unlink(s, idx);
    e->live = 0;
    e->ptr = NULL;
    e->parent = 0;
    e->type = 0;
    e->gen = e->gen == 0xFFFFu ? 1u : (uint16_t)(e->gen + 1u);
    e->free_next = s->free_head;
    s->free_head = idx;
    s->free_count++;
    s->live_count--;
}

hqqrpc_status hqqrpc_handle_put(hqqrpc_session *s, uint16_t type, void *ptr, uint32_t parent_id, uint32_t *id) {
    hqqrpc_cstate *cs = &s->cs;
    hqqrpc_hentry *e;
    uint32_t idx;

    if (cs->hreserved == 0 || type == 0 || type > s->iface->handle_type_count || ptr == NULL || id == NULL) {
        return HQQRPC_E_INTERNAL;
    }
    if (parent_id != 0 && tab_lookup(s, parent_id) == NULL) {
        return HQQRPC_E_INTERNAL;
    }
    if (cs->nreg == cs->regcap) {
        unsigned ncap = cs->regcap != 0 ? cs->regcap * 2u : 4u;
        uint32_t *nr = (uint32_t *)hqq_alloc(s->alloc, (size_t)ncap * sizeof *nr);

        if (nr == NULL) {
            return HQQRPC_E_INTERNAL;
        }
        if (cs->reg != NULL) {
            memcpy(nr, cs->reg, (size_t)cs->nreg * sizeof *nr);
            hqq_free(s->alloc, cs->reg, (size_t)cs->regcap * sizeof *nr);
        }
        cs->reg = nr;
        cs->regcap = ncap;
    }
    if (s->free_head != 0) {
        idx = s->free_head;
        s->free_head = s->tab[idx].free_next;
        s->free_count--;
    } else {
        if (s->high_water >= s->cfg.max_handles || s->high_water + 1u >= s->tab_cap) {
            return HQQRPC_E_INTERNAL;
        }
        idx = ++s->high_water;
        s->tab[idx].gen = 1u;
    }
    e = &s->tab[idx];
    e->ptr = ptr;
    e->type = type;
    e->parent = parent_id;
    e->live = 1;
    e->free_next = 0;
    e->prev = s->live_tail;
    e->next = 0;
    if (s->live_tail != 0) {
        s->tab[s->live_tail].next = idx;
    } else {
        s->live_head = idx;
    }
    s->live_tail = idx;
    s->live_count++;
    cs->hreserved--;
    *id = ((uint32_t)e->gen << 16) | idx;
    cs->reg[cs->nreg++] = *id;
    return HQQRPC_OK;
}

void hqqrpc_handle_consume(hqqrpc_session *s, uint32_t id) {
    if (tab_lookup(s, id) != NULL) {
        entry_free(s, id & 0xFFFFu);
    }
}

/* Calls the destroy adapter of the entry and consumes it whatever the adapter returns. The caller
 * holds the execution lock. */
static void entry_destroy(hqqrpc_session *s, uint32_t idx) {
    hqqrpc_hentry *e = &s->tab[idx];
    uint16_t type = e->type;
    void *ptr = e->ptr;
    int (*destroy)(void *) = s->iface->handle_types[type - 1u].destroy;
    int failed = 0;

    if (destroy != NULL) {
        failed = destroy(ptr) != 0;
    }
    entry_free(s, idx);
    if (failed) {
        s->destroy_failures++;
        hqqrpc_log(s, HQQRPC_LOG_WARN, "destroy of a %s handle failed; the object may leak",
                   s->iface->handle_types[type - 1u].name != NULL ? s->iface->handle_types[type - 1u].name : "?");
    }
}

/* 1 if the chain of parents of e reaches root_id. The walk is bounded by the number of types: a
 * parent link is stale only through a glue that skipped the cascade, never a cycle of live ids. */
static int is_descendant(hqqrpc_session *s, const hqqrpc_hentry *e, uint32_t root_id) {
    uint32_t pid = e->parent;
    unsigned depth;

    for (depth = 0; pid != 0 && depth <= s->iface->handle_type_count; depth++) {
        const hqqrpc_hentry *pe;

        if (pid == root_id) {
            return 1;
        }
        pe = tab_lookup(s, pid);
        if (pe == NULL) {
            return 0;
        }
        pid = pe->parent;
    }
    return 0;
}

void hqqrpc_handle_destroy_children(hqqrpc_session *s, uint32_t id) {
    uint32_t i;

    if (tab_lookup(s, id) == NULL) {
        return;
    }
    i = s->live_tail;
    while (i != 0) {
        uint32_t prev = s->tab[i].prev;

        if (is_descendant(s, &s->tab[i], id)) {
            entry_destroy(s, i);
        }
        i = prev;
    }
}

void hqqrpc_handles_destroy_all(hqqrpc_session *s) {
    while (s->live_tail != 0) {
        hqqrpc_exec_begin(s);
        entry_destroy(s, s->live_tail);
        hqqrpc_exec_end(s);
    }
}

void hqqrpc_call_handles_end(hqqrpc_session *s, int rollback) {
    hqqrpc_cstate *cs = &s->cs;

    if (rollback) {
        while (cs->nreg > 0) {
            uint32_t id = cs->reg[--cs->nreg];

            if (tab_lookup(s, id) != NULL) {
                hqqrpc_exec_begin(s);
                entry_destroy(s, id & 0xFFFFu);
                hqqrpc_exec_end(s);
            }
        }
    }
    cs->nreg = 0;
    cs->hreserved = 0;
}
