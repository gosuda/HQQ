/*
 * fuzz_rpc_frame: libFuzzer target (BINDGEN.md 10.2). The input is a sequence of frames fed to the
 * server session state machine over the in-memory transport (hqqrpc_test_exchange) with the
 * microlib glue. After a valid HELLO, each frame is a 16-bit length (modulo the slot size) followed
 * by that many bytes. The first input byte selects the geometry (S = 128 or 16384).
 *
 * Properties (abort on violation): every outcome is a response, a protocol-violation teardown or a
 * session end; a protocol violation answers the E_PROTOCOL final frame; the published frames are
 * well formed; a request rejected before the call does not enter the library; the handle table
 * stays within max_handles; every library object is destroyed when the session is freed.
 */
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

#include "rpc_fuzz_driver.h"
#include "rpc_ml.h"
#include "rpc_ml_server.h"
#include "rpc_test_util.h"

#define PROP(cond)                                                              \
    do {                                                                        \
        if (!(cond)) {                                                          \
            fprintf(stderr, "%s:%d: property failed: %s\n", __FILE__, __LINE__, #cond); \
            abort();                                                            \
        }                                                                       \
    } while (0)

#define MAX_OUT 300000u

static uint8_t g_out[MAX_OUT];

/* Checks one exchange's output; returns the number of complete messages. */
static unsigned check_out(const uint8_t *out, size_t n, uint32_t S, uint32_t *first_aux, unsigned *first_kind) {
    size_t off = 0;
    unsigned msgs = 0;
    uint32_t chunk = S - RTU_HDR;

    while (off < n) {
        const uint8_t *f = out + off;
        unsigned kind;
        uint32_t total;
        uint32_t aux;
        uint32_t id;
        uint32_t sent;
        uint32_t first;

        PROP(n - off >= RTU_HDR);
        kind = f[0];
        total = rtu_le32(f + 8);
        aux = rtu_le32(f + 12);
        id = rtu_le32(f + 4);
        PROP(kind == HQQRPC_K_RESPONSE || kind == HQQRPC_K_HELLO_REPLY || kind == HQQRPC_K_CLOSE_ACK);
        if (kind == HQQRPC_K_RESPONSE) {
            PROP((f[1] & ~3u) == 0 && ((f[1] & 1u) == 0 || (f[1] & 2u) != 0));
            PROP(id == 0 ? (aux == HQQRPC_E_PROTOCOL || aux == HQQRPC_E_SHUTTING_DOWN)
                         : (aux <= HQQRPC_E_INTERNAL || (aux >= HQQRPC_E_APP_MIN && aux <= HQQRPC_E_APP_MAX)));
            PROP(aux == 0 || (f[1] == 0 && total <= HQQRPC_ERRBODY_MAX));
        }
        PROP(total <= HQQRPC_HARD_MAX_MESSAGE);
        first = total < chunk ? total : chunk;
        PROP(n - off >= (size_t)RTU_HDR + first);
        if (msgs == 0) {
            *first_aux = aux;
            *first_kind = kind;
        }
        off += RTU_HDR + first;
        sent = first;
        while (sent < total) {
            uint32_t want = total - sent < chunk ? total - sent : chunk;
            const uint8_t *g = out + off;

            PROP(n - off >= (size_t)RTU_HDR + want);
            PROP(g[0] == HQQRPC_K_CONT && g[1] == 0 && rtu_le32(g + 4) == id && rtu_le32(g + 8) == total &&
                 rtu_le32(g + 12) == sent);
            off += RTU_HDR + want;
            sent += want;
        }
        msgs++;
    }
    return msgs;
}

int LLVMFuzzerTestOneInput(const uint8_t *data, size_t size) {
    hqqrpc_server_options o;
    hqqrpc_session *s;
    uint32_t S = size > 0 && (data[0] & 1u) ? 128u : 16384u;
    uint8_t hello[RTU_HDR + 64];
    size_t out_len = 0;
    size_t pos = 1;

    rtu_opts(&o, S, 64, 65536);
    o.max_handles = 8;
    s = hqqrpc_test_session_new(&microlib_iface, &o);
    PROP(s != NULL);
    ml_test_reset();
    PROP(hqqrpc_test_exchange(s, hello, rtu_hello(hello, &microlib_iface, S, 64, 0, 0), g_out, sizeof g_out,
                              &out_len) == HQQRPC_OK);
    PROP(out_len == RTU_HDR + 64 && rtu_le32(g_out + 12) == 0);

    while (pos + 2 <= size) {
        uint8_t frame[16384 + 64];
        size_t len = ((size_t)data[pos] | ((size_t)data[pos + 1] << 8)) % ((size_t)S + 1u);
        hqqrpc_status st;
        long entries = ml_test_entries();
        unsigned msgs;
        uint32_t aux = 0;
        unsigned kind = 0;

        pos += 2;
        if (len > size - pos) {
            len = size - pos;
        }
        memcpy(frame, data + pos, len);
        pos += len;
        st = hqqrpc_test_exchange(s, frame, len, g_out, sizeof g_out, &out_len);
        PROP(st == HQQRPC_OK || st == HQQRPC_E_PROTOCOL);
        msgs = check_out(g_out, out_len, S, &aux, &kind);
        PROP(msgs <= 1);
        if (st == HQQRPC_E_PROTOCOL) {
            PROP(out_len == 39 && rtu_le32(g_out + 12) == HQQRPC_E_PROTOCOL);
            PROP(hqqrpc_test_ended(s));
            break;
        }
        if (msgs == 1 && kind == HQQRPC_K_RESPONSE && aux >= HQQRPC_E_UNKNOWN_OP && aux <= HQQRPC_E_DENIED) {
            PROP(ml_test_entries() == entries); /* rejected before the call */
        }
        PROP(hqqrpc_test_live_handles(s) <= 8);
        if (hqqrpc_test_ended(s)) {
            break;
        }
    }
    hqqrpc_test_session_free(s);
    PROP(ml_test_live_objects() == 0);
    return 0;
}
