/* Compares the library against spec/constants.json (through the generated
 * header): struct offsets and sizes, magics, opcodes, error codes, layout
 * vectors, packet vectors, name vectors and geometry limits. */
#include <string.h>

#include "hqq_internal.h"
#include "hqq_test.h"
#include "spec_constants.h"

_Static_assert(sizeof(hqq_superblock) == HQQ_SPEC_SB_SIZE, "superblock size");
_Static_assert(sizeof(hqq_endpoint_record) == HQQ_SPEC_EP_SIZE, "endpoint record size");
_Static_assert(offsetof(hqq_superblock, magic) == HQQ_SPEC_SB_MAGIC_OFFSET, "magic");
_Static_assert(offsetof(hqq_superblock, state) == HQQ_SPEC_SB_STATE_OFFSET, "state");
_Static_assert(offsetof(hqq_superblock, major) == HQQ_SPEC_SB_MAJOR_OFFSET, "major");
_Static_assert(offsetof(hqq_superblock, minor) == HQQ_SPEC_SB_MINOR_OFFSET, "minor");
_Static_assert(offsetof(hqq_superblock, page_size) == HQQ_SPEC_SB_PAGE_SIZE_OFFSET, "pageSize");
_Static_assert(offsetof(hqq_superblock, buffer_count) == HQQ_SPEC_SB_BUFFER_COUNT_OFFSET, "bufferCount");
_Static_assert(offsetof(hqq_superblock, buffer_size) == HQQ_SPEC_SB_BUFFER_SIZE_OFFSET, "bufferSize");
_Static_assert(offsetof(hqq_superblock, layout_size) == HQQ_SPEC_SB_LAYOUT_SIZE_OFFSET, "layoutSize");
_Static_assert(offsetof(hqq_superblock, epoch) == HQQ_SPEC_SB_EPOCH_OFFSET, "epoch");
_Static_assert(offsetof(hqq_superblock, init_token) == HQQ_SPEC_SB_INIT_TOKEN_OFFSET, "initToken");
_Static_assert(offsetof(hqq_superblock, init_heartbeat) == HQQ_SPEC_SB_INIT_HEARTBEAT_OFFSET, "initHeartbeat");
_Static_assert(offsetof(hqq_superblock, required_features) == HQQ_SPEC_SB_REQUIRED_FEATURES_OFFSET, "requiredFeatures");
_Static_assert(offsetof(hqq_superblock, creator_token) == HQQ_SPEC_SB_CREATOR_TOKEN_OFFSET, "creatorToken");
_Static_assert(offsetof(hqq_superblock, endpoints) == HQQ_SPEC_SB_ENDPOINTS_OFFSET, "endpoints");
_Static_assert(offsetof(hqq_endpoint_record, token) == HQQ_SPEC_EP_TOKEN_OFFSET, "ep token");
_Static_assert(offsetof(hqq_endpoint_record, pid) == HQQ_SPEC_EP_PID_OFFSET, "ep pid");
_Static_assert(offsetof(hqq_endpoint_record, heartbeat) == HQQ_SPEC_EP_HEARTBEAT_OFFSET, "ep heartbeat");
_Static_assert(offsetof(hqq_endpoint_record, interval) == HQQ_SPEC_EP_INTERVAL_OFFSET, "ep interval");
_Static_assert(sizeof(hqq_ring_header) == HQQ_SPEC_RING_HEADER_SIZE, "ring header size");
_Static_assert(offsetof(hqq_ring_header, magic) == HQQ_SPEC_RING_MAGIC_OFFSET, "ring magic");
_Static_assert(offsetof(hqq_ring_header, size) == HQQ_SPEC_RING_SIZE_OFFSET, "ring size");
_Static_assert(offsetof(hqq_ring_header, flag) == HQQ_SPEC_RING_FLAG_OFFSET, "ring flag");
_Static_assert(offsetof(hqq_ring_header, stride) == HQQ_SPEC_RING_STRIDE_OFFSET, "ring stride");
_Static_assert(offsetof(hqq_ring_header, version) == HQQ_SPEC_RING_VERSION_OFFSET, "ring version");
_Static_assert(offsetof(hqq_ring_header, r) == HQQ_SPEC_RING_R_OFFSET, "ring r");
_Static_assert(offsetof(hqq_ring_header, w) == HQQ_SPEC_RING_W_OFFSET, "ring w");
_Static_assert(offsetof(hqq_ring_header, not_empty) == HQQ_SPEC_RING_NOT_EMPTY_OFFSET, "notEmpty");
_Static_assert(offsetof(hqq_ring_header, empty_waiters) == HQQ_SPEC_RING_EMPTY_WAITERS_OFFSET, "emptyWaiters");
_Static_assert(offsetof(hqq_ring_header, not_full) == HQQ_SPEC_RING_NOT_FULL_OFFSET, "notFull");
_Static_assert(offsetof(hqq_ring_header, full_waiters) == HQQ_SPEC_RING_FULL_WAITERS_OFFSET, "fullWaiters");
_Static_assert(sizeof(hqq_slot) == HQQ_SPEC_RING_SLOT_STRIDE, "slot stride");
_Static_assert(offsetof(hqq_slot, packet) == HQQ_SPEC_RING_SLOT_PAYLOAD_OFFSET, "slot payload");
_Static_assert(HQQ_RING_HEADER_SIZE == HQQ_SPEC_RING_HEADER_SIZE, "ring header");
_Static_assert(HQQ_RING_SLOT_STRIDE == HQQ_SPEC_RING_SLOT_STRIDE, "ring stride");
_Static_assert(HQQ_RING_PACKET_SIZE == HQQ_SPEC_PACKET_SIZE, "packet size");
_Static_assert(HQQ_OP_STD_COPY == HQQ_SPEC_OP_STANDARD_LINK_COPY, "copy opcode");
_Static_assert(HQQ_OP_STD_TOMBSTONE == HQQ_SPEC_OP_STANDARD_LINK_TOMBSTONE, "tombstone opcode");
_Static_assert(HQQ_OP_ERROR == HQQ_SPEC_OP_ERROR, "error opcode");
_Static_assert(HQQ_OP_CONN_COPY == HQQ_SPEC_OP_CONN_COPY, "conn copy opcode");
_Static_assert(HQQ_OP_PROTO_NEGOTIATE == HQQ_SPEC_OP_PROTO_NEGOTIATE, "negotiate opcode");
_Static_assert(HQQ_OP_PROTO_ACK == HQQ_SPEC_OP_PROTO_ACK, "ack opcode");
_Static_assert(HQQ_SB_MAGIC == HQQ_SPEC_MAGIC_SB, "superblock magic");
_Static_assert(HQQ_RING_MAGIC == HQQ_SPEC_MAGIC_RING, "ring magic");
_Static_assert(HQQ_SB_MAJOR == HQQ_SPEC_VERSION_SEGMENT_LAYOUT_MAJOR, "layout major");
_Static_assert(HQQ_SB_MINOR == HQQ_SPEC_VERSION_SEGMENT_LAYOUT_MINOR, "layout minor");
_Static_assert(HQQ_RING_VERSION == HQQ_SPEC_VERSION_RING_LAYOUT, "ring layout version");
_Static_assert(HQQ_STATE_EMPTY == HQQ_SPEC_STATE_EMPTY && HQQ_STATE_INITIALIZING == HQQ_SPEC_STATE_INITIALIZING &&
               HQQ_STATE_READY == HQQ_SPEC_STATE_READY, "states");
