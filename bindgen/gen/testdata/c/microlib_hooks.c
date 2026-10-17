/* Test hooks for the generated microlib glue: the interceptor and the shim of BINDGEN.md 9.6. */
#include "microlib_server.h"

/* ml_counter_add: refuse negative deltas, clamp large ones. */
hqqrpc_status microlib_intercept_ml_counter_add(hqqrpc_session *s, microlib_args_ml_counter_add *a) {
    (void)s;
    if (a->delta < 0)
        return HQQRPC_E_DENIED;
    if (a->delta > 1000000)
        a->delta = 1000000;
    return HQQRPC_OK;
}

/* ml_cells: `max` cells (at most 256), cycling Int(-2 + i / 3), Text "hi", Null. */
hqqrpc_status ml_cells(ml_counter *c, int32_t max, hqqrpc_wr *ret) {
    size_t mark;
    uint32_t i, n = max < 0 ? 0u : (uint32_t)max;
    hqqrpc_status st = hqqrpc_wr_array_begin(ret, &mark);

    (void)c;
    if (n > 256u)
        n = 256u;
    for (i = 0; st == HQQRPC_OK && i < n; i++) {
        if (i % 3 == 2)
            st = microlib_Cell_put_Null(ret);
        else if (i % 3 == 1)
            st = microlib_Cell_put_Text(ret, "hi", 2);
        else
            st = microlib_Cell_put_Int(ret, -2 + (int64_t)(i / 3));
    }
    return st != HQQRPC_OK ? st : hqqrpc_wr_array_end(ret, mark, n, 256u);
}
