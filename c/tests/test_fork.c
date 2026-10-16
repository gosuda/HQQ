/* Two-process tests on POSIX: the child process is the Secondary of a named
 * segment created by the parent. Covers create/open/echo (including full-size
 * messages), opener-before-creator, graceful close, crash detection with
 * drain-before-failure, and reuse of the name after both endpoints died. */
#include <errno.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <unistd.h>

#include "hqq_internal.h"
#include "hqq_test.h"
#include "hqq_testutil.h"

#define FK_COUNT 8u
#define FK_SIZE 4096u
#define FK_END_MARKER 0xFEu

static hqq_options fk_opts(void) {
    hqq_options o;
    hqq_options_init(&o);
    o.heartbeat_interval_ns = 50 * (int64_t)HQQ_NS_PER_MS;
    o.peer_timeout_ns = 500 * (int64_t)HQQ_NS_PER_MS;
    o.attach_timeout_ns = 100 * (int64_t)HQQ_NS_PER_MS;
    return o;
}

/* Opens as Secondary, retrying the transient errors of SPEC 4 (the Dial rule). */
static hqq_status open_retry(const char *name, hqq_link **out) {
    hqq_options o = fk_opts();
    hqq_status st = HQQ_ERR_NOT_EXIST;
    int i;
    for (i = 0; i < 1000; i++) {
        st = hqq_standard_open(name, FK_COUNT, FK_SIZE, &o, out, NULL);
        if (st == HQQ_OK || !hqq_error_is_transient_open(st)) {
            return st;
        }
        hqq_test_sleep_ms(5);
    }
    return st;
}

static unsigned char pattern_byte(unsigned seq, size_t i) { return (unsigned char)(seq * 131u + i * 7u + 3u); }

static size_t message_size(unsigned seq) {
    if (seq % 50u == 49u) {
        return FK_SIZE; /* full buffer */
    }
    return 1u + (seq * 37u) % 200u;
}

/* Echo child: opens, echoes every message until the end marker, then closes. */
static int echo_child(const char *name) {
    hqq_link *l = NULL;
    unsigned char *buf = (unsigned char *)malloc(FK_SIZE);
    int rc = 0;
    if (buf == NULL || open_retry(name, &l) != HQQ_OK) {
        free(buf);
        return 1;
    }
    for (;;) {
        size_t len = 0;
        hqq_status st = hqq_read(l, buf, FK_SIZE, &len, NULL);
        if (st != HQQ_OK) {
            rc = 2;
            break;
        }
        if (len == 1 && buf[0] == FK_END_MARKER) {
            break;
        }
        if (hqq_write(l, buf, len, NULL) != HQQ_OK) {
            rc = 3;
            break;
        }
    }
    free(buf);
    if (hqq_close(l) != HQQ_OK) {
        rc = rc ? rc : 4;
    }
    return rc;
}

static void check_echo_across_processes(void) {
    char name[64];
    hqq_link *p = NULL;
    hqq_options o = fk_opts();
    unsigned char *out = (unsigned char *)malloc(FK_SIZE), *in = (unsigned char *)malloc(FK_SIZE);
    pid_t pid;
    int status = -1;
    unsigned seq, i;
    const unsigned n = 300;

    HQQ_REQUIRE(out != NULL && in != NULL);
    hqq_test_unique_name(name, sizeof name, "echo");
    pid = fork();
    HQQ_REQUIRE(pid >= 0);
    if (pid == 0) {
        _exit(echo_child(name));
    }
    HQQ_REQUIRE(hqq_standard_create(name, FK_COUNT, FK_SIZE, 0600, &o, &p, NULL) == HQQ_OK);

    for (seq = 0; seq < n; seq++) {
        size_t len = message_size(seq), got = 0;
        for (i = 0; i < len; i++) {
            out[i] = pattern_byte(seq, i);
        }
        HQQ_CHECK_STATUS(hqq_write(p, out, len, NULL), HQQ_OK);
        HQQ_CHECK_STATUS(hqq_read(p, in, FK_SIZE, &got, NULL), HQQ_OK);
        HQQ_CHECK_EQ_U64(got, len);
        HQQ_CHECK(memcmp(in, out, len) == 0);
        if (got != len || memcmp(in, out, len) != 0) {
            break;
        }
    }
    out[0] = FK_END_MARKER;
    HQQ_CHECK_STATUS(hqq_write(p, out, 1, NULL), HQQ_OK);
    HQQ_CHECK_EQ_INT(waitpid(pid, &status, 0), pid);
    HQQ_CHECK(WIFEXITED(status) && WEXITSTATUS(status) == 0);
    HQQ_CHECK_EQ_INT(hqq_peer_state(p), HQQ_PEER_CLOSED);
    HQQ_CHECK_STATUS(hqq_close(p), HQQ_OK);
    free(out);
    free(in);
}

