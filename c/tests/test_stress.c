/* Bounded thread stress on one caller-owned link (a few seconds in total):
 * several writer and reader threads per direction, rings of 2 and 256 slots,
 * full-size messages mixed with small ones. Every message must arrive exactly
 * once and intact (length, writer and sequence numbers, checksum). */
#include <stdlib.h>
#include <string.h>

#include "hqq_internal.h"
#include "hqq_test.h"
#include "hqq_testutil.h"

#define ST_WRITERS 3u
#define ST_READERS 3u
#define ST_PER_WRITER 3000u
#define ST_MAX_SIZE 256u
#define ST_TOTAL (ST_WRITERS * ST_PER_WRITER)

/* Message layout: writer (4 bytes), sequence (4), payload, checksum of the payload (4). */
#define ST_HEADER 8u
#define ST_TRAILER 4u
#define ST_MIN_SIZE (ST_HEADER + ST_TRAILER)

typedef struct st_dir {
    hqq_link *writer_end; /* the link end that writes in this direction */
    hqq_link *reader_end; /* the link end that reads in this direction */
    _Atomic uint64_t received;
    _Atomic unsigned char *seen; /* one flag per (writer, sequence) */
} st_dir;

typedef struct st_worker {
    st_dir *dir;
    unsigned id;
    _Atomic int bad;
} st_worker;

static uint32_t checksum(const unsigned char *d, size_t n) {
    uint32_t h = 2166136261u;
    size_t i;
    for (i = 0; i < n; i++) {
        h = (h ^ d[i]) * 16777619u;
    }
    return h;
}

static size_t size_of(unsigned writer, unsigned seq) {
    if ((seq % 97u) == 0u) {
        return ST_MAX_SIZE; /* full buffer */
    }
    return ST_MIN_SIZE + ((writer * 131u + seq * 17u) % (ST_MAX_SIZE - ST_MIN_SIZE));
}

static void fill_message(unsigned char *buf, unsigned writer, unsigned seq, size_t n) {
    size_t i;
    uint32_t crc;
    memcpy(buf, &writer, 4);
    memcpy(buf + 4, &seq, 4);
    for (i = ST_HEADER; i < n - ST_TRAILER; i++) {
        buf[i] = (unsigned char)(writer * 7u + seq * 13u + i);
    }
    crc = checksum(buf + ST_HEADER, n - ST_MIN_SIZE);
    memcpy(buf + n - ST_TRAILER, &crc, 4);
}

/* Returns 1 when the message is well formed and its checksum matches. */
static int check_message(const unsigned char *buf, size_t len, unsigned *writer, unsigned *seq) {
    uint32_t crc, want;
    if (len < ST_MIN_SIZE) {
        return 0;
    }
    memcpy(writer, buf, 4);
    memcpy(seq, buf + 4, 4);
    memcpy(&crc, buf + len - ST_TRAILER, 4);
    want = checksum(buf + ST_HEADER, len - ST_MIN_SIZE);
    return *writer < ST_WRITERS && *seq < ST_PER_WRITER && len == size_of(*writer, *seq) && crc == want;
}

static void writer_main(void *p) {
    st_worker *w = (st_worker *)p;
    unsigned char *buf = (unsigned char *)malloc(ST_MAX_SIZE);
    unsigned seq;
    int bad = buf == NULL;
    for (seq = 0; !bad && seq < ST_PER_WRITER; seq++) {
        size_t n = size_of(w->id, seq);
        fill_message(buf, w->id, seq, n);
        if (hqq_write(w->dir->writer_end, buf, n, NULL) != HQQ_OK) {
            bad = 1;
        }
    }
    atomic_store(&w->bad, bad);
    free(buf);
}

