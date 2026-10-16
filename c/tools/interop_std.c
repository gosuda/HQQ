/*
 * interop_std.c - the Standard scenarios of the C interop helper (X-01 .. X-21,
 * conformance/INTEROP_CLI.md). Each scenario follows cmd/hqq-interop/std_*.go
 * and raw_*.go: the same variants, steps, roles, expectations and check names
 * (where the C API can express them).
 */
#include "interop_internal.h"

#include <inttypes.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "hqq_internal.h"

#define NS_PER_MS INT64_C(1000000)
#define STD_SLACK_NS (1500 * NS_PER_MS)
#define STD_READ_BOUND_NS (5000 * NS_PER_MS)
#define STD_PERM 0600u
#define NELEM(a) ((int)(sizeof(a) / sizeof((a)[0])))
#define IHX_PATH_LEN 1024

/* ---- Configuration -------------------------------------------------------------- */

static hqq_options std_options(const ihx_config *c) {
    hqq_options o;
    hqq_options_init(&o);
    o.heartbeat_interval_ns = c->heartbeat_ns;
    if (c->peer_timeout_ns > 0) {
        o.peer_timeout_ns = c->peer_timeout_ns;
    }
    o.attach_timeout_ns = c->attach_timeout_ns;
    o.wait_policy = c->spin ? HQQ_WAIT_SPIN : HQQ_WAIT_ADAPTIVE;
    return o;
}

/* stdPeerTimeout, stdStaleAfter and stdPeerBound of the Go helper (SPEC 6.3). */
static int64_t peer_timeout_ns(const ihx_config *c) {
    return c->peer_timeout_ns > 0 ? c->peer_timeout_ns : HQQ_DEFAULT_PEER_TIMEOUT_NS;
}

static int64_t heartbeat_interval_ns(const ihx_config *c) {
    return c->heartbeat_ns > 0 ? c->heartbeat_ns : HQQ_DEFAULT_HEARTBEAT_INTERVAL_NS;
}

static int64_t stale_after_ns(const ihx_config *c) {
    int64_t peer = peer_timeout_ns(c), four = 4 * heartbeat_interval_ns(c);
    return peer > four ? peer : four;
}

static int64_t peer_bound_ns(const ihx_config *c) { return stale_after_ns(c) + STD_SLACK_NS; }

/* ---- Link helpers ----------------------------------------------------------------- */

static hqq_status std_create(ihx_env *e, const char *name, hqq_link **out) {
    hqq_options o = std_options(&e->cfg);
    *out = NULL;
    return hqq_standard_create(name, e->cfg.buffer_count, e->cfg.buffer_size, STD_PERM, &o, out, NULL);
}

static hqq_status std_open(ihx_env *e, const char *name, hqq_link **out) {
    hqq_options o = std_options(&e->cfg);
    *out = NULL;
    return hqq_standard_open(name, e->cfg.buffer_count, e->cfg.buffer_size, &o, out, NULL);
}

static void std_close(hqq_link **l) {
    if (*l != NULL) {
        (void)hqq_close(*l);
        *l = NULL;
    }
}

static hqq_status std_close_checked(hqq_link **l) {
    hqq_status st = HQQ_OK;
    if (*l != NULL) {
        st = hqq_close(*l);
        *l = NULL;
    }
    return st;
}

/* Bound of one blocking call: the time left to the process deadline, or cap_ns when smaller. */
static hqq_wait std_wait(const ihx_env *e, int64_t cap_ns) {
    hqq_wait w;
    int64_t rem = ihx_remaining_ns(e);
    if (rem < 1) {
        rem = 1;
    }
    if (cap_ns > 0 && cap_ns < rem) {
        rem = cap_ns;
    }
    w.timeout_ns = rem;
    w.cancel = NULL;
    return w;
}

static hqq_status std_send(ihx_env *e, hqq_link *l, const void *p, size_t n, int64_t cap_ns) {
    hqq_wait w = std_wait(e, cap_ns);
    return hqq_write(l, p, n, &w);
}

static hqq_status std_recv(ihx_env *e, hqq_link *l, void *buf, size_t cap, size_t *n, int64_t cap_ns) {
    hqq_wait w = std_wait(e, cap_ns);
    *n = 0;
    return hqq_read(l, buf, cap, n, &w);
}

/* Sends Payload(seed, seq, size) as one message. scratch holds size bytes. */
static int send_payload(ihx_env *e, hqq_link *l, uint64_t seed, uint64_t seq, uint8_t *scratch, const char *what) {
    hqq_status st;
    ihx_payload(seed, seq, scratch, e->cfg.size);
    st = std_send(e, l, scratch, e->cfg.size, 0);
    if (st != HQQ_OK) {
        return ihx_fail_status(e, st, "%s %" PRIu64, what, seq);
    }
    return IHX_OK;
}

/* Receives one message of exactly size bytes into buf (size bytes). */
static int recv_message(ihx_env *e, hqq_link *l, uint8_t *buf, const char *what, uint64_t seq) {
    size_t n = 0;
    hqq_status st = std_recv(e, l, buf, e->cfg.size, &n, 0);
    if (st != HQQ_OK) {
        return ihx_fail_status(e, st, "%s %" PRIu64, what, seq);
    }
    if (n != e->cfg.size) {
        return ihx_fail(e, "CheckFailed", "%s %" PRIu64 ": %zu bytes, want %zu", what, seq, n, e->cfg.size);
    }
    return IHX_OK;
}

/* Creates the segment and posts the "created" barrier (makes it openable). */
static int create_signal(ihx_env *e, hqq_link **out) {
    hqq_status st = std_create(e, e->cfg.name, out);
    if (st != HQQ_OK) {
        return ihx_fail_status(e, st, "create %s", e->cfg.name);
    }
    if (ihx_barrier(e, "created") != IHX_OK) {
        std_close(out);
        return IHX_FAIL;
    }
    return IHX_OK;
}

/* Waits for the creator's "created" barrier, then opens the segment. */
static int open_after(ihx_env *e, hqq_link **out) {
    hqq_status st;
    if (ihx_barrier(e, "created") != IHX_OK) {
        return IHX_FAIL;
    }
    st = std_open(e, e->cfg.name, out);
    if (st != HQQ_OK) {
        return ihx_fail_status(e, st, "open %s", e->cfg.name);
    }
    return IHX_OK;
}

/* Creates the segment, retrying while the endpoints of a previous session are
 * still alive (ErrLinkBusy until their heartbeats age out). */
static hqq_status create_retry(ihx_env *e, int64_t limit_ns, hqq_link **out) {
    uint64_t deadline = ihx_now_ns() + (uint64_t)limit_ns;
    for (;;) {
        hqq_status st = std_create(e, e->cfg.name, out);
        if (st != HQQ_ERR_LINK_BUSY || ihx_now_ns() > deadline) {
            return st;
        }
        ihx_sleep_ns(e, 20 * NS_PER_MS);
        if (ihx_remaining_ns(e) <= 0) {
            return HQQ_ERR_TIMEOUT;
        }
    }
}

/* Opens and closes the segment: the status of the probe. */
static hqq_status probe_open(ihx_env *e, const char *name) {
    hqq_link *p = NULL;
    hqq_status st = std_open(e, name, &p);
    std_close(&p);
    return st;
}

static int64_t elapsed_since(uint64_t t0) { return (int64_t)((ihx_now_ns() - t0) / 1000000u); }

/* ---- X-01: create, open, exchange, close ------------------------------------------------ */