/* Opener starts first and retries until the creator has initialized the segment. */
static void check_opener_first(void) {
    char name[64];
    hqq_link *p = NULL;
    hqq_options o = fk_opts();
    unsigned char buf[8];
    size_t len = 0;
    pid_t pid;
    int status = -1;

    hqq_test_unique_name(name, sizeof name, "first");
    pid = fork();
    HQQ_REQUIRE(pid >= 0);
    if (pid == 0) {
        hqq_link *l = NULL;
        size_t got = 0;
        unsigned char b[8];
        int rc = 0;
        if (open_retry(name, &l) != HQQ_OK) {
            _exit(1);
        }
        if (hqq_read(l, b, sizeof b, &got, NULL) != HQQ_OK || got != 4 || memcmp(b, "ping", 4) != 0) {
            rc = 2;
        }
        if (hqq_write(l, "pong", 4, NULL) != HQQ_OK) {
            rc = 3;
        }
        hqq_close(l);
        _exit(rc);
    }
    hqq_test_sleep_ms(150); /* the opener has been retrying with no segment */
    HQQ_REQUIRE(hqq_standard_create(name, FK_COUNT, FK_SIZE, 0600, &o, &p, NULL) == HQQ_OK);
    HQQ_CHECK_STATUS(hqq_write(p, "ping", 4, NULL), HQQ_OK);
    HQQ_CHECK_STATUS(hqq_read(p, buf, sizeof buf, &len, NULL), HQQ_OK);
    HQQ_CHECK_EQ_U64(len, 4);
    HQQ_CHECK(memcmp(buf, "pong", 4) == 0);
    HQQ_CHECK_EQ_INT(waitpid(pid, &status, 0), pid);
    HQQ_CHECK(WIFEXITED(status) && WEXITSTATUS(status) == 0);
    HQQ_CHECK_STATUS(hqq_close(p), HQQ_OK);
}

/* The child writes two messages and exits without closing: the parent must receive both
 * (drain before failure), then ErrPeerGone once the heartbeat stops. */
