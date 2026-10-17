/*
 * rpc_fuzz_driver.h - entry point for the libFuzzer harnesses of the C runtime when libFuzzer is
 * not linked (HQQ_FUZZ_STANDALONE). Run with file arguments it replays them; without arguments it
 * feeds HQQ_FUZZ_ITERS (default 5000) pseudo-random inputs, a smoke run that ctest executes.
 */
#ifndef RPC_FUZZ_DRIVER_H
#define RPC_FUZZ_DRIVER_H

#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

int LLVMFuzzerTestOneInput(const uint8_t *data, size_t size);

#ifdef HQQ_FUZZ_STANDALONE
int main(int argc, char **argv) {
    uint64_t x = UINT64_C(0x9E3779B97F4A7C15);
    unsigned long iters = 5000;
    unsigned long i;
    int a;
    static uint8_t buf[1 << 16];

    for (a = 1; a < argc; a++) {
        FILE *f = fopen(argv[a], "rb");
        size_t n;

        if (f == NULL) {
            fprintf(stderr, "cannot open %s\n", argv[a]);
            return 2;
        }
        n = fread(buf, 1, sizeof buf, f);
        fclose(f);
        (void)LLVMFuzzerTestOneInput(buf, n);
    }
    if (argc > 1) {
        return 0;
    }
    if (getenv("HQQ_FUZZ_ITERS") != NULL) {
        iters = strtoul(getenv("HQQ_FUZZ_ITERS"), NULL, 10);
    }
    for (i = 0; i < iters; i++) {
        size_t n;
        size_t j;

        x ^= x << 13;
        x ^= x >> 7;
        x ^= x << 17;
        n = (size_t)(x >> 40) % 600u;
        for (j = 0; j < n; j++) {
            x ^= x << 13;
            x ^= x >> 7;
            x ^= x << 17;
            /* biased towards small values and the bytes of valid frames */
            buf[j] = (uint8_t)((x >> 24) % 4u == 0 ? (x >> 32) : (x >> 24) % 20u);
        }
        (void)LLVMFuzzerTestOneInput(buf, n);
    }
    printf("fuzz smoke: %lu inputs\n", iters);
    return 0;
}
#endif

#endif /* RPC_FUZZ_DRIVER_H */
