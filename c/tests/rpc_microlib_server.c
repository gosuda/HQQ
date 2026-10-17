/* The process entry of the microlib fixture server (BINDGEN.md 9.6 main.c): the program that
 * test_rpc_main starts. */
#include "hqq/hqq_rpc.h"
#include "rpc_ml_server.h"

int main(int argc, char **argv) {
    hqqrpc_server_options o;

    hqqrpc_server_options_init(&o);
    return hqqrpc_main(&microlib_iface, &o, argc, argv);
}
