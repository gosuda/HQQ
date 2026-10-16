/* Named segments in one process (POSIX): name validation, create / open,
 * existing-name rules, transient opener errors, foreign and undersized objects,
 * safe unlink identity, reuse of a segment whose endpoints died. */
#include <fcntl.h>
#include <stdio.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <unistd.h>

#include "hqq_internal.h"
#include "hqq_test.h"
#include "hqq_testutil.h"

#define NM_COUNT 4u
#define NM_SIZE 64u

static hqq_options nm_opts(void) {
    hqq_options o;
    hqq_options_init(&o);
    o.heartbeat_interval_ns = 20 * (int64_t)HQQ_NS_PER_MS;
    o.peer_timeout_ns = 300 * (int64_t)HQQ_NS_PER_MS;
    o.attach_timeout_ns = 100 * (int64_t)HQQ_NS_PER_MS;
    return o;
}

/* Raw POSIX object helpers, to fake foreign or half-created segments. Bytes go through
 * a mapping: pwrite/pread are not portable on POSIX shared-memory objects. */
static int raw_open(const char *name, int oflag) {
    char path[300];
#if defined(__linux__)
    snprintf(path, sizeof path, "/dev/shm/%s", name);
    return open(path, oflag | O_CLOEXEC | O_NOFOLLOW, 0600);
#else
    snprintf(path, sizeof path, "/%s", name);
    return shm_open(path, oflag, 0600);
#endif
}

static int raw_exists(const char *name) {
    int fd = raw_open(name, O_RDONLY);
    if (fd < 0) {
        return 0;
    }
    close(fd);
    return 1;
}

