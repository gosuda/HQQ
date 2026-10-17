/* POSIX tests of the parent lifeline (hqq_lifeline.h). The "supervisor" is the
 * test process or a forked intermediary; the "service" is a forked child whose
 * standard input is the read end of a pipe (or a pseudo-terminal). Every child
 * arms alarm() so that a failure cannot leave it running. */
#define _XOPEN_SOURCE 600

#include <errno.h>
#include <fcntl.h>
#include <pthread.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

#include "hqq/hqq_lifeline.h"
#include "hqq_test.h"
#include "hqq_testutil.h"

#define LT_CHILD_GUARD_S 30u

/* Callback probe: counts the firings and keeps the last reason. */
typedef struct lt_probe {
    pthread_mutex_t mu;
    pthread_cond_t cv;
    int calls;
    hqq_lifeline_reason reason;
} lt_probe;

static void probe_init(lt_probe *p) {
    pthread_mutex_init(&p->mu, NULL);
    pthread_cond_init(&p->cv, NULL);
    p->calls = 0;
    p->reason = HQQ_LIFELINE_REASON_EOF;
}

static void probe_cb(void *ctx, hqq_lifeline_reason reason) {
    lt_probe *p = ctx;
    pthread_mutex_lock(&p->mu);
    p->calls++;
    p->reason = reason;
    pthread_cond_broadcast(&p->cv);
    pthread_mutex_unlock(&p->mu);
}

static void probe_snapshot(lt_probe *p, int *calls, hqq_lifeline_reason *reason) {
    pthread_mutex_lock(&p->mu);
    *calls = p->calls;
    *reason = p->reason;
    pthread_mutex_unlock(&p->mu);
}

/* Waits up to ms for the first firing. Returns 1 when it happened. */
static int probe_wait(lt_probe *p, unsigned ms) {
    struct timespec ts;
    long long ns;
    int calls;
    int rc = 0;
    clock_gettime(CLOCK_REALTIME, &ts);
    ns = (long long)ts.tv_nsec + (long long)ms * 1000000LL;
    ts.tv_sec += (time_t)(ns / 1000000000LL);
    ts.tv_nsec = (long)(ns % 1000000000LL);
    pthread_mutex_lock(&p->mu);
    while (p->calls == 0 && rc == 0) {
        rc = pthread_cond_timedwait(&p->cv, &p->mu, &ts);
    }
    calls = p->calls;
    pthread_mutex_unlock(&p->mu);
    return calls > 0;
}

/* Waits up to timeout_ms for pid to exit and reaps it. Returns 1 when reaped. */
static int wait_exit(pid_t pid, unsigned timeout_ms, int *status) {
    uint64_t deadline = hqq_test_now_ns() + (uint64_t)timeout_ms * 1000000u;
    for (;;) {
        pid_t r = waitpid(pid, status, WNOHANG);
        if (r == pid) {
            return 1;
        }
        if (r < 0 && errno != EINTR) {
            return 0;
        }
        if (hqq_test_now_ns() >= deadline) {
            return 0;
        }
        hqq_test_sleep_ms(5);
    }
}

static int exited_with(int status, int code) { return WIFEXITED(status) && WEXITSTATUS(status) == code; }

/* ---- Service bodies (run in a forked child; each ends with _exit) ------------------ */

/* Watches stdin (already the pipe): exit 0 when it fired exactly once, with EOF. */
static void body_eof_once(void) {
    lt_probe pr;
    hqq_lifeline_options o;
    hqq_lifeline *l = NULL;
    int code = 1, calls = 0;
    hqq_lifeline_reason reason = HQQ_LIFELINE_REASON_ERROR;
    probe_init(&pr);
    hqq_lifeline_options_init(&o);
    o.callback = probe_cb;
    o.ctx = &pr;
    if (hqq_lifeline_start(&o, &l) == HQQ_OK && hqq_lifeline_watching(l) && probe_wait(&pr, 10000)) {
        hqq_test_sleep_ms(150); /* a second firing would show up now */
        probe_snapshot(&pr, &calls, &reason);
        code = (calls == 1 && reason == HQQ_LIFELINE_REASON_EOF) ? 0 : 2;
    }
    hqq_lifeline_stop(l);
    _exit(code);
}

