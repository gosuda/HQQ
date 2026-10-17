/* Process entry of the microlib server (BINDGEN.md 9.6): hqqrpc_main parses the command line, prints
 * "ready <listener>" and serves until SIGINT, SIGTERM or end of file on the lifeline (--parent-stdin). */
#include "hqq/hqq_rpc.h"
#include "microlib_server.h"

int main(int argc, char **argv) {
    hqqrpc_server_options o;

    hqqrpc_server_options_init(&o);
    return hqqrpc_main(&microlib_iface, &o, argc, argv);
}
