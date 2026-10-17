/* Test hook for the generated microbench glue: the interceptor of ml_counter_add (it admits every call). */
#include "microbench_server.h"

hqqrpc_status microbench_intercept_ml_counter_add(hqqrpc_session *s, microbench_args_ml_counter_add *a) {
    (void)s;
    (void)a;
    return HQQRPC_OK;
}