/* The callback fires and the service never stops: the lifeline forces the exit after grace_ms. */
static void body_grace_no_stop(void) {
    lt_probe pr;
    hqq_lifeline_options o;
    hqq_lifeline *l = NULL;
    probe_init(&pr);
    hqq_lifeline_options_init(&o);
    o.callback = probe_cb;
    o.ctx = &pr;
    o.grace_ms = 200;
    if (hqq_lifeline_start(&o, &l) != HQQ_OK) {
        _exit(3);
    }
    if (probe_wait(&pr, 10000)) {
        hqq_test_sleep_ms(5000); /* must not get this far: the lifeline ends the process */
    }
    _exit(3);
}

/* The callback fires and the service stops the lifeline within grace_ms: exit 0 on time. */
static void body_grace_stop_in_time(void) {
    lt_probe pr;
    hqq_lifeline_options o;
    hqq_lifeline *l = NULL;
    uint64_t t0;
    probe_init(&pr);
    hqq_lifeline_options_init(&o);
    o.callback = probe_cb;
    o.ctx = &pr;
    o.grace_ms = 5000;
    if (hqq_lifeline_start(&o, &l) != HQQ_OK || !probe_wait(&pr, 10000)) {
        _exit(3);
    }
    t0 = hqq_test_now_ns();
    hqq_lifeline_stop(l);
    _exit(hqq_test_now_ns() - t0 < 2000u * 1000000u ? 0 : 4);
}

/* ---- Tests ---------------------------------------------------------------------- */

/* The service's stdin is a pipe; the parent keeps the write end. Closing it fires
 * the callback exactly once with EOF, and the service then exits cleanly. */
static void check_eof_fires_once(void) {
    int p[2];
    pid_t pid;
    int st = 0;
    HQQ_REQUIRE(pipe(p) == 0);
    pid = fork();
    HQQ_REQUIRE(pid >= 0);
    if (pid == 0) {
        alarm(LT_CHILD_GUARD_S);
        close(p[1]);
        dup2(p[0], STDIN_FILENO);
        close(p[0]);
        body_eof_once();
    }
    close(p[0]);
    HQQ_CHECK(!wait_exit(pid, 150, &st)); /* alive while the write end is open */
    close(p[1]);
    HQQ_CHECK(wait_exit(pid, 10000, &st));
    HQQ_CHECK(exited_with(st, 0));
}

/* Keepalive bytes are drained and discarded: the lifeline does not fire on them. */
static void check_keepalive_discarded(void) {
    int p[2];
    pid_t pid;
    int st = 0, i;
    unsigned char chunk[512];
    HQQ_REQUIRE(pipe(p) == 0);
    memset(chunk, 'k', sizeof chunk);
    pid = fork();
    HQQ_REQUIRE(pid >= 0);
    if (pid == 0) {
        alarm(LT_CHILD_GUARD_S);
        close(p[1]);
        dup2(p[0], STDIN_FILENO);
        close(p[0]);
        body_eof_once();
    }
    close(p[0]);
    /* 256 KiB of keepalives: more than a pipe buffer, so the helper must keep draining. */
    for (i = 0; i < 512; i++) {
        size_t off = 0;
        while (off < sizeof chunk) {
            ssize_t w = write(p[1], chunk + off, sizeof chunk - off);
            if (w < 0 && errno == EINTR) {
                continue;
            }
            HQQ_REQUIRE(w > 0);
            off += (size_t)w;
        }
    }
    HQQ_CHECK(!wait_exit(pid, 200, &st)); /* no firing: still alive */
    close(p[1]);
    HQQ_CHECK(wait_exit(pid, 10000, &st));
    HQQ_CHECK(exited_with(st, 0));
}

/* A pseudo-terminal stdin is not watched by default (an interactive run survives
 * a closed terminal); with force it is, and closing the master fires once. */