static int x01_run(ihx_env *e) {
    size_t size = e->cfg.size;
    uint64_t seed = e->cfg.seed;
    int creator = e->cfg.role == 0;
    int creator_first = strcmp(e->cfg.variant, "creator-closes-first") == 0;
    hqq_link *l = NULL;
    uint8_t *buf = calloc(1, size), *pay = calloc(1, size);
    int rc = IHX_OK;
    if (buf == NULL || pay == NULL) {
        rc = ihx_fail(e, "Unknown", "out of memory");
        goto done;
    }
    rc = creator ? create_signal(e, &l) : open_after(e, &l);
    if (rc != IHX_OK) {
        goto done;
    }
    if (creator) {
        rc = send_payload(e, l, seed, 0, pay, "send");
        if (rc != IHX_OK) {
            goto done;
        }
        e->sent++;
        rc = recv_message(e, l, buf, "read", 0);
        if (rc != IHX_OK) {
            goto done;
        }
        e->received++;
        ihx_check(e, "creator receives the reply intact", ihx_payload_verify(seed + 1, 0, buf, size) == 0, "");
    } else {
        rc = recv_message(e, l, buf, "read", 0);
        if (rc != IHX_OK) {
            goto done;
        }
        e->received++;
        ihx_check(e, "opener receives the message intact", ihx_payload_verify(seed, 0, buf, size) == 0, "");
        rc = send_payload(e, l, seed + 1, 0, pay, "write");
        if (rc != IHX_OK) {
            goto done;
        }
        e->sent++;
    }
    rc = ihx_barrier(e, "replied");
    if (rc != IHX_OK) {
        goto done;
    }

    if (creator == creator_first) {
        /* This side closes first. */
        hqq_status st = std_close_checked(&l);
        if (st != HQQ_OK) {
            rc = ihx_fail_status(e, st, "close");
            goto done;
        }
        if (creator) {
            ihx_check_status(e, "creator close removes the name", probe_open(e, e->cfg.name), HQQ_ERR_NOT_EXIST);
        } else {
            ihx_check_status(e, "opener close leaves the name in place", probe_open(e, e->cfg.name), HQQ_ERR_LINK_BUSY);
        }
        rc = ihx_barrier(e, "closed");
        goto done;
    }

    /* The other side waits for the peer's close, then sees the end of stream. */
    {
        size_t n = 0;
        hqq_status st = std_recv(e, l, buf, size, &n, 0);
        ihx_check_status(e, "peer close is seen as io.EOF after buffered data", st, HQQ_ERR_PEER_CLOSED);
    }
    rc = ihx_barrier(e, "closed");
    if (rc != IHX_OK) {
        goto done;
    }
    if (creator) {
        /* The opener closed first; the creator's close must now remove the name. */
        hqq_status st = std_close_checked(&l);
        if (st != HQQ_OK) {
            rc = ihx_fail_status(e, st, "close");
            goto done;
        }
        ihx_check_status(e, "creator close removes the name", probe_open(e, e->cfg.name), HQQ_ERR_NOT_EXIST);
    }

done:
    std_close(&l);
    free(buf);
    free(pay);
    return rc;
}

/* ---- X-02: geometry mismatch (count, size) ------------------------------------------------- */

static int x02_run(ihx_env *e) {
    hqq_link *l = NULL;
    hqq_status st;
    int rc;
    if (e->cfg.role == 0) {
        rc = create_signal(e, &l);
        if (rc != IHX_OK) {
            return rc;
        }
        ihx_check(e, "creator creates the segment", 1, "");
        rc = ihx_barrier(e, "checked");
        std_close(&l);
        return rc;
    }
    rc = ihx_barrier(e, "created");
    if (rc != IHX_OK) {
        return rc;
    }
    st = std_open(e, e->cfg.name, &l);
    std_close(&l);
    ihx_check_status(e, "mismatched geometry fails with ErrInvalidSize", st, HQQ_ERR_INVALID_SIZE);
    ihx_meta_bool(e, "geometry_error", st == HQQ_ERR_GEOMETRY_MISMATCH);
    return ihx_barrier(e, "checked");
}

/* ---- X-03: third endpoint, re-join after own close, creator vs live segment ---------------------- */

static int x03_run(ihx_env *e) {
    hqq_link *l = NULL, *third = NULL, *again = NULL;
    hqq_status st;
    int rc;
    if (e->cfg.role == 0) {
        rc = create_signal(e, &l);
        if (rc != IHX_OK) {
            return rc;
        }
        rc = ihx_barrier(e, "joined");
        if (rc == IHX_OK) {
            st = std_create(e, e->cfg.name, &again);
            std_close(&again);
            ihx_check_status(e, "creator on a live name fails with ErrLinkBusy", st, HQQ_ERR_LINK_BUSY);
            rc = ihx_barrier(e, "creator-checked");
        }
        if (rc == IHX_OK) {
            rc = ihx_barrier(e, "reopen-done");
        }
        std_close(&l);
        return rc;
    }

    rc = open_after(e, &l);
    if (rc != IHX_OK) {
        return rc;
    }
    rc = ihx_barrier(e, "joined");
    if (rc != IHX_OK) {
        std_close(&l);
        return rc;
    }
    st = std_open(e, e->cfg.name, &third);
    std_close(&third);
    ihx_check_status(e, "third endpoint fails with ErrLinkBusy", st, HQQ_ERR_LINK_BUSY);
    rc = ihx_barrier(e, "creator-checked");
    if (rc != IHX_OK) {
        std_close(&l);
        return rc;
    }
    st = std_close_checked(&l);
    if (st != HQQ_OK) {
        return ihx_fail_status(e, st, "close");
    }
    st = std_open(e, e->cfg.name, &again);
    std_close(&again);
    ihx_check_status(e, "re-join after own close fails with ErrLinkBusy", st, HQQ_ERR_LINK_BUSY);
    return ihx_barrier(e, "reopen-done");
}

/* ---- X-04: opener before creator, opener on an initializing or dead segment ----------------------- */

/* Opens an initializing segment with the configured attach timeout: ErrFailedInit
 * within that timeout, and the first page unchanged. */
static int open_during_init(ihx_env *e) {
    ihx_view before, after;
    hqq_link *l = NULL;
    hqq_status st;
    int64_t attach = e->cfg.attach_timeout_ns > 0 ? e->cfg.attach_timeout_ns : HQQ_DEFAULT_ATTACH_TIMEOUT_NS;
    uint64_t t0;
    int64_t elapsed;
    if (ihx_observe(e, e->cfg.name, &before) != IHX_OK) {
        return IHX_FAIL;
    }
    t0 = ihx_now_ns();
    st = std_open(e, e->cfg.name, &l);
    elapsed = elapsed_since(t0);
    std_close(&l);
    ihx_check_status(e, "open of an initializing segment fails with ErrFailedInit", st, HQQ_ERR_FAILED_INIT);
    ihx_check(e, "failure comes within the attach timeout", elapsed <= (attach + STD_SLACK_NS) / NS_PER_MS,
              "%lld ms, attach %lld ms", (long long)elapsed, (long long)(attach / NS_PER_MS));
    if (ihx_observe(e, e->cfg.name, &after) != IHX_OK) {
        return IHX_FAIL;
    }
    ihx_check(e, "opener wrote nothing to the first page", before.page_crc == after.page_crc, "");
    return IHX_OK;
}

static int x04_before_creator_opener(ihx_env *e) {
    hqq_link *l = NULL;
    hqq_status st;
    uint64_t t0 = ihx_now_ns();
    int rc;
    st = std_open(e, e->cfg.name, &l);
    std_close(&l);
    ihx_check(e, "first attempt before the creator fails transiently",
              st == HQQ_ERR_NOT_EXIST || hqq_error_is(st, HQQ_ERR_FAILED_INIT) != 0, "%s", hqq_strerror(st));
    ihx_meta_u64(e, "first_attempt_ms", (uint64_t)elapsed_since(t0));
    rc = ihx_barrier(e, "attempt");
    if (rc == IHX_OK) {
        rc = ihx_barrier(e, "created");
    }
    if (rc != IHX_OK) {
        return rc;
    }
    st = std_open(e, e->cfg.name, &l);
    if (st != HQQ_OK) {
        return ihx_fail_status(e, st, "open %s", e->cfg.name);
    }
    ihx_check(e, "opener joins as Secondary", hqq_link_mode(l) == HQQ_MODE_SECONDARY,
              "mode %d", (int)hqq_link_mode(l));
    rc = ihx_barrier(e, "joined");
    std_close(&l);
    return rc;
}

