/*
 * hqq_interop.c - the C interop helper (conformance/INTEROP_CLI.md): command
 * line, catalogue (list), version, result line, barrier, process deadline,
 * deterministic payloads, error kinds, crash points and raw segment access.
 * The scenarios are tables in interop_std.c and interop_adv.c.
 */
#include "interop_internal.h"

#include <errno.h>
#include <inttypes.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "hqq_internal.h"

#if defined(_WIN32)
#  include <windows.h>
#else
#  include <signal.h>
#endif

#define IHX_NS_PER_MS INT64_C(1000000)
#define IHX_BARRIER_POLL_NS (2 * IHX_NS_PER_MS)
#define IHX_RAW_POLL_NS UINT64_C(200000)
#define IHX_PATH_MAX 1024
#define IHX_JSON_MAX 4096
#define IHX_RUN_NAME_MAX 30

/* ---- Small helpers ----------------------------------------------------------- */

static const char *role_str(const ihx_env *e) { return e->cfg.role == 0 ? "creator" : "opener"; }
static const char *peer_role_str(const ihx_env *e) { return e->cfg.role == 0 ? "opener" : "creator"; }

const char *ihx_role_name(const ihx_env *e) { return role_str(e); }
const char *ihx_peer_role_name(const ihx_env *e) { return peer_role_str(e); }

uint64_t ihx_now_ns(void) { return hqq_plat_mono_ns(); }

int64_t ihx_remaining_ns(const ihx_env *e) {
    uint64_t now = hqq_plat_mono_ns();
    if (now >= e->deadline_mono_ns) {
        return 0;
    }
    return (int64_t)(e->deadline_mono_ns - now);
}

void ihx_sleep_ns(ihx_env *e, uint64_t ns) {
    while (ns > 0) {
        int64_t rem = ihx_remaining_ns(e);
        uint64_t slice;
        if (rem <= 0) {
            return;
        }
        slice = ns < (uint64_t)rem ? ns : (uint64_t)rem;
        hqq_plat_sleep_ns(slice);
        ns -= slice;
    }
}