static void check_tty_policy(void) {
    static const int forces[2] = {0, 1};
    int k;
    for (k = 0; k < 2; k++) {
        int master, ready[2];
        char name[256];
        char *pts;
        pid_t pid;
        int st = 0;
        master = posix_openpt(O_RDWR | O_NOCTTY);
        if (master < 0) {
            fprintf(stderr, "test_lifeline: no pseudo-terminal: skipping the TTY policy checks\n");
            return;
        }
        if (grantpt(master) != 0 || unlockpt(master) != 0 || (pts = ptsname(master)) == NULL) {
            close(master);
            fprintf(stderr, "test_lifeline: pseudo-terminal setup failed: skipping the TTY policy checks\n");
            return;
        }
        strncpy(name, pts, sizeof name - 1);
        name[sizeof name - 1] = '\0';
        HQQ_REQUIRE(pipe(ready) == 0);
        pid = fork();
        HQQ_REQUIRE(pid >= 0);
        if (pid == 0) {
            lt_probe pr;
            hqq_lifeline_options o;
            hqq_lifeline *l = NULL;
            int slave, code = 1, calls = 0, watching;
            hqq_lifeline_reason reason = HQQ_LIFELINE_REASON_ERROR;
            alarm(LT_CHILD_GUARD_S);
            close(ready[0]);
            close(master); /* the inherited master: only the parent may hang up the terminal */
            slave = open(name, O_RDWR | O_NOCTTY);
            if (slave < 0) {
                _exit(5);
            }
            dup2(slave, STDIN_FILENO);
            close(slave);
            probe_init(&pr);
            hqq_lifeline_options_init(&o);
            o.callback = probe_cb;
            o.ctx = &pr;
            o.force = forces[k];
            if (hqq_lifeline_start(&o, &l) != HQQ_OK) {
                _exit(6);
            }
            watching = hqq_lifeline_watching(l);
            (void)write(ready[1], "r", 1);
            if (!forces[k]) {
                /* Not watched: nothing fires, even after a while. */
                hqq_test_sleep_ms(300);
                probe_snapshot(&pr, &calls, &reason);
                code = (!watching && calls == 0) ? 0 : 2;
            } else {
                /* Watched with force: the hang-up (master closed) fires once. */
                if (watching && probe_wait(&pr, 10000)) {
                    probe_snapshot(&pr, &calls, &reason);
                    code = calls == 1 ? 0 : 2;
                } else {
                    code = 3;
                }
            }
            hqq_lifeline_stop(l);
            _exit(code);
        }
        close(ready[1]);
        {
            char b = 0;
            HQQ_CHECK(read(ready[0], &b, 1) == 1); /* the service has started its lifeline */
        }
        close(ready[0]);
        if (forces[k]) {
            close(master); /* hang up the terminal */
            master = -1;
        }
        HQQ_CHECK(wait_exit(pid, 15000, &st));
        HQQ_CHECK(exited_with(st, 0));
        if (master >= 0) {
            close(master);
        }
    }
}

/* Stop before any firing: the callback is never run, even after the pipe closes. */
static void check_stop_without_firing(void) {
    int p[2];
    lt_probe pr;
    hqq_lifeline_options o;
    hqq_lifeline *l = NULL;
    int calls = 0;
    hqq_lifeline_reason reason = HQQ_LIFELINE_REASON_EOF;
    HQQ_REQUIRE(pipe(p) == 0);
    probe_init(&pr);
    hqq_lifeline_options_init(&o);
    o.fd = p[0];
    o.callback = probe_cb;
    o.ctx = &pr;
    HQQ_CHECK_STATUS(hqq_lifeline_start(&o, &l), HQQ_OK);
    HQQ_CHECK(l != NULL && hqq_lifeline_watching(l) == 1);
    hqq_lifeline_stop(l);
    close(p[1]);
    hqq_test_sleep_ms(100);
    probe_snapshot(&pr, &calls, &reason);
    HQQ_CHECK_EQ_INT(calls, 0);
    close(p[0]);
}

/* A descriptor that is not open fires ERROR (EBADF), once. The number is picked
 * above the descriptors in use so that the self-pipe cannot take it. */