_Static_assert(HQQ_DETACHED_TOKEN == HQQ_SPEC_TOKEN_DETACHED, "detached token");
_Static_assert(HQQ_RING_FLAG_INIT == 2, "ring flag init bit is 2 (SPEC 7.1)");
_Static_assert(HQQ_MIN_BUFFER_COUNT == HQQ_SPEC_GEOM_MIN_BUFFER_COUNT, "min count");
_Static_assert(HQQ_MAX_BUFFER_COUNT == HQQ_SPEC_GEOM_MAX_BUFFER_COUNT, "max count");
_Static_assert(HQQ_MIN_BUFFER_SIZE == HQQ_SPEC_GEOM_MIN_BUFFER_SIZE, "min size");
_Static_assert(HQQ_MAX_BUFFER_SIZE == HQQ_SPEC_GEOM_MAX_BUFFER_SIZE, "max size");
_Static_assert(HQQ_BUFFER_SIZE_ALIGNMENT == HQQ_SPEC_GEOM_BUFFER_SIZE_ALIGNMENT, "size alignment");
_Static_assert(HQQ_DEFAULT_HEARTBEAT_INTERVAL_NS == HQQ_SPEC_TIMING_NS_DEFAULT_HEARTBEAT_INTERVAL, "hb");
_Static_assert(HQQ_DEFAULT_PEER_TIMEOUT_NS == HQQ_SPEC_TIMING_NS_DEFAULT_PEER_TIMEOUT, "peer timeout");
_Static_assert(HQQ_DEFAULT_ATTACH_TIMEOUT_NS == HQQ_SPEC_TIMING_NS_DEFAULT_ATTACH_TIMEOUT, "attach timeout");
_Static_assert(HQQ_ERRCODE_MEMORY_ALIGN == HQQ_SPEC_ERRCODE_MEMORY_ALIGN, "errcode 1");
_Static_assert(HQQ_ERRCODE_INVALID_SIZE == HQQ_SPEC_ERRCODE_INVALID_SIZE, "errcode 2");
_Static_assert(HQQ_ERRCODE_MEMORY_SMALL == HQQ_SPEC_ERRCODE_MEMORY_SMALL, "errcode 3");
_Static_assert(HQQ_ERRCODE_FAILED_INIT == HQQ_SPEC_ERRCODE_FAILED_INIT, "errcode 4");
_Static_assert(HQQ_ERRCODE_CONN_NOT_FOUND == HQQ_SPEC_ERRCODE_CONN_NOT_FOUND, "errcode 5");
_Static_assert(HQQ_ERRCODE_INVALID_OP == HQQ_SPEC_ERRCODE_INVALID_OP, "errcode 6");
_Static_assert(HQQ_ERRCODE_BUFFER_OVERFLOW == HQQ_SPEC_ERRCODE_BUFFER_OVERFLOW, "errcode 7");
_Static_assert(HQQ_SPEC_PACKET_OPCODE_MASK == 255, "opcode mask");

