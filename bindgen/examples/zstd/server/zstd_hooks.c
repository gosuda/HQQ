/* Hand-written part of the zstd server: the interceptor of ZSTD_CCtx_setParameter and the two
 * streaming shims declared in zstd.idl. */
#include "zstd_server.h"

#include <stddef.h>
#include <stdint.h>

#include "zstd_errors.h"

/* The decompressor refuses frames with a window above 2^27 unless the caller raises
 * ZSTD_d_windowLogMax, which this server does not expose; a larger compression window would
 * produce frames that the same server cannot decode. */
#define ZSTDSRV_MAX_WINDOWLOG 27

/* ZSTD_error_xxx as the size_t that zstd functions return (the ZSTD_ERROR macro is internal). */
#define ZSTDSRV_ERROR(code) ((size_t)0 - (size_t)(code))

hqqrpc_status zstd_intercept_ZSTD_CCtx_setParameter(hqqrpc_session *s, zstd_args_ZSTD_CCtx_setParameter *args) {
    (void)s;
    switch (args->param) {
    case ZSTD_c_compressionLevel:
    case ZSTD_c_strategy:
    case ZSTD_c_contentSizeFlag:
    case ZSTD_c_checksumFlag:
    case ZSTD_c_dictIDFlag:
        return HQQRPC_OK;
    case ZSTD_c_windowLog:
        return args->value > ZSTDSRV_MAX_WINDOWLOG ? HQQRPC_E_DENIED : HQQRPC_OK;
    default:
        return HQQRPC_E_DENIED;
    }
}

/* ZSTD_compressStream2 with the in and out buffer structs unpacked. On failure the positions are
 * not sent, so they are only written for a result that is not an error. */
size_t ZSTD_compressStream2_flat(ZSTD_CCtx *cctx, void *dst, size_t dstCapacity, const void *src, size_t srcSize,
                                 uint32_t endOp, size_t *dstPos, size_t *srcPos) {
    ZSTD_outBuffer out = {dst, dstCapacity, 0};
    ZSTD_inBuffer in = {src, srcSize, 0};
    size_t r;

    if (endOp > (uint32_t)ZSTD_e_end)
        return ZSTDSRV_ERROR(ZSTD_error_parameter_outOfBound);
    r = ZSTD_compressStream2(cctx, &out, &in, (ZSTD_EndDirective)endOp);
    *dstPos = out.pos;
    *srcPos = in.pos;
    return r;
}

size_t ZSTD_decompressStream_flat(ZSTD_DCtx *dctx, void *dst, size_t dstCapacity, const void *src, size_t srcSize,
                                  size_t *dstPos, size_t *srcPos) {
    ZSTD_outBuffer out = {dst, dstCapacity, 0};
    ZSTD_inBuffer in = {src, srcSize, 0};
    size_t r = ZSTD_decompressStream(dctx, &out, &in);

    *dstPos = out.pos;
    *srcPos = in.pos;
    return r;
}