static void check_bad_descriptor_fires_error(void) {
    int bad = 100;
    lt_probe pr;
    hqq_lifeline_options o;
    hqq_lifeline *l = NULL;
    int calls = 0;
    hqq_lifeline_reason reason = HQQ_LIFELINE_REASON_EOF;
    while (fcntl(bad, F_GETFD) != -1 && bad < 1000) {
        bad++;
    }
    HQQ_REQUIRE(bad < 1000 && errno == EBADF);
    probe_init(&pr);
    hqq_lifeline_options_init(&o);
    o.fd = bad;
    o.callback = probe_cb;
    o.ctx = &pr;
    HQQ_CHECK_STATUS(hqq_lifeline_start(&o, &l), HQQ_OK);
    HQQ_CHECK(probe_wait(&pr, 5000));
    hqq_test_sleep_ms(50);
    probe_snapshot(&pr, &calls, &reason);
    HQQ_CHECK_EQ_INT(calls, 1);
    HQQ_CHECK_EQ_INT(reason, HQQ_LIFELINE_REASON_ERROR);
    hqq_lifeline_stop(l);
}

/* Start and stop: two lifelines at once, repeated start/stop, NULL stop, and the
 * argument errors. */
static void check_start_stop_safety(void) {
    int a[2], b[2], i;
    lt_probe pa, pb;
    hqq_lifeline_options o;
    hqq_lifeline *la = NULL, *lb = NULL, *lc = NULL;
    HQQ_REQUIRE(pipe(a) == 0);
    HQQ_REQUIRE(pipe(b) == 0);
    probe_init(&pa);
    probe_init(&pb);
    hqq_lifeline_options_init(&o);
    o.fd = a[0];
    o.callback = probe_cb;
    o.ctx = &pa;
    HQQ_CHECK_STATUS(hqq_lifeline_start(&o, &la), HQQ_OK);
    o.fd = b[0];
    o.ctx = &pb;
    HQQ_CHECK_STATUS(hqq_lifeline_start(&o, &lb), HQQ_OK);
    hqq_lifeline_stop(lb);
    hqq_lifeline_stop(la);
    hqq_lifeline_stop(NULL);

    for (i = 0; i < 20; i++) {
        lt_probe pc;
        probe_init(&pc);
        o.fd = a[0];
        o.ctx = &pc;
        HQQ_CHECK_STATUS(hqq_lifeline_start(&o, &lc), HQQ_OK);
        hqq_lifeline_stop(lc);
        lc = NULL;
    }

    /* Argument errors: no out pointer, a wrong struct size. */
    HQQ_CHECK_STATUS(hqq_lifeline_start(&o, NULL), HQQ_ERR_INVALID_ARGUMENT);
    o.struct_size = 0;
    HQQ_CHECK_STATUS(hqq_lifeline_start(&o, &lc), HQQ_ERR_INVALID_ARGUMENT);
    HQQ_CHECK(lc == NULL);
    close(a[0]);
    close(a[1]);
    close(b[0]);
    close(b[1]);
}

/* The grace period: a callback that does not lead to a stop gets a forced exit
 * with HQQ_LIFELINE_EXIT_CODE; a stop within the grace period exits cleanly. */
static void check_grace(void) {
    int p[2];
    pid_t pid;
    int st = 0;
    HQQ_REQUIRE(pipe(p) == 0);
    pid = fork();
    HQQ_REQUIRE(pid >= 0);
    if (pid == 0) {
        alarm(LT_CHILD_GUARD_S);
        close(p[1]);
        dup2(p[0], STDIN_FILENO);
        close(p[0]);
        body_grace_no_stop();
    }
    close(p[0]);
    HQQ_CHECK(!wait_exit(pid, 100, &st));
    close(p[1]);
    HQQ_CHECK(wait_exit(pid, 10000, &st));
    HQQ_CHECK(WIFEXITED(st) && WEXITSTATUS(st) == HQQ_LIFELINE_EXIT_CODE);

    HQQ_REQUIRE(pipe(p) == 0);
    pid = fork();
    HQQ_REQUIRE(pid >= 0);
    if (pid == 0) {
        alarm(LT_CHILD_GUARD_S);
        close(p[1]);
        dup2(p[0], STDIN_FILENO);
        close(p[0]);
        body_grace_stop_in_time();
    }
    close(p[0]);
    close(p[1]);
    HQQ_CHECK(wait_exit(pid, 10000, &st));
    HQQ_CHECK(exited_with(st, 0));
}