/* Page-size independent check: total bytes of every layout vector. */
static void check_layout_vectors(void) {
    unsigned i;
    for (i = 0; i < HQQ_SPEC_LAYOUT_VECTOR_COUNT; i++) {
        const hqq_spec_layout_vector *v = &hqq_spec_layout_vectors[i];
        hqq_layout l;
        int ok = hqq_layout_compute(&l, v->page_size, v->buffer_count, v->buffer_size, UINT64_MAX);
        HQQ_CHECK(ok);
        HQQ_CHECK_EQ_U64(l.ring0, v->ring0);
        HQQ_CHECK_EQ_U64(l.buf0, v->buffers0);
        HQQ_CHECK_EQ_U64(l.ring1, v->ring1);
        HQQ_CHECK_EQ_U64(l.buf1, v->buffers1);
        HQQ_CHECK_EQ_U64(l.total, v->total);
        if (v->page_size == hqq_plat_page_size()) {
            HQQ_CHECK_EQ_U64(hqq_standard_size(v->buffer_count, v->buffer_size), v->total);
        }
    }
}

/* Packet vectors: the bytes are the little-endian words of opcode and operands. */
static void check_packet_vectors(void) {
    unsigned i;
    for (i = 0; i < HQQ_SPEC_PACKET_VECTOR_COUNT; i++) {
        const hqq_spec_packet_vector *v = &hqq_spec_packet_vectors[i];
        hqq_packet p;
        unsigned k;
        memset(&p, 0, sizeof p);
        p.w[0] = v->opcode;
        for (k = 0; k < v->operand_count; k++) {
            p.w[1u + k] = v->operands[k];
        }
        HQQ_CHECK(memcmp(&p, v->bytes, sizeof p) == 0);
        HQQ_CHECK_EQ_U64(hqq_packet_op(&p), v->opcode);
        for (k = 0; k < v->operand_count; k++) {
            HQQ_CHECK_EQ_U64(hqq_packet_operand(&p, k), v->operands[k]);
        }
        /* Decoding the bytes: opcode from the low byte of word 0 only. */
        memcpy(&p, v->bytes, sizeof p);
        p.w[0] |= UINT64_C(0xFFFFFF00); /* reserved bits must be ignored */
        HQQ_CHECK_EQ_U64(hqq_packet_op(&p), v->opcode);
    }
    /* hqq_packet_make writes exactly the words of the standard copy vector. */
    {
        hqq_packet p;
        hqq_packet_make(&p, HQQ_OP_STD_COPY, 1, 2, 3);
        HQQ_CHECK(strcmp(hqq_spec_packet_vectors[0].name, "standard_copy") == 0);
        HQQ_CHECK(memcmp(&p, hqq_spec_packet_vectors[0].bytes, sizeof p) == 0);
    }
}

