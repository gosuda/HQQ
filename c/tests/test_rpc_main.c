/*
 * test_rpc_main: hqqrpc_main as a process (BINDGEN.md 9.5, D17). The program under test is the
 * microlib fixture server (rpc_microlib_server.c), started with posix_spawn:
 *
 *  - the command line: usage, errors (exit code 2), flags that change the server's behavior;
 *  - the ready line "ready <listener>" on stdout once the listener exists, and not before;
 *  - SIGTERM and SIGINT stop the server gracefully (exit code 0);
 *  - --parent-stdin: end of file on standard input stops it gracefully;
 *  - a failure to create the server is exit code 1 with no ready line.
 *
 * argv[1] is the path of the server binary.
 */
#if defined(__linux__) && !defined(_GNU_SOURCE)
#  define _GNU_SOURCE 1
#endif
#if defined(__APPLE__) && !defined(_DARWIN_C_SOURCE)
#  define _DARWIN_C_SOURCE 1
#endif

#include <errno.h>
#include <poll.h>
#include <signal.h>
#include <fcntl.h>
#include <spawn.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <unistd.h>

#include "hqq_test.h"
#include "hqq_testutil.h"
#include "rpc_ml_server.h"
#include "rpc_client.h"

extern char **environ;

static const char *g_server;

typedef struct proc {
    pid_t pid;
    int out_fd; /* the server's stdout */
    int in_fd;  /* the server's stdin (the lifeline) */
    char buf[4096];
    size_t len;
} proc;

static void proc_start(proc *p, const char *const *args) {
    int out[2];
    int in[2];
    posix_spawn_file_actions_t fa;
    char *argv[32];
    size_t n = 0;
    int rc;

    HQQ_REQUIRE(pipe(out) == 0 && pipe(in) == 0);
    HQQ_REQUIRE(posix_spawn_file_actions_init(&fa) == 0);
    HQQ_REQUIRE(posix_spawn_file_actions_adddup2(&fa, out[1], STDOUT_FILENO) == 0);
    HQQ_REQUIRE(posix_spawn_file_actions_adddup2(&fa, in[0], STDIN_FILENO) == 0);
    HQQ_REQUIRE(posix_spawn_file_actions_addclose(&fa, out[0]) == 0);
    HQQ_REQUIRE(posix_spawn_file_actions_addclose(&fa, in[1]) == 0);
    argv[n++] = (char *)g_server;
    while (*args != NULL && n < 30) {
        argv[n++] = (char *)*args++;
    }
    argv[n] = NULL;
    memset(p, 0, sizeof *p);
    rc = posix_spawn(&p->pid, g_server, &fa, NULL, argv, environ);
    HQQ_REQUIRE(rc == 0);
    posix_spawn_file_actions_destroy(&fa);
    close(out[1]);
    close(in[0]);
    p->out_fd = out[0];
    p->in_fd = in[1];
}

/* Reads from the server's stdout until `want` appears or the timeout passes. 1 if it appeared. */
static int proc_expect(proc *p, const char *want, int timeout_ms) {
    uint64_t deadline = hqq_test_now_ns() + (uint64_t)timeout_ms * UINT64_C(1000000);

    for (;;) {
        struct pollfd pf;
        int64_t left = (int64_t)(deadline - hqq_test_now_ns());
        ssize_t r;

        p->buf[p->len] = '\0';
        if (strstr(p->buf, want) != NULL) {
            return 1;
        }
        if (left <= 0 || p->len + 1 >= sizeof p->buf) {
            return 0;
        }
        pf.fd = p->out_fd;
        pf.events = POLLIN;
        if (poll(&pf, 1, (int)(left / 1000000) + 1) <= 0) {
            return 0;
        }
        r = read(p->out_fd, p->buf + p->len, sizeof p->buf - 1 - p->len);
        if (r <= 0) {
            p->buf[p->len] = '\0';
            return strstr(p->buf, want) != NULL;
        }
        p->len += (size_t)r;
    }
}

/* Waits for the exit; returns the exit code, or -1000 - signal if killed, or -1 on timeout (and kills). */
static int proc_wait(proc *p, int timeout_ms) {
    uint64_t deadline = hqq_test_now_ns() + (uint64_t)timeout_ms * UINT64_C(1000000);
    int st = 0;

    for (;;) {
        pid_t r = waitpid(p->pid, &st, WNOHANG);

        if (r == p->pid) {
            break;
        }
        if (hqq_test_now_ns() > deadline) {
            kill(p->pid, SIGKILL);
            (void)waitpid(p->pid, &st, 0);
            close(p->out_fd);
            close(p->in_fd);
            return -1;
        }
        hqq_test_sleep_ms(5);
    }
    close(p->out_fd);
    close(p->in_fd);
    return WIFEXITED(st) ? WEXITSTATUS(st) : -1000 - WTERMSIG(st);
}

/* Starts the server with args, expects `ready <name>` and returns the process. */
static void start_ready(proc *p, const char *name, const char *const *extra) {
    const char *args[32];
    size_t n = 0;
    char want[80];

    args[n++] = name;
    while (*extra != NULL && n < 30) {
        args[n++] = *extra++;
    }
    args[n] = NULL;
    proc_start(p, args);
    snprintf(want, sizeof want, "ready %s\n", name);
    HQQ_REQUIRE(proc_expect(p, want, 15000));
}