static int x04_before_creator_creator(ihx_env *e) {
    hqq_link *l = NULL;
    hqq_status st;
    int rc = ihx_barrier(e, "attempt");
    if (rc != IHX_OK) {
        return rc;
    }
    st = std_create(e, e->cfg.name, &l);
    if (st != HQQ_OK) {
        return ihx_fail_status(e, st, "create %s", e->cfg.name);
    }
    rc = ihx_barrier(e, "created");
    if (rc == IHX_OK) {
        rc = ihx_barrier(e, "joined");
    }
    std_close(&l);
    return rc;
}

/* Dead segment: step 1 joins and the creator dies at exit; step 2 is the opener
 * on the dead segment, which must fail quickly with ErrFailedInit. */
static int x04_dead(ihx_env *e) {
    hqq_link *l = NULL;
    hqq_status st;
    int rc;
    if (e->cfg.step == 2) {
        uint64_t t0 = ihx_now_ns();
        int64_t elapsed, bound = stale_after_ns(&e->cfg) + 3000 * NS_PER_MS;
        st = std_open(e, e->cfg.name, &l);
        elapsed = (int64_t)(ihx_now_ns() - t0);
        std_close(&l);
        ihx_check_status(e, "opener on a dead segment fails with ErrFailedInit", st, HQQ_ERR_FAILED_INIT);
        ihx_check(e, "failure is quick (errNoLiveEndpoint)", elapsed <= bound, "%lld ms, bound %lld ms",
                  (long long)(elapsed / NS_PER_MS), (long long)(bound / NS_PER_MS));
        return IHX_OK;
    }
    if (e->cfg.role == 0) {
        rc = create_signal(e, &l);
        if (rc != IHX_OK) {
            return rc;
        }
        rc = ihx_barrier(e, "joined");
        if (rc == IHX_OK) {
            ihx_crash_point(e, "exit");
        }
        std_close(&l);
        return rc;
    }
    rc = open_after(e, &l);
    if (rc != IHX_OK) {
        return rc;
    }
    rc = ihx_barrier(e, "joined");
    std_close(&l);
    return rc;
}

static int x04_run(ihx_env *e) {
    if (strcmp(e->cfg.variant, "initializing") == 0) {
        if (e->cfg.role == 0) {
            return ihx_raw_init_victim(e);
        }
        return open_during_init(e);
    }
    if (strcmp(e->cfg.variant, "dead") == 0) {
        return x04_dead(e);
    }
    return e->cfg.role == 1 ? x04_before_creator_opener(e) : x04_before_creator_creator(e);
}

/* ---- X-05: ring full / backpressure, deadline on a full ring ----------------------------------- */

static int x05_run(ihx_env *e) {
    size_t size = e->cfg.size;
    uint64_t seed = e->cfg.seed;
    hqq_link *l = NULL;
    uint8_t *buf = calloc(1, size), *pay = calloc(1, size);
    int rc = IHX_OK;
    if (buf == NULL || pay == NULL) {
        rc = ihx_fail(e, "Unknown", "out of memory");
        goto done;
    }
    if (e->cfg.role == 0) {
        hqq_wait w;
        hqq_status st;
        rc = create_signal(e, &l);
        if (rc != IHX_OK) {
            goto done;
        }
        for (uint64_t i = 0; i < 2; i++) {
            rc = send_payload(e, l, seed, i, pay, "message");
            if (rc != IHX_OK) {
                goto done;
            }
            e->sent++;
        }
        ihx_payload(seed, 2, pay, size);
        w.timeout_ns = 200 * NS_PER_MS;
        w.cancel = NULL;
        st = hqq_write(l, pay, size, &w);
        ihx_check_status(e, "third write on a full ring times out", st, HQQ_ERR_TIMEOUT);
        rc = ihx_barrier(e, "filled");
        if (rc == IHX_OK) {
            rc = ihx_barrier(e, "drained");
        }
        if (rc == IHX_OK) {
            rc = send_payload(e, l, seed, 2, pay, "message 2 after drain");
        }
        if (rc == IHX_OK) {
            e->sent++;
            rc = ihx_barrier(e, "received");
        }
        goto done;
    }
    rc = open_after(e, &l);
    if (rc != IHX_OK) {
        goto done;
    }
    rc = ihx_barrier(e, "filled");
    if (rc != IHX_OK) {
        goto done;
    }
    {
        int in_order = 1;
        for (uint64_t i = 0; i < 2; i++) {
            rc = recv_message(e, l, buf, "message", i);
            if (rc != IHX_OK) {
                goto done;
            }
            e->received++;
            if (ihx_payload_verify(seed, i, buf, size) != 0) {
                in_order = 0;
            }
        }
        ihx_check(e, "the two buffered messages arrive in order and intact", in_order, "");
    }
    rc = ihx_barrier(e, "drained");
    if (rc != IHX_OK) {
        goto done;
    }
    rc = recv_message(e, l, buf, "message 2", 2);
    if (rc != IHX_OK) {
        goto done;
    }
    e->received++;
    ihx_check(e, "the third message arrives intact after the drain", ihx_payload_verify(seed, 2, buf, size) == 0, "");
    rc = ihx_barrier(e, "received");

done:
    std_close(&l);
    free(buf);
    free(pay);
    return rc;
}

/* ---- Streams: X-06 (bidirectional), X-12, X-13, X-14 (ping-pong) ------------------------------- */

typedef struct bidi_tx {
    ihx_env *e;
    hqq_link *l;
    uint64_t n, seed, sent, fail_at;
    size_t size;
    hqq_status st;
} bidi_tx;

/* The writer of the bidirectional stream: n messages, Payload(seed, i). */
static void bidi_tx_main(void *arg) {
    bidi_tx *t = arg;
    uint8_t *pay = calloc(1, t->size);
    uint64_t i;
    if (pay == NULL) {
        t->st = HQQ_ERR_NO_MEMORY;
        return;
    }
    for (i = 0; i < t->n; i++) {
        hqq_status st;
        ihx_payload(t->seed, i, pay, t->size);
        st = std_send(t->e, t->l, pay, t->size, 0);
        if (st != HQQ_OK) {
            t->st = st;
            t->fail_at = i;
            break;
        }
        t->sent++;
    }
    free(pay);
}

/* stdBidirectional: the creator's stream uses seed, the opener's seed+1; each
 * side reads and verifies n messages while it writes n of its own. */
static int bidirectional(ihx_env *e, hqq_link *l, uint64_t n) {
    size_t size = e->cfg.size;
    uint64_t seed = e->cfg.seed;
    uint64_t out_seed = seed, in_seed = seed + 1, got = 0, rfail = 0, i;
    hqq_status rst = HQQ_OK;
    int verify_failed = 0, os_err = 0;
    uint8_t *buf = calloc(1, size);
    bidi_tx tx;
    hqq_plat_thread *th;
    if (buf == NULL) {
        return ihx_fail(e, "Unknown", "out of memory");
    }
    if (e->cfg.role == 1) {
        out_seed = seed + 1;
        in_seed = seed;
    }
    memset(&tx, 0, sizeof tx);
    tx.e = e;
    tx.l = l;
    tx.n = n;
    tx.seed = out_seed;
    tx.size = size;
    tx.st = HQQ_OK;
    th = hqq_plat_thread_start(NULL, bidi_tx_main, &tx, &os_err);
    if (th == NULL) {
        free(buf);
        return ihx_fail(e, "Unknown", "cannot start the writer thread (os error %d)", os_err);
    }
    for (i = 0; i < n; i++) {
        size_t rn = 0;
        rst = std_recv(e, l, buf, size, &rn, 0);
        if (rst != HQQ_OK) {
            rfail = i;
            break;
        }
        if (rn != size) {
            rst = HQQ_ERR_INVALID_SIZE;
            rfail = i;
            break;
        }
        if (ihx_payload_verify(in_seed, i, buf, size) != 0) {
            verify_failed = 1;
            rfail = i;
            break;
        }
        got++;
    }
    hqq_plat_thread_join(NULL, th);
    free(buf);
    e->sent = tx.sent;
    e->received = got;
    if (verify_failed) {
        ihx_check(e, "both streams intact and in order", 0, "message %" PRIu64 ": payload mismatch", rfail);
        return IHX_OK;
    }
    if (tx.st != HQQ_OK) {
        return ihx_fail_status(e, tx.st, "write %" PRIu64, tx.fail_at);
    }
    if (rst != HQQ_OK) {
        return ihx_fail_status(e, rst, "read %" PRIu64, rfail);
    }
    ihx_check(e, "both streams intact and in order", 1, "");
    return IHX_OK;
}