/* Magic words are little-endian in memory: "3LTS_QQH" and "02 67 09 3F D4 C1 D8 C9". */
static void check_magic_bytes(void) {
    uint64_t sb = HQQ_SB_MAGIC, ring = HQQ_RING_MAGIC;
    const unsigned char sb_bytes[8] = {0x33, 0x4c, 0x54, 0x53, 0x5f, 0x51, 0x51, 0x48};
    const unsigned char ring_bytes[8] = {0x02, 0x67, 0x09, 0x3f, 0xd4, 0xc1, 0xd8, 0xc9};
    unsigned char got[8];
    memcpy(got, &sb, 8);
    HQQ_CHECK(memcmp(got, sb_bytes, 8) == 0);
    memcpy(got, &ring, 8);
    HQQ_CHECK(memcmp(got, ring_bytes, 8) == 0);
    HQQ_CHECK(memcmp(HQQ_SPEC_MAGIC_SB_ASCII, "HQQ_STL3", 8) == 0);
}

/* Name validation vectors (SPEC 11.1). */
static void check_name_vectors(void) {
    unsigned i;
    for (i = 0; i < HQQ_SPEC_INVALID_NAME_COUNT; i++) {
        const hqq_spec_bytes *v = &hqq_spec_invalid_names[i];
        /* Only the portable rules here; the length rules are per platform. */
        char buf[16];
        size_t n = v->len < sizeof buf - 1 ? v->len : sizeof buf - 1;
        memcpy(buf, v->s, n);
        buf[n] = '\0';
        if (memchr(v->s, 0, v->len) != NULL) {
            /* An embedded NUL cannot reach the API as one string: nothing to check. */
            continue;
        }
        HQQ_CHECK_STATUS(hqq_name_base(buf, 0, NULL, NULL), HQQ_ERR_INVALID_NAME);
    }
    for (i = 0; i < HQQ_SPEC_WIN_NAME_VECTOR_COUNT; i++) {
        const hqq_spec_name_vector *v = &hqq_spec_win_name_vectors[i];
        char out[512];
        size_t len = 0;
        HQQ_CHECK_STATUS(hqq_windows_object_name(v->name, out, sizeof out, &len), HQQ_OK);
        HQQ_CHECK(strcmp(out, v->object) == 0);
        HQQ_CHECK_EQ_U64(len, strlen(v->object));
    }
    for (i = 0; i < HQQ_SPEC_WIN_INVALID_NAME_COUNT; i++) {
        const hqq_spec_bytes *v = &hqq_spec_win_invalid_names[i];
        char buf[64];
        memcpy(buf, v->s, v->len);
        buf[v->len] = '\0';
        HQQ_CHECK_STATUS(hqq_windows_object_name(buf, NULL, 0, NULL), HQQ_ERR_INVALID_NAME);
    }
    /* Length budgets: 255 bytes of base name are fine, 256 are not. */
    {
        char name[300];
        memset(name, 'a', sizeof name);
        name[255] = '\0';
        HQQ_CHECK_STATUS(hqq_name_base(name, 0, NULL, NULL), HQQ_OK);
        name[256] = '\0'; /* after the first NUL the string is 256 bytes long */
        name[255] = 'a';
        HQQ_CHECK_STATUS(hqq_name_base(name, 0, NULL, NULL), HQQ_ERR_INVALID_NAME);
    }
    /* A leading slash is removed exactly once. */
    HQQ_CHECK_STATUS(hqq_name_base("//x", 0, NULL, NULL), HQQ_ERR_INVALID_NAME);
    HQQ_CHECK_STATUS(hqq_name_base("/x", 0, NULL, NULL), HQQ_OK);
    HQQ_CHECK_STATUS(hqq_name_base("x\\y", 0, NULL, NULL), HQQ_ERR_INVALID_NAME);
}

