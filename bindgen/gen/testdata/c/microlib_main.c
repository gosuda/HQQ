/* main.c of the microlib server, as in BINDGEN.md 9.6. */
#include "hqq/hqq_rpc.h"
#include "microlib_server.h"

int main(int argc, char **argv) {
    hqqrpc_server_options o;
    hqqrpc_server_options_init(&o);
    return hqqrpc_main(&microlib_iface, &o, argc, argv);
}
