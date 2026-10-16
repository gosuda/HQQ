/*
 * hqq_echo - interop CLI for a named Standard link (libhqq).
 *
 *   hqq_echo (--create | --open) NAME [options]
 *
 *   --create            create the segment as Primary (creator)
 *   --open              attach to an existing segment as Secondary (opener)
 *   --client            send --messages messages and check every echo (default)
 *   --server            echo every message back until the END marker arrives
 *   --messages N        number of messages of a client (default 10)
 *   --size B            buffer size of the segment (default 4096)
 *   --count N           buffer count of the segment (default 8)
 *   --wait P            adaptive (default), spin or simple
 *   --heartbeat-ms H    heartbeat interval; 0 selects the default, < 0 disables
 *   --peer-timeout-ms T peer timeout (default 5000)
 *   --perm OCTAL        mode of a created segment (default 600)
 *   --delay-ms D        server: wait D ms before each echo (exercises parked waits)
 *   --darwin-ulock      macOS: wait and wake with __ulock_* instead of os_sync_*
 *
 * Message i of a client carries its sequence number (4 bytes, little endian)
 * followed by a deterministic pattern; every 10th message is exactly --size
 * bytes. The client ends the exchange with the 3-byte END marker and prints the
 * round-trip statistics. Exit status: 0 success, 1 usage, 2 library error,
 * 3 peer gone (no heartbeat), 4 peer closed before END, 5 echo mismatch.
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#ifdef _WIN32
#  include <windows.h>
#else
#  include <unistd.h>
#endif

#include "hqq/hqq.h"

#define END_MARKER "END"
#define END_LEN 3u
#define NS_PER_MS 1000000LL

enum { EXIT_OK = 0, EXIT_USAGE = 1, EXIT_LIB = 2, EXIT_GONE = 3, EXIT_CLOSED = 4, EXIT_MISMATCH = 5 };

typedef struct echo_cfg {
    const char *name;
    int create;
    int server;
    unsigned long messages;
    unsigned long size;
    unsigned long count;
    hqq_wait_policy policy;
    long long heartbeat_ms;
    long long peer_timeout_ms;
    unsigned perm;
    long long delay_ms;
    int darwin_ulock;
} echo_cfg;

static void usage(void) {
    fprintf(stderr,
            "usage: hqq_echo (--create | --open) NAME [--client | --server] [--messages N]\n"
            "                [--size B] [--count N] [--wait adaptive|spin|simple]\n"
            "                [--heartbeat-ms H] [--peer-timeout-ms T] [--perm OCTAL]\n"
            "                [--delay-ms D] [--darwin-ulock]\n");
}

static void sleep_ms(long long ms) {
    if (ms <= 0) {
        return;
    }
#ifdef _WIN32
    Sleep((DWORD)ms);
#else
    struct timespec ts;
    ts.tv_sec = (time_t)(ms / 1000);
    ts.tv_nsec = (long)((ms % 1000) * 1000000LL);
    while (nanosleep(&ts, &ts) != 0) {
    }
#endif
}

/* Wall-clock nanoseconds for round-trip statistics only. */
static long long now_ns(void) {
    struct timespec ts;
    timespec_get(&ts, TIME_UTC);
    return (long long)ts.tv_sec * 1000000000LL + (long long)ts.tv_nsec;
}

static int parse_ul(const char *s, unsigned long *out) {
    char *end = NULL;
    unsigned long v;
    if (s == NULL || *s == '\0') {
        return 0;
    }
    v = strtoul(s, &end, 10);
    if (end == NULL || *end != '\0') {
        return 0;
    }
    *out = v;
    return 1;
}

