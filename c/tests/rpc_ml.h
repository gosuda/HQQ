/*
 * rpc_ml.h - a small stand-in for the microlib test library of BINDGEN.md 2.7 (same C API, same
 * observable behavior), with probes for the tests of the C runtime: how many threads are inside
 * the library at once, how many objects are alive, and a hook that runs inside ml_add.
 */
#ifndef RPC_ML_H
#define RPC_ML_H

#include <stddef.h>
#include <stdint.h>

#define ML_OK 0
#define ML_EMPTY 1
#define ML_PARTIAL 2

int32_t ml_add(int32_t a, int32_t b);
typedef struct ml_counter ml_counter;
ml_counter *ml_counter_new(int64_t start);
int32_t ml_counter_add(ml_counter *c, int64_t delta, int64_t *total);
int32_t ml_counter_check(ml_counter *c, int32_t limit);
const char *ml_counter_errmsg(ml_counter *c);
void ml_counter_free(ml_counter *c);
typedef struct ml_cursor ml_cursor;
ml_cursor *ml_cursor_open(ml_counter *c, int32_t step);
int32_t ml_cursor_next(ml_cursor *cur, int64_t *value);
int32_t ml_cursor_free(ml_cursor *cur);
size_t ml_xor(const uint8_t *src, size_t n, uint8_t key, uint8_t *dst, size_t cap);
int ml_xor_failed(size_t result);
uint64_t ml_hash(const char *s);

/* ---- Probes for the tests ----------------------------------------------------------------- */

/* fn(ctx) is called inside ml_add (the library is "entered"), or never when fn is NULL. */
typedef void (*ml_probe_fn)(void *ctx);
void ml_test_set_probe(ml_probe_fn fn, void *ctx);
long ml_test_live_objects(void);  /* counters and cursors created and not yet freed */
long ml_test_max_overlap(void);   /* most threads inside the library at the same time */
long ml_test_entries(void);       /* library entries so far */
void ml_test_reset(void);         /* clears the overlap and entry counters (not the live objects) */

#endif /* RPC_ML_H */
