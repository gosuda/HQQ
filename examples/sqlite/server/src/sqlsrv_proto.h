/*
 * sqlsrv_proto.h - constants, status codes and byte codecs of the hqq-sqlite
 * protocol 1.0 (examples/sqlite/PROTOCOL.md). Integers are little-endian and
 * fixed width; there is no padding.
 */
#ifndef SQLSRV_PROTO_H
#define SQLSRV_PROTO_H

#include <stddef.h>
#include <stdint.h>

#define SQLSRV_PROTO_MAJOR 1u
#define SQLSRV_PROTO_MINOR 0u

/* Limits (PROTOCOL.md section 11). */
#define SQLSRV_MAX_MESSAGE_BYTES (UINT64_C(16) << 20)
#define SQLSRV_MAX_SQL_BYTES (UINT32_C(1) << 20)
#define SQLSRV_MAX_PARAMS 32766u
#define SQLSRV_MAX_COLUMNS 2000u
#define SQLSRV_MAX_FETCH_ROWS 4096u
#define SQLSRV_DEFAULT_FETCH_ROWS 256u
#define SQLSRV_MAX_BATCH_BYTES (UINT32_C(1) << 20)
#define SQLSRV_MAX_NAME_BYTES 1024u
#define SQLSRV_MAX_PATH_BYTES 4096u
#define SQLSRV_MAX_PRAGMAS 32u
#define SQLSRV_MAX_PRAGMA_NAME 64u
#define SQLSRV_MAX_PRAGMA_VALUE 255u
#define SQLSRV_MAX_STATEMENTS 256u
#define SQLSRV_MAX_CURSORS 64u

/* Open flags (PROTOCOL.md section 6.1). */
#define SQLSRV_OPEN_READONLY 0x1u
#define SQLSRV_OPEN_READWRITE 0x2u
#define SQLSRV_OPEN_CREATE 0x4u

/* Value tags: the SQLite fundamental type codes (section 3). */
#define SQLSRV_TAG_INTEGER 1u
#define SQLSRV_TAG_FLOAT 2u
#define SQLSRV_TAG_TEXT 3u
#define SQLSRV_TAG_BLOB 4u
#define SQLSRV_TAG_NULL 5u

enum sqlsrv_method {
    SQLSRV_M_OPEN = 1,
    SQLSRV_M_EXEC = 2,
    SQLSRV_M_QUERY = 3,
    SQLSRV_M_FETCH = 4,
    SQLSRV_M_CLOSE_CURSOR = 5,
    SQLSRV_M_PREPARE = 6,
    SQLSRV_M_EXEC_STMT = 7,
    SQLSRV_M_QUERY_STMT = 8,
    SQLSRV_M_CLOSE_STMT = 9,
    SQLSRV_M_BEGIN = 10,
    SQLSRV_M_COMMIT = 11,
    SQLSRV_M_ROLLBACK = 12,
    SQLSRV_M_PING = 13,
    SQLSRV_M_CLOSE = 14
};

/* Protocol error codes (section 7). */
enum sqlsrv_perr {
    SQLSRV_PE_BAD_REQUEST = 1,
    SQLSRV_PE_UNKNOWN_METHOD = 2,
    SQLSRV_PE_NO_SESSION = 3,
    SQLSRV_PE_ALREADY_OPEN = 4,
    SQLSRV_PE_NO_STATEMENT = 5,
    SQLSRV_PE_NO_CURSOR = 6,
    SQLSRV_PE_LIMIT = 7,
    SQLSRV_PE_VERSION = 8,
    SQLSRV_PE_FORBIDDEN = 9,
    SQLSRV_PE_INTERNAL = 10
};

/* Response statuses: 0x10000 + SQLite result code, 0x20000 + protocol code. */
#define SQLSRV_STATUS_SQLITE(rc) (UINT64_C(0x10000) + (uint64_t)(rc))
#define SQLSRV_STATUS_PROTO(code) (UINT64_C(0x20000) + (uint64_t)(code))

#define SQLSRV_ERRMSG_MAX 1280

/* An error of a request: the response status and its message. */
typedef struct sqlsrv_err {
    uint64_t status;
    char msg[SQLSRV_ERRMSG_MAX];
} sqlsrv_err;

void sqlsrv_errf(sqlsrv_err *e, uint64_t status, const char *fmt, ...)
    __attribute__((format(printf, 3, 4)));
void sqlsrv_err_proto(sqlsrv_err *e, enum sqlsrv_perr code, const char *msg);

/* Growable output buffer. Writes after an allocation failure set oom. */
typedef struct sqlsrv_buf {
    uint8_t *p;
    size_t len;
    size_t cap;
    int oom;
} sqlsrv_buf;

void sqlsrv_buf_free(sqlsrv_buf *b);
void sqlsrv_buf_u8(sqlsrv_buf *b, uint8_t v);
void sqlsrv_buf_u16(sqlsrv_buf *b, uint16_t v);
void sqlsrv_buf_u32(sqlsrv_buf *b, uint32_t v);
void sqlsrv_buf_i64(sqlsrv_buf *b, int64_t v);
void sqlsrv_buf_f64(sqlsrv_buf *b, double v);
void sqlsrv_buf_bytes(sqlsrv_buf *b, const void *p, size_t n);
/* string: u32 length, then the bytes. */
void sqlsrv_buf_str(sqlsrv_buf *b, const void *p, size_t n);
void sqlsrv_buf_put_u32(sqlsrv_buf *b, size_t pos, uint32_t v);
void sqlsrv_buf_put_u8(sqlsrv_buf *b, size_t pos, uint8_t v);

/* Bounds-checked reader. Every read returns 0, or -1 when the body is short. */
typedef struct sqlsrv_rd {
    const uint8_t *p;
    size_t n;
    size_t off;
} sqlsrv_rd;

void sqlsrv_rd_init(sqlsrv_rd *r, const void *p, size_t n);
int sqlsrv_rd_u8(sqlsrv_rd *r, uint8_t *v);
int sqlsrv_rd_u16(sqlsrv_rd *r, uint16_t *v);
int sqlsrv_rd_u32(sqlsrv_rd *r, uint32_t *v);
int sqlsrv_rd_i64(sqlsrv_rd *r, int64_t *v);
int sqlsrv_rd_f64(sqlsrv_rd *r, double *v);
/* string: the bytes are borrowed from the request body. */
int sqlsrv_rd_str(sqlsrv_rd *r, const uint8_t **p, uint32_t *n);
/* 0 when the whole body was consumed, -1 when bytes are left over. */
int sqlsrv_rd_end(const sqlsrv_rd *r);

/* Input validation of Open (section 6.1). */
int sqlsrv_valid_open_flags(uint32_t flags);
int sqlsrv_valid_pragma_name(const uint8_t *s, size_t n);
int sqlsrv_valid_pragma_value(const uint8_t *s, size_t n);

#endif /* SQLSRV_PROTO_H */