static int parse_args(int argc, char **argv, echo_cfg *c) {
    int i;
    int have_role = 0;
    memset(c, 0, sizeof *c);
    c->messages = 10;
    c->size = 4096;
    c->count = 8;
    c->policy = HQQ_WAIT_ADAPTIVE;
    c->peer_timeout_ms = 5000;
    c->perm = 0600;
    for (i = 1; i < argc; i++) {
        const char *a = argv[i];
        const char *next = (i + 1 < argc) ? argv[i + 1] : NULL;
        if (strcmp(a, "--create") == 0 || strcmp(a, "--open") == 0) {
            if (have_role || next == NULL) {
                return 0;
            }
            c->create = strcmp(a, "--create") == 0;
            have_role = 1;
            c->name = next;
            i++;
        } else if (strcmp(a, "--client") == 0) {
            c->server = 0;
        } else if (strcmp(a, "--server") == 0) {
            c->server = 1;
        } else if (strcmp(a, "--darwin-ulock") == 0) {
            c->darwin_ulock = 1;
        } else if (strcmp(a, "--messages") == 0 && next != NULL) {
            if (!parse_ul(next, &c->messages)) {
                return 0;
            }
            i++;
        } else if (strcmp(a, "--size") == 0 && next != NULL) {
            if (!parse_ul(next, &c->size)) {
                return 0;
            }
            i++;
        } else if (strcmp(a, "--count") == 0 && next != NULL) {
            if (!parse_ul(next, &c->count)) {
                return 0;
            }
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
        } else if (strcmp(a, "--heartbeat-ms") == 0 && next != NULL) {
            c->heartbeat_ms = atoll(next);
            i++;
        } else if (strcmp(a, "--peer-timeout-ms") == 0 && next != NULL) {
            c->peer_timeout_ms = atoll(next);
            i++;
        } else if (strcmp(a, "--delay-ms") == 0 && next != NULL) {
            c->delay_ms = atoll(next);
            i++;
        } else if (strcmp(a, "--perm") == 0 && next != NULL) {
            c->perm = (unsigned)strtoul(next, NULL, 8);
            i++;
        } else {
            return 0;
        }
    }
    return have_role && c->size >= 8 && c->count >= 2;
}

static void fill_pattern(unsigned char *buf, unsigned long seq, size_t n) {
    size_t i;
    for (i = 4; i < n; i++) {
        buf[i] = (unsigned char)(seq * 29u + i * 11u + 5u);
    }
    buf[0] = (unsigned char)(seq & 0xFFu);
    buf[1] = (unsigned char)((seq >> 8) & 0xFFu);
    buf[2] = (unsigned char)((seq >> 16) & 0xFFu);
    buf[3] = (unsigned char)((seq >> 24) & 0xFFu);
}

static size_t message_len(const echo_cfg *c, unsigned long seq) {
    if (seq % 10u == 9u) {
        return (size_t)c->size; /* full buffer */
    }
    if (c->size <= 4) {
        return 4;
    }
    return 4u + (size_t)((seq * 37u) % (c->size - 4u));
}

static int is_end(const unsigned char *buf, size_t len) { return len == END_LEN && memcmp(buf, END_MARKER, END_LEN) == 0; }

static int status_exit(hqq_status st) {
    switch (st) {
    case HQQ_ERR_PEER_GONE:
        return EXIT_GONE;
    case HQQ_ERR_PEER_CLOSED:
        return EXIT_CLOSED;
    default:
        return EXIT_LIB;
    }
}

/* Reads one message with a bounded wait; a silent peer for 10 s is a library error. */
static hqq_status read_one(hqq_link *l, void *buf, size_t cap, size_t *len) {
    hqq_wait w;
    w.timeout_ns = 10LL * 1000000000LL;
    w.cancel = NULL;
    return hqq_read(l, buf, cap, len, &w);
}

static int run_client(hqq_link *l, const echo_cfg *c) {
    unsigned char *out = (unsigned char *)malloc(c->size);
    unsigned char *in = (unsigned char *)malloc(c->size);
    unsigned long seq;
    long long total_ns = 0, max_ns = 0, first_ns = 0;
    int rc = EXIT_OK;
    if (out == NULL || in == NULL) {
        free(out);
        free(in);
        return EXIT_LIB;
    }
    for (seq = 0; seq < c->messages; seq++) {
        size_t len = message_len(c, seq), got = 0;
        hqq_status st;
        long long t0;
        long long rtt;
        fill_pattern(out, seq, len);
        t0 = now_ns();
        st = hqq_write(l, out, len, NULL);
        if (st != HQQ_OK) {
            fprintf(stderr, "write %lu: %s\n", seq, hqq_strerror(st));
            rc = status_exit(st);
            break;
        }
        st = read_one(l, in, c->size, &got);
        rtt = now_ns() - t0;
        if (st != HQQ_OK) {
            fprintf(stderr, "echo %lu: %s\n", seq, hqq_strerror(st));
            rc = status_exit(st);
            break;
        }
        if (got != len || memcmp(in, out, len) != 0) {
            fprintf(stderr, "echo %lu: mismatch (got %lu bytes, want %lu)\n", seq, (unsigned long)got,
                    (unsigned long)len);
            rc = EXIT_MISMATCH;
            break;
        }
        if (seq == 0) {
            first_ns = rtt;
        }
        total_ns += rtt;
        if (rtt > max_ns) {
            max_ns = rtt;
        }
    }
    if (rc == EXIT_OK) {
        hqq_status st = hqq_write(l, END_MARKER, END_LEN, NULL);
        if (st != HQQ_OK) {
            fprintf(stderr, "end marker: %s\n", hqq_strerror(st));
            rc = status_exit(st);
        }
    }
    if (rc == EXIT_OK) {
        printf("client: %lu messages echoed intact (buffer %lu, count %lu)\n", c->messages, c->size, c->count);
        if (c->messages > 1) {
            /* The first round trip includes the peer's attach when it started late. */
            printf("client: round trip first %.3f ms; then mean %.3f ms, max %.3f ms\n",
                   (double)first_ns / 1e6, (double)(total_ns - first_ns) / (double)(c->messages - 1) / 1e6,
                   (double)max_ns / 1e6);
        } else if (c->messages == 1) {
            printf("client: round trip %.3f ms\n", (double)first_ns / 1e6);
        }
    }
    free(out);
    free(in);
    return rc;
}

