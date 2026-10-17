/* Verbatim copy of the generated microlib server of BINDGEN.md 9.6 (interface digest 06a738744a1ea9f11fb08cc41afbe48e1891c76e60830a276e98cf8c15944e5c), kept here as a
 * test fixture of the C runtime until the emitter output of bindgen/e2e/microlib is available. */
#ifndef MICROLIB_SERVER_H
#define MICROLIB_SERVER_H

#include "hqq/hqq_rpc.h"
#include "rpc_ml.h"

#define MICROLIB_H_COUNTER 1u              /* handle type ids = indices into microlib_iface.handle_types, 1-based */
#define MICROLIB_H_CURSOR 2u

extern const hqqrpc_iface microlib_iface;

/* Interceptors, required: ml_counter_add is declared @intercept (7.3). The args hold no str/bytes member
 * here; for a function that has them the members point into private scratch. */
typedef struct microlib_args_ml_counter_add {
    ml_counter *c;
    int64_t delta;
} microlib_args_ml_counter_add;
hqqrpc_status microlib_intercept_ml_counter_add(hqqrpc_session *s, microlib_args_ml_counter_add *args);

/* Shims, required. A shim with a composite return gets the writer as its last parameter and returns
 * a response status (HQQRPC_OK, or an application status in 0x0100..0xFFFF; anything else becomes
 * E_INTERNAL, 4.6.6). */
hqqrpc_status ml_cells(ml_counter *c, int32_t max, hqqrpc_wr *ret);

/* Writers for the variants of union Cell (4.5). Text carries the bound of the IDL (str<=16). */
hqqrpc_status microlib_Cell_put_Int(hqqrpc_wr *w, int64_t v);
hqqrpc_status microlib_Cell_put_Text(hqqrpc_wr *w, const char *s, uint32_t n);
hqqrpc_status microlib_Cell_put_Null(hqqrpc_wr *w);

#endif
