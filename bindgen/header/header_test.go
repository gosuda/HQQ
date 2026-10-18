package header_test

import (
	"strings"
	"testing"

	"gosuda.org/hqq/bindgen/gen"
	"gosuda.org/hqq/bindgen/header"
	"gosuda.org/hqq/bindgen/idl"
)

func TestExtractZstdStyleHeader(t *testing.T) {
	const cHeader = `
#ifndef ZSTD_H
#define ZSTD_H

#include <stddef.h>

#ifdef __cplusplus
extern "C" {
#endif

#define ZSTD_VERSION_MAJOR 1
#define ZSTD_VERSION_MINOR 5
#define ZSTD_MAGICNUMBER 0xFD2FB528U

typedef enum {
    ZSTD_e_continue = 0,
    ZSTD_e_flush    = 1,
    ZSTD_e_end      = 2
} ZSTD_EndDirective;

typedef struct ZSTD_CCtx_s ZSTD_CCtx;
typedef struct ZSTD_DCtx_s ZSTD_DCtx;

ZSTDLIB_API unsigned ZSTD_versionNumber(void);
ZSTDLIB_API const char* ZSTD_versionString(void);

ZSTDLIB_API size_t ZSTD_compress(void* dst, size_t dstCapacity, const void* src, size_t srcSize, int compressionLevel);
ZSTDLIB_API size_t ZSTD_decompress(void* dst, size_t dstCapacity, const void* src, size_t compressedSize);

ZSTDLIB_API ZSTD_CCtx* ZSTD_createCCtx(void);
ZSTDLIB_API size_t ZSTD_freeCCtx(ZSTD_CCtx* cctx);

ZSTDLIB_API ZSTD_DCtx* ZSTD_createDCtx(void);
ZSTDLIB_API size_t ZSTD_freeDCtx(ZSTD_DCtx* dctx);

ZSTDLIB_API size_t ZSTD_compressCCtx(ZSTD_CCtx* cctx, void* dst, size_t dstCapacity, const void* src, size_t srcSize, int compressionLevel);

#ifdef __cplusplus
}
#endif
#endif
`
	idlBytes, err := header.Extract("zstd.h", []byte(cHeader), header.Options{
		Service:       "zstd",
		Prefix:        "ZSTD_",
		SerializeNone: true,
	})
	if err != nil {
		t.Fatalf("Extract failed: %v", err)
	}
	idlText := string(idlBytes)
	for _, want := range []string{
		"@serialize(none)",
		"service zstd;",
		`include "zstd.h";`,
		"const ZSTD_VERSION_MAJOR: i32 = 1;",
		"const ZSTD_e_end: i32 = 2;",
		`handle CCtx destroy ZSTD_freeCCtx;`,
		`handle DCtx destroy ZSTD_freeDCtx;`,
		"fn ZSTD_compress(out dst: bytes<=MAX_BUF_BYTES, dstCapacity: size @len_of(dst), src: bytes<=MAX_BUF_BYTES, srcSize: size @len_of(src), compressionLevel: i32) -> size @len_of(dst);",
	} {
		if !strings.Contains(idlText, want) {
			t.Errorf("generated IDL missing %q\nFull IDL:\n%s", want, idlText)
		}
	}

	spec, err := idl.Parse("zstd.idl", idlBytes, idl.Options{GoStrip: "ZSTD_"})
	if err != nil {
		t.Fatalf("idl.Parse failed on extracted IDL:\n%v\nFull IDL:\n%s", err, idlText)
	}
	if _, err := gen.Go(spec, gen.GoOptions{Package: "zstd", Strip: "ZSTD_"}); err != nil {
		t.Fatalf("gen.Go failed: %v", err)
	}
	if _, err := gen.C(spec, gen.COptions{Source: "zstd.idl"}); err != nil {
		t.Fatalf("gen.C failed: %v", err)
	}
}