static void test_usage_and_errors(void) {
    static const char *const bad[][4] = {
        {"--bogus", NULL, NULL, NULL},
        {"name", "extra", NULL, NULL},
        {NULL, NULL, NULL, NULL},
        {"name", "--max-sessions", "0", NULL},
        {"name", "--max-sessions", NULL, NULL},
        {"name", "--perm", "999", NULL},
        {"name", "--max-message", "10", NULL},
        {"name", "--max-handles", "70000", NULL},
        {"name", "--idle-timeout-ms", "-5", NULL},
    };
    size_t i;
    proc p;

    {
        const char *args[] = {"--help", NULL};

        proc_start(&p, args);
        HQQ_CHECK(proc_expect(&p, "usage:", 10000));
        HQQ_CHECK_EQ_INT(proc_wait(&p, 10000), 0);
    }
    {
        const char *args[] = {"--version", NULL};

        proc_start(&p, args);
        HQQ_CHECK(proc_expect(&p, "microlib", 10000));
        HQQ_CHECK_EQ_INT(proc_wait(&p, 10000), 0);
    }
    for (i = 0; i < sizeof bad / sizeof bad[0]; i++) {
        proc_start(&p, bad[i]);
        HQQ_CHECK_EQ_INT(proc_wait(&p, 10000), 2); /* a usage error: exit code 2, nothing on stdout */
    }
}

static void test_creation_failure(void) {
    proc p;
    char name[40];
    const char *args[6];

    /* 2 * B * S above max_segment_bytes: the server cannot be created */
    hqq_test_unique_name(name, sizeof name, "cf");
    args[0] = name;
    args[1] = "--max-segment-bytes";
    args[2] = "1000";
    args[3] = NULL;
    proc_start(&p, args);
    HQQ_CHECK_EQ_INT(proc_wait(&p, 10000), 1);
    p.buf[0] = '\0';

    /* the listener name is taken by a live server */
    {
        proc a;
        proc b;
        const char *none[] = {NULL};
        const char *dup[2];

        hqq_test_unique_name(name, sizeof name, "du");
        start_ready(&a, name, none);
        dup[0] = name;
        dup[1] = NULL;
        proc_start(&b, dup);
        HQQ_CHECK_EQ_INT(proc_wait(&b, 10000), 1);
        kill(a.pid, SIGTERM);
        HQQ_CHECK_EQ_INT(proc_wait(&a, 15000), 0);
    }
}

/* A run: flags take effect, SIGTERM and SIGINT end it with exit code 0. */
static void test_run_and_signals(void) {
    static const int sigs[2] = {SIGTERM, SIGINT};
    int k;

    for (k = 0; k < 2; k++) {
        proc p;
        char name[40];
        const char *extra[] = {"--max-sessions", "2", "--buffer-size", "4096", "--buffer-count", "8",
                               "--open-timeout-ms", "3000", NULL};
        client c1;
        client c2;
        hqq_adv *ctl3 = NULL;
        hqq_msg *resp = NULL;
        uint64_t status = 0;

        hqq_test_unique_name(name, sizeof name, k == 0 ? "tm" : "in");
        start_ready(&p, name, extra);
        cli_open(&c1, name, 0, 0);
        HQQ_CHECK_EQ_INT(c1.B, 8); /* --buffer-count and --buffer-size are the defaults of OPEN */
        HQQ_CHECK_EQ_INT(c1.S, 4096);
        HQQ_CHECK_EQ_U64(cli_hello(&c1, &microlib_iface, NULL, NULL), 0);
        call_add(&c1, 40, 2, 42);
        cli_open(&c2, name, 0, 0);
        HQQ_CHECK_EQ_U64(cli_hello(&c2, &microlib_iface, NULL, NULL), 0);
        /* --max-sessions 2 */
        HQQ_REQUIRE(ctl_connect(name, &ctl3) == HQQ_OK);
        HQQ_CHECK_STATUS(ctl_open(ctl3, 0, 0, 1, "HQBO", &status, &resp, 5 * SEC), HQQ_ERR_RESPONSE);
        HQQ_CHECK_EQ_U64(status, HQQRPC_OPEN_STATUS_BASE + HQQRPC_OPEN_E_BUSY);
        hqq_msg_release(resp);
        (void)hqq_adv_close(ctl3);

        HQQ_REQUIRE(kill(p.pid, sigs[k]) == 0);
        HQQ_CHECK_EQ_INT(proc_wait(&p, 20000), 0);
        cli_free(&c1);
        cli_free(&c2);
    }
}

/* --parent-stdin: when the write end of the lifeline closes, the server shuts down gracefully. */
static void test_parent_stdin(void) {
    proc p;
    char name[40];
    const char *extra[] = {"--parent-stdin", "--lifeline-grace-ms", "10000", NULL};
    client c;

    hqq_test_unique_name(name, sizeof name, "ps");
    start_ready(&p, name, extra);
    cli_open(&c, name, 0, 0);
    HQQ_CHECK_EQ_U64(cli_hello(&c, &microlib_iface, NULL, NULL), 0);
    call_add(&c, 1, 2, 3);
    /* keepalive bytes are discarded */
    HQQ_CHECK(write(p.in_fd, "x", 1) == 1);
    hqq_test_sleep_ms(100);
    HQQ_CHECK(waitpid(p.pid, NULL, WNOHANG) == 0); /* still running */
    close(p.in_fd);
    p.in_fd = open("/dev/null", O_RDONLY);
    HQQ_CHECK_EQ_INT(proc_wait(&p, 20000), 0);
    cli_free(&c);
}

int main(int argc, char **argv) {
    if (argc != 2) {
        fprintf(stderr, "usage: test_rpc_main <server binary>\n");
        return 2;
    }
    g_server = argv[1];
    signal(SIGPIPE, SIG_IGN);
    test_usage_and_errors();
    test_creation_failure();
    test_run_and_signals();
    test_parent_stdin();
    return HQQ_TEST_RESULT("rpc_main");
}
