/*
 * hqq_adv_echo - an Advanced-link echo server for measuring hqq_adv_serve.
 *
 *   hqq_adv_echo NAME [--count N] [--size B] [--wait adaptive|spin|simple]
 *                [--delay-us D] [--peer-timeout-ms T]
 *
 * Creates the named Advanced link as Primary, prints "ready" on stdout, negotiates
 * every feature, and serves: every request is answered with its own payload.
 * --delay-us makes the handler sleep D microseconds first. It returns when the peer
 * closes the link, printing the link statistics on stderr.
 *
 * Exit status: 0 served until the peer closed, 1 usage, 2 library error.
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#ifdef _WIN32
#  include <windows.h>
#else
#  include <time.h>
#endif

#include "hqq/hqq_advanced.h"

typedef struct echo_cfg {
    const char *name;
    unsigned long count;
    unsigned long size;
    hqq_wait_policy policy;
    unsigned long delay_us;
    unsigned long peer_timeout_ms;
} echo_cfg;

static void usage(void) {
    fprintf(stderr,
            "usage: hqq_adv_echo NAME [--count N] [--size B] [--wait adaptive|spin|simple]\n"
            "                    [--delay-us D] [--peer-timeout-ms T]\n");
}

static void sleep_us(unsigned long us) {
#ifdef _WIN32
    Sleep((DWORD)((us + 999u) / 1000u));
#else
    struct timespec ts;
    ts.tv_sec = (time_t)(us / 1000000u);
    ts.tv_nsec = (long)((us % 1000000u) * 1000u);
    while (nanosleep(&ts, &ts) != 0) {
    }
#endif
}

static int parse_ul(const char *s, unsigned long *out) {
    char *end = NULL;
    if (s == NULL || *s == '\0') {
        return 0;
    }
    *out = strtoul(s, &end, 10);
    return end != NULL && *end == '\0';
}

static int parse_args(int argc, char **argv, echo_cfg *c) {
    int i;
    memset(c, 0, sizeof *c);
    c->count = 1024;
    c->size = 256;
    c->policy = HQQ_WAIT_ADAPTIVE;
    c->peer_timeout_ms = 5000;
    if (argc < 2 || argv[1][0] == '-') {
        return 0;
    }
    c->name = argv[1];
    for (i = 2; i < argc; i++) {
        const char *a = argv[i];
        const char *next = i + 1 < argc ? argv[i + 1] : NULL;
        if (strcmp(a, "--count") == 0 && parse_ul(next, &c->count)) {
            i++;
        } else if (strcmp(a, "--size") == 0 && parse_ul(next, &c->size)) {
            i++;
        } else if (strcmp(a, "--delay-us") == 0 && parse_ul(next, &c->delay_us)) {
            i++;
        } else if (strcmp(a, "--peer-timeout-ms") == 0 && parse_ul(next, &c->peer_timeout_ms)) {
            i++;
        } else if (strcmp(a, "--wait") == 0 && next != NULL) {
            if (strcmp(next, "adaptive") == 0) {
                c->policy = HQQ_WAIT_ADAPTIVE;
            } else if (strcmp(next, "spin") == 0) {
                c->policy = HQQ_WAIT_SPIN;
            } else if (strcmp(next, "simple") == 0) {
                c->policy = HQQ_WAIT_SIMPLE;
            } else {
                return 0;
            }
            i++;
        } else {
            return 0;
        }
    }
    return c->count >= 2 && c->size >= 64;
}

static void echo_handler(void *user, hqq_req_ctx *ctx, hqq_response *w, const hqq_request *req) {
    const echo_cfg *c = (const echo_cfg *)user;
    (void)ctx;
    if (c->delay_us != 0) {
        sleep_us(c->delay_us);
    }
    (void)hqq_response_write(w, hqq_request_data(req), hqq_request_len(req));
}

int main(int argc, char **argv) {
    echo_cfg cfg;
    hqq_adv_options opts;
    hqq_adv *link = NULL;
    hqq_adv_stats stats;
    hqq_status st;

    if (!parse_args(argc, argv, &cfg)) {
        usage();
        return 1;
    }
    hqq_adv_options_init(&opts);
    opts.link.wait_policy = cfg.policy;
    opts.link.peer_timeout_ns = (int64_t)cfg.peer_timeout_ms * 1000000;
    st = hqq_adv_create(cfg.name, cfg.count, cfg.size, 0600, &opts, &link, NULL);
    if (st != HQQ_OK) {
        fprintf(stderr, "hqq_adv_echo: create: %s\n", hqq_strerror(st));
        return 2;
    }
    printf("ready\n");
    fflush(stdout);
    st = hqq_adv_negotiate(link, HQQ_FEATURES_ALL, NULL);
    if (st != HQQ_OK) {
        fprintf(stderr, "hqq_adv_echo: negotiate: %s\n", hqq_strerror(st));
        (void)hqq_adv_close(link);
        return 2;
    }
    st = hqq_adv_serve(link, echo_handler, &cfg, NULL);
    hqq_adv_stats_get(link, &stats);
    fprintf(stderr, "hqq_adv_echo: serve ended: %s; requests %llu responses %llu canceled %llu\n",
            hqq_strerror(st), (unsigned long long)stats.requests,
            (unsigned long long)stats.responses, (unsigned long long)stats.canceled_requests);
    (void)hqq_adv_close(link);
    return st == HQQ_OK || st == HQQ_ERR_PEER_CLOSED ? 0 : 2;
}
