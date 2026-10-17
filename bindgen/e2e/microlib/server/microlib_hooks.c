/* Hand-written part of the microlib server (BINDGEN.md 9.6): the interceptor of ml_counter_add and the
 * ml_cells shim, both declared in the generated microlib_server.h.
 *
 * Test hook. If the environment variable ML_E2E_STALL_MS holds a positive number, a call of
 * ml_counter_add with delta == ML_E2E_STALL_DELTA first writes "microlib_hooks: stall" and a newline
 * to stderr and then sleeps that many milliseconds inside the interceptor, which runs under the
 * execution lock (7.3): it stands for a library call that does not return (6.4). The e2e tests use it
 * to check the shutdown timeout and the lock; without the variable the interceptor is exactly the one
 * of 9.6. */
#include <stdio.h>
#include <stdlib.h>
#include <time.h>

#include "microlib_server.h"

#define ML_E2E_STALL_DELTA 424242
#define ML_CLAMP 1000000
#define ML_CELLS_MAX 256u

/* Read at the first use; the interceptor runs inside the execution lock, so there is no race. */
static long stall_ms(void) {
    static int read;
    static long ms;

    if (!read) {
        const char *v = getenv("ML_E2E_STALL_MS");

        ms = v != NULL ? strtol(v, NULL, 10) : 0;
        read = 1;
    }
    return ms;
}

static void stall(long ms) {
    struct timespec ts;

    (void)fputs("microlib_hooks: stall\n", stderr);
    (void)fflush(stderr);
    ts.tv_sec = ms / 1000;
    ts.tv_nsec = (ms % 1000) * 1000000L;
    while (nanosleep(&ts, &ts) != 0) {
    }
}

/* ml_counter_add: refuse negative deltas, clamp large ones. */
hqqrpc_status microlib_intercept_ml_counter_add(hqqrpc_session *s, microlib_args_ml_counter_add *a) {
    (void)s;
    if (a->delta == ML_E2E_STALL_DELTA && stall_ms() > 0)
        stall(stall_ms());
    if (a->delta < 0)
        return HQQRPC_E_DENIED;
    if (a->delta > ML_CLAMP)
        a->delta = ML_CLAMP;
    return HQQRPC_OK;
}

/* ml_cells: `max` cells (at most 256), cycling Int(-2 + i / 3), Text "hi", Null. */
hqqrpc_status ml_cells(ml_counter *c, int32_t max, hqqrpc_wr *ret) {
    size_t mark;
    uint32_t i, n = max < 0 ? 0u : (uint32_t)max;
    hqqrpc_status st = hqqrpc_wr_array_begin(ret, &mark);

    (void)c;
    if (n > ML_CELLS_MAX)
        n = ML_CELLS_MAX;
    for (i = 0; st == HQQRPC_OK && i < n; i++) {
        if (i % 3 == 2)
            st = microlib_Cell_put_Null(ret);
        else if (i % 3 == 1)
            st = microlib_Cell_put_Text(ret, "hi", 2);
        else
            st = microlib_Cell_put_Int(ret, -2 + (int64_t)(i / 3));
    }
    return st != HQQRPC_OK ? st : hqqrpc_wr_array_end(ret, mark, n, ML_CELLS_MAX);
}