static void check_crash_detection_and_drain(void) {
    char name[64];
    hqq_link *p = NULL;
    hqq_options o = fk_opts();
    unsigned char buf[8];
    size_t len = 0;
    pid_t pid;
    int status = -1;
    uint64_t t0;
    hqq_wait w;

    hqq_test_unique_name(name, sizeof name, "crash");
    pid = fork();
    HQQ_REQUIRE(pid >= 0);
    if (pid == 0) {
        hqq_link *l = NULL;
        if (open_retry(name, &l) != HQQ_OK) {
            _exit(1);
        }
        if (hqq_write(l, "one", 3, NULL) != HQQ_OK || hqq_write(l, "two", 3, NULL) != HQQ_OK) {
            _exit(2);
        }
        _exit(0); /* crash: no close, no detach, the heartbeat stops */
    }
    HQQ_REQUIRE(hqq_standard_create(name, FK_COUNT, FK_SIZE, 0600, &o, &p, NULL) == HQQ_OK);
    HQQ_CHECK_EQ_INT(waitpid(pid, &status, 0), pid);
    HQQ_CHECK(WIFEXITED(status) && WEXITSTATUS(status) == 0);

    HQQ_CHECK_STATUS(hqq_read(p, buf, sizeof buf, &len, NULL), HQQ_OK);
    HQQ_CHECK(len == 3 && memcmp(buf, "one", 3) == 0);
    HQQ_CHECK_STATUS(hqq_read(p, buf, sizeof buf, &len, NULL), HQQ_OK);
    HQQ_CHECK(len == 3 && memcmp(buf, "two", 3) == 0);

    w.timeout_ns = 10 * (int64_t)HQQ_NS_PER_SEC;
    w.cancel = NULL;
    t0 = hqq_plat_mono_ns();
    HQQ_CHECK_STATUS(hqq_read(p, buf, sizeof buf, &len, &w), HQQ_ERR_PEER_GONE);
    /* Within max(peer timeout, 4 x interval) plus scheduling slack (X-07). */
    HQQ_CHECK(hqq_plat_mono_ns() - t0 < 3 * HQQ_NS_PER_SEC);
    HQQ_CHECK_EQ_INT(hqq_peer_state(p), HQQ_PEER_GONE);
    HQQ_CHECK_STATUS(hqq_write(p, "x", 1, NULL), HQQ_ERR_PEER_GONE);

    /* The creator closes: the name is removed, and the name can be created again (reuse). */
    HQQ_CHECK_STATUS(hqq_close(p), HQQ_OK);
    p = NULL;
    HQQ_CHECK_STATUS(hqq_standard_create(name, FK_COUNT, FK_SIZE, 0600, &o, &p, NULL), HQQ_OK);
    HQQ_CHECK_EQ_INT(hqq_link_mode(p), HQQ_MODE_PRIMARY);
    HQQ_CHECK_STATUS(hqq_close(p), HQQ_OK);
}

/* Graceful close by the peer: the failure is PeerClosed at once, after the drain. */
static void check_graceful_close(void) {
    char name[64];
    hqq_link *p = NULL;
    hqq_options o = fk_opts();
    unsigned char buf[8];
    size_t len = 0;
    pid_t pid;
    int status = -1;
    uint64_t t0;

    hqq_test_unique_name(name, sizeof name, "gclose");
    pid = fork();
    HQQ_REQUIRE(pid >= 0);
    if (pid == 0) {
        hqq_link *l = NULL;
        if (open_retry(name, &l) != HQQ_OK) {
            _exit(1);
        }
        if (hqq_write(l, "bye", 3, NULL) != HQQ_OK) {
            _exit(2);
        }
        _exit(hqq_close(l) == HQQ_OK ? 0 : 3);
    }
    HQQ_REQUIRE(hqq_standard_create(name, FK_COUNT, FK_SIZE, 0600, &o, &p, NULL) == HQQ_OK);
    HQQ_CHECK_EQ_INT(waitpid(pid, &status, 0), pid);
    HQQ_CHECK(WIFEXITED(status) && WEXITSTATUS(status) == 0);
    HQQ_CHECK_STATUS(hqq_read(p, buf, sizeof buf, &len, NULL), HQQ_OK);
    HQQ_CHECK(len == 3 && memcmp(buf, "bye", 3) == 0);
    t0 = hqq_plat_mono_ns();
    HQQ_CHECK_STATUS(hqq_read(p, buf, sizeof buf, &len, NULL), HQQ_ERR_PEER_CLOSED);
    HQQ_CHECK(hqq_plat_mono_ns() - t0 < HQQ_NS_PER_SEC);
    HQQ_CHECK_STATUS(hqq_close(p), HQQ_OK);
}

int main(void) {
    check_echo_across_processes();
    check_opener_first();
    check_crash_detection_and_drain();
    check_graceful_close();
    return HQQ_TEST_RESULT("test_fork");
}
