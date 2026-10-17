/*
 * test_units.c - unit tests of the hqq-sqlite server: the byte codec (with the
 * golden encodings of PROTOCOL.md section 16), status values, input
 * validation, and the database path policy. The request paths (SQL, batches,
 * cancellation) are covered end to end by sqlite/e2e.
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "sqlsrv_pathrules.h"
#include "sqlsrv_policy.h"
#include "sqlsrv_port.h"
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

/* ---- Path rules (pure, the same on every host) ------------------------- */

static void expect_raw(const char *s, sqlsrv_path_style style, int accepted) {
    const char *why = sqlsrv_path_check_raw((const uint8_t *)s, strlen(s), style);
    if ((why == NULL) != (accepted != 0)) {
        fprintf(stderr, "%s:%d: raw path \"%s\" should be %s (%s)\n", __FILE__, __LINE__, s,
                accepted ? "accepted" : "refused", why != NULL ? why : "no reason");
        failures++;
    }
}

static void expect_kind(const char *s, sqlsrv_path_style style, sqlsrv_path_kind want) {
    CHECK(sqlsrv_path_classify((const uint8_t *)s, strlen(s), style) == want);
}

static void expect_join(const char *base, const char *p, sqlsrv_path_style style, const char *want) {
    char *got = sqlsrv_path_join(base, (const uint8_t *)p, strlen(p), style);
    if (got == NULL || strcmp(got, want) != 0) {
        fprintf(stderr, "%s:%d: join(\"%s\", \"%s\") is \"%s\", want \"%s\"\n", __FILE__, __LINE__,
                base != NULL ? base : "", p, got != NULL ? got : "(null)", want);
        failures++;
    }
    free(got);
}

static void expect_split(const char *path, sqlsrv_path_style style, size_t want_dir, size_t want_leaf) {
    size_t dir = 0;
    size_t leaf = 0;
    CHECK(sqlsrv_path_split(path, style, &dir, &leaf) == 0 && dir == want_dir && leaf == want_leaf);
}

static void expect_prefix(const char *dir, sqlsrv_path_style style, const char *want) {
    char *got = sqlsrv_path_dir_prefix(dir, style);
    if (got == NULL || strcmp(got, want) != 0) {
        fprintf(stderr, "%s:%d: dir_prefix(\"%s\") is \"%s\", want \"%s\"\n", __FILE__, __LINE__, dir,
                got != NULL ? got : "(null)", want);
        failures++;
    }
    free(got);
}

static void test_path_rules(void) {
    static const char *const win_bad[] = {
        "..\\x.db", "a\\..\\b.db", "a/../b.db", "..", "...", "x:stream", "C:x.db", "C:..\\x", "\\x.db",
        "/etc/hosts", "\\\\server\\share\\x.db", "\\\\?\\C:\\x.db", "\\\\.\\pipe\\x", "//?/C:/x.db",
        "nul", "NUL.txt", "con.db", "COM1", "com9.log", "LPT1.txt", "CONIN$", "a\\\\b.db", "name.",
        "name ", "a?b.db", "a<b.db", "a|b", "a\x01.db", "C:\\", "C:\\x\\", "",
    };
    static const char *const win_good[] = {
        "main.db", "sub\\main.db", "sub/main.db", "a.b.c.db", ".hidden", "CONX.db", "COM10.db", "a..b",
        "C:\\data\\x.db", "C:/data/x.db", "x\\.\\y.db", "name with spaces.db",
    };
    static const char *const posix_bad[] = {"../x", "a/..", "..", "/x/../y"};
    static const char *const posix_good[] = {"a\\..\\b", "..a", "a..b", "...", "/etc/hosts", "x.db"};
    size_t i;

    for (i = 0; i < sizeof win_bad / sizeof win_bad[0]; i++) {
        expect_raw(win_bad[i], SQLSRV_PATH_WINDOWS, 0);
    }
    for (i = 0; i < sizeof win_good / sizeof win_good[0]; i++) {
        expect_raw(win_good[i], SQLSRV_PATH_WINDOWS, 1);
    }
    for (i = 0; i < sizeof posix_bad / sizeof posix_bad[0]; i++) {
        expect_raw(posix_bad[i], SQLSRV_PATH_POSIX, 0);
    }
    for (i = 0; i < sizeof posix_good / sizeof posix_good[0]; i++) {
        expect_raw(posix_good[i], SQLSRV_PATH_POSIX, 1);
    }

    expect_kind("C:\\x", SQLSRV_PATH_WINDOWS, SQLSRV_PATHK_ABSOLUTE);
    expect_kind("c:/x", SQLSRV_PATH_WINDOWS, SQLSRV_PATHK_ABSOLUTE);
    expect_kind("C:x", SQLSRV_PATH_WINDOWS, SQLSRV_PATHK_DRIVE_RELATIVE);
    expect_kind("\\\\server\\s", SQLSRV_PATH_WINDOWS, SQLSRV_PATHK_UNC);
    expect_kind("\\\\?\\C:\\x", SQLSRV_PATH_WINDOWS, SQLSRV_PATHK_UNC);
    expect_kind("\\x", SQLSRV_PATH_WINDOWS, SQLSRV_PATHK_ROOTED);
    expect_kind("/x", SQLSRV_PATH_WINDOWS, SQLSRV_PATHK_ROOTED);
    expect_kind("x\\y", SQLSRV_PATH_WINDOWS, SQLSRV_PATHK_RELATIVE);
    expect_kind("/x", SQLSRV_PATH_POSIX, SQLSRV_PATHK_ABSOLUTE);
    expect_kind("x", SQLSRV_PATH_POSIX, SQLSRV_PATHK_RELATIVE);

    expect_join("C:\\allow\\", "main.db", SQLSRV_PATH_WINDOWS, "C:\\allow\\main.db");
    expect_join("C:\\allow", "sub/x.db", SQLSRV_PATH_WINDOWS, "C:\\allow\\sub\\x.db");
    expect_join("C:\\allow\\", "D:/x.db", SQLSRV_PATH_WINDOWS, "D:\\x.db");
    expect_join("/a/", "b", SQLSRV_PATH_POSIX, "/a/b");
    expect_join("/a", "b", SQLSRV_PATH_POSIX, "/a/b");
    expect_join("/a", "/etc", SQLSRV_PATH_POSIX, "/etc");

    expect_split("C:\\x.db", SQLSRV_PATH_WINDOWS, 3, 3);
    expect_split("C:\\dir\\x.db", SQLSRV_PATH_WINDOWS, 6, 7);
    expect_split("\\x", SQLSRV_PATH_WINDOWS, 1, 1);
    expect_split("/x/y", SQLSRV_PATH_POSIX, 2, 3);
    expect_split("/x", SQLSRV_PATH_POSIX, 1, 1);

    expect_prefix("C:\\", SQLSRV_PATH_WINDOWS, "C:\\");
    expect_prefix("C:\\x", SQLSRV_PATH_WINDOWS, "C:\\x\\");
    expect_prefix("/x", SQLSRV_PATH_POSIX, "/x/");
    expect_prefix("/x/", SQLSRV_PATH_POSIX, "/x/");

    CHECK(sqlsrv_path_inside("C:\\Allow\\x.db", "c:\\allow\\", SQLSRV_PATH_WINDOWS) == 1);
    CHECK(sqlsrv_path_inside("C:\\allow", "C:\\allow\\", SQLSRV_PATH_WINDOWS) == 0);
    CHECK(sqlsrv_path_inside("C:\\allowed\\x", "C:\\allow\\", SQLSRV_PATH_WINDOWS) == 0);
    CHECK(sqlsrv_path_inside("D:\\allow\\x", "C:\\allow\\", SQLSRV_PATH_WINDOWS) == 0);
    CHECK(sqlsrv_path_inside("/a/b", "/a/", SQLSRV_PATH_POSIX) == 1);
    CHECK(sqlsrv_path_inside("/A/b", "/a/", SQLSRV_PATH_POSIX) == 0);
    CHECK(sqlsrv_path_inside("/ab", "/a/", SQLSRV_PATH_POSIX) == 0);
}

