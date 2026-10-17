/* Process entry of the zstd server: hqqrpc_main parses the command line, prints "ready <listener>"
 * and serves until SIGINT, SIGTERM or end of file on the lifeline (--parent-stdin).
 *
 * The defaults below are the geometry documented in README.md; every one of them can be
 * overridden on the command line (--buffer-size, --buffer-count, --max-message, ...). */
#include <stdio.h>

#include "hqq/hqq_rpc.h"
#include "zstd_server.h"

/* One line per session on stderr. on_session_end runs after the runtime destroyed every handle the
 * client left behind (BINDGEN.md 5.2, step 3), so "session ended" is also the signal that the
 * contexts of that session are freed. */
static void *session_start(void *user, hqqrpc_session *s) {
    (void)user;
    (void)s;
    fputs("zstd_server: session started\n", stderr);
    return NULL;
}

static void session_end(void *user, void *session_user, hqqrpc_session *s) {
    (void)user;
    (void)session_user;
    (void)s;
    fputs("zstd_server: session ended\n", stderr);
}

int main(int argc, char **argv) {
    hqqrpc_server_options o;

    hqqrpc_server_options_init(&o);
    o.on_session_start = session_start;
    o.on_session_end = session_end;
    /* 64 KiB frames: a call with a destination of at most 65,501 bytes has a one-frame response bound
     * (cap plus 19 bytes of framing, frame body 65,520 bytes), so the library writes its output
     * straight into the response slot and the server copies nothing. */
    o.default_buffer_size = 64u << 10;
    o.default_buffer_count = 64;
    /* 1 MiB frames at B = 64 need a 128 MiB segment, above the 64 MiB default of max_segment_bytes. */
    o.max_segment_bytes = 256u << 20;
    /* A 64 MiB buffer and its compressed form fit; the limit applies per message and per session. */
    o.max_message = 128u << 20;
    /* Multi-frame messages are assembled in per-session scratch; keep up to 8 MiB between calls so
     * that mid-size calls do not map and unmap memory on every call. */
    o.scratch_keep = 8u << 20;
    return hqqrpc_main(&zstd_iface, &o, argc, argv);
}