static int x06_run(ihx_env *e) {
    uint64_t n = e->cfg.messages;
    hqq_link *l = NULL;
    int rc;
    if (e->cfg.long_sizes) {
        n = 100000;
    }
    rc = e->cfg.role == 0 ? create_signal(e, &l) : open_after(e, &l);
    if (rc != IHX_OK) {
        return rc;
    }
    rc = bidirectional(e, l, n);
    if (rc == IHX_OK) {
        rc = ihx_barrier(e, "done");
    }
    std_close(&l);
    return rc;
}

/* stdPingPong: the creator sleeps idle, sends Payload(seed, i) and waits for the
 * echo; the opener echoes every message. The creator checks each echo and that
 * the worst round trip is at most max_rtt. */
static int ping_pong(ihx_env *e, hqq_link *l, uint64_t n, int64_t idle_ns, int64_t max_rtt_ns,
                     const char *rtt_check) {
    size_t size = e->cfg.size;
    uint64_t seed = e->cfg.seed, i;
    uint8_t *buf = calloc(1, size), *pay = calloc(1, size);
    int64_t worst = 0;
    int verify_bad = 0;
    int rc = IHX_OK;
    if (buf == NULL || pay == NULL) {
        rc = ihx_fail(e, "Unknown", "out of memory");
        goto done;
    }
    for (i = 0; i < n; i++) {
        if (e->cfg.role == 0) {
            uint64_t t0;
            ihx_sleep_ns(e, (uint64_t)idle_ns);
            if (ihx_remaining_ns(e) <= 0) {
                rc = ihx_fail_status(e, HQQ_ERR_TIMEOUT, "round %" PRIu64 ": idle", i);
                goto done;
            }
            t0 = ihx_now_ns();
            rc = send_payload(e, l, seed, i, pay, "round send");
            if (rc != IHX_OK) {
                goto done;
            }
            e->sent++;
            rc = recv_message(e, l, buf, "round echo", i);
            if (rc != IHX_OK) {
                goto done;
            }
            if ((int64_t)(ihx_now_ns() - t0) > worst) {
                worst = (int64_t)(ihx_now_ns() - t0);
            }
            if (ihx_payload_verify(seed, i, buf, size) != 0) {
                verify_bad = 1;
            }
            e->received++;
            continue;
        }
        rc = recv_message(e, l, buf, "round receive", i);
        if (rc != IHX_OK) {
            goto done;
        }
        e->received++;
        if (ihx_payload_verify(seed, i, buf, size) != 0) {
            verify_bad = 1;
        }
        {
            hqq_status st = std_send(e, l, buf, size, 0);
            if (st != HQQ_OK) {
                rc = ihx_fail_status(e, st, "round echo %" PRIu64, i);
                goto done;
            }
        }
        e->sent++;
    }
    ihx_check(e, "every message intact", !verify_bad, "");
    if (e->cfg.role == 0) {
        char detail[64];
        snprintf(detail, sizeof detail, "worst %lld ms", (long long)(worst / NS_PER_MS));
        ihx_check(e, rtt_check, worst <= max_rtt_ns, "%s", detail);
        ihx_meta_u64(e, "worst_rtt_ms", (uint64_t)(worst / NS_PER_MS));
    }

done:
    free(buf);
    free(pay);
    return rc;
}

static int ping_run(ihx_env *e, uint64_t n, int64_t idle_ns, int64_t max_rtt_ns, const char *rtt_check) {
    hqq_link *l = NULL;
    int rc = e->cfg.role == 0 ? create_signal(e, &l) : open_after(e, &l);
    if (rc != IHX_OK) {
        return rc;
    }
    rc = ping_pong(e, l, n, idle_ns, max_rtt_ns, rtt_check);
    if (rc == IHX_OK) {
        rc = ihx_barrier(e, "done");
    }
    std_close(&l);
    return rc;
}

/* ---- X-07, X-09 (peer kill and reuse), X-13 step 2: runKillPeer ----------------------------------- */

static int kill_peer_run(ihx_env *e) {
    uint64_t n = e->cfg.messages, i;
    size_t size = e->cfg.size;
    uint64_t seed = e->cfg.seed;
    int64_t bound = peer_bound_ns(&e->cfg);
    hqq_link *l = NULL;
    uint8_t *buf = calloc(1, size), *pay = calloc(1, size);
    int rc = IHX_OK;
    if (buf == NULL || pay == NULL) {
        rc = ihx_fail(e, "Unknown", "out of memory");
        goto done;
    }
    rc = e->cfg.role == 0 ? create_signal(e, &l) : open_after(e, &l);
    if (rc != IHX_OK) {
        goto done;
    }
    rc = ihx_barrier(e, "joined");
    if (rc != IHX_OK) {
        goto done;
    }
    if (e->cfg.role == 0) {
        char point[64];
        hqq_status st;
        uint64_t t0;
        int64_t elapsed;
        for (i = 0; i < n; i++) {
            rc = send_payload(e, l, seed, i, pay, "message");
            if (rc != IHX_OK) {
                goto done;
            }
            e->sent++;
        }
        snprintf(point, sizeof point, "msgs:%" PRIu64, n);
        ihx_crash_point(e, point);
        /* The opener was killed: its blocked read must end with ErrPeerGone. */
        t0 = ihx_now_ns();
        {
            size_t got = 0;
            st = std_recv(e, l, buf, size, &got, 0);
        }
        elapsed = (int64_t)(ihx_now_ns() - t0);
        ihx_check_status(e, "blocked read after the opener died gets ErrPeerGone", st, HQQ_ERR_PEER_GONE);
        ihx_check(e, "peer loss detected within the bound", elapsed <= bound, "%lld ms, bound %lld ms",
                  (long long)(elapsed / NS_PER_MS), (long long)(bound / NS_PER_MS));
        goto done;
    }
    {
        int intact = 1;
        for (i = 0; i < n; i++) {
            rc = recv_message(e, l, buf, "message", i);
            if (rc != IHX_OK) {
                goto done;
            }
            e->received++;
            if (ihx_payload_verify(seed, i, buf, size) != 0) {
                intact = 0;
            }
        }
        ihx_check(e, "all buffered messages delivered intact before the peer loss",
                  intact && e->received == n, "%" PRIu64 " of %" PRIu64, e->received, n);
    }
    ihx_crash_point(e, "blocked");
    {
        /* The creator was killed right after its last send: ErrPeerGone must follow
         * the buffered messages within the bound. */
        size_t got = 0;
        uint64_t t0 = ihx_now_ns();
        hqq_status st = std_recv(e, l, buf, size, &got, 0);
        int64_t elapsed = (int64_t)(ihx_now_ns() - t0);
        ihx_check_status(e, "read after the buffered messages gets ErrPeerGone", st, HQQ_ERR_PEER_GONE);
        ihx_check(e, "peer loss detected within the bound", elapsed <= bound, "%lld ms, bound %lld ms",
                  (long long)(elapsed / NS_PER_MS), (long long)(bound / NS_PER_MS));
    }

done:
    std_close(&l);
    free(buf);
    free(pay);
    return rc;
}

/* ---- X-08: kill -9 of the initializer between claim and publish -------------------------------- */