static void reader_main(void *p) {
    st_worker *w = (st_worker *)p;
    st_dir *d = w->dir;
    unsigned char *buf = (unsigned char *)malloc(ST_MAX_SIZE);
    int bad = buf == NULL;
    while (!bad && atomic_load(&d->received) < ST_TOTAL) {
        size_t len = 0;
        unsigned writer = 0, seq = 0;
        hqq_wait wt;
        hqq_status st;
        wt.timeout_ns = 50 * (int64_t)HQQ_NS_PER_MS;
        wt.cancel = NULL;
        st = hqq_read(d->reader_end, buf, ST_MAX_SIZE, &len, &wt);
        if (st == HQQ_ERR_TIMEOUT) {
            continue;
        }
        if (st != HQQ_OK || !check_message(buf, len, &writer, &seq)) {
            bad = 1;
            break;
        }
        /* Exactly once: the flag of this message must not be set yet. */
        if (atomic_exchange(&d->seen[writer * ST_PER_WRITER + seq], 1) != 0) {
            bad = 1;
            break;
        }
        atomic_fetch_add(&d->received, 1);
    }
    atomic_store(&w->bad, bad);
    free(buf);
}

static void run_direction(st_dir *d, hqq_link *writer_end, hqq_link *reader_end) {
    st_worker writers[ST_WRITERS], readers[ST_READERS];
    hqq_test_thread *th[ST_WRITERS + ST_READERS];
    unsigned i, missing = 0;

    memset(d, 0, sizeof *d);
    d->writer_end = writer_end;
    d->reader_end = reader_end;
    d->seen = (_Atomic unsigned char *)calloc(ST_TOTAL, sizeof(unsigned char));
    HQQ_REQUIRE(d->seen != NULL);
    for (i = 0; i < ST_READERS; i++) {
        readers[i].dir = d;
        readers[i].id = i;
        atomic_init(&readers[i].bad, 0);
        th[ST_WRITERS + i] = hqq_test_thread_start(reader_main, &readers[i]);
    }
    for (i = 0; i < ST_WRITERS; i++) {
        writers[i].dir = d;
        writers[i].id = i;
        atomic_init(&writers[i].bad, 0);
        th[i] = hqq_test_thread_start(writer_main, &writers[i]);
    }
    for (i = 0; i < ST_WRITERS + ST_READERS; i++) {
        hqq_test_thread_join(th[i]);
    }
    for (i = 0; i < ST_READERS; i++) {
        HQQ_CHECK_EQ_INT(atomic_load(&readers[i].bad), 0);
    }
    for (i = 0; i < ST_WRITERS; i++) {
        HQQ_CHECK_EQ_INT(atomic_load(&writers[i].bad), 0);
    }
    for (i = 0; i < ST_TOTAL; i++) {
        missing += atomic_load(&d->seen[i]) == 0;
    }
    HQQ_CHECK_EQ_INT(missing, 0);
    HQQ_CHECK_EQ_U64(atomic_load(&d->received), ST_TOTAL);
    free((void *)d->seen);
}

/* Ring of `count` slots with 256-byte buffers, both directions one after the other. */
static void run_geometry(uint64_t count) {
    hqq_options o;
    size_t size = hqq_standard_size(count, ST_MAX_SIZE);
    unsigned char *mem = (unsigned char *)hqq_test_pages(size);
    hqq_link *p = NULL, *s = NULL;
    st_dir fwd, back;

    hqq_options_init(&o);
    o.heartbeat_interval_ns = 50 * (int64_t)HQQ_NS_PER_MS;
    o.peer_timeout_ns = 10 * (int64_t)HQQ_NS_PER_SEC;
    HQQ_REQUIRE(hqq_standard_open_memory(mem, size, count, ST_MAX_SIZE, &o, &p, NULL) == HQQ_OK);
    HQQ_REQUIRE(hqq_standard_open_memory(mem, size, count, ST_MAX_SIZE, &o, &s, NULL) == HQQ_OK);

    run_direction(&fwd, p, s);  /* Primary -> Secondary */
    run_direction(&back, s, p); /* Secondary -> Primary */

    HQQ_CHECK_STATUS(hqq_close(s), HQQ_OK);
    HQQ_CHECK_STATUS(hqq_close(p), HQQ_OK);
    hqq_test_pages_free(mem, size);
}

int main(void) {
    run_geometry(2);
    run_geometry(256);
    return HQQ_TEST_RESULT("test_stress");
}