/* Geometry limits (SPEC 2.2, G-04): invalid geometries have size 0. */
static void check_geometry(void) {
    HQQ_CHECK_EQ_U64(hqq_standard_size(0, 64), 0);
    HQQ_CHECK_EQ_U64(hqq_standard_size(1, 64), 0);
    HQQ_CHECK_EQ_U64(hqq_standard_size(3, 64), 0);
    HQQ_CHECK_EQ_U64(hqq_standard_size(4, 0), 0);
    HQQ_CHECK_EQ_U64(hqq_standard_size(4, 4), 0);
    HQQ_CHECK_EQ_U64(hqq_standard_size(4, 12), 0);
    HQQ_CHECK_EQ_U64(hqq_standard_size(HQQ_SPEC_GEOM_MAX_BUFFER_COUNT * 2, 64), 0);
    HQQ_CHECK_EQ_U64(hqq_standard_size(4, HQQ_SPEC_GEOM_MAX_BUFFER_SIZE + 8), 0);
    HQQ_CHECK(hqq_standard_size(2, 8) != 0);
    HQQ_CHECK(hqq_standard_size(4, 64) != 0);
}

/* Error mapping (SPEC 12, error_codes and the sentinel relations). */
static void check_error_relations(void) {
    HQQ_CHECK(hqq_error_is(HQQ_ERR_LINK_BUSY, HQQ_ERR_FAILED_INIT));
    HQQ_CHECK(hqq_error_is(HQQ_ERR_INCOMPATIBLE_LAYOUT, HQQ_ERR_FAILED_INIT));
    HQQ_CHECK(hqq_error_is(HQQ_ERR_SEGMENT_NOT_READY, HQQ_ERR_FAILED_INIT));
    HQQ_CHECK(hqq_error_is(HQQ_ERR_INIT_CONTENDED, HQQ_ERR_FAILED_INIT));
    HQQ_CHECK(hqq_error_is(HQQ_ERR_NO_LIVE_ENDPOINT, HQQ_ERR_FAILED_INIT));
    HQQ_CHECK(hqq_error_is(HQQ_ERR_GEOMETRY_MISMATCH, HQQ_ERR_INVALID_SIZE));
    HQQ_CHECK(hqq_error_is(HQQ_ERR_PEER_GONE, HQQ_ERR_CONN_NOT_FOUND));
    HQQ_CHECK(hqq_error_is(HQQ_ERR_UNEXPECTED_PACKET, HQQ_ERR_INVALID_OP));
    HQQ_CHECK(hqq_error_is(HQQ_ERR_PROTOCOL, HQQ_ERR_INVALID_OP));
    HQQ_CHECK(!hqq_error_is(HQQ_ERR_PEER_CLOSED, HQQ_ERR_CONN_NOT_FOUND));
    HQQ_CHECK(!hqq_error_is(HQQ_ERR_TIMEOUT, HQQ_ERR_FAILED_INIT));
    HQQ_CHECK(hqq_error_is_transient_open(HQQ_ERR_NOT_EXIST));
    HQQ_CHECK(!hqq_error_is_transient_open(HQQ_ERR_LINK_BUSY));
    HQQ_CHECK(strcmp(hqq_strerror(HQQ_OK), "ok") == 0);
    HQQ_CHECK(hqq_strerror((hqq_status)9999) != NULL);
}

int main(void) {
    check_layout_vectors();
    check_packet_vectors();
    check_magic_bytes();
    check_name_vectors();
    check_geometry();
    check_error_relations();
    return HQQ_TEST_RESULT("test_constants");
}