static int x08_run(ihx_env *e) {
    hqq_link *l = NULL;
    ihx_view v;
    hqq_status st;
    uint64_t t0;
    int64_t elapsed, stale;
    int rc;
    if (e->cfg.role == 1) {
        rc = open_during_init(e);
        if (rc != IHX_OK) {
            return rc;
        }
        return ihx_barrier(e, "opener-done");
    }
    if (e->cfg.crash_after[0] != '\0') {
        return ihx_raw_init_victim(e);
    }
    /* Survivor: the creator takes the initialization over after the stale window. */
    t0 = ihx_now_ns();
    st = std_create(e, e->cfg.name, &l);
    if (st != HQQ_OK) {
        return ihx_fail_status(e, st, "create %s", e->cfg.name);
    }
    elapsed = (int64_t)(ihx_now_ns() - t0);
    /* initStale = max(peer timeout, 2 s). */
    stale = peer_timeout_ns(&e->cfg) > 2000 * NS_PER_MS ? peer_timeout_ns(&e->cfg) : 2000 * NS_PER_MS;
    ihx_check(e, "takeover waited for the stale window", elapsed >= stale - 200 * NS_PER_MS,
              "%lld ms, window %lld ms", (long long)(elapsed / NS_PER_MS), (long long)(stale / NS_PER_MS));
    if (ihx_observe(e, e->cfg.name, &v) != IHX_OK) {
        std_close(&l);
        return IHX_FAIL;
    }
    ihx_check(e, "survivor completed the initialization (Ready, epoch 1)",
              v.state == HQQ_STATE_READY && v.magic == HQQ_SB_MAGIC && v.epoch == 1,
              "state %" PRIu64 " magic %#" PRIx64 " epoch %" PRIu64, v.state, v.magic, v.epoch);
    ihx_meta_u64(e, "epoch", v.epoch);
    rc = ihx_barrier(e, "opener-done");
    std_close(&l);
    return rc;
}

/* ---- X-09: reuse of a name after both endpoints died ---------------------------------------------- */

static int read_token(ihx_env *e, const char *tag, uint64_t *epoch, uint64_t *token) {
    char path[IHX_PATH_LEN];
    FILE *f;
    snprintf(path, sizeof path, "%s/%s.token", e->cfg.sync_dir, tag);
    f = fopen(path, "rb");
    if (f == NULL) {
        return ihx_fail(e, "Unknown", "%s.token: cannot be read", tag);
    }
    if (fscanf(f, "%" SCNu64 " %" SCNu64, epoch, token) != 2) {
        fclose(f);
        return ihx_fail(e, "Unknown", "%s.token: want \"epoch token\"", tag);
    }
    fclose(f);
    return IHX_OK;
}

static int write_token(ihx_env *e, const char *tag, uint64_t epoch, uint64_t token) {
    char path[IHX_PATH_LEN];
    FILE *f;
    snprintf(path, sizeof path, "%s/%s.token", e->cfg.sync_dir, tag);
    f = fopen(path, "wb");
    if (f == NULL) {
        return ihx_fail(e, "Unknown", "%s.token: cannot be written", tag);
    }
    fprintf(f, "%" PRIu64 " %" PRIu64 "\n", epoch, token);
    fclose(f);
    return IHX_OK;
}

/* One session of steps 1 and 2. A reusing creator waits for the old endpoints to die. */
static int x09_session(ihx_env *e, const char *tag, int reuse) {
    size_t size = e->cfg.size;
    uint64_t seed = e->cfg.seed;
    hqq_link *l = NULL;
    ihx_view view;
    uint8_t *buf = calloc(1, size), *pay = calloc(1, size);
    int rc = IHX_OK;
    hqq_status st;
    if (buf == NULL || pay == NULL) {
        rc = ihx_fail(e, "Unknown", "out of memory");
        goto done;
    }
    if (e->cfg.role == 0) {
        st = reuse ? create_retry(e, 15000 * NS_PER_MS, &l) : std_create(e, e->cfg.name, &l);
        if (st != HQQ_OK) {
            rc = ihx_fail_status(e, st, "create %s", e->cfg.name);
            goto done;
        }
        rc = ihx_barrier(e, "created");
    } else {
        rc = ihx_barrier(e, "created");
        if (rc == IHX_OK) {
            st = std_open(e, e->cfg.name, &l);
            if (st != HQQ_OK) {
                rc = ihx_fail_status(e, st, "open %s", e->cfg.name);
            }
        }
    }
    if (rc != IHX_OK) {
        goto done;
    }
    rc = ihx_barrier(e, "joined");
    if (rc != IHX_OK) {
        goto done;
    }
    if (ihx_observe(e, e->cfg.name, &view) != IHX_OK) {
        rc = IHX_FAIL;
        goto done;
    }
    if (e->cfg.role == 0) {
        if (reuse) {
            uint64_t old_epoch = 0, old_token = 0;
            rc = read_token(e, "x09-1", &old_epoch, &old_token);
            if (rc != IHX_OK) {
                goto done;
            }
            ihx_check(e, "reuse advances the epoch by one", view.epoch == old_epoch + 1,
                      "old %" PRIu64 " new %" PRIu64, old_epoch, view.epoch);
            ihx_check(e, "reuse takes a new creatorToken", view.creator_token != 0 && view.creator_token != old_token,
                      "old %#" PRIx64 " new %#" PRIx64, old_token, view.creator_token);
            ihx_meta_u64(e, "old_epoch", old_epoch);
        }
        {
            char tok[32];
            snprintf(tok, sizeof tok, "0x%" PRIx64, view.creator_token);
            ihx_meta_u64(e, "epoch", view.epoch);
            ihx_meta_str(e, "creator_token", tok);
        }
        rc = send_payload(e, l, seed, 0, pay, "write");
        if (rc != IHX_OK) {
            goto done;
        }
        e->sent++;
        rc = write_token(e, tag, view.epoch, view.creator_token);
    } else {
        rc = recv_message(e, l, buf, "read", 0);
        if (rc != IHX_OK) {
            goto done;
        }
        e->received++;
        ihx_check(e, "opener reads the message intact", ihx_payload_verify(seed, 0, buf, size) == 0, "");
        ihx_meta_u64(e, "epoch", view.epoch);
    }
    if (rc != IHX_OK) {
        goto done;
    }
    rc = ihx_barrier(e, "exchanged");
    if (rc != IHX_OK) {
        goto done;
    }
    if (e->cfg.role == 1 && !reuse) {
        /* The creator dies at exit right after the barrier: its heartbeat stops, so
         * this read must end with ErrPeerGone within the bound. */
        size_t got = 0;
        uint64_t t0 = ihx_now_ns();
        hqq_status rs = std_recv(e, l, buf, size, &got, 0);
        int64_t elapsed = (int64_t)(ihx_now_ns() - t0);
        int64_t bound = peer_bound_ns(&e->cfg);
        ihx_check_status(e, "creator's death seen as ErrPeerGone", rs, HQQ_ERR_PEER_GONE);
        ihx_check(e, "peer loss detected within the bound", elapsed <= bound, "%lld ms, bound %lld ms",
                  (long long)(elapsed / NS_PER_MS), (long long)(bound / NS_PER_MS));
    }
    ihx_crash_point(e, "exit");

done:
    std_close(&l);
    free(buf);
    free(pay);
    return rc;
}

