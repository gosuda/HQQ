/* Hand-written hooks of the benchmark server (BINDGEN.md 10.5). */
#include "microbench_server.h"

/*
 * ml_counter_add: refuse negative deltas, clamp large ones. This is the policy of the microlib
 * server (BINDGEN.md 9.6); bindgen/bench/cgobench applies the same rule in Go so that both sides of
 * BenchmarkCounterAdd compute the same results.
 */
hqqrpc_status microbench_intercept_ml_counter_add(hqqrpc_session *s, microbench_args_ml_counter_add *a) {
    (void)s;
    if (a->delta < 0)
        return HQQRPC_E_DENIED;
    if (a->delta > 1000000)
        a->delta = 1000000;
    return HQQRPC_OK;
}
