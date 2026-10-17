/*
 * Test driver of the C emitter: runs frames through the generated glue of one service with the runtime's
 * test entry hqqrpc_test_exchange, without a link. Compile with -DIFACE=<service>_iface.
 *
 * stdin: one frame per line, in hexadecimal; the line "#last <n>" sets the id of the last request instead.
 * stdout: for each line "<status> <hex>", where status is the return of hqqrpc_test_exchange and hex the
 * frames it produced (the status of "#last" is 0); then "end live=<handles> failures=<n> tombstones=<n>".
 * argv[1], if given, is the buffer size of the session (default 16384).
 */
#define _POSIX_C_SOURCE 200809L
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "hqq/hqq_rpc.h"
#include "hqq_rpc_internal.h"

extern const hqqrpc_iface IFACE;

static int hexval(int c) {
    if (c >= '0' && c <= '9')
        return c - '0';
    if (c >= 'a' && c <= 'f')
        return c - 'a' + 10;
    if (c >= 'A' && c <= 'F')
        return c - 'A' + 10;
    return -1;
}

int main(int argc, char **argv) {
    enum { MAXF = 1 << 21 };
    hqqrpc_server_options opts;
    hqqrpc_session *s;
    uint8_t *in = malloc(MAXF), *out = malloc(MAXF);
    char *line = NULL;
    size_t cap = 0;
    ssize_t len;

    hqqrpc_server_options_init(&opts);
    if (argc > 1)
        opts.default_buffer_size = (uint32_t)strtoul(argv[1], NULL, 10);
    s = hqqrpc_test_session_new(&IFACE, &opts);
    if (s == NULL || in == NULL || out == NULL) {
        fprintf(stderr, "driver: setup failed\n");
        return 2;
    }
    while ((len = getline(&line, &cap, stdin)) > 0) {
        size_t n = 0, out_len = 0, i;
        hqqrpc_status st;

        if (strncmp(line, "#last ", 6) == 0) {
            hqqrpc_test_set_last_id(s, (uint32_t)strtoul(line + 6, NULL, 10));
            printf("0 \n");
            continue;
        }

        for (i = 0; i + 1 < (size_t)len; i += 2) {
            int hi = hexval(line[i]), lo = hexval(line[i + 1]);
            if (hi < 0 || lo < 0)
                break;
            in[n++] = (uint8_t)(hi << 4 | lo);
        }
        st = hqqrpc_test_exchange(s, in, n, out, MAXF, &out_len);
        printf("%u ", (unsigned)st);
        for (i = 0; i < out_len; i++)
            printf("%02x", out[i]);
        printf("\n");
    }
    printf("end live=%u failures=%u tombstones=%llu\n", (unsigned)hqqrpc_test_live_handles(s),
           (unsigned)hqqrpc_test_destroy_failures(s), (unsigned long long)hqqrpc_test_tombstones(s));
    hqqrpc_test_session_free(s);
    free(line);
    free(in);
    free(out);
    return 0;
}