/* Step 3: an opener on a segment whose endpoints are all dead. */
static int x09_reopen(ihx_env *e) {
    uint64_t old_epoch = 0, old_token = 0, new_epoch = 0, new_token = 0;
    hqq_link *l = NULL;
    ihx_view view;
    hqq_status st;
    int rc = read_token(e, "x09-1", &old_epoch, &old_token);
    if (rc != IHX_OK) {
        return rc;
    }
    rc = read_token(e, "x09-2", &new_epoch, &new_token);
    if (rc != IHX_OK) {
        return rc;
    }
    ihx_meta_u64(e, "old_epoch", old_epoch);
    ihx_meta_u64(e, "epoch", new_epoch);
    /* Both endpoints of step 2 are dead: wait until their heartbeats age out. */
    ihx_sleep_ns(e, (uint64_t)peer_bound_ns(&e->cfg));
    if (ihx_remaining_ns(e) <= 0) {
        return ihx_fail_status(e, HQQ_ERR_TIMEOUT, "wait for the dead endpoints");
    }
    st = std_open(e, e->cfg.name, &l);
    std_close(&l);
    ihx_check_status(e, "open after both endpoints died fails with ErrFailedInit", st, HQQ_ERR_FAILED_INIT);
    if (ihx_observe(e, e->cfg.name, &view) != IHX_OK) {
        return IHX_FAIL;
    }
    ihx_check(e, "segment still carries the step-2 epoch", view.epoch == new_epoch && new_epoch == old_epoch + 1,
              "old %" PRIu64 ", step 2 %" PRIu64 ", now %" PRIu64, old_epoch, new_epoch, view.epoch);
    return IHX_OK;
}

static int x09_run(ihx_env *e) {
    switch (e->cfg.step) {
    case 1: return x09_session(e, "x09-1", 0);
    case 2: return x09_session(e, "x09-2", 1);
    default: return x09_reopen(e);
    }
}

/* ---- X-10: safe unlink (linux, darwin) -------------------------------------------------------------- */

static int x10_run(ihx_env *e) {
    const char *name = e->cfg.name;
    hqq_link *l = NULL, *gone = NULL;
    hqq_status st;
    int rc = IHX_OK;
    int os_err = 0;
    st = std_create(e, name, &l);
    if (st != HQQ_OK) {
        return ihx_fail_status(e, st, "create %s", name);
    }
    ihx_check_status(e, "unlink of the live name returns nil", hqq_unlink(name), HQQ_OK);
    st = probe_open(e, name);
    ihx_check_status(e, "name is gone after unlink", st, HQQ_ERR_NOT_EXIST);
    {
        size_t page = (size_t)hqq_plat_page_size();
        hqq_shm *repl = NULL;
        hqq_status rs = hqq_plat_shm_open(NULL, name, 2 * page, 1, STD_PERM, &repl, &os_err);
        if (rs != HQQ_OK) {
            std_close(&l);
            return ihx_fail_status(e, rs, "creating the replacement object");
        }
        hqq_plat_shm_close(NULL, repl);
    }
    ihx_check_status(e, "creator close returns nil while the name was replaced", std_close_checked(&l), HQQ_OK);
    {
        hqq_shm *probe = NULL;
        hqq_status ps = hqq_plat_shm_open(NULL, name, 0, 0, 0, &probe, &os_err);
        if (ps != HQQ_OK) {
            ihx_check(e, "replacement survives the creator's close", 0, "%s", hqq_strerror(ps));
        } else {
            hqq_plat_shm_close(NULL, probe);
            ihx_check(e, "replacement survives the creator's close", 1, "");
        }
    }
    ihx_check_status(e, "unlink removes the replacement", hqq_unlink(name), HQQ_OK);
    /* SPEC 11.4: an endpoint's Unlink of a missing name is a no-op returning success. */
    st = std_create(e, name, &gone);
    if (st != HQQ_OK) {
        return ihx_fail_status(e, st, "create %s", name);
    }
    ihx_check_status(e, "package Unlink removes the live name", hqq_unlink(name), HQQ_OK);
    ihx_check_status(e, "endpoint Unlink of a missing name is a no-op", hqq_link_unlink(gone), HQQ_OK);
    ihx_check_status(e, "close after the name went away returns nil", std_close_checked(&gone), HQQ_OK);
    std_close(&gone);
    std_close(&l);
    return rc;
}

/* ---- X-11: invalid names per platform, long names on macOS -------------------------------------------- */

/* A check name for an invalid name: the name quoted, shortened when long. */
static void name_label(char *out, size_t cap, const char *name, const char *suffix) {
    size_t n = strlen(name);
    if (n > 40) {
        snprintf(out, cap, "create \"%.40s...(%zu bytes)\" -> %s", name, n, suffix);
    } else {
        snprintf(out, cap, "create \"%s\" -> %s", name, suffix);
    }
}

static int x11_run(ihx_env *e) {
    static const char *const fixed[] = {"", "/", "a/b", ".", "..", NULL};
    char long_name[257], darwin_name[32], label[192], valid[64], prefix[21];
    const char *names[10];
    int n = 0, i;
    hqq_link *l = NULL;
    hqq_status st;
    /* The C string API cannot carry the embedded NUL of the Go case "a\x00b". */
    for (i = 0; fixed[i] != NULL; i++) {
        names[n++] = fixed[i];
    }
    memset(long_name, 'a', 256);
    long_name[256] = '\0';
    names[n++] = long_name;
    if (strcmp(ihx_goos(), "darwin") == 0) {
        memset(darwin_name, 'a', 31);
        darwin_name[31] = '\0';
        names[n++] = darwin_name;
    }
    if (strcmp(ihx_goos(), "windows") == 0) {
        names[n++] = "Global\\";
    }
    for (i = 0; i < n; i++) {
        st = std_create(e, names[i], &l);
        std_close(&l);
        name_label(label, sizeof label, names[i], "ErrInvalidName");
        ihx_check_status(e, label, st, HQQ_ERR_INVALID_NAME);
        st = std_open(e, names[i], &l);
        std_close(&l);
        snprintf(label, sizeof label, "open \"%.40s\" -> ErrInvalidName", names[i]);
        ihx_check_status(e, label, st, HQQ_ERR_INVALID_NAME);
    }

    /* A valid 30-byte name: the prefix of the run name, padded to exactly 30 bytes. */
    snprintf(prefix, sizeof prefix, "%.20s", e->cfg.name);
    snprintf(valid, sizeof valid, "%s-x11-000000000000000000000000000000", prefix);
    valid[30] = '\0';
    st = std_create(e, valid, &l);
    if (st != HQQ_OK) {
        ihx_check(e, "valid 30-byte name is created", 0, "%s", hqq_strerror(st));
        return IHX_OK;
    }
    ihx_check(e, "valid 30-byte name is created", 1, "");
    ihx_check_status(e, "valid 30-byte name is removed on close", std_close_checked(&l), HQQ_OK);
    ihx_check_status(e, "valid 30-byte name is gone after close", probe_open(e, valid), HQQ_ERR_NOT_EXIST);
    return IHX_OK;
}

/* ---- X-21: protocol violations from a crafted peer on the Standard receive path ------------------------- */

/* One raw packet on the transmit ring; each one counts as a message of the scenario. */
static int x21_send(ihx_env *e, ihx_raw *raw, const uint64_t w[8], const void *payload) {
    int rc = ihx_raw_put(e, raw, w, payload, payload != NULL ? e->cfg.size : 0);
    if (rc == IHX_OK) {
        e->sent++;
    }
    return rc;
}

/* valid(seq): a copy of Payload(seed, seq, size) under the next copy id. */
static int x21_valid(ihx_env *e, ihx_raw *raw, uint64_t *id, uint64_t seq, uint8_t *pay) {
    uint64_t w[8] = {HQQ_OP_STD_COPY, ++(*id), 0, e->cfg.size, 0, 0, 0, 0};
    ihx_payload(e->cfg.seed, seq, pay, e->cfg.size);
    return x21_send(e, raw, w, pay);
}

/* The creator: a Primary plus the raw producer on its ring 0 (SPEC 7.4). Sequence:
 * valid(0), opcode 0x42, valid(1), copy DataSize 0, copy DataSize bufferSize+1,
 * OpError 5, tombstone, valid(2), OpConnCopy, valid(3). */