func TestExtractSQLiteStyleHeaderWithOwnedByAndCancel(t *testing.T) {
	const cHeader = `
#define SQLITE_OK 0
#define SQLITE_OPEN_READWRITE (1 << 1)

typedef struct sqlite3 sqlite3;
typedef struct sqlite3_stmt sqlite3_stmt;
typedef long long sqlite3_int64;

SQLITE_API int sqlite3_open_v2(const char *filename, sqlite3 **ppDb, int flags, const char *zVfs);
SQLITE_API int sqlite3_close(sqlite3 *db);
SQLITE_API void sqlite3_interrupt(sqlite3 *db);
SQLITE_API int sqlite3_prepare_v2(sqlite3 *db, const char *zSql, int nByte, sqlite3_stmt **ppStmt, const char **pzTail);
SQLITE_API int sqlite3_step(sqlite3_stmt *pStmt);
SQLITE_API int sqlite3_finalize(sqlite3_stmt *pStmt);
SQLITE_API sqlite3_int64 sqlite3_column_int64(sqlite3_stmt *pStmt, int iCol);
SQLITE_API int sqlite3_exec(sqlite3 *db, const char *sql, int (*callback)(void*,int,char**,char**), void *arg, char **errmsg);
SQLITE_API char *sqlite3_mprintf(const char *zFormat, ...);
`
	idlBytes, err := header.Extract("sqlite3.h", []byte(cHeader), header.Options{
		Service: "sqlite3",
		Prefix:  "sqlite3_",
	})
	if err != nil {
		t.Fatalf("Extract failed: %v", err)
	}
	idlText := string(idlBytes)
	for _, want := range []string{
		"@cancel(sqlite3_interrupt)",
		"handle Db destroy sqlite3_close;",
		"handle Stmt owned_by Db destroy sqlite3_finalize;",
		"fn sqlite3_open_v2(filename: str<=MAX_STR_BYTES, out ppDb: Db, flags: i32, zVfs: str<=MAX_STR_BYTES) -> i32;",
		"fn sqlite3_prepare_v2(db: Db, zSql: str<=MAX_STR_BYTES, nByte: i32 @len_of(zSql), out ppStmt: Stmt, out pzTail: u32 @offset_in(zSql)) -> i32;",
		"// skipped sqlite3_exec:",
		"// skipped sqlite3_mprintf:",
	} {
		if !strings.Contains(idlText, want) {
			t.Errorf("generated IDL missing %q\nFull IDL:\n%s", want, idlText)
		}
	}

	spec, err := idl.Parse("sqlite3.idl", idlBytes, idl.Options{})
	if err != nil {
		t.Fatalf("idl.Parse failed on extracted IDL:\n%v\nFull IDL:\n%s", err, idlText)
	}
	if _, err := gen.Go(spec, gen.GoOptions{Package: "sqlite3"}); err != nil {
		t.Fatalf("gen.Go failed: %v", err)
	}
	if _, err := gen.C(spec, gen.COptions{Source: "sqlite3.idl"}); err != nil {
		t.Fatalf("gen.C failed: %v", err)
	}
}

func TestExtractStructsAndSynthesizedDestroyShim(t *testing.T) {
	const cHeader = `
#include <stdint.h>
#include <stddef.h>

typedef struct {
    int32_t x;
    int32_t y;
    uint8_t tag[4];
} geom_point_t;

typedef struct geom_ctx_s geom_ctx_t;

geom_ctx_t *geom_ctx_new(int32_t seed);
int32_t geom_compute(geom_ctx_t *ctx, const int32_t *coords, size_t n_coords, geom_point_t *out_pt);
`
	idlBytes, err := header.Extract("geom.h", []byte(cHeader), header.Options{})
	if err != nil {
		t.Fatalf("Extract failed: %v", err)
	}
	idlText := string(idlBytes)
	for _, want := range []string{
		"handle Ctx destroy destroy_ctx;",
		"shim fn destroy_ctx(h: Ctx);",
		"struct Point {",
		"fn geom_compute(ctx: Ctx, coords: i32[]<=65536, n_coords: size @len_of(coords), out out_pt: Point) -> i32;",
	} {
		if !strings.Contains(idlText, want) {
			t.Errorf("generated IDL missing %q\nFull IDL:\n%s", want, idlText)
		}
	}

	spec, err := idl.Parse("geom.idl", idlBytes, idl.Options{GoStrip: "geom_"})
	if err != nil {
		t.Fatalf("idl.Parse failed on extracted IDL:\n%v\nFull IDL:\n%s", err, idlText)
	}
	if _, err := gen.Go(spec, gen.GoOptions{Package: "geom", Strip: "geom_"}); err != nil {
		t.Fatalf("gen.Go failed: %v", err)
	}
	if _, err := gen.C(spec, gen.COptions{Source: "geom.idl"}); err != nil {
		t.Fatalf("gen.C failed: %v", err)
	}
}
