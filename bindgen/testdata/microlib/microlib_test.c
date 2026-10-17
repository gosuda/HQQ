/*
 * Self-test of microlib.c: the behavior of every function and the exact text
 * of the ML_TRACE file. POSIX only (setenv, mkstemp, system).
 *
 *   cc -std=c11 -Wall -Wextra -Wpedantic -Werror -fsanitize=address,undefined \
 *      microlib.c microlib_test.c -o microlib_test && ./microlib_test
 *
 * Run it from any directory; it needs a writable $TMPDIR (default /tmp) and
 * starts itself once more as a child (argv[0]) with ML_TRACE unset. Exit
 * status 0 and the line "microlib_test: ok" mean success.
 */
#define _POSIX_C_SOURCE 200809L

#include "microlib.h"

#include <inttypes.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

static int failures;

#define CHECK(cond)                                                                                \
    do {                                                                                           \
        if (!(cond)) {                                                                             \
            fprintf(stderr, "%s:%d: check failed: %s\n", __FILE__, __LINE__, #cond);               \
            failures++;                                                                            \
        }                                                                                          \
    } while (0)

/* The text the trace file must have received so far; stays empty in the untraced run. */
static char expected[8192];
static size_t expected_len;
static int tracing;

#if defined(__GNUC__)
__attribute__((format(printf, 1, 2)))
#endif
static void want(const char *fmt, ...) {
    va_list ap;
    int n;

    if (!tracing)
        return;
    va_start(ap, fmt);
    n = vsnprintf(expected + expected_len, sizeof expected - expected_len, fmt, ap);
    va_end(ap);
    if (n < 0 || (size_t)n >= sizeof expected - expected_len) {
        fprintf(stderr, "expected-trace buffer overflow\n");
        exit(2);
    }
    expected_len += (size_t)n;
}

static uint64_t fnv1a(const unsigned char *s) {
    uint64_t h = UINT64_C(0xcbf29ce484222325);

    for (; *s != '\0'; s++)
        h = (h ^ *s) * UINT64_C(0x100000001b3);
    return h;
}

static void test_add(void) {
    CHECK(ml_add(7, 35) == 42);
    want("ml_add a=7 b=35 ret=42\n");
    CHECK(ml_add(-5, 3) == -2);
    want("ml_add a=-5 b=3 ret=-2\n");
    CHECK(ml_add(INT32_MAX, 1) == INT32_MIN);
    want("ml_add a=2147483647 b=1 ret=-2147483648\n");
    CHECK(ml_add(INT32_MIN, -1) == INT32_MAX);
    want("ml_add a=-2147483648 b=-1 ret=2147483647\n");
}

/* The sequence of vectors R1, R2 and R12: a counter next to INT64_MAX. */
static void test_counter_errors(void) {
    ml_counter *c = ml_counter_new(INT64_C(9223372036854775800));
    int64_t total = -77;

    CHECK(c != NULL);
    want("ml_counter_new start=9223372036854775800 counter=1\n");
    if (c == NULL)
        return;
    CHECK(strcmp(ml_counter_errmsg(c), "") == 0);
    want("ml_counter_errmsg counter=1 msg=\"\"\n");

    CHECK(ml_counter_add(c, 100, &total) == -1);
    want("ml_counter_add counter=1 delta=100 ret=-1\n");
    CHECK(total == -77);
    CHECK(strcmp(ml_counter_errmsg(c), "overflow") == 0);
    want("ml_counter_errmsg counter=1 msg=\"overflow\"\n");

    CHECK(ml_counter_check(c, 5) == -1);
    want("ml_counter_check counter=1 limit=5 ret=-1\n");
    CHECK(strcmp(ml_counter_errmsg(c), "over limit") == 0);
    want("ml_counter_errmsg counter=1 msg=\"over limit\"\n");

    /* A failed add left the total alone: exactly INT64_MAX is reachable. */
    CHECK(ml_counter_add(c, 7, &total) == 0);
    want("ml_counter_add counter=1 delta=7 ret=0 total=9223372036854775807\n");
    CHECK(total == INT64_MAX);
    CHECK(strcmp(ml_counter_errmsg(c), "") == 0);
    want("ml_counter_errmsg counter=1 msg=\"\"\n");
    CHECK(ml_counter_add(c, 1, &total) == -1);
    want("ml_counter_add counter=1 delta=1 ret=-1\n");
    CHECK(total == INT64_MAX);
    ml_counter_free(c);
    want("ml_counter_free counter=1\n");
}

static void test_counter_values(void) {
    ml_counter *c = ml_counter_new(100);
    int64_t total = 0;
    const char *m;

    CHECK(c != NULL);
    want("ml_counter_new start=100 counter=2\n");
    if (c == NULL)
        return;
    CHECK(ml_counter_add(c, 5, &total) == 0 && total == 105);
    want("ml_counter_add counter=2 delta=5 ret=0 total=105\n");
    CHECK(ml_counter_add(c, -200, &total) == 0 && total == -95);
    want("ml_counter_add counter=2 delta=-200 ret=0 total=-95\n");
    CHECK(ml_counter_add(c, 0, &total) == 0 && total == -95);
    want("ml_counter_add counter=2 delta=0 ret=0 total=-95\n");

    /* "at most limit": equal passes, one below fails, and a pass clears the message. */
    CHECK(ml_counter_add(c, 200, &total) == 0 && total == 105);
    want("ml_counter_add counter=2 delta=200 ret=0 total=105\n");
    CHECK(ml_counter_check(c, 105) == 0);
    want("ml_counter_check counter=2 limit=105 ret=0\n");
    CHECK(ml_counter_check(c, 104) == -1);
    want("ml_counter_check counter=2 limit=104 ret=-1\n");
    m = ml_counter_errmsg(c);
    want("ml_counter_errmsg counter=2 msg=\"over limit\"\n");
    CHECK(strcmp(m, "over limit") == 0);
    CHECK(ml_counter_check(c, INT32_MAX) == 0);
    want("ml_counter_check counter=2 limit=2147483647 ret=0\n");
    CHECK(strcmp(ml_counter_errmsg(c), "") == 0);
    want("ml_counter_errmsg counter=2 msg=\"\"\n");
    CHECK(ml_counter_check(c, INT32_MIN) == -1);
    want("ml_counter_check counter=2 limit=-2147483648 ret=-1\n");

    /* The borrowed message survives later calls. */
    CHECK(ml_counter_add(c, 1, &total) == 0 && total == 106);
    want("ml_counter_add counter=2 delta=1 ret=0 total=106\n");
    CHECK(strcmp(m, "over limit") == 0);

    /* A failed check does not touch the total, and add reports its own failure after it. */
    CHECK(ml_counter_add(c, 0, &total) == 0 && total == 106);
    want("ml_counter_add counter=2 delta=0 ret=0 total=106\n");
    ml_counter_free(c);
    want("ml_counter_free counter=2\n");

    c = ml_counter_new(INT64_MIN);
    want("ml_counter_new start=-9223372036854775808 counter=3\n");
    CHECK(c != NULL);
    if (c == NULL)
        return;
    CHECK(ml_counter_add(c, -1, &total) == -1 && total == 106);
    want("ml_counter_add counter=3 delta=-1 ret=-1\n");
    CHECK(strcmp(ml_counter_errmsg(c), "overflow") == 0);
    want("ml_counter_errmsg counter=3 msg=\"overflow\"\n");
    CHECK(ml_counter_add(c, INT64_MAX, &total) == 0 && total == -1);
    want("ml_counter_add counter=3 delta=9223372036854775807 ret=0 total=-1\n");
    CHECK(ml_counter_add(c, INT64_MIN, &total) == -1 && total == -1);
    want("ml_counter_add counter=3 delta=-9223372036854775808 ret=-1\n");
    ml_counter_free(c);
    want("ml_counter_free counter=3\n");
}

static void test_cursor(void) {
    ml_counter *c = ml_counter_new(105);
    ml_cursor *cur;
    int64_t v;

    want("ml_counter_new start=105 counter=4\n");
    CHECK(c != NULL);
    if (c == NULL)
        return;

    /* Three values, then ML_EMPTY for good; v is not written on ML_EMPTY. */
    cur = ml_cursor_open(c, 3);
    want("ml_cursor_open counter=4 step=3 cursor=1\n");
    CHECK(cur != NULL);
    if (cur == NULL)
        return;
    v = 0;
    CHECK(ml_cursor_next(cur, &v) == ML_OK && v == 105);
    want("ml_cursor_next cursor=1 ret=0 value=105\n");
    CHECK(ml_cursor_next(cur, &v) == ML_OK && v == 108);
    want("ml_cursor_next cursor=1 ret=0 value=108\n");
    CHECK(ml_cursor_next(cur, &v) == ML_OK && v == 111);
    want("ml_cursor_next cursor=1 ret=0 value=111\n");
    v = -9;
    CHECK(ml_cursor_next(cur, &v) == ML_EMPTY && v == -9);
    want("ml_cursor_next cursor=1 ret=1\n");
    CHECK(ml_cursor_next(cur, &v) == ML_EMPTY && v == -9);
    want("ml_cursor_next cursor=1 ret=1\n");
    CHECK(ml_cursor_free(cur) == ML_OK);
    want("ml_cursor_free cursor=1 ret=0\n");

    /* One value still unread is already ML_PARTIAL; the first value is the total at open. */
    cur = ml_cursor_open(c, -1);
    want("ml_cursor_open counter=4 step=-1 cursor=2\n");
    CHECK(cur != NULL);
    if (cur == NULL)
        return;
    CHECK(ml_cursor_next(cur, &v) == ML_OK && v == 105);
    want("ml_cursor_next cursor=2 ret=0 value=105\n");
    CHECK(ml_cursor_next(cur, &v) == ML_OK && v == 104);
    want("ml_cursor_next cursor=2 ret=0 value=104\n");
    CHECK(ml_cursor_free(cur) == ML_PARTIAL);
    want("ml_cursor_free cursor=2 ret=2\n");

    cur = ml_cursor_open(c, 0);
    want("ml_cursor_open counter=4 step=0 cursor=3\n");
    CHECK(cur != NULL);
    if (cur == NULL)
        return;
    CHECK(ml_cursor_free(cur) == ML_PARTIAL);
    want("ml_cursor_free cursor=3 ret=2\n");

    /* Reading all three values is enough for ML_OK; the cursor is a snapshot and outlives its counter. */
    cur = ml_cursor_open(c, 10);
    want("ml_cursor_open counter=4 step=10 cursor=4\n");
    CHECK(cur != NULL);
    if (cur == NULL)
        return;
    {
        int64_t total;

        CHECK(ml_counter_add(c, 1000, &total) == 0 && total == 1105);
        want("ml_counter_add counter=4 delta=1000 ret=0 total=1105\n");
    }
    ml_counter_free(c);
    want("ml_counter_free counter=4\n");
    CHECK(ml_cursor_next(cur, &v) == ML_OK && v == 105);
    want("ml_cursor_next cursor=4 ret=0 value=105\n");
    CHECK(ml_cursor_next(cur, &v) == ML_OK && v == 115);
    want("ml_cursor_next cursor=4 ret=0 value=115\n");
    CHECK(ml_cursor_next(cur, &v) == ML_OK && v == 125);
    want("ml_cursor_next cursor=4 ret=0 value=125\n");
    CHECK(ml_cursor_free(cur) == ML_OK);
    want("ml_cursor_free cursor=4 ret=0\n");

    /* The values wrap modulo 2^64, as a Go int64 sum does. */
    c = ml_counter_new(INT64_MAX - 1);
    want("ml_counter_new start=9223372036854775806 counter=5\n");
    CHECK(c != NULL);
    if (c == NULL)
        return;
    cur = ml_cursor_open(c, 2);
    want("ml_cursor_open counter=5 step=2 cursor=5\n");
    CHECK(cur != NULL);
    if (cur == NULL) {
        ml_counter_free(c);
        return;
    }
    CHECK(ml_cursor_next(cur, &v) == ML_OK && v == INT64_MAX - 1);
    want("ml_cursor_next cursor=5 ret=0 value=9223372036854775806\n");
    CHECK(ml_cursor_next(cur, &v) == ML_OK && v == INT64_MIN);
    want("ml_cursor_next cursor=5 ret=0 value=-9223372036854775808\n");
    CHECK(ml_cursor_next(cur, &v) == ML_OK && v == INT64_MIN + 2);
    want("ml_cursor_next cursor=5 ret=0 value=-9223372036854775806\n");
    CHECK(ml_cursor_free(cur) == ML_OK);
    want("ml_cursor_free cursor=5 ret=0\n");
    ml_counter_free(c);
    want("ml_counter_free counter=5\n");

    /* The destroy order of a server teardown: the child first, then the parent. */
    c = ml_counter_new(0);
    want("ml_counter_new start=0 counter=6\n");
    CHECK(c != NULL);
    if (c == NULL)
        return;
    cur = ml_cursor_open(c, 1);
    want("ml_cursor_open counter=6 step=1 cursor=6\n");
    CHECK(cur != NULL);
    CHECK(ml_cursor_free(cur) == ML_PARTIAL);
    want("ml_cursor_free cursor=6 ret=2\n");
    ml_counter_free(c);
    want("ml_counter_free counter=6\n");

    /* NULL is accepted by the two destroy functions only. */
    CHECK(ml_cursor_free(NULL) == ML_OK);
    want("ml_cursor_free cursor=0 ret=0\n");
    ml_counter_free(NULL);
    want("ml_counter_free counter=0\n");
}

static void test_xor(void) {
    static const uint8_t src[3] = {1, 2, 3};
    uint8_t dst[8];
    uint8_t big_src[256];
    uint8_t big_dst[256];
    size_t i;

    memset(dst, 0xaa, sizeof dst);
    CHECK(ml_xor(src, 3, 0xff, dst, 8) == 3);
    want("ml_xor n=3 key=255 cap=8 ret=3\n");
    CHECK(dst[0] == 0xfe && dst[1] == 0xfd && dst[2] == 0xfc);
    for (i = 3; i < sizeof dst; i++)
        CHECK(dst[i] == 0xaa);

    /* cap < n: SIZE_MAX and dst is not touched. */
    memset(dst, 0xaa, sizeof dst);
    CHECK(ml_xor(src, 3, 0xff, dst, 2) == SIZE_MAX);
    want("ml_xor n=3 key=255 cap=2 ret=%" PRIu64 "\n", (uint64_t)SIZE_MAX);
    for (i = 0; i < sizeof dst; i++)
        CHECK(dst[i] == 0xaa);

    /* cap == n is enough. */
    memset(dst, 0xaa, sizeof dst);
    CHECK(ml_xor(src, 3, 0x0f, dst, 3) == 3);
    want("ml_xor n=3 key=15 cap=3 ret=3\n");
    CHECK(dst[0] == 0x0e && dst[1] == 0x0d && dst[2] == 0x0c && dst[3] == 0xaa);

    /* n == 0 uses no pointer. */
    CHECK(ml_xor(NULL, 0, 0x5a, NULL, 0) == 0);
    want("ml_xor n=0 key=90 cap=0 ret=0\n");

    for (i = 0; i < sizeof big_src; i++)
        big_src[i] = (uint8_t)i;
    CHECK(ml_xor(big_src, sizeof big_src, 0, big_dst, sizeof big_dst) == sizeof big_dst);
    want("ml_xor n=256 key=0 cap=256 ret=256\n");
    CHECK(memcmp(big_src, big_dst, sizeof big_src) == 0);
    CHECK(ml_xor(big_src, sizeof big_src, 0x5a, big_dst, sizeof big_dst) == sizeof big_dst);
    want("ml_xor n=256 key=90 cap=256 ret=256\n");
    for (i = 0; i < sizeof big_dst; i++)
        CHECK(big_dst[i] == (uint8_t)(i ^ 0x5a));
    CHECK(ml_xor(big_dst, sizeof big_dst, 0x5a, big_src, sizeof big_src) == sizeof big_src);
    want("ml_xor n=256 key=90 cap=256 ret=256\n");
    for (i = 0; i < sizeof big_src; i++)
        CHECK(big_src[i] == (uint8_t)i);

    CHECK(ml_xor_failed(SIZE_MAX) == 1);
    want("ml_xor_failed result=%" PRIu64 " ret=1\n", (uint64_t)SIZE_MAX);
    CHECK(ml_xor_failed(SIZE_MAX - 1) == 0);
    want("ml_xor_failed result=%" PRIu64 " ret=0\n", (uint64_t)(SIZE_MAX - 1));
    CHECK(ml_xor_failed(0) == 0);
    want("ml_xor_failed result=0 ret=0\n");
    CHECK(ml_xor_failed(3) == 0);
    want("ml_xor_failed result=3 ret=0\n");
}

static void test_hash(void) {
    CHECK(ml_hash("") == UINT64_C(0xcbf29ce484222325));
    want("ml_hash len=0 ret=14695981039346656037\n");
    CHECK(ml_hash("a") == UINT64_C(0xaf63dc4c8601ec8c));
    want("ml_hash len=1 ret=12638187200555641996\n");
    CHECK(ml_hash("foobar") == UINT64_C(0x85944171f73967e8));
    want("ml_hash len=6 ret=9625390261332436968\n");

    /* Bytes at or above 0x80 are unsigned, and the NUL ends the string. */
    CHECK(ml_hash("\xff\x80z") == fnv1a((const unsigned char *)"\xff\x80z"));
    want("ml_hash len=3 ret=%" PRIu64 "\n", fnv1a((const unsigned char *)"\xff\x80z"));
    CHECK(ml_hash("ab\0cd") == ml_hash("ab"));
    want("ml_hash len=2 ret=%" PRIu64 "\n", fnv1a((const unsigned char *)"ab"));
    want("ml_hash len=2 ret=%" PRIu64 "\n", fnv1a((const unsigned char *)"ab"));
}

static void test_work(void) {
    CHECK(ml_work(0) == UINT64_C(0x9e3779b97f4a7c15));
    want("ml_work iters=0 ret=11400714819323198485\n");
    CHECK(ml_work(1) == UINT64_C(0xca009717327d1f6f));
    want("ml_work iters=1 ret=14555800121548545903\n");
    CHECK(ml_work(2) == UINT64_C(0xfab98b79dfa4b7a7));
    want("ml_work iters=2 ret=18066624735732873127\n");
    CHECK(ml_work(1000) == UINT64_C(0x00d57ab5d6d4375f));
    want("ml_work iters=1000 ret=60089091451271007\n");
    CHECK(ml_work(1000) == ml_work(1000));
    want("ml_work iters=1000 ret=60089091451271007\n");
    want("ml_work iters=1000 ret=60089091451271007\n");
}

static void run_behavior(void) {
    test_add();
    test_counter_errors();
    test_counter_values();
    test_cursor();
    test_xor();
    test_hash();
    test_work();
}

static char *slurp(const char *path, size_t *len) {
    FILE *f = fopen(path, "rb");
    char *buf;
    size_t cap = 1 << 16;

    *len = 0;
    if (f == NULL)
        return NULL;
    buf = malloc(cap);
    if (buf != NULL)
        *len = fread(buf, 1, cap - 1, f);
    fclose(f);
    if (buf != NULL)
        buf[*len] = '\0';
    return buf;
}

/* The trace file holds the preamble written before the first call and then exactly `expected`. */
static void check_trace(const char *path, const char *preamble) {
    size_t len, plen = strlen(preamble);
    char *got = slurp(path, &len);

    CHECK(got != NULL);
    if (got == NULL)
        return;
    CHECK(len == plen + expected_len);
    CHECK(strncmp(got, preamble, plen) == 0);
    if (len != plen + expected_len || memcmp(got + plen, expected, expected_len) != 0) {
        fprintf(stderr, "trace differs; got:\n%s\nwant:\n%.*s\n", got + (len >= plen ? plen : 0),
                (int)expected_len, expected);
        failures++;
    }
    free(got);
}

int main(int argc, char **argv) {
    static const char preamble[] = "preexisting line\n";
    const char *tmpdir = getenv("TMPDIR");
    char path[1024];
    char cmd[2048];
    size_t before_len, after_len;
    char *before, *after;
    int fd;
    FILE *f;

    if (argc == 2 && strcmp(argv[1], "--untraced") == 0) {
        unsetenv("ML_TRACE");
        run_behavior();
        return failures == 0 ? 0 : 1;
    }

    snprintf(path, sizeof path, "%s/microlib_trace_XXXXXX", tmpdir != NULL && *tmpdir != '\0' ? tmpdir : "/tmp");
    fd = mkstemp(path);
    if (fd < 0) {
        perror("mkstemp");
        return 2;
    }
    f = fdopen(fd, "wb");
    if (f == NULL || fputs(preamble, f) == EOF || fclose(f) != 0) {
        perror("preamble");
        return 2;
    }
    setenv("ML_TRACE", path, 1);

    tracing = 1;
    run_behavior();
    check_trace(path, preamble);

    /* A fresh process with ML_TRACE unset behaves the same and leaves the file as it was. */
    before = slurp(path, &before_len);
    snprintf(cmd, sizeof cmd, "\"%s\" --untraced", argv[0]);
    CHECK(system(cmd) == 0);
    after = slurp(path, &after_len);
    CHECK(before != NULL && after != NULL && before_len == after_len && memcmp(before, after, before_len) == 0);
    free(before);
    free(after);
    unlink(path);

    if (failures != 0) {
        fprintf(stderr, "microlib_test: %d check(s) failed\n", failures);
        return 1;
    }
    puts("microlib_test: ok");
    return 0;
}