/* The supervisor is SIGKILLed while the service holds only the pipe's read end:
 * the service sees EOF and exits. The supervisor is an intermediary process that
 * holds the write end and forks the service. The test is not the service's parent,
 * so it learns of the service's exit from a pipe that only the service holds. */
static void check_intermediary_sigkill(void) {
    int in[2], rep[2], alive[2];
    pid_t inter;
    int got = 0;
    char b = 0;
    uint64_t t0, t1;
    HQQ_REQUIRE(pipe(in) == 0);
    HQQ_REQUIRE(pipe(rep) == 0);
    HQQ_REQUIRE(pipe(alive) == 0);
    inter = fork();
    HQQ_REQUIRE(inter >= 0);
    if (inter == 0) {
        pid_t svc;
        alarm(LT_CHILD_GUARD_S);
        close(in[0]);
        close(rep[0]);
        close(alive[0]);
        svc = fork();
        if (svc == 0) {
            lt_probe pr;
            hqq_lifeline_options o;
            hqq_lifeline *l = NULL;
            int calls = 0, code = 1;
            hqq_lifeline_reason reason = HQQ_LIFELINE_REASON_ERROR;
            alarm(LT_CHILD_GUARD_S);
            close(in[1]); /* only the intermediary holds the write end */
            dup2(in[0], STDIN_FILENO);
            close(in[0]);
            probe_init(&pr);
            hqq_lifeline_options_init(&o);
            o.callback = probe_cb;
            o.ctx = &pr;
            if (hqq_lifeline_start(&o, &l) == HQQ_OK) {
                (void)write(rep[1], "s", 1); /* started and watching */
                if (probe_wait(&pr, 20000)) {
                    probe_snapshot(&pr, &calls, &reason);
                    if (calls == 1 && reason == HQQ_LIFELINE_REASON_EOF) {
                        (void)write(rep[1], "e", 1);
                        code = 0;
                    } else {
                        code = 2;
                    }
                }
            }
            hqq_lifeline_stop(l);
            _exit(code); /* closes alive[1]: the test sees EOF on alive */
        }
        close(alive[1]); /* the service keeps the only copy */
        for (;;) {
            pause();
        }
    }
    close(in[0]);
    close(in[1]);
    close(rep[1]);
    close(alive[1]);
    /* Wait for the service's "started" byte, then SIGKILL the intermediary: its
     * death closes the last write end of the service's stdin. */
    HQQ_CHECK(read(rep[0], &b, 1) == 1 && b == 's');
    t0 = hqq_test_now_ns();
    HQQ_CHECK(kill(inter, SIGKILL) == 0);
    {
        int st = 0;
        HQQ_CHECK(wait_exit(inter, 5000, &st));
        HQQ_CHECK(WIFSIGNALED(st) && WTERMSIG(st) == SIGKILL);
    }
    /* The service's exit closes alive[1]: EOF on alive ends this wait. */
    {
        char tmp[16];
        ssize_t r;
        do {
            r = read(alive[0], tmp, sizeof tmp);
        } while (r > 0 || (r < 0 && errno == EINTR));
    }
    t1 = hqq_test_now_ns();
    /* The service's firing byte, then EOF on rep (only the service held the write end). */
    for (;;) {
        ssize_t r = read(rep[0], &b, 1);
        if (r == 1) {
            if (b == 'e') {
                got = 1;
            }
            continue;
        }
        if (r < 0 && errno == EINTR) {
            continue;
        }
        break;
    }
    HQQ_CHECK(got);
    HQQ_CHECK(t1 - t0 < 2000u * 1000000u); /* the spec's "about 1 s", with slack for a loaded host */
    fprintf(stderr, "test_lifeline: service exit after SIGKILL of the supervisor: %.1f ms\n",
            (double)(t1 - t0) / 1e6);
    close(rep[0]);
    close(alive[0]);
}

int main(void) {
    check_eof_fires_once();
    check_keepalive_discarded();
    check_tty_policy();
    check_stop_without_firing();
    check_bad_descriptor_fires_error();
    check_start_stop_safety();
    check_grace();
    check_intermediary_sigkill();
    return HQQ_TEST_RESULT("test_lifeline");
}