int ihx_name_is_valid(const char *s) {
    size_t i, n = strlen(s);
    if (n == 0 || n > IHX_RUN_NAME_MAX) {
        return 0;
    }
    for (i = 0; i < n; i++) {
        char c = s[i];
        int ok = (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-';
        if (!ok) {
            return 0;
        }
    }
    return 1;
}

const char *ihx_goos(void) {
#if defined(_WIN32)
    return "windows";
#elif defined(__APPLE__)
    return "darwin";
#elif defined(__linux__)
    return "linux";
#elif defined(__FreeBSD__)
    return "freebsd";
#else
    return "unknown";
#endif
}

int ihx_platform_ok(const char *platforms) {
    const char *p = platforms, *goos = ihx_goos();
    size_t glen = strlen(goos);
    if (p == NULL || *p == '\0') {
        return 1;
    }
    for (;;) {
        const char *end = strchr(p, ',');
        size_t n = end != NULL ? (size_t)(end - p) : strlen(p);
        if ((n == 3 && strncmp(p, "any", 3) == 0) || (n == glen && strncmp(p, goos, n) == 0)) {
            return 1;
        }
        if (end == NULL) {
            return 0;
        }
        p = end + 1;
    }
}

static void put_le64(unsigned char *p, uint64_t v) {
    unsigned i;
    for (i = 0; i < 8; i++) {
        p[i] = (unsigned char)(v >> (8 * i));
    }
}

static void put_le32(unsigned char *p, uint32_t v) {
    unsigned i;
    for (i = 0; i < 4; i++) {
        p[i] = (unsigned char)(v >> (8 * i));
    }
}

static uint64_t get_le64(const unsigned char *p) {
    uint64_t v = 0;
    unsigned i;
    for (i = 0; i < 8; i++) {
        v |= (uint64_t)p[i] << (8 * i);
    }
    return v;
}

static uint32_t get_le32(const unsigned char *p) {
    uint32_t v = 0;
    unsigned i;
    for (i = 0; i < 4; i++) {
        v |= (uint32_t)p[i] << (8 * i);
    }
    return v;
}

/* ---- JSON ---------------------------------------------------------------------- */

const char *ihx_json_escape(const char *in, char *out, size_t cap) {
    size_t o = 0;
    if (cap == 0) {
        return out;
    }
    for (; *in != '\0'; in++) {
        unsigned char c = (unsigned char)*in;
        char tmp[8];
        const char *rep = tmp;
        size_t n;
        switch (c) {
        case '"': rep = "\\\""; break;
        case '\\': rep = "\\\\"; break;
        case '\n': rep = "\\n"; break;
        case '\r': rep = "\\r"; break;
        case '\t': rep = "\\t"; break;
        default:
            if (c < 0x20) {
                snprintf(tmp, sizeof tmp, "\\u%04x", (unsigned)c);
            } else {
                tmp[0] = (char)c;
                tmp[1] = '\0';
            }
        }
        n = strlen(rep);
        if (o + n + 1 > cap) {
            break;
        }
        memcpy(out + o, rep, n);
        o += n;
    }
    out[o] = '\0';
    return out;
}

/* Writes s as a quoted, escaped JSON string. Strings longer than the
 * escape buffer are cut at a whole escape sequence (still valid JSON). */
static void put_jstr(FILE *f, const char *s) {
    char esc[IHX_JSON_MAX];
    fputc('"', f);
    fputs(ihx_json_escape(s != NULL ? s : "", esc, sizeof esc), f);
    fputc('"', f);
}

static const char *json_bool(int v) { return v ? "true" : "false"; }

/* ---- Result line -------------------------------------------------------------- */

/* Prints the result line (INTEROP_CLI.md, section 4). with_checks = 0 prints an
 * empty checks list: the watchdog does not read state the scenario thread owns. */
static void print_result(const ihx_env *e, int ok, int skipped, const char *kind, const char *err,
                         int with_checks) {
    FILE *f = stdout;
    int i;
    uint64_t elapsed_ms = (hqq_plat_mono_ns() - e->start_mono_ns) / 1000000u;

    fputs("{\"scenario\":", f);
    put_jstr(f, e->cfg.scenario);
    fputs(",\"variant\":", f);
    put_jstr(f, e->cfg.variant);
    fprintf(f, ",\"step\":%d", e->cfg.step);
    fputs(",\"role\":", f);
    put_jstr(f, role_str(e));
    fputs(",\"impl\":", f);
    put_jstr(f, e->cfg.impl);
    fputs(",\"peer_impl\":", f);
    put_jstr(f, e->cfg.peer_impl);
    fprintf(f, ",\"ok\":%s,\"skipped\":%s,\"crashed\":%s", json_bool(ok), json_bool(skipped),
            json_bool(e->crashed));
    fputs(",\"error_kind\":", f);
    put_jstr(f, kind != NULL ? kind : "");
    fputs(",\"error\":", f);
    put_jstr(f, err != NULL ? err : "");
    fputs(",\"checks\":[", f);
    for (i = 0; with_checks && i < e->nchecks; i++) {
        const ihx_check_rec *c = &e->checks[i];
        if (i > 0) {
            fputc(',', f);
        }
        fputs("{\"name\":", f);
        put_jstr(f, c->name);
        fprintf(f, ",\"ok\":%s,\"detail\":", json_bool(c->ok));
        put_jstr(f, c->detail);
        fputc('}', f);
    }
    fprintf(f, "],\"sent\":%" PRIu64 ",\"received\":%" PRIu64 ",\"elapsed_ms\":%" PRIu64, e->sent, e->received,
            elapsed_ms);
    if (e->meta_status_set || e->meta[0] != '\0') {
        fputs(",\"meta\":{", f);
        if (e->meta_status_set) {
            fprintf(f, "\"status\":%" PRIu64 "%s", e->meta_status, e->meta[0] != '\0' ? "," : "");
        }
        fputs(e->meta, f);
        fputc('}', f);
    }
    fputs("}\n", f);
    fflush(f);
}

/* Exactly one result line per process: the first claimant prints. The loser of
 * a race (the watchdog, when the scenario finishes at the deadline) parks: the
 * winner ends the process. */
static _Atomic int g_result_claimed;

static int claim_result(void) {
    int expected = 0;
    return atomic_compare_exchange_strong(&g_result_claimed, &expected, 1);
}

static void park_forever(void) {
    for (;;) {
        hqq_plat_sleep_ns(1000 * IHX_NS_PER_MS);
    }
}

static void kill_self(void) {
#if defined(_WIN32)
    TerminateProcess(GetCurrentProcess(), (UINT)IHX_EXIT_FAILED);
#else
    raise(SIGKILL);
#endif
    park_forever();
}

/* ---- Outcomes, errors and checks -------------------------------------------------- */

void ihx_check(ihx_env *e, const char *name, int ok, const char *fmt, ...) {
    ihx_check_rec *c;
    va_list ap;
    if (!ok) {
        e->failed = 1;
    }
    if (e->nchecks >= IHX_MAX_CHECKS) {
        return;
    }
    c = &e->checks[e->nchecks++];
    snprintf(c->name, sizeof c->name, "%s", name);
    c->ok = ok ? 1 : 0;
    va_start(ap, fmt);
    vsnprintf(c->detail, sizeof c->detail, fmt, ap);
    va_end(ap);
}

void ihx_check_status(ihx_env *e, const char *name, hqq_status got, hqq_status want) {
    int ok = want == HQQ_OK ? got == HQQ_OK : hqq_error_is(got, want) != 0;
    ihx_check(e, name, ok, "%s", hqq_strerror(got));
}

const char *ihx_error_kind_of(hqq_status st) {
    switch (st) {
    case HQQ_ERR_CLOSED: return "ErrClosed";
    case HQQ_ERR_PEER_CLOSED: return "ErrPeerClosed";
    case HQQ_ERR_PEER_GONE: return "ErrPeerGone";
    case HQQ_ERR_TIMEOUT: return "ErrTimeout";
    case HQQ_ERR_LINK_BUSY: return "ErrLinkBusy";
    case HQQ_ERR_INCOMPATIBLE_LAYOUT: return "ErrIncompatibleLayout";
    case HQQ_ERR_FAILED_INIT:
    case HQQ_ERR_SEGMENT_NOT_READY:
    case HQQ_ERR_INIT_CONTENDED:
    case HQQ_ERR_NO_LIVE_ENDPOINT:
    case HQQ_ERR_LISTENER_NOT_READY: return "ErrFailedInit";
    case HQQ_ERR_NOT_EXIST: return "ErrNotExist";
    case HQQ_ERR_INVALID_SIZE:
    case HQQ_ERR_GEOMETRY_MISMATCH: return "ErrInvalidSize";
    case HQQ_ERR_INVALID_NAME: return "ErrInvalidName";
    case HQQ_ERR_BUFFER_OVERFLOW:
    case HQQ_ERR_BUFFER_TOO_SMALL: return "ErrBufferOverflow";
    case HQQ_ERR_INVALID_OP: return "ErrInvalidOp";
    case HQQ_ERR_NOT_NEGOTIATED: return "ErrNotNegotiated";
    case HQQ_ERR_ALREADY_NEGOTIATED: return "ErrAlreadyNegotiated";
    case HQQ_ERR_NEGOTIATION_FAILED: return "ErrNegotiationFailed";
    case HQQ_ERR_NEGOTIATION_REJECTED: return "ErrNegotiationRejected";
    case HQQ_ERR_UNEXPECTED_PACKET: return "ErrUnexpectedPacket";
    case HQQ_ERR_PROTOCOL: return "ErrProtocol";
    case HQQ_ERR_MESSAGE_ABORTED: return "ErrMessageAborted";
    case HQQ_ERR_INCOMING_OVERFLOW: return "ErrIncomingOverflow";
    case HQQ_ERR_REASSEMBLY_LIMIT: return "ErrReassemblyLimit";
    case HQQ_ERR_CONN_NOT_FOUND: return "ErrConnNotFound";
    case HQQ_ERR_MEMORY_ALIGN: return "ErrMemoryAlign";
    case HQQ_ERR_MEMORY_SMALL: return "ErrMemorySmall";
    case HQQ_ERR_LISTENER_BUSY: return "ErrListenerBusy";
    case HQQ_ERR_LISTENER_GONE: return "ErrListenerGone";
    case HQQ_ERR_LISTENER_CLOSED: return "ErrListenerClosed";
    case HQQ_ERR_ACCEPTOR_CLOSED: return "ErrAcceptorClosed";
    case HQQ_ERR_NOT_LISTENING: return "ErrNotListening";
    case HQQ_ERR_ALREADY_LISTENING: return "ErrAlreadyListening";
    case HQQ_ERR_SHUTTING_DOWN: return "ErrShuttingDown";
    case HQQ_ERR_RESPONSE: return "ResponseError";
    default: return "Unknown";
    }
}

int ihx_fail_status(ihx_env *e, hqq_status st, const char *fmt, ...) {
    va_list ap;
    size_t n;
    va_start(ap, fmt);
    vsnprintf(e->error, sizeof e->error, fmt, ap);
    va_end(ap);
    n = strlen(e->error);
    snprintf(e->error + n, sizeof e->error - n, ": %s", hqq_strerror(st));
    e->error_kind = ihx_error_kind_of(st);
    return IHX_FAIL;
}

int ihx_fail(ihx_env *e, const char *kind, const char *fmt, ...) {
    va_list ap;
    va_start(ap, fmt);
    vsnprintf(e->error, sizeof e->error, fmt, ap);
    va_end(ap);
    e->error_kind = kind;
    return IHX_FAIL;
}

/* ---- Meta ---------------------------------------------------------------------------- */

/* Appends "key":value to the meta object. A pair that does not fit is dropped. */
static void meta_put(ihx_env *e, const char *key, const char *json_value) {
    char pair[512];
    size_t used = strlen(e->meta);
    int n = snprintf(pair, sizeof pair, "%s\"%s\":%s", used > 0 ? "," : "", key, json_value);
    if (n < 0 || (size_t)n >= sizeof e->meta - used) {
        return;
    }
    memcpy(e->meta + used, pair, (size_t)n + 1);
}

void ihx_meta_u64(ihx_env *e, const char *key, uint64_t v) {
    char num[32];
    snprintf(num, sizeof num, "%" PRIu64, v);
    meta_put(e, key, num);
}

void ihx_meta_bool(ihx_env *e, const char *key, int v) { meta_put(e, key, json_bool(v)); }

void ihx_meta_str(ihx_env *e, const char *key, const char *v) {
    char esc[512], quoted[576];
    ihx_json_escape(v, esc, sizeof esc);
    snprintf(quoted, sizeof quoted, "\"%s\"", esc);
    meta_put(e, key, quoted);
}

/* ---- Crash points ----------------------------------------------------------------------- */

int ihx_crash_is(const ihx_env *e, const char *point) {
    return e->cfg.crash_after[0] != '\0' && strcmp(e->cfg.crash_after, point) == 0;
}

void ihx_crash_point(ihx_env *e, const char *point) {
    if (!ihx_crash_is(e, point)) {
        return;
    }
    e->crashed = 1;
    if (!claim_result()) {
        park_forever();
    }
    print_result(e, !e->failed, 0, "", "", 1);
    kill_self();
}

/* ---- Barrier (INTEROP_CLI.md, section 6) ---------------------------------------------- */

int ihx_barrier(ihx_env *e, const char *phase) {
    char mine[IHX_PATH_MAX], peer[IHX_PATH_MAX];
    FILE *f;
    snprintf(mine, sizeof mine, "%s/s%d-%s.%s", e->cfg.sync_dir, e->cfg.step, phase, role_str(e));
    snprintf(peer, sizeof peer, "%s/s%d-%s.%s", e->cfg.sync_dir, e->cfg.step, phase, peer_role_str(e));
    f = fopen(mine, "wb");
    if (f == NULL) {
        return ihx_fail(e, "Unknown", "barrier %s: cannot create %s", phase, mine);
    }
    fclose(f);
    for (;;) {
        f = fopen(peer, "rb");
        if (f != NULL) {
            fclose(f);
            return IHX_OK;
        }
        if (ihx_remaining_ns(e) <= 0) {
            return ihx_fail_status(e, HQQ_ERR_TIMEOUT, "barrier %s: waiting for the peer", phase);
        }
        hqq_plat_sleep_ns(IHX_BARRIER_POLL_NS);
    }
}

/* ---- Deterministic payloads (INTEROP_CLI.md, section 5) --------------------------------------- */

#define IHX_GOLDEN_GAMMA UINT64_C(0x9E3779B97F4A7C15)
#define IHX_SEQ_MULT UINT64_C(0xD1B54A32D192ED03)

uint32_t ihx_crc32(const uint8_t *p, size_t n) {
    uint32_t c = 0xFFFFFFFFu;
    size_t i;
    unsigned k;
    for (i = 0; i < n; i++) {
        c ^= p[i];
        for (k = 0; k < 8; k++) {
            c = (c & 1u) ? (c >> 1) ^ 0xEDB88320u : c >> 1;
        }
    }
    return ~c;
}

static uint64_t splitmix_next(uint64_t *s) {
    uint64_t z;
    *s += IHX_GOLDEN_GAMMA;
    z = *s;
    z = (z ^ (z >> 30)) * UINT64_C(0xBF58476D1CE4E5B9);
    z = (z ^ (z >> 27)) * UINT64_C(0x94D049BB133111EB);
    return z ^ (z >> 31);
}

/* Writes the first n bytes of the body stream of (seed, seq). */
static void body_fill(uint64_t seed, uint64_t seq, unsigned char *out, size_t n) {
    uint64_t s = seed ^ (seq * IHX_SEQ_MULT);
    size_t i;
    for (i = 0; i < n; i += 8) {
        uint64_t w = splitmix_next(&s);
        size_t k;
        for (k = 0; k < 8 && i + k < n; k++) {
            out[i + k] = (unsigned char)(w >> (8 * k));
        }
    }
}

/* 1 when buf holds the first n bytes of the body stream of (seed, seq). */
static int body_matches(uint64_t seed, uint64_t seq, const unsigned char *buf, size_t n) {
    uint64_t s = seed ^ (seq * IHX_SEQ_MULT);
    size_t i;
    for (i = 0; i < n; i += 8) {
        uint64_t w = splitmix_next(&s);
        size_t k;
        for (k = 0; k < 8 && i + k < n; k++) {
            if (buf[i + k] != (unsigned char)(w >> (8 * k))) {
                return 0;
            }
        }
    }
    return 1;
}

void ihx_payload(uint64_t seed, uint64_t seq, uint8_t *out, size_t size) {
    if (size == 0) {
        return;
    }
    if (size < 16) {
        body_fill(seed, seq, out, size);
        return;
    }
    body_fill(seed, seq, out + 12, size - 12);
    put_le64(out, seq);
    put_le32(out + 8, ihx_crc32(out + 12, size - 12));
}

int ihx_payload_verify(uint64_t seed, uint64_t seq, const uint8_t *buf, size_t size) {
    if (size == 0) {
        return 0;
    }
    if (size < 16) {
        return body_matches(seed, seq, buf, size) ? 0 : 1;
    }
    if (get_le64(buf) != seq || get_le32(buf + 8) != ihx_crc32(buf + 12, size - 12)) {
        return 1;
    }
    return body_matches(seed, seq, buf + 12, size - 12) ? 0 : 1;
}

/* ---- Raw segment access (SPEC 3, 5, 7) ----------------------------------------------------- */

typedef struct raw_seg {
    hqq_shm *shm;
    unsigned char *mem;
    size_t total;
    hqq_layout lay;
} raw_seg;

struct ihx_raw {
    raw_seg seg;
    hqq_plat_wait wait; /* wake context of the transmit ring (its ops_ctx) */
    hqq_ring ring;
    unsigned char *bufs; /* payload pool of the transmit ring: slot i owns buffer i */
    uint64_t buf_size;
};

/* Maps the named object with the layout of count x size, shared and read-write.
 * create: exclusive creation (zero-filled); otherwise the object must exist and
 * be at least as large as the layout (SPEC 11.3). */
static int seg_open(ihx_env *e, const char *name, uint64_t count, uint64_t size, int create, raw_seg *s) {
    int os_err = 0;
    hqq_status st;
    void *p = NULL;
    size_t page = (size_t)hqq_plat_page_size();
    if (!hqq_layout_compute(&s->lay, page, count, size, (uint64_t)SIZE_MAX)) {
        return ihx_fail(e, "ErrInvalidSize", "raw: geometry %" PRIu64 " x %" PRIu64 " is outside SPEC 2.2", count,
                        size);
    }
    s->total = (size_t)s->lay.total;
    st = hqq_plat_shm_open(NULL, name, s->total, create, 0600u, &s->shm, &os_err);
    if (st != HQQ_OK) {
        return ihx_fail_status(e, st, "raw: open %s", name);
    }
    if (!create) {
        int64_t cur = hqq_plat_shm_size(s->shm, &os_err);
        if (cur < (int64_t)s->total) {
            hqq_plat_shm_close(NULL, s->shm);
            return ihx_fail(e, "Unknown", "raw: %s has size %lld, want at least %zu", name, (long long)cur,
                            s->total);
        }
    }
    st = hqq_plat_shm_map(s->shm, s->total, &p, &os_err);
    if (st != HQQ_OK) {
        hqq_plat_shm_close(NULL, s->shm);
        return ihx_fail_status(e, st, "raw: map %s", name);
    }
    s->mem = (unsigned char *)p;
    return IHX_OK;
}

static void seg_close(raw_seg *s) {
    hqq_plat_shm_unmap(s->shm, s->mem, s->total);
    hqq_plat_shm_close(NULL, s->shm);
}

int ihx_raw_open(ihx_env *e, ihx_raw **out) {
    ihx_raw *r;
    int idx = e->cfg.role; /* 0: creator sends on ring 0, 1: opener on ring 1 */
    uint64_t ring_off, buf_off;
    *out = NULL;
    r = calloc(1, sizeof *r);
    if (r == NULL) {
        return ihx_fail(e, "Unknown", "raw: out of memory");
    }
    if (seg_open(e, e->cfg.name, e->cfg.buffer_count, e->cfg.buffer_size, 0, &r->seg) != IHX_OK) {
        free(r);
        return IHX_FAIL;
    }
    ring_off = idx == 0 ? r->seg.lay.ring0 : r->seg.lay.ring1;
    buf_off = idx == 0 ? r->seg.lay.buf0 : r->seg.lay.buf1;
    hqq_plat_wait_init(&r->wait, r->seg.shm, idx, 0);
    for (;;) {
        int rc = hqq_ring_attach(&r->ring, r->seg.mem + ring_off);
        if (rc == HQQ_RING_OK) {
            break;
        }
        if (rc == HQQ_RING_INCOMPATIBLE || ihx_remaining_ns(e) <= 0) {
            seg_close(&r->seg);
            free(r);
            return ihx_fail(e, "ErrIncompatibleLayout", "raw: ring %d header not usable", idx);
        }
        hqq_plat_sleep_ns(IHX_RAW_POLL_NS);
    }
    r->ring.ops = hqq_plat_ring_ops();
    r->ring.ops_ctx = &r->wait;
    r->bufs = r->seg.mem + buf_off;
    r->buf_size = e->cfg.buffer_size;
    *out = r;
    return IHX_OK;
}

int ihx_raw_put(ihx_env *e, ihx_raw *r, const uint64_t words[8], const void *payload, size_t len) {
    unsigned char pkt[HQQ_RING_PACKET_SIZE];
    unsigned i;
    if ((uint64_t)len > r->buf_size) {
        return ihx_fail(e, "ErrBufferOverflow", "raw: payload %zu bytes, buffer size %" PRIu64, len, r->buf_size);
    }
    for (i = 0; i < 8; i++) {
        put_le64(pkt + 8 * i, words[i]);
    }
    for (;;) {
        uint64_t pos = 0;
        if (hqq_ring_try_claim_producer(&r->ring, &pos)) {
            unsigned char *slot_buf = r->bufs + (size_t)(pos & r->ring.mask) * (size_t)r->buf_size;
            if (payload != NULL && len > 0) {
                memcpy(slot_buf, payload, len);
            }
            memcpy(hqq_ring_slot(&r->ring, pos)->packet, pkt, sizeof pkt);
            hqq_ring_publish(&r->ring, pos);
            return IHX_OK;
        }
        if (ihx_remaining_ns(e) <= 0) {
            return ihx_fail_status(e, HQQ_ERR_TIMEOUT, "raw: transmit ring full");
        }
        hqq_plat_sleep_ns(IHX_RAW_POLL_NS);
    }
}

void ihx_raw_close(ihx_raw *r) {
    if (r == NULL) {
        return;
    }
    seg_close(&r->seg);
    free(r);
}

int ihx_observe(ihx_env *e, const char *name, ihx_view *v) {
    int os_err = 0;
    hqq_shm *shm = NULL;
    void *p = NULL;
    size_t page = (size_t)hqq_plat_page_size();
    hqq_superblock *sb;
    hqq_status st = hqq_plat_shm_open(NULL, name, 0, 0, 0, &shm, &os_err);
    int64_t cur;
    if (st != HQQ_OK) {
        return ihx_fail_status(e, st, "raw: open %s", name);
    }
    cur = hqq_plat_shm_size(shm, &os_err);
    if (cur < (int64_t)page) {
        hqq_plat_shm_close(NULL, shm);
        return ihx_fail(e, "Unknown", "raw: %s is not sized yet (%lld bytes)", name, (long long)cur);
    }
    st = hqq_plat_shm_map(shm, page, &p, &os_err);
    if (st != HQQ_OK) {
        hqq_plat_shm_close(NULL, shm);
        return ihx_fail_status(e, st, "raw: map %s", name);
    }
    sb = (hqq_superblock *)p;
    v->magic = atomic_load(&sb->magic);
    v->state = atomic_load(&sb->state);
    v->major = atomic_load(&sb->major);
    v->epoch = atomic_load(&sb->epoch);
    v->init_token = atomic_load(&sb->init_token);
    v->creator_token = atomic_load(&sb->creator_token);
    v->page_crc = ihx_crc32((const uint8_t *)p, page);
    hqq_plat_shm_unmap(shm, p, page);
    hqq_plat_shm_close(NULL, shm);
    return IHX_OK;
}

/* Raw initializer (SPEC 5.1 to 5.3 and 7.2, by hand). Each step checks the
 * ownership of the initialization token as the library does, so a takeover by
 * a surviving endpoint is detected in the same places. */
static int sb_owns(hqq_superblock *sb, uint64_t token) {
    return atomic_load(&sb->init_token) == token;
}

static void sb_progress(hqq_superblock *sb, uint64_t token) {
    if (sb_owns(sb, token)) {
        atomic_store(&sb->init_heartbeat, hqq_plat_wall_ns());
    }
}

static void sb_stamp_layout(hqq_superblock *sb) {
    atomic_store(&sb->major, HQQ_SB_MAJOR);
    atomic_store(&sb->minor, HQQ_SB_MINOR);
    atomic_store(&sb->required_features, 0);
}

int ihx_raw_init_victim(ihx_env *e) {
    const char *pt = e->cfg.crash_after;
    raw_seg seg;
    hqq_superblock *sb;
    uint64_t token = 0, creator_token = 0, pool_bytes, pools[2];
    int i, first = 1;
    if (strcmp(pt, "claim") != 0 && strcmp(pt, "format") != 0 && strcmp(pt, "publish") != 0 &&
        strcmp(pt, "zeroing") != 0) {
        snprintf(e->error, sizeof e->error, "raw initializer does not implement crash point \"%s\"", pt);
        return IHX_SKIP;
    }
    if (seg_open(e, e->cfg.name, e->cfg.buffer_count, e->cfg.buffer_size, 1, &seg) != IHX_OK) {
        return IHX_FAIL;
    }
    sb = (hqq_superblock *)seg.mem;
    if (hqq_new_token(&token) != 0 || hqq_new_token(&creator_token) != 0) {
        seg_close(&seg);
        return ihx_fail(e, "Unknown", "raw: no random token");
    }
    /* SPEC 5.1: Empty -> Initializing, then the claim (initToken). */
    {
        uint64_t expect = HQQ_STATE_EMPTY;
        if (!atomic_compare_exchange_strong(&sb->state, &expect, HQQ_STATE_INITIALIZING)) {
            seg_close(&seg);
            return ihx_fail(e, "ErrFailedInit", "raw: segment was not empty");
        }
    }
    /* SPEC 5.2 beginInit. */
    sb_stamp_layout(sb);
    {
        uint64_t expect = 0;
        if (!atomic_compare_exchange_strong(&sb->init_token, &expect, token)) {
            seg_close(&seg);
            return ihx_fail(e, "ErrFailedInit", "raw: lost initToken at claim");
        }
    }
    atomic_store(&sb->creator_token, creator_token);
    atomic_store(&sb->init_heartbeat, hqq_plat_wall_ns());
    ihx_crash_point(e, "claim");

    /* SPEC 5.3 step 1: format both rings (SPEC 7.2). */
    for (i = 0; i < 2; i++) {
        uint64_t off = i == 0 ? seg.lay.ring0 : seg.lay.ring1;
        if (hqq_ring_format(seg.mem + off, e->cfg.buffer_count, NULL, NULL) != 0) {
            seg_close(&seg);
            return ihx_fail(e, "ErrInvalidSize", "raw: ring format rejected %" PRIu64 " slots", e->cfg.buffer_count);
        }
        sb_progress(sb, token);
    }
    ihx_crash_point(e, "format");

    /* SPEC 5.3 step 2: zero both payload pools in chunks of at most 8 MiB. */
    pools[0] = seg.lay.buf0;
    pools[1] = seg.lay.buf1;
    pool_bytes = e->cfg.buffer_count * e->cfg.buffer_size;
    for (i = 0; i < 2; i++) {
        uint64_t off;
        for (off = 0; off < pool_bytes; off += HQQ_ZERO_CHUNK_BYTES) {
            uint64_t n = pool_bytes - off < HQQ_ZERO_CHUNK_BYTES ? pool_bytes - off : HQQ_ZERO_CHUNK_BYTES;
            if (!sb_owns(sb, token)) {
                seg_close(&seg);
                return ihx_fail(e, "ErrFailedInit", "raw: lost initToken while zeroing");
            }
            memset(seg.mem + pools[i] + off, 0, (size_t)n);
            sb_progress(sb, token);
            if (first) {
                first = 0;
                ihx_crash_point(e, "zeroing");
            }
        }
    }

    /* SPEC 5.3 step 3 (P1-P5): layout, geometry, records, magic last. */
    if (!sb_owns(sb, token)) {
        seg_close(&seg);
        return ihx_fail(e, "ErrFailedInit", "raw: lost initToken before publish");
    }
    sb_stamp_layout(sb);
    atomic_store(&sb->page_size, (uint64_t)hqq_plat_page_size());
    atomic_store(&sb->buffer_count, e->cfg.buffer_count);
    atomic_store(&sb->buffer_size, e->cfg.buffer_size);
    atomic_store(&sb->layout_size, (uint64_t)seg.total);
    atomic_store(&sb->endpoints[HQQ_ENDPOINT_SECONDARY].token, 0);
    atomic_store(&sb->endpoints[HQQ_ENDPOINT_SECONDARY].pid, 0);
    atomic_store(&sb->endpoints[HQQ_ENDPOINT_SECONDARY].heartbeat, 0);
    atomic_store(&sb->endpoints[HQQ_ENDPOINT_SECONDARY].interval, 0);
    atomic_store(&sb->endpoints[HQQ_ENDPOINT_PRIMARY].token, token);
    atomic_store(&sb->endpoints[HQQ_ENDPOINT_PRIMARY].pid, hqq_plat_pid());
    atomic_store(&sb->endpoints[HQQ_ENDPOINT_PRIMARY].interval, (uint64_t)HQQ_DEFAULT_HEARTBEAT_INTERVAL_NS);
    atomic_store(&sb->endpoints[HQQ_ENDPOINT_PRIMARY].heartbeat, hqq_plat_wall_ns());
    atomic_store(&sb->magic, HQQ_SB_MAGIC);
    ihx_crash_point(e, "publish");

    /* SPEC 5.3 commit: the initToken CAS, then epoch and Ready. */
    {
        uint64_t expect = token;
        if (!atomic_compare_exchange_strong(&sb->init_token, &expect, 0)) {
            seg_close(&seg);
            return ihx_fail(e, "ErrFailedInit", "raw: lost initToken at commit");
        }
    }
    atomic_fetch_add(&sb->epoch, 1);
    atomic_store(&sb->state, HQQ_STATE_READY);
    seg_close(&seg);
    return IHX_OK;
}

/* ---- Catalogue (list) and version ----------------------------------------------------------- */

static size_t table_len(const ihx_scenario *t) {
    size_t n = 0;
    while (t[n].id != NULL) {
        n++;
    }
    return n;
}

static const ihx_scenario *find_scenario(const char *id) {
    const ihx_scenario *tables[2] = {ihx_std_scenarios, ihx_adv_scenarios};
    int t;
    for (t = 0; t < 2; t++) {
        size_t i;
        for (i = 0; tables[t][i].id != NULL; i++) {
            if (strcmp(tables[t][i].id, id) == 0) {
                return &tables[t][i];
            }
        }
    }
    return NULL;
}

static const ihx_variant *find_variant(const ihx_scenario *s, const char *name) {
    int i;
    if (name[0] == '\0') {
        return &s->variants[0];
    }
    for (i = 0; i < s->nvariants; i++) {
        if (strcmp(s->variants[i].name, name) == 0) {
            return &s->variants[i];
        }
    }
    return NULL;
}

/* Writes a comma-separated string as a JSON array of strings. */
static void put_csv_array(FILE *f, const char *csv) {
    const char *p = csv;
    int first = 1;
    fputc('[', f);
    for (;;) {
        const char *end = strchr(p, ',');
        size_t n = end != NULL ? (size_t)(end - p) : strlen(p);
        char item[64];
        if (n > 0 && n < sizeof item) {
            memcpy(item, p, n);
            item[n] = '\0';
            if (!first) {
                fputc(',', f);
            }
            put_jstr(f, item);
            first = 0;
        }
        if (end == NULL) {
            break;
        }
        p = end + 1;
    }
    fputc(']', f);
}

/* Writes a space-separated argument string as a JSON array of tokens. */
static void put_args_array(FILE *f, const char *s) {
    char tok[128];
    size_t n = 0;
    int first = 1;
    fputc('[', f);
    for (;; s++) {
        if (*s == ' ' || *s == '\0') {
            if (n > 0) {
                tok[n] = '\0';
                if (!first) {
                    fputc(',', f);
                }
                put_jstr(f, tok);
                first = 0;
                n = 0;
            }
            if (*s == '\0') {
                break;
            }
        } else if (n < sizeof tok - 1) {
            tok[n++] = *s;
        }
    }
    fputc(']', f);
}

static void list_params(FILE *f, const ihx_variant *v) {
    int first = 1;
#define IHX_PARAM_U(field, key)                                                            \
    do {                                                                                   \
        if (v->field != 0) {                                                               \
            fprintf(f, "%s\"%s\":%" PRIu64, first ? "" : ",", key, (uint64_t)v->field);    \
            first = 0;                                                                     \
        }                                                                                  \
    } while (0)
#define IHX_PARAM_I(field, key)                                                            \
    do {                                                                                   \
        if (v->field != 0) {                                                               \
            fprintf(f, "%s\"%s\":%" PRId64, first ? "" : ",", key, (int64_t)v->field);     \
            first = 0;                                                                     \
        }                                                                                  \
    } while (0)
    IHX_PARAM_U(buffer_count, "buffer_count");
    IHX_PARAM_U(buffer_size, "buffer_size");
    IHX_PARAM_U(size, "size");
    IHX_PARAM_U(messages, "messages");
    IHX_PARAM_U(seed, "seed");
    IHX_PARAM_I(heartbeat_ms, "heartbeat_ms");
    IHX_PARAM_I(peer_timeout_ms, "peer_timeout_ms");
    IHX_PARAM_I(attach_timeout_ms, "attach_timeout_ms");
    IHX_PARAM_U(max_message, "max_message");
    IHX_PARAM_U(max_reassembly, "max_reassembly");
    IHX_PARAM_U(queue_size, "queue_size");
#undef IHX_PARAM_U
#undef IHX_PARAM_I
}

static void list_variant(FILE *f, const ihx_variant *v) {
    int i, j;
    fputs("{\"name\":", f);
    put_jstr(f, v->name);
    fputs(",\"params\":{", f);
    list_params(f, v);
    fputs("},\"steps\":[", f);
    for (i = 0; i < v->nsteps; i++) {
        const ihx_step *st = &v->steps[i];
        int any_args = 0;
        if (i > 0) {
            fputc(',', f);
        }
        fputs("{\"roles\":[", f);
        for (j = 0; j < st->nroles; j++) {
            if (j > 0) {
                fputc(',', f);
            }
            put_jstr(f, st->roles[j]);
        }
        fputs("],\"expect\":{", f);
        for (j = 0; j < st->nroles; j++) {
            if (j > 0) {
                fputc(',', f);
            }
            put_jstr(f, st->roles[j]);
            fputc(':', f);
            put_jstr(f, st->expect[j]);
        }
        fputc('}', f);
        for (j = 0; j < st->nroles; j++) {
            any_args = any_args || st->args[j] != NULL;
        }
        if (any_args) {
            int first = 1;
            fputs(",\"args\":{", f);
            for (j = 0; j < st->nroles; j++) {
                if (st->args[j] == NULL) {
                    continue;
                }
                if (!first) {
                    fputc(',', f);
                }
                put_jstr(f, st->roles[j]);
                fputc(':', f);
                put_args_array(f, st->args[j]);
                first = 0;
            }
            fputc('}', f);
        }
        fputc('}', f);
    }
    fputs("]}", f);
}

static void list_scenario(FILE *f, const ihx_scenario *s) {
    int i;
    fputs("{\"id\":", f);
    put_jstr(f, s->id);
    fputs(",\"title\":", f);
    put_jstr(f, s->title);
    fputs(",\"kind\":", f);
    put_jstr(f, s->kind);
    fputs(",\"platforms\":", f);
    put_csv_array(f, s->platforms);
    fprintf(f, ",\"timeout_s\":%d,\"variants\":[", s->timeout_s > 0 ? s->timeout_s : 30);
    for (i = 0; i < s->nvariants; i++) {
        if (i > 0) {
            fputc(',', f);
        }
        list_variant(f, &s->variants[i]);
    }
    fputs("]}", f);
}

static int scenario_cmp(const void *a, const void *b) {
    const ihx_scenario *const *x = a;
    const ihx_scenario *const *y = b;
    return strcmp((*x)->id, (*y)->id);
}

/* Parses "--impl LABEL" / "--impl=LABEL" of list and version. */
static int parse_impl_flag(int argc, char **argv, const char **impl) {
    int i;
    *impl = "c";
    for (i = 0; i < argc; i++) {
        const char *a = argv[i];
        if (strncmp(a, "--impl", 6) != 0) {
            return 0;
        }
        if (a[6] == '=') {
            *impl = a + 7;
        } else if (a[6] == '\0' && i + 1 < argc) {
            *impl = argv[++i];
        } else {
            return 0;
        }
    }
    return 1;
}

static int cmd_list(int argc, char **argv) {
    const char *impl;
    size_t n_std, n_adv, n = 0, i;
    const ihx_scenario **all;
    FILE *f = stdout;
    if (!parse_impl_flag(argc, argv, &impl)) {
        fprintf(stderr, "hqq-interop list: usage: list [--impl LABEL]\n");
        return IHX_EXIT_USAGE;
    }
    n_std = table_len(ihx_std_scenarios);
    n_adv = table_len(ihx_adv_scenarios);
    all = malloc((n_std + n_adv + 1) * sizeof *all);
    if (all == NULL) {
        return IHX_EXIT_INTERNAL;
    }
    for (i = 0; i < n_std; i++) {
        all[n++] = &ihx_std_scenarios[i];
    }
    for (i = 0; i < n_adv; i++) {
        all[n++] = &ihx_adv_scenarios[i];
    }
    qsort(all, n, sizeof *all, scenario_cmp);
    fputs("{\"helper\":\"hqq-interop\",\"impl\":", f);
    put_jstr(f, impl);
    fputs(",\"protocol\":\"1.1\",\"layout\":\"HQQ_STL3\",\"runtime\":\"c11\",\"scenarios\":[", f);
    for (i = 0; i < n; i++) {
        if (i > 0) {
            fputc(',', f);
        }
        list_scenario(f, all[i]);
    }
    fputs("]}\n", f);
    free((void *)all);
    return IHX_EXIT_OK;
}

static int cmd_version(int argc, char **argv) {
    const char *impl;
    if (!parse_impl_flag(argc, argv, &impl)) {
        fprintf(stderr, "hqq-interop version: usage: version [--impl LABEL]\n");
        return IHX_EXIT_USAGE;
    }
    fputs("{\"helper\":\"hqq-interop\",\"impl\":", stdout);
    put_jstr(stdout, impl);
    fprintf(stdout, ",\"protocol\":\"1.1\",\"segment_layout\":\"HQQ_STL3\",\"ring_layout\":%d}\n",
            (int)HQQ_RING_VERSION);
    return IHX_EXIT_OK;
}

/* ---- run: flags (INTEROP_CLI.md, section 2) -------------------------------------------------- */

/* Parameters the variant may set; a flag given on the command line wins. */
enum {
    EX_BUFFER_COUNT = 1u << 0,
    EX_BUFFER_SIZE = 1u << 1,
    EX_SIZE = 1u << 2,
    EX_MESSAGES = 1u << 3,
    EX_SEED = 1u << 4,
    EX_HEARTBEAT = 1u << 5,
    EX_PEER_TIMEOUT = 1u << 6,
    EX_ATTACH_TIMEOUT = 1u << 7,
    EX_MAX_MESSAGE = 1u << 8,
    EX_MAX_REASSEMBLY = 1u << 9,
    EX_QUEUE_SIZE = 1u << 10
};

typedef enum flag_id {
    F_SCENARIO,
    F_VARIANT,
    F_STEP,
    F_ROLE,
    F_NAME,
    F_SYNC_DIR,
    F_IMPL,
    F_PEER_IMPL,
    F_BUFFER_COUNT,
    F_BUFFER_SIZE,
    F_SIZE,
    F_MESSAGES,
    F_SEED,
    F_TIMEOUT,
    F_CRASH_AFTER,
    F_HEARTBEAT,
    F_PEER_TIMEOUT,
    F_ATTACH_TIMEOUT,
    F_WAIT,
    F_LONG,
    F_MAX_MESSAGE,
    F_MAX_REASSEMBLY,
    F_QUEUE_SIZE,
    F_COUNT
} flag_id;

typedef enum flag_kind { FK_STR, FK_INT, FK_U64, FK_DBL, FK_BOOL } flag_kind;

static const struct {
    const char *name;
    flag_kind kind;
    unsigned explicit_bit;
} run_flags[F_COUNT] = {
    {"scenario", FK_STR, 0},
    {"variant", FK_STR, 0},
    {"step", FK_INT, 0},
    {"role", FK_STR, 0},
    {"name", FK_STR, 0},
    {"sync-dir", FK_STR, 0},
    {"impl", FK_STR, 0},
    {"peer-impl", FK_STR, 0},
    {"buffer-count", FK_INT, EX_BUFFER_COUNT},
    {"buffer-size", FK_INT, EX_BUFFER_SIZE},
    {"size", FK_INT, EX_SIZE},
    {"messages", FK_INT, EX_MESSAGES},
    {"seed", FK_U64, EX_SEED},
    {"timeout", FK_DBL, 0},
    {"crash-after", FK_STR, 0},
    {"heartbeat", FK_INT, EX_HEARTBEAT},
    {"peer-timeout", FK_INT, EX_PEER_TIMEOUT},
    {"attach-timeout", FK_INT, EX_ATTACH_TIMEOUT},
    {"wait", FK_STR, 0},
    {"long", FK_BOOL, 0},
    {"max-message", FK_U64, EX_MAX_MESSAGE},
    {"max-reassembly", FK_U64, EX_MAX_REASSEMBLY},
    {"queue-size", FK_INT, EX_QUEUE_SIZE},
};

static int usage_error(const char *fmt, const char *arg) {
    fprintf(stderr, "hqq-interop: ");
    fprintf(stderr, fmt, arg);
    fprintf(stderr, "\n");
    return IHX_EXIT_USAGE;
}

static int parse_i64(const char *s, int64_t *out) {
    char *end = NULL;
    long long v;
    errno = 0;
    v = strtoll(s, &end, 10);
    if (s[0] == '\0' || *end != '\0' || errno == ERANGE) {
        return 0;
    }
    *out = (int64_t)v;
    return 1;
}

static int parse_u64(const char *s, uint64_t *out) {
    char *end = NULL;
    unsigned long long v;
    if (s[0] == '-' || s[0] == '\0') {
        return 0;
    }
    errno = 0;
    v = strtoull(s, &end, 10);
    if (*end != '\0' || errno == ERANGE) {
        return 0;
    }
    *out = (uint64_t)v;
    return 1;
}

static int parse_seconds(const char *s, int64_t *out_ns) {
    char *end = NULL;
    double v;
    if (s[0] == '\0') {
        return 0;
    }
    v = strtod(s, &end);
    if (*end != '\0' || !(v >= 0.0) || v > 1e9) {
        return 0;
    }
    *out_ns = (int64_t)(v * 1e9);
    return 1;
}

/* Applies one flag. Returns 0 on success, otherwise prints the usage error and returns -1. */
static int apply_flag(ihx_config *c, unsigned *ex, flag_id id, const char *val) {
    int64_t iv = 0;
    uint64_t uv = 0;
    switch (id) {
    case F_SCENARIO: c->scenario = val; return 0;
    case F_VARIANT: c->variant = val; return 0;
    case F_ROLE:
        if (strcmp(val, "creator") == 0 || strcmp(val, "a") == 0) {
            c->role = 0;
        } else if (strcmp(val, "opener") == 0 || strcmp(val, "b") == 0) {
            c->role = 1;
        } else {
            return usage_error("--role must be creator|opener|a|b, got %s", val);
        }
        return 0;
    case F_NAME: c->name = val; return 0;
    case F_SYNC_DIR: c->sync_dir = val; return 0;
    case F_IMPL: c->impl = val; return 0;
    case F_PEER_IMPL: c->peer_impl = val; return 0;
    case F_CRASH_AFTER: c->crash_after = val; return 0;
    case F_WAIT:
        if (strcmp(val, "adaptive") == 0) {
            c->spin = 0;
        } else if (strcmp(val, "spin") == 0) {
            c->spin = 1;
        } else {
            return usage_error("--wait must be adaptive|spin, got %s", val);
        }
        return 0;
    case F_TIMEOUT:
        if (!parse_seconds(val, &c->timeout_ns)) {
            return usage_error("--timeout must be a non-negative number of seconds, got %s", val);
        }
        return 0;
    case F_SEED:
        if (!parse_u64(val, &uv)) {
            return usage_error("--seed must be an unsigned integer, got %s", val);
        }
        c->seed = uv;
        *ex |= EX_SEED;
        return 0;
    case F_MAX_MESSAGE:
    case F_MAX_REASSEMBLY:
        if (!parse_u64(val, &uv)) {
            return usage_error("flag value must be an unsigned integer, got %s", val);
        }
        if (id == F_MAX_MESSAGE) {
            c->max_message = uv;
        } else {
            c->max_reassembly = uv;
        }
        *ex |= run_flags[id].explicit_bit;
        return 0;
    case F_LONG: c->long_sizes = 1; return 0;
    default:
        break;
    }
    if (!parse_i64(val, &iv)) {
        return usage_error("flag value must be an integer, got %s", val);
    }
    switch (id) {
    case F_STEP: c->step = (int)iv; break;
    case F_BUFFER_COUNT: c->buffer_count = (uint64_t)iv; break;
    case F_BUFFER_SIZE: c->buffer_size = (uint64_t)iv; break;
    case F_SIZE: c->size = (uint64_t)iv; break;
    case F_MESSAGES: c->messages = (uint64_t)iv; break;
    case F_HEARTBEAT: c->heartbeat_ns = iv * IHX_NS_PER_MS; break;
    case F_PEER_TIMEOUT: c->peer_timeout_ns = iv > 0 ? iv * IHX_NS_PER_MS : 0; break;
    case F_ATTACH_TIMEOUT: c->attach_timeout_ns = iv > 0 ? iv * IHX_NS_PER_MS : 0; break;
    case F_QUEUE_SIZE: c->queue_size = (uint64_t)iv; break;
    default: break;
    }
    *ex |= run_flags[id].explicit_bit;
    return 0;
}

/* Parses the run flags: "--name value" and "--name=value". Returns 0 or the usage code. */
static int parse_run(ihx_config *c, unsigned *ex, int argc, char **argv) {
    int i;
    for (i = 0; i < argc; i++) {
        const char *a = argv[i], *name, *eq;
        size_t nlen;
        flag_id id;
        const char *val = NULL;
        if (strncmp(a, "--", 2) != 0) {
            return usage_error("unexpected argument %s", a);
        }
        name = a + 2;
        eq = strchr(name, '=');
        nlen = eq != NULL ? (size_t)(eq - name) : strlen(name);
        for (id = 0; id < F_COUNT; id++) {
            if (strlen(run_flags[id].name) == nlen && strncmp(run_flags[id].name, name, nlen) == 0) {
                break;
            }
        }
        if (id == F_COUNT) {
            return usage_error("unknown flag %s", a);
        }
        if (run_flags[id].kind == FK_BOOL) {
            if (eq != NULL) {
                return usage_error("flag --%s takes no value", run_flags[id].name);
            }
            apply_flag(c, ex, id, "");
            continue;
        }
        if (eq != NULL) {
            val = eq + 1;
        } else if (i + 1 < argc) {
            val = argv[++i];
        } else {
            return usage_error("flag --%s needs a value", run_flags[id].name);
        }
        if (apply_flag(c, ex, id, val) != 0) {
            return IHX_EXIT_USAGE;
        }
    }
    return 0;
}

static void config_defaults(ihx_config *c) {
    memset(c, 0, sizeof *c);
    c->scenario = "";
    c->variant = "";
    c->step = 1;
    c->role = -1;
    c->name = "";
    c->sync_dir = "";
    c->impl = "c";
    c->peer_impl = "";
    c->buffer_count = 4;
    c->buffer_size = 256;
    c->messages = 100;
    c->seed = 1;
    c->timeout_ns = 20 * INT64_C(1000000000);
    c->crash_after = "";
}

/* Variant parameters fill the flags that were not given explicitly. */
static void apply_params(ihx_config *c, unsigned ex, const ihx_variant *v) {
    if (!(ex & EX_BUFFER_COUNT) && v->buffer_count != 0) {
        c->buffer_count = v->buffer_count;
    }
    if (!(ex & EX_BUFFER_SIZE) && v->buffer_size != 0) {
        c->buffer_size = v->buffer_size;
    }
    if (!(ex & EX_SIZE) && v->size != 0) {
        c->size = v->size;
    }
    if (!(ex & EX_MESSAGES) && v->messages != 0) {
        c->messages = v->messages;
    }
    if (!(ex & EX_SEED) && v->seed != 0) {
        c->seed = v->seed;
    }
    if (!(ex & EX_HEARTBEAT) && v->heartbeat_ms != 0) {
        c->heartbeat_ns = v->heartbeat_ms * IHX_NS_PER_MS;
    }
    if (!(ex & EX_PEER_TIMEOUT) && v->peer_timeout_ms != 0) {
        c->peer_timeout_ns = v->peer_timeout_ms * IHX_NS_PER_MS;
    }
    if (!(ex & EX_ATTACH_TIMEOUT) && v->attach_timeout_ms != 0) {
        c->attach_timeout_ns = v->attach_timeout_ms * IHX_NS_PER_MS;
    }
    if (!(ex & EX_MAX_MESSAGE) && v->max_message != 0) {
        c->max_message = v->max_message;
    }
    if (!(ex & EX_MAX_REASSEMBLY) && v->max_reassembly != 0) {
        c->max_reassembly = v->max_reassembly;
    }
    if (!(ex & EX_QUEUE_SIZE) && v->queue_size != 0) {
        c->queue_size = v->queue_size;
    }
}

/* ---- run: watchdog and driver ------------------------------------------------------------------ */

/* The process deadline (--timeout): prints HarnessTimeout and exits 3 when it passes. */
static void watchdog_main(void *arg) {
    ihx_env *e = arg;
    int64_t rem;
    while ((rem = ihx_remaining_ns(e)) > 0) {
        hqq_plat_sleep_ns((uint64_t)rem);
    }
    if (!claim_result()) {
        return;
    }
    print_result(e, 0, 0, "HarnessTimeout", "process exceeded its deadline", 0);
    exit(IHX_EXIT_TIMEOUT);
}

static int role_in_step(const ihx_step *st, const char *role) {
    int j;
    for (j = 0; j < st->nroles; j++) {
        if (strcmp(st->roles[j], role) == 0) {
            return 1;
        }
    }
    return 0;
}

static void env_init(ihx_env *e) {
    memset(e, 0, sizeof *e);
    e->error_kind = "";
}

/* Prints the final result line and returns the exit code of the role. */
static int finish(ihx_env *e, int rc) {
    const char *kind = "";
    const char *err = e->error;
    int ok = 0, skipped = 0;
    if (rc == IHX_OK && !e->failed) {
        ok = 1;
    } else if (rc == IHX_SKIP) {
        skipped = 1;
        ok = 1;
        if (err[0] == '\0') {
            err = "scenario not applicable here";
        }
    } else {
        if (rc == IHX_OK) {
            kind = "CheckFailed";
            err = "one or more checks failed";
        } else {
            kind = e->error_kind[0] != '\0' ? e->error_kind : (e->failed ? "CheckFailed" : "Unknown");
        }
    }
    if (!claim_result()) {
        park_forever();
    }
    print_result(e, ok, skipped, kind, err, 1);
    if (skipped) {
        return IHX_EXIT_SKIPPED;
    }
    return ok ? IHX_EXIT_OK : IHX_EXIT_FAILED;
}

static int cmd_run(int argc, char **argv) {
    ihx_env env;
    unsigned ex = 0;
    const ihx_scenario *s;
    const ihx_variant *v;
    const ihx_step *st;
    int rc, os_err = 0;
    uint64_t now;
    env_init(&env);
    config_defaults(&env.cfg);
    if (parse_run(&env.cfg, &ex, argc, argv) != 0) {
        return IHX_EXIT_USAGE;
    }
    if (env.cfg.scenario[0] == '\0') {
        return usage_error("%s", "--scenario is required");
    }
    if (env.cfg.role < 0) {
        return usage_error("%s", "--role is required (creator|opener|a|b)");
    }
    if (env.cfg.name[0] == '\0') {
        return usage_error("%s", "--name is required");
    }
    if (env.cfg.step < 1) {
        return usage_error("%s", "--step must be >= 1");
    }
    if (env.cfg.buffer_count == 0 || env.cfg.buffer_size == 0) {
        return usage_error("%s", "sizes and counts must be positive");
    }
    if (!ihx_name_is_valid(env.cfg.name)) {
        return usage_error("--name must be 1 to 30 bytes of [A-Za-z0-9_-], got %s", env.cfg.name);
    }
    s = find_scenario(env.cfg.scenario);
    if (s == NULL) {
        return usage_error("unknown scenario %s", env.cfg.scenario);
    }
    v = find_variant(s, env.cfg.variant);
    if (v == NULL) {
        return usage_error("scenario has no variant %s", env.cfg.variant);
    }
    if (env.cfg.step > v->nsteps) {
        fprintf(stderr, "hqq-interop: variant %s has %d steps\n", v->name, v->nsteps);
        return IHX_EXIT_USAGE;
    }
    st = &v->steps[env.cfg.step - 1];
    if (!role_in_step(st, env.cfg.role == 0 ? "creator" : "opener")) {
        return usage_error("role %s is not part of step", role_str(&env));
    }
    env.cfg.variant = v->name;
    apply_params(&env.cfg, ex, v);
    if (env.cfg.size == 0) {
        env.cfg.size = env.cfg.buffer_size;
    }

    now = hqq_plat_mono_ns();
    env.start_mono_ns = now;
    env.deadline_mono_ns = now + (uint64_t)env.cfg.timeout_ns;

    if (!ihx_platform_ok(s->platforms)) {
        snprintf(env.error, sizeof env.error, "scenario not applicable on %s", ihx_goos());
        return finish(&env, IHX_SKIP);
    }
    if (env.cfg.sync_dir[0] == '\0' && st->nroles > 1) {
        return usage_error("%s", "--sync-dir is required for multi-process steps");
    }
    if (hqq_plat_thread_start(NULL, watchdog_main, &env, &os_err) == NULL) {
        fprintf(stderr, "hqq-interop: cannot start the deadline watchdog (os error %d)\n", os_err);
        return IHX_EXIT_INTERNAL;
    }

    rc = s->run(&env);
    if (rc == IHX_OK && env.cfg.crash_after[0] != '\0' && !env.crashed) {
        /* The point is not reached by this role of this scenario: not a pass. */
        snprintf(env.error, sizeof env.error, "crash point \"%s\" is not reached by this role", env.cfg.crash_after);
        rc = IHX_SKIP;
    }
    return finish(&env, rc);
}

/* ---- Entry point ------------------------------------------------------------------------------------ */

static void usage(FILE *f) {
    fprintf(f, "usage:\n"
               "  hqq-interop run --scenario ID --role creator|opener|a|b --name NAME [flags]\n"
               "  hqq-interop list [--impl LABEL]\n"
               "  hqq-interop gen-vectors [--out DIR]\n"
               "  hqq-interop version [--impl LABEL]\n");
}

int main(int argc, char **argv) {
    if (argc < 2) {
        usage(stderr);
        return IHX_EXIT_USAGE;
    }
    if (strcmp(argv[1], "run") == 0) {
        return cmd_run(argc - 2, argv + 2);
    }
    if (strcmp(argv[1], "list") == 0) {
        return cmd_list(argc - 2, argv + 2);
    }
    if (strcmp(argv[1], "version") == 0) {
        return cmd_version(argc - 2, argv + 2);
    }
    if (strcmp(argv[1], "gen-vectors") == 0) {
        /* Optional for this helper (INTEROP_CLI.md, section 1): not implemented here. */
        fprintf(stderr, "hqq-interop: gen-vectors is not implemented by the C helper\n");
        return IHX_EXIT_SKIPPED;
    }
    if (strcmp(argv[1], "-h") == 0 || strcmp(argv[1], "--help") == 0 || strcmp(argv[1], "help") == 0) {
        usage(stdout);
        return IHX_EXIT_OK;
    }
    fprintf(stderr, "hqq-interop: unknown subcommand %s\n", argv[1]);
    usage(stderr);
    return IHX_EXIT_USAGE;
}
