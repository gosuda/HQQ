#include "hqq/hqq_rpc.h"
#include "microbench_server.h"

int main(int argc, char **argv) {
    hqqrpc_server_options o;
    hqqrpc_server_options_init(&o);
    return hqqrpc_main(&microbench_iface, &o, argc, argv);
}