static int x21_writer(ihx_env *e) {
    hqq_link *l = NULL;
    ihx_raw *raw = NULL;
    uint8_t *pay = calloc(1, e->cfg.size);
    uint64_t id = 0;
    int rc;
    if (pay == NULL) {
        return ihx_fail(e, "Unknown", "out of memory");
    }
    rc = create_signal(e, &l);
    if (rc == IHX_OK) {
        rc = ihx_barrier(e, "joined");
    }
    if (rc == IHX_OK) {
        rc = ihx_raw_open(e, &raw);
    }
    if (rc == IHX_OK) {
        uint64_t bad[8] = {0x42, 0, 0, 0, 0, 0, 0, 0};
        uint64_t size0[8] = {HQQ_OP_STD_COPY, 0, 0, 0, 0, 0, 0, 0};
        uint64_t big[8] = {HQQ_OP_STD_COPY, 0, 0, 0, 0, 0, 0, 0};
        uint64_t err5[8] = {HQQ_OP_ERROR, 0, 5, 0, 0, 0, 0, 0};
        uint64_t tomb[8] = {HQQ_OP_STD_TOMBSTONE, 0, 0, 0x07, 0, 0, 0, 0};
        uint64_t conn[8] = {HQQ_OP_CONN_COPY, 0, 1, 1, 0, 1, 0, 0};
        rc = x21_valid(e, raw, &id, 0, pay);
        if (rc == IHX_OK) {
            rc = x21_send(e, raw, bad, NULL);
        }
        if (rc == IHX_OK) {
            rc = x21_valid(e, raw, &id, 1, pay);
        }
        if (rc == IHX_OK) {
            size0[1] = ++id;
            rc = x21_send(e, raw, size0, NULL);
        }
        if (rc == IHX_OK) {
            big[1] = ++id;
            big[3] = e->cfg.buffer_size + 1;
            rc = x21_send(e, raw, big, NULL);
        }
        if (rc == IHX_OK) {
            rc = x21_send(e, raw, err5, NULL);
        }
        if (rc == IHX_OK) {
            tomb[1] = ++id;
            rc = x21_send(e, raw, tomb, NULL);
        }
        if (rc == IHX_OK) {
            rc = x21_valid(e, raw, &id, 2, pay);
        }
        if (rc == IHX_OK) {
            rc = x21_send(e, raw, conn, NULL);
        }
        if (rc == IHX_OK) {
            rc = x21_valid(e, raw, &id, 3, pay);
        }
        if (rc == IHX_OK) {
            rc = ihx_barrier(e, "done");
        }
    }
    ihx_raw_close(raw);
    std_close(&l);
    free(pay);
    return rc;
}

static void x21_expect_valid(ihx_env *e, hqq_link *l, uint8_t *buf, const char *name, uint64_t seq) {
    size_t n = 0;
    hqq_status st = std_recv(e, l, buf, e->cfg.buffer_size, &n, STD_READ_BOUND_NS);
    int ok = st == HQQ_OK && n == e->cfg.size && ihx_payload_verify(e->cfg.seed, seq, buf, n) == 0;
    if (ok) {
        e->received++;
    }
    ihx_check(e, name, ok, "%s", st == HQQ_OK ? "" : hqq_strerror(st));
}

static void x21_expect_err(ihx_env *e, hqq_link *l, uint8_t *buf, const char *name, hqq_status want) {
    size_t n = 0;
    hqq_status st = std_recv(e, l, buf, e->cfg.buffer_size, &n, STD_READ_BOUND_NS);
    ihx_check_status(e, name, st, want);
}

/* The opener: the Standard reader under test. */
static int x21_reader(ihx_env *e) {
    hqq_link *l = NULL;
    uint8_t *buf = calloc(1, e->cfg.buffer_size);
    int rc;
    if (buf == NULL) {
        return ihx_fail(e, "Unknown", "out of memory");
    }
    rc = open_after(e, &l);
    if (rc == IHX_OK) {
        rc = ihx_barrier(e, "joined");
    }
    if (rc == IHX_OK) {
        x21_expect_valid(e, l, buf, "valid packet delivered", 0);
        x21_expect_err(e, l, buf, "unknown opcode 0x42 -> ErrUnexpectedPacket", HQQ_ERR_UNEXPECTED_PACKET);
        x21_expect_valid(e, l, buf, "next valid packet still received after the bad one", 1);
        x21_expect_err(e, l, buf, "OpStandardLinkCopy DataSize 0 -> ErrInvalidSize", HQQ_ERR_INVALID_SIZE);
        x21_expect_err(e, l, buf, "OpStandardLinkCopy DataSize > bufferSize -> ErrInvalidSize", HQQ_ERR_INVALID_SIZE);
        x21_expect_err(e, l, buf, "OpError code 5 -> ErrConnNotFound", HQQ_ERR_CONN_NOT_FOUND);
        x21_expect_valid(e, l, buf, "tombstone skipped silently", 2);
        x21_expect_err(e, l, buf, "OpConnCopy on a raw Standard read -> ErrUnexpectedPacket", HQQ_ERR_UNEXPECTED_PACKET);
        x21_expect_valid(e, l, buf, "valid packet after the Advanced frame", 3);
        rc = ihx_barrier(e, "done");
    }
    std_close(&l);
    free(buf);
    return rc;
}

static int x21_run(ihx_env *e) { return e->cfg.role == 1 ? x21_reader(e) : x21_writer(e); }

/* ---- Catalogue ------------------------------------------------------------------------------------------ */

#define STEP2(ec, eo, ac, ao) {2, {"creator", "opener"}, {ec, eo}, {ac, ao}}
#define STEP1C(ec, ac) {1, {"creator"}, {ec}, {ac}}
#define STEP1O(eo, ao) {1, {"opener"}, {eo}, {ao}}

static const ihx_variant x01_variants[] = {
    {.name = "creator-closes-first", .buffer_size = 256, .messages = 1, .nsteps = 1,
     .steps = {STEP2("ok", "ok", NULL, NULL)}},
    {.name = "opener-closes-first", .buffer_size = 256, .messages = 1, .nsteps = 1,
     .steps = {STEP2("ok", "ok", NULL, NULL)}},
};

static const ihx_variant x02_variants[] = {
    {.name = "count-double", .buffer_count = 4, .buffer_size = 256, .nsteps = 1,
     .steps = {STEP2("ok", "ok", NULL, "--buffer-count 8")}},
    {.name = "size-double", .buffer_count = 4, .buffer_size = 256, .nsteps = 1,
     .steps = {STEP2("ok", "ok", NULL, "--buffer-size 512")}},
    {.name = "count-half", .buffer_count = 4, .buffer_size = 256, .nsteps = 1,
     .steps = {STEP2("ok", "ok", NULL, "--buffer-count 2")}},
    {.name = "size-half", .buffer_count = 4, .buffer_size = 256, .nsteps = 1,
     .steps = {STEP2("ok", "ok", NULL, "--buffer-size 128")}},
};

static const ihx_variant x03_variants[] = {
    {.name = "default", .buffer_count = 4, .buffer_size = 256, .nsteps = 1,
     .steps = {STEP2("ok", "ok", NULL, NULL)}},
};

static const ihx_variant x04_variants[] = {
    {.name = "before-creator", .buffer_count = 4, .buffer_size = 256, .attach_timeout_ms = 300, .nsteps = 1,
     .steps = {STEP2("ok", "ok", NULL, NULL)}},
    {.name = "initializing", .buffer_count = 4, .buffer_size = 256, .attach_timeout_ms = 300, .nsteps = 2,
     .steps = {STEP1C("crash", "--crash-after claim"), STEP1O("ok", NULL)}},
    {.name = "dead", .buffer_count = 4, .buffer_size = 256, .heartbeat_ms = 50, .peer_timeout_ms = 200, .nsteps = 2,
     .steps = {STEP2("crash", "ok", "--crash-after exit", NULL), STEP1O("ok", NULL)}},
};

static const ihx_variant x05_variants[] = {
    {.name = "ring2", .buffer_count = 2, .buffer_size = 256, .nsteps = 1,
     .steps = {STEP2("ok", "ok", NULL, NULL)}},
};

