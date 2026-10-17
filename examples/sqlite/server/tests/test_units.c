/*
 * test_units.c - unit tests of the hqq-sqlite server: the byte codec (with the
 * golden encodings of PROTOCOL.md section 16), status values, input
 * validation, and the database path policy. The request paths (SQL, batches,
 * cancellation) are covered end to end by sqlite/e2e.
 */
#define _POSIX_C_SOURCE 200809L

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

#include "sqlsrv_policy.h"
#include "sqlsrv_proto.h"

static int failures;

#define CHECK(cond)                                                        \
    do {                                                                   \
        if (!(cond)) {                                                     \
            fprintf(stderr, "%s:%d: CHECK failed: %s\n", __FILE__, __LINE__, #cond); \
            failures++;                                                    \
        }                                                                  \
    } while (0)

static int bytes_equal(const sqlsrv_buf *b, const uint8_t *want, size_t n) {
    return b->len == n && (n == 0 || memcmp(b->p, want, n) == 0);
}

static void test_golden_encodings(void) {
    /* PROTOCOL.md section 16: little-endian, length-prefixed strings. */
    static const uint8_t open_head[] = {0x01, 0x00};
    static const uint8_t db_main[] = {0x04, 0x00, 0x00, 0x00, 'm', 'a', 'i', 'n'};
    static const uint8_t busy_5000[] = {0x88, 0x13, 0x00, 0x00};
    static const uint8_t int42[] = {0x01, 0x2a, 0, 0, 0, 0, 0, 0, 0};
    static const uint8_t text_hi[] = {0x03, 0x02, 0x00, 0x00, 0x00, 'h', 'i'};
    static const uint8_t float15[] = {0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xf8, 0x3f};
    sqlsrv_buf b;

    memset(&b, 0, sizeof b);
    sqlsrv_buf_u16(&b, 1);
    sqlsrv_buf_str(&b, "main", 4);
    sqlsrv_buf_u32(&b, 0x6);
    sqlsrv_buf_u32(&b, 5000);
    CHECK(b.len == 2 + sizeof db_main + 4 + 4);
    CHECK(memcmp(b.p, open_head, 2) == 0);
    CHECK(memcmp(b.p + 2, db_main, sizeof db_main) == 0);
    CHECK(memcmp(b.p + 2 + sizeof db_main + 4, busy_5000, 4) == 0);
    sqlsrv_buf_free(&b);

    memset(&b, 0, sizeof b);
    sqlsrv_buf_u8(&b, SQLSRV_TAG_INTEGER);
    sqlsrv_buf_i64(&b, 42);
    CHECK(bytes_equal(&b, int42, sizeof int42));
    sqlsrv_buf_free(&b);

    memset(&b, 0, sizeof b);
    sqlsrv_buf_u8(&b, SQLSRV_TAG_TEXT);
    sqlsrv_buf_str(&b, "hi", 2);
    CHECK(bytes_equal(&b, text_hi, sizeof text_hi));
    sqlsrv_buf_free(&b);

    memset(&b, 0, sizeof b);
    sqlsrv_buf_u8(&b, SQLSRV_TAG_FLOAT);
    sqlsrv_buf_f64(&b, 1.5);
    CHECK(bytes_equal(&b, float15, sizeof float15));
    sqlsrv_buf_free(&b);
}

static void test_reader(void) {
    static const uint8_t body[] = {0x01, 0x00, 0x03, 0x00, 0x00, 0x00, 'a', 'b', 'c', 0xff};
    sqlsrv_rd r;
    uint16_t u16 = 0;
    uint32_t u32 = 0;
    const uint8_t *p = NULL;
    uint32_t n = 0;

    sqlsrv_rd_init(&r, body, sizeof body);
    CHECK(sqlsrv_rd_u16(&r, &u16) == 0 && u16 == 1);
    CHECK(sqlsrv_rd_str(&r, &p, &n) == 0 && n == 3 && memcmp(p, "abc", 3) == 0);
    CHECK(sqlsrv_rd_end(&r) == -1); /* one byte left over */
    CHECK(sqlsrv_rd_u32(&r, &u32) == -1); /* only one byte left: truncated */

    /* A string whose length runs past the end is refused, not read. */
    {
        static const uint8_t bad[] = {0x09, 0x00, 0x00, 0x00, 'x'};
        sqlsrv_rd_init(&r, bad, sizeof bad);
        CHECK(sqlsrv_rd_str(&r, &p, &n) == -1);
    }
    /* A complete body consumes everything. */
    sqlsrv_rd_init(&r, body, 2);
    CHECK(sqlsrv_rd_u16(&r, &u16) == 0);
    CHECK(sqlsrv_rd_end(&r) == 0);
}

static void test_statuses(void) {
    sqlsrv_err e;
    CHECK(SQLSRV_STATUS_SQLITE(2067) == 0x10813u);
    CHECK(SQLSRV_STATUS_SQLITE(5) == 0x10005u);
    CHECK(SQLSRV_STATUS_PROTO(SQLSRV_PE_FORBIDDEN) == 0x20009u);
    CHECK(SQLSRV_STATUS_PROTO(SQLSRV_PE_INTERNAL) == 0x2000au);
    sqlsrv_err_proto(&e, SQLSRV_PE_LIMIT, "too many");
    CHECK(e.status == 0x20007u);
    CHECK(strcmp(e.msg, "too many") == 0);
}

static void test_limits(void) {
    /* PROTOCOL.md section 11, pinned: a change here is a protocol change. */
    CHECK(SQLSRV_MAX_MESSAGE_BYTES == 16u * 1024u * 1024u);
    CHECK(SQLSRV_MAX_SQL_BYTES == 1048576u);
    CHECK(SQLSRV_MAX_PARAMS == 32766u);
    CHECK(SQLSRV_MAX_COLUMNS == 2000u);
    CHECK(SQLSRV_MAX_FETCH_ROWS == 4096u);
    CHECK(SQLSRV_DEFAULT_FETCH_ROWS == 256u);
    CHECK(SQLSRV_MAX_BATCH_BYTES == 1048576u);
    CHECK(SQLSRV_MAX_NAME_BYTES == 1024u);
    CHECK(SQLSRV_MAX_PATH_BYTES == 4096u);
    CHECK(SQLSRV_MAX_PRAGMAS == 32u);
    CHECK(SQLSRV_MAX_STATEMENTS == 256u);
    CHECK(SQLSRV_MAX_CURSORS == 64u);
}

static void test_validation(void) {
    CHECK(sqlsrv_valid_open_flags(0x1));
    CHECK(sqlsrv_valid_open_flags(0x2));
    CHECK(sqlsrv_valid_open_flags(0x6));
    CHECK(!sqlsrv_valid_open_flags(0x0));
    CHECK(!sqlsrv_valid_open_flags(0x3));
    CHECK(!sqlsrv_valid_open_flags(0x4));
    CHECK(!sqlsrv_valid_open_flags(0x8));

    CHECK(sqlsrv_valid_pragma_name((const uint8_t *)"journal_mode", 12));
    CHECK(sqlsrv_valid_pragma_name((const uint8_t *)"_x1", 3));
    CHECK(!sqlsrv_valid_pragma_name((const uint8_t *)"1x", 2));
    CHECK(!sqlsrv_valid_pragma_name((const uint8_t *)"Journal", 7));
    CHECK(!sqlsrv_valid_pragma_name((const uint8_t *)"a;b", 3));
    CHECK(!sqlsrv_valid_pragma_name((const uint8_t *)"", 0));

    CHECK(sqlsrv_valid_pragma_value((const uint8_t *)"WAL", 3));
    CHECK(sqlsrv_valid_pragma_value((const uint8_t *)"-2000", 5));
    CHECK(sqlsrv_valid_pragma_value((const uint8_t *)"", 0));
    CHECK(!sqlsrv_valid_pragma_value((const uint8_t *)"a b", 3));
    CHECK(!sqlsrv_valid_pragma_value((const uint8_t *)"a;DROP", 6));
}

/* ---- Path policy ------------------------------------------------------- */

static int make_dir(const char *path) {
    return mkdir(path, 0700);
}

static int resolve(const sqlsrv_policy *p, const char *db, char **out, sqlsrv_err *e) {
    return sqlsrv_policy_resolve(p, (const uint8_t *)db, strlen(db), out, e);
}

static void test_policy(void) {
    char root[128];
    char allow[1024];
    char outside[1024];
    char link_path[1024];
    char err[256];
    sqlsrv_policy p;
    sqlsrv_err e;
    char *out = NULL;

    (void)snprintf(root, sizeof root, "/tmp/hqqsqlsrv-policy-%ld", (long)getpid());
    (void)rmdir(root);
    CHECK(make_dir(root) == 0);
    snprintf(allow, sizeof allow, "%s/allow", root);
    snprintf(outside, sizeof outside, "%s/outside", root);
    CHECK(make_dir(allow) == 0);
    CHECK(make_dir(outside) == 0);

    sqlsrv_policy_init(&p);
    /* Raw paths are disabled until --allow-dir is given. */
    CHECK(resolve(&p, "main", &out, &e) == -1 && e.status == 0x20009u);

    CHECK(sqlsrv_policy_set_allow_dir(&p, allow, err, sizeof err) == 0);
    {
        char *ra = realpath(allow, NULL);
        char *ro = realpath(outside, NULL);
        CHECK(ra != NULL && ro != NULL);
        if (ra != NULL) {
            (void)snprintf(allow, sizeof allow, "%s", ra);
            free(ra);
        }
        if (ro != NULL) {
            (void)snprintf(outside, sizeof outside, "%s", ro);
            free(ro);
        }
    }
    CHECK(sqlsrv_policy_set_allow_dir(&p, "/no/such/dir/here", err, sizeof err) == -1);

    /* A plain name lands inside the directory (the file need not exist yet). */
    CHECK(resolve(&p, "main.db", &out, &e) == 0);
    CHECK(out != NULL && strncmp(out, allow, strlen(allow)) == 0);
    free(out);
    out = NULL;

    /* Traversal and absolute paths outside the directory are refused. */
    CHECK(resolve(&p, "../outside/x.db", &out, &e) == -1 && e.status == 0x20009u);
    CHECK(resolve(&p, "sub/../../outside/x.db", &out, &e) == -1 && e.status == 0x20009u);
    CHECK(resolve(&p, "/etc/hosts", &out, &e) == -1 && e.status == 0x20009u);
    CHECK(resolve(&p, "..", &out, &e) == -1 && e.status == 0x20009u);
    CHECK(resolve(&p, ".", &out, &e) == -1);

    /* A missing parent directory is CANTOPEN, not a path escape. */
    CHECK(resolve(&p, "missing/x.db", &out, &e) == -1 && e.status == 0x1000eu);

    /* A symbolic link out of the directory is followed and refused. */
    snprintf(link_path, sizeof link_path, "%s/hosts.db", allow);
    CHECK(symlink("/etc/hosts", link_path) == 0);
    CHECK(resolve(&p, "hosts.db", &out, &e) == -1 && e.status == 0x20009u);
    CHECK(unlink(link_path) == 0);

    /* A dangling symbolic link is refused as well. */
    snprintf(link_path, sizeof link_path, "%s/dangling.db", allow);
    CHECK(symlink("/no/such/target", link_path) == 0);
    CHECK(resolve(&p, "dangling.db", &out, &e) == -1 && e.status == 0x20009u);
    CHECK(unlink(link_path) == 0);

    /* An embedded NUL is a bad request. */
    CHECK(sqlsrv_policy_resolve(&p, (const uint8_t *)"a\0b", 3, &out, &e) == -1 && e.status == 0x20001u);

    /* Aliases win and are canonical; they may lie outside the directory. */
    CHECK(sqlsrv_policy_add_alias(&p, "app", outside, err, sizeof err) == 0);
    CHECK(sqlsrv_policy_add_alias(&p, "app", outside, err, sizeof err) == -1);
    CHECK(resolve(&p, "app", &out, &e) == 0);
    CHECK(out != NULL && strncmp(out, outside, strlen(outside)) == 0);
    free(out);
    out = NULL;

    sqlsrv_policy_free(&p);
    (void)rmdir(allow);
    (void)rmdir(outside);
    (void)rmdir(root);
}

int main(void) {
    test_golden_encodings();
    test_reader();
    test_statuses();
    test_limits();
    test_validation();
    test_policy();
    if (failures != 0) {
        fprintf(stderr, "%d check(s) failed\n", failures);
        return 1;
    }
    printf("hqqsqlite_server units: all checks passed\n");
    return 0;
}