static void make_raw_object(const char *name, size_t size, unsigned char byte) {
    int fd = raw_open(name, O_RDWR | O_CREAT | O_EXCL);
    HQQ_REQUIRE(fd >= 0);
    if (size > 0) {
        HQQ_REQUIRE(ftruncate(fd, (off_t)size) == 0);
    }
    if (byte != 0) {
        unsigned char *m = (unsigned char *)mmap(NULL, size, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
        HQQ_REQUIRE(m != MAP_FAILED);
        m[100] = byte;
        munmap(m, size);
    }
    close(fd);
}

/* Copies the first n bytes of a named object. */
static void read_raw(const char *name, unsigned char *buf, size_t n) {
    int fd = raw_open(name, O_RDONLY);
    unsigned char *m;
    HQQ_REQUIRE(fd >= 0);
    m = (unsigned char *)mmap(NULL, n, PROT_READ, MAP_SHARED, fd, 0);
    HQQ_REQUIRE(m != MAP_FAILED);
    memcpy(buf, m, n);
    munmap(m, n);
    close(fd);
}

static void check_names(void) {
    hqq_link *l = NULL;
    char name[300];
    size_t i;

    HQQ_CHECK_STATUS(hqq_standard_create("", NM_COUNT, NM_SIZE, 0600, NULL, &l, NULL), HQQ_ERR_INVALID_NAME);
    HQQ_CHECK_STATUS(hqq_standard_create("a/b", NM_COUNT, NM_SIZE, 0600, NULL, &l, NULL), HQQ_ERR_INVALID_NAME);
    HQQ_CHECK_STATUS(hqq_standard_create(".", NM_COUNT, NM_SIZE, 0600, NULL, &l, NULL), HQQ_ERR_INVALID_NAME);
    HQQ_CHECK_STATUS(hqq_standard_open("..", NM_COUNT, NM_SIZE, NULL, &l, NULL), HQQ_ERR_INVALID_NAME);
    HQQ_CHECK_STATUS(hqq_standard_open("a\\b", NM_COUNT, NM_SIZE, NULL, &l, NULL), HQQ_ERR_INVALID_NAME);
    HQQ_CHECK(l == NULL);
    for (i = 0; i < sizeof name - 1; i++) {
        name[i] = 'n';
    }
    name[sizeof name - 1] = '\0';
    HQQ_CHECK_STATUS(hqq_standard_open(name, NM_COUNT, NM_SIZE, NULL, &l, NULL), HQQ_ERR_INVALID_NAME);
    HQQ_CHECK(l == NULL);
    HQQ_CHECK_STATUS(hqq_unlink(""), HQQ_ERR_INVALID_NAME);
#if defined(__APPLE__)
    /* macOS: the whole name including the slash is at most 31 bytes. */
    memset(name, 'm', 31);
    name[31] = '\0';
    HQQ_CHECK_STATUS(hqq_standard_open(name, NM_COUNT, NM_SIZE, NULL, &l, NULL), HQQ_ERR_INVALID_NAME);
#endif
    /* A name that does not exist is ErrNotExist (a retrying opener retries it). */
    hqq_test_unique_name(name, sizeof name, "missing");
    HQQ_CHECK_STATUS(hqq_standard_open(name, NM_COUNT, NM_SIZE, NULL, &l, NULL), HQQ_ERR_NOT_EXIST);
    HQQ_CHECK(hqq_error_is_transient_open(HQQ_ERR_NOT_EXIST));
    HQQ_CHECK_STATUS(hqq_unlink(name), HQQ_ERR_NOT_EXIST);
}

static void check_create_open_close(void) {
    char name[64];
    hqq_link *p = NULL, *s = NULL, *l2 = NULL;
    hqq_options o = nm_opts();
    hqq_open_info info;
    unsigned char buf[NM_SIZE];
    size_t len = 0;

    hqq_test_unique_name(name, sizeof name, "cos");
    hqq_open_info_init(&info);
    HQQ_CHECK_STATUS(hqq_standard_create(name, NM_COUNT, NM_SIZE, 0600, &o, &p, &info), HQQ_OK);
    HQQ_REQUIRE(p != NULL);
    HQQ_CHECK_EQ_INT(hqq_link_mode(p), HQQ_MODE_PRIMARY);
    HQQ_CHECK(strcmp(hqq_link_name(p), name) == 0);

    /* A second creator while the first is alive is refused; the name stays. */
    HQQ_CHECK_STATUS(hqq_standard_create(name, NM_COUNT, NM_SIZE, 0600, &o, &s, NULL), HQQ_ERR_LINK_BUSY);
    HQQ_CHECK(s == NULL);
    /* A smaller geometry on the live name: geometry mismatch, both geometries reported. */
    HQQ_CHECK_STATUS(hqq_standard_open(name, 2, 8, &o, &s, &info), HQQ_ERR_GEOMETRY_MISMATCH);
    HQQ_CHECK(s == NULL);
    HQQ_CHECK_EQ_U64(info.got.buffer_count, NM_COUNT);

    HQQ_CHECK_STATUS(hqq_standard_open(name, NM_COUNT, NM_SIZE, &o, &s, NULL), HQQ_OK);
    HQQ_REQUIRE(s != NULL);
    HQQ_CHECK_EQ_INT(hqq_link_mode(s), HQQ_MODE_SECONDARY);
    HQQ_CHECK_STATUS(hqq_write(s, "named", 5, NULL), HQQ_OK);
    HQQ_CHECK_STATUS(hqq_read(p, buf, sizeof buf, &len, NULL), HQQ_OK);
    HQQ_CHECK_EQ_U64(len, 5);
    HQQ_CHECK(memcmp(buf, "named", 5) == 0);

    /* A third endpoint is busy. */
    {
        hqq_link *third = NULL;
        HQQ_CHECK_STATUS(hqq_standard_open(name, NM_COUNT, NM_SIZE, &o, &third, NULL), HQQ_ERR_LINK_BUSY);
        HQQ_CHECK(third == NULL);
    }

    /* Unlink by any endpoint of the instance removes the name (SPEC 11.4). */
    HQQ_CHECK_STATUS(hqq_link_unlink(s), HQQ_OK);
    HQQ_CHECK_STATUS(hqq_standard_open(name, NM_COUNT, NM_SIZE, &o, &l2, NULL), HQQ_ERR_NOT_EXIST);
    HQQ_CHECK(l2 == NULL);
    /* Closing the opener never removes a name: checked on a second, unlinked instance below. */
    HQQ_CHECK_STATUS(hqq_close(s), HQQ_OK);
    HQQ_CHECK_STATUS(hqq_close(p), HQQ_OK);

    /* Closing the opener keeps the name; closing the creator removes it. */
    HQQ_CHECK_STATUS(hqq_standard_create(name, NM_COUNT, NM_SIZE, 0600, &o, &p, NULL), HQQ_OK);
    HQQ_CHECK_STATUS(hqq_standard_open(name, NM_COUNT, NM_SIZE, &o, &s, NULL), HQQ_OK);
    HQQ_CHECK_STATUS(hqq_close(s), HQQ_OK);
    HQQ_CHECK(raw_exists(name));
    HQQ_CHECK_STATUS(hqq_close(p), HQQ_OK);
    HQQ_CHECK(!raw_exists(name));
}

/* Both endpoints die without closing (a crash, simulated by detaching both records):
 * the name still exists, a new creator re-initializes it, and the old creator's close
 * must not remove the replacement. */
static void check_reuse_after_death(void) {
    char name[64];
    hqq_link *p = NULL, *s = NULL, *again = NULL, *o = NULL;
    hqq_options opts = nm_opts();
    hqq_superblock *sb;

    hqq_test_unique_name(name, sizeof name, "reuse");
    HQQ_REQUIRE(hqq_standard_create(name, NM_COUNT, NM_SIZE, 0600, &opts, &p, NULL) == HQQ_OK);
    HQQ_REQUIRE(hqq_standard_open(name, NM_COUNT, NM_SIZE, &opts, &s, NULL) == HQQ_OK);
    HQQ_REQUIRE(hqq_write(p, "first", 5, NULL) == HQQ_OK);

    sb = p->sb;
    atomic_store(&sb->endpoints[0].token, HQQ_DETACHED_TOKEN);
    atomic_store(&sb->endpoints[1].token, HQQ_DETACHED_TOKEN);

    /* The name exists and is reused by a new creator (new creatorToken, epoch 2). */
    HQQ_CHECK_STATUS(hqq_standard_create(name, NM_COUNT, NM_SIZE, 0600, &opts, &again, NULL), HQQ_OK);
    HQQ_REQUIRE(again != NULL);
    HQQ_CHECK_EQ_INT(hqq_link_mode(again), HQQ_MODE_PRIMARY);
    HQQ_CHECK_EQ_U64(atomic_load(&again->sb->epoch), 2);

    /* The old creator closes: the replacement's name survives. */
    HQQ_CHECK_STATUS(hqq_close(p), HQQ_OK);
    HQQ_CHECK_STATUS(hqq_standard_open(name, NM_COUNT, NM_SIZE, &opts, &o, NULL), HQQ_OK);
    HQQ_CHECK_STATUS(hqq_close(o), HQQ_OK);
    HQQ_CHECK_STATUS(hqq_close(s), HQQ_OK);
    HQQ_CHECK_STATUS(hqq_close(again), HQQ_OK);
    HQQ_CHECK_STATUS(hqq_standard_open(name, NM_COUNT, NM_SIZE, &opts, &o, NULL), HQQ_ERR_NOT_EXIST);
}

/* A zero-size object is the transient "creator has not sized it yet". A smaller object is
 * ErrInvalidSize. A foreign object is refused and never written. */
static void check_existing_objects(void) {
    char name[64];
    hqq_link *l = NULL;
    hqq_options o = nm_opts();
    unsigned char before[256], after[256];

    hqq_test_unique_name(name, sizeof name, "zero");
    make_raw_object(name, 0, 0);
    HQQ_CHECK_STATUS(hqq_standard_open(name, NM_COUNT, NM_SIZE, &o, &l, NULL), HQQ_ERR_SEGMENT_NOT_READY);
    HQQ_CHECK(hqq_error_is_transient_open(HQQ_ERR_SEGMENT_NOT_READY));
    HQQ_CHECK_STATUS(hqq_standard_create(name, NM_COUNT, NM_SIZE, 0600, &o, &l, NULL), HQQ_ERR_SEGMENT_NOT_READY);
    HQQ_CHECK(l == NULL);
    HQQ_CHECK_STATUS(hqq_unlink(name), HQQ_OK);

    hqq_test_unique_name(name, sizeof name, "small");
    make_raw_object(name, 4096, 0);
    HQQ_CHECK_STATUS(hqq_standard_open(name, NM_COUNT, NM_SIZE, &o, &l, NULL), HQQ_ERR_INVALID_SIZE);
    HQQ_CHECK_STATUS(hqq_standard_create(name, NM_COUNT, NM_SIZE, 0600, &o, &l, NULL), HQQ_ERR_INVALID_SIZE);
    HQQ_CHECK_STATUS(hqq_unlink(name), HQQ_OK);

    hqq_test_unique_name(name, sizeof name, "foreign");
    make_raw_object(name, hqq_standard_size(NM_COUNT, NM_SIZE), 0x42);
    read_raw(name, before, sizeof before);
    HQQ_CHECK_STATUS(hqq_standard_create(name, NM_COUNT, NM_SIZE, 0600, &o, &l, NULL), HQQ_ERR_INCOMPATIBLE_LAYOUT);
    HQQ_CHECK(l == NULL);
    HQQ_CHECK_STATUS(hqq_standard_open(name, NM_COUNT, NM_SIZE, &o, &l, NULL), HQQ_ERR_INCOMPATIBLE_LAYOUT);
    HQQ_CHECK(l == NULL);
    read_raw(name, after, sizeof after);
    HQQ_CHECK(memcmp(before, after, sizeof before) == 0);
    HQQ_CHECK_STATUS(hqq_unlink(name), HQQ_OK);
}

/* Safe unlink: a creator whose name was replaced by another instance must not remove the
 * replacement when it closes, and hqq_unlink of a replaced name removes only that name. */
static void check_safe_unlink(void) {
    char name[64];
    hqq_link *a = NULL, *b = NULL, *o = NULL;
    hqq_options opts = nm_opts();

    hqq_test_unique_name(name, sizeof name, "safe");
    HQQ_REQUIRE(hqq_standard_create(name, NM_COUNT, NM_SIZE, 0600, &opts, &a, NULL) == HQQ_OK);
    /* The name is removed behind the live creator and another creator takes it. */
    HQQ_CHECK_STATUS(hqq_unlink(name), HQQ_OK);
    HQQ_REQUIRE(hqq_standard_create(name, NM_COUNT, NM_SIZE, 0600, &opts, &b, NULL) == HQQ_OK);

    /* Closing the first creator leaves the replacement in place. */
    HQQ_CHECK_STATUS(hqq_close(a), HQQ_OK);
    HQQ_CHECK_STATUS(hqq_standard_open(name, NM_COUNT, NM_SIZE, &opts, &o, NULL), HQQ_OK);
    HQQ_CHECK_STATUS(hqq_close(o), HQQ_OK);

    HQQ_CHECK_STATUS(hqq_close(b), HQQ_OK);
    HQQ_CHECK_STATUS(hqq_standard_open(name, NM_COUNT, NM_SIZE, &opts, &o, NULL), HQQ_ERR_NOT_EXIST);
}

int main(void) {
    check_names();
    check_create_open_close();
    check_reuse_after_death();
    check_existing_objects();
    check_safe_unlink();
    return HQQ_TEST_RESULT("test_named");
}