static const ihx_variant x06_variants[] = {
    {.name = "ring2", .buffer_count = 2, .buffer_size = 256, .messages = 20000, .nsteps = 1,
     .steps = {STEP2("ok", "ok", NULL, NULL)}},
    {.name = "ring256", .buffer_count = 256, .buffer_size = 256, .messages = 20000, .nsteps = 1,
     .steps = {STEP2("ok", "ok", NULL, NULL)}},
};

static const ihx_variant x07_variants[] = {
    {.name = "creator-killed-running", .buffer_count = 4, .buffer_size = 256, .messages = 50, .nsteps = 1,
     .steps = {STEP2("crash", "ok", "--crash-after msgs:50", NULL)}},
    {.name = "opener-killed-parked", .buffer_count = 4, .buffer_size = 256, .messages = 50, .nsteps = 1,
     .steps = {STEP2("ok", "crash", NULL, "--crash-after blocked")}},
    {.name = "short-peer-timeout", .buffer_count = 4, .buffer_size = 256, .messages = 50, .heartbeat_ms = 50,
     .peer_timeout_ms = 300, .nsteps = 1,
     .steps = {STEP2("crash", "ok", "--crash-after msgs:50", NULL)}},
};

static const ihx_variant x08_variants[] = {
    {.name = "claim", .buffer_count = 4, .buffer_size = 256, .peer_timeout_ms = 2000, .nsteps = 2,
     .steps = {STEP1C("crash", "--crash-after claim"), STEP2("ok", "ok", NULL, "--attach-timeout 300")}},
    {.name = "format", .buffer_count = 4, .buffer_size = 256, .peer_timeout_ms = 2000, .nsteps = 2,
     .steps = {STEP1C("crash", "--crash-after format"), STEP2("ok", "ok", NULL, "--attach-timeout 300")}},
    {.name = "publish", .buffer_count = 4, .buffer_size = 256, .peer_timeout_ms = 2000, .nsteps = 2,
     .steps = {STEP1C("crash", "--crash-after publish"), STEP2("ok", "ok", NULL, "--attach-timeout 300")}},
    /* 4 x 4 MiB per pool: the zeroing crash happens after the first 8 MiB chunk of buffer pool 0. */
    {.name = "zeroing", .buffer_count = 4, .buffer_size = (uint64_t)4 << 20, .peer_timeout_ms = 2000, .nsteps = 2,
     .steps = {STEP1C("crash", "--crash-after zeroing"), STEP2("ok", "ok", NULL, "--attach-timeout 300")}},
};

static const ihx_variant x09_variants[] = {
    {.name = "reuse", .buffer_count = 4, .buffer_size = 256, .heartbeat_ms = 50, .peer_timeout_ms = 300, .nsteps = 3,
     .steps = {STEP2("crash", "crash", "--crash-after exit", "--crash-after exit"),
               STEP2("crash", "crash", "--crash-after exit", "--crash-after exit"),
               STEP1O("ok", NULL)}},
};

static const ihx_variant x10_variants[] = {
    {.name = "default", .buffer_count = 4, .buffer_size = 256, .nsteps = 1,
     .steps = {STEP1C("ok", NULL)}},
};

static const ihx_variant x11_variants[] = {
    {.name = "default", .buffer_count = 4, .buffer_size = 256, .nsteps = 1,
     .steps = {STEP1C("ok", NULL)}},
};

static const ihx_variant x12_variants[] = {
    {.name = "ring2", .buffer_count = 2, .buffer_size = 256, .messages = 20, .nsteps = 1,
     .steps = {STEP2("ok", "ok", NULL, NULL)}},
};

static const ihx_variant x13_variants[] = {
    {.name = "semaphores", .buffer_count = 4, .buffer_size = 256, .messages = 1000, .heartbeat_ms = 50,
     .peer_timeout_ms = 300, .nsteps = 3,
     .steps = {STEP2("ok", "ok", NULL, NULL), STEP2("ok", "crash", NULL, "--crash-after blocked"),
               STEP2("ok", "ok", NULL, NULL)}},
};

static const ihx_variant x14_variants[] = {
    {.name = "adaptive", .buffer_count = 4, .buffer_size = 256, .messages = 5, .nsteps = 1,
     .steps = {STEP2("ok", "ok", NULL, NULL)}},
    {.name = "spin-peer", .buffer_count = 4, .buffer_size = 256, .messages = 5, .nsteps = 1,
     .steps = {STEP2("ok", "ok", NULL, "--wait spin")}},
};

static const ihx_variant x21_variants[] = {
    {.name = "violations", .buffer_count = 4, .buffer_size = 256, .nsteps = 1,
     .steps = {STEP2("ok", "ok", NULL, NULL)}},
};

static int x12_run(ihx_env *e) {
    return ping_run(e, e->cfg.messages, 50 * NS_PER_MS, 250 * NS_PER_MS, "every round trip <= 250ms");
}

static int x13_run(ihx_env *e) {
    switch (e->cfg.step) {
    case 1: return ping_run(e, e->cfg.messages, 2 * NS_PER_MS, 250 * NS_PER_MS, "every round trip <= 250ms");
    case 2: return kill_peer_run(e);
    default: return ping_run(e, 100, 2 * NS_PER_MS, 250 * NS_PER_MS, "every round trip <= 250ms");
    }
}

static int x14_run(ihx_env *e) {
    return ping_run(e, e->cfg.messages, 300 * NS_PER_MS, 150 * NS_PER_MS, "every round trip <= 150ms");
}

const ihx_scenario ihx_std_scenarios[] = {
    {"X-01", "create, open, exchange, close", "standard", "any", 30, NELEM(x01_variants), x01_variants, x01_run},
    {"X-02", "geometry mismatch (count, size)", "standard", "any", 30, NELEM(x02_variants), x02_variants, x02_run},
    {"X-03", "third endpoint, re-join after own close, creator vs live segment", "standard", "any", 30,
     NELEM(x03_variants), x03_variants, x03_run},
    {"X-04", "opener before creator, opener on an initializing or dead segment", "standard", "any", 30,
     NELEM(x04_variants), x04_variants, x04_run},
    {"X-05", "ring full / backpressure, deadline on a full ring", "standard", "any", 30, NELEM(x05_variants),
     x05_variants, x05_run},
    {"X-06", "bidirectional stream, sequence and CRC, ring of 2 and 256", "standard", "any", 120,
     NELEM(x06_variants), x06_variants, x06_run},
    {"X-07", "kill -9 of the peer (running and parked), default and short peer timeout", "standard", "any", 60,
     NELEM(x07_variants), x07_variants, kill_peer_run},
    {"X-08", "kill -9 of the initializer between claim and publish", "standard", "any", 60, NELEM(x08_variants),
     x08_variants, x08_run},
    {"X-09", "reuse of a name after both endpoints died (epoch and creatorToken change)", "standard", "any", 60,
     NELEM(x09_variants), x09_variants, x09_run},
    {"X-10", "safe unlink: replaced name survives the creator's close", "standard", "linux,darwin", 30,
     NELEM(x10_variants), x10_variants, x10_run},
    {"X-11", "invalid names per platform, long names on macOS", "standard", "any", 30, NELEM(x11_variants),
     x11_variants, x11_run},
    {"X-12", "parked consumer woken by a producer (ping-pong, ring of 2)", "standard", "any", 60,
     NELEM(x12_variants), x12_variants, x12_run},
    {"X-13", "Windows named semaphores: ping-pong, parked kill, session reuse", "standard", "windows", 120,
     NELEM(x13_variants), x13_variants, x13_run},
    {"X-14", "simple-mode peer: round trips with 300 ms idle", "standard", "any", 60, NELEM(x14_variants),
     x14_variants, x14_run},
    {"X-21", "protocol violations from a crafted peer on the Standard receive path", "standard", "any", 60,
     NELEM(x21_variants), x21_variants, x21_run},
    {NULL, NULL, NULL, NULL, 0, 0, NULL, NULL},
};