/* ---- Path policy (uses the platform layer: real directories) ----------- */

static int make_dir(const char *path) {
    return sqlsrv_port_mkdir(path);
}

static int resolve(const sqlsrv_policy *p, const char *db, char **out, sqlsrv_err *e) {
    return sqlsrv_policy_resolve(p, (const uint8_t *)db, strlen(db), out, e);
}

static void test_policy(void) {
    char *tmp = sqlsrv_port_tempdir();
    char root[1024];
    char allow[1024];
    char outside[1024];
    char secret[1024];
    char link_path[1024];
    char target[1024];
    char err[256];
    sqlsrv_policy p;
    sqlsrv_err e;
    char *out = NULL;

    if (tmp == NULL) {
        CHECK(tmp != NULL);
        return;
    }
    (void)snprintf(root, sizeof root, "%s/hqqsqlsrv-policy-%lu", tmp, sqlsrv_process_id());
    free(tmp);
    (void)sqlsrv_port_remove_dir(root);
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
        char *ra = NULL;
        char *ro = NULL;
        int missing = 0;
        CHECK(sqlsrv_port_realpath(allow, &ra, &missing) == 0);
        CHECK(sqlsrv_port_realpath(outside, &ro, &missing) == 0);
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
    snprintf(secret, sizeof secret, "%s/secret.db", outside);
    CHECK(sqlsrv_port_create_file(secret) == 0);
    snprintf(link_path, sizeof link_path, "%s/hosts.db", allow);
    if (sqlsrv_port_symlink_file(secret, link_path) == 0) {
        CHECK(resolve(&p, "hosts.db", &out, &e) == -1 && e.status == 0x20009u);
        CHECK(sqlsrv_port_remove_file(link_path) == 0);
    } else {
#if defined(_WIN32)
        /* Creating links needs a privilege on some Windows hosts: the link checks do not run there. */
        fprintf(stderr, "note: symbolic links are not permitted on this host; link checks skipped\n");
#else
        fprintf(stderr, "%s:%d: cannot create a symbolic link\n", __FILE__, __LINE__);
        failures++;
#endif
    }

    /* A dangling symbolic link is refused as well. */
    snprintf(target, sizeof target, "%s/no-such.db", outside);
    snprintf(link_path, sizeof link_path, "%s/dangling.db", allow);
    if (sqlsrv_port_symlink_file(target, link_path) == 0) {
        CHECK(resolve(&p, "dangling.db", &out, &e) == -1 && e.status == 0x20009u);
        CHECK(sqlsrv_port_remove_file(link_path) == 0);
    }

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
    (void)sqlsrv_port_remove_file(secret);
    (void)sqlsrv_port_remove_dir(allow);
    (void)sqlsrv_port_remove_dir(outside);
    (void)sqlsrv_port_remove_dir(root);
}

int main(void) {
    test_golden_encodings();
    test_reader();
    test_statuses();
    test_limits();
    test_validation();
    test_path_rules();
    test_policy();
    if (failures != 0) {
        fprintf(stderr, "%d check(s) failed\n", failures);
        return 1;
    }
    printf("hqqsqlite_server units: all checks passed\n");
    return 0;
}