static int run_server(hqq_link *l, const echo_cfg *c) {
    unsigned char *buf = (unsigned char *)malloc(c->size);
    unsigned long n = 0;
    int rc = EXIT_OK;
    if (buf == NULL) {
        return EXIT_LIB;
    }
    for (;;) {
        size_t len = 0;
        hqq_status st = read_one(l, buf, c->size, &len);
        if (st != HQQ_OK) {
            fprintf(stderr, "server read: %s\n", hqq_strerror(st));
            rc = status_exit(st);
            break;
        }
        if (is_end(buf, len)) {
            break;
        }
        sleep_ms(c->delay_ms);
        st = hqq_write(l, buf, len, NULL);
        if (st != HQQ_OK) {
            fprintf(stderr, "server write: %s\n", hqq_strerror(st));
            rc = status_exit(st);
            break;
        }
        n++;
    }
    if (rc == EXIT_OK) {
        printf("server: echoed %lu messages\n", n);
    }
    free(buf);
    return rc;
}

int main(int argc, char **argv) {
    echo_cfg c;
    hqq_options o;
    hqq_link *l = NULL;
    hqq_open_info info;
    hqq_status st;
    int rc;

    if (!parse_args(argc, argv, &c)) {
        usage();
        return EXIT_USAGE;
    }
    hqq_options_init(&o);
    o.wait_policy = c.policy;
    o.heartbeat_interval_ns = c.heartbeat_ms == 0 ? 0 : (c.heartbeat_ms < 0 ? -1 : c.heartbeat_ms * NS_PER_MS);
    o.peer_timeout_ns = c.peer_timeout_ms * NS_PER_MS;
    if (c.darwin_ulock) {
        o.flags |= HQQ_FLAG_DARWIN_FORCE_ULOCK;
    }
    hqq_open_info_init(&info);

    if (c.create) {
        st = hqq_standard_create(c.name, c.count, c.size, c.perm, &o, &l, &info);
    } else {
        st = hqq_standard_open(c.name, c.count, c.size, &o, &l, &info);
    }
    if (st != HQQ_OK) {
        fprintf(stderr, "%s %s: %s", c.create ? "create" : "open", c.name, hqq_strerror(st));
        if (st == HQQ_ERR_GEOMETRY_MISMATCH) {
            fprintf(stderr, " (segment has count %llu size %llu)", (unsigned long long)info.got.buffer_count,
                    (unsigned long long)info.got.buffer_size);
        }
        fprintf(stderr, "\n");
        return EXIT_LIB;
    }
    printf("%s %s as %s\n", c.create ? "created" : "opened", c.name,
           hqq_link_mode(l) == HQQ_MODE_PRIMARY ? "primary" : "secondary");
    fflush(stdout);

    rc = c.server ? run_server(l, &c) : run_client(l, &c);
    if (rc == EXIT_OK) {
        printf("peer state: %d\n", (int)hqq_peer_state(l));
    }
    st = hqq_close(l);
    if (st != HQQ_OK) {
        fprintf(stderr, "close: %s\n", hqq_strerror(st));
        if (rc == EXIT_OK) {
            rc = EXIT_LIB;
        }
    }
    return rc;
}
