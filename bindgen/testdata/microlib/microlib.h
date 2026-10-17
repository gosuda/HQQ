/*
 * microlib: the MIT-licensed test library of hqq-bindgen (spec/toolchain/BINDGEN.md 2.7).
 *
 * Portable C11, no dependencies. The library has global state (the trace
 * switch and the id counters) and is NOT thread-safe: callers enter it one
 * at a time, which is the contract of `@serialize(session)`.
 *
 * Handles are never NULL in a valid call, except where a function says
 * otherwise. Arguments are not validated.
 *
 * Trace. If the environment variable ML_TRACE names a file, every call
 * appends one line to it, opened and closed per call, so a line survives a
 * `kill -9` of the process. The variable is read at the first call: it is
 * unset or empty (no tracing) or a path, for the life of the process. A trace
 * write that fails is ignored. Lines end in "\n" and contain no pointer, pid
 * or time, so the trace of a fixed call sequence is the same on every run.
 * Counters and cursors are numbered from 1 in creation order, in separate
 * sequences per process; 0 stands for "none" (a failed allocation or a NULL
 * argument). Fields are `name=value`, in this order:
 *
 *   ml_add a=<i32> b=<i32> ret=<i32>
 *   ml_counter_new start=<i64> counter=<id>
 *   ml_counter_add counter=<id> delta=<i64> ret=<i32> [total=<i64>]
 *   ml_counter_check counter=<id> limit=<i32> ret=<i32>
 *   ml_counter_errmsg counter=<id> msg="<text>"
 *   ml_counter_free counter=<id>
 *   ml_cursor_open counter=<id> step=<i32> cursor=<id>
 *   ml_cursor_next cursor=<id> ret=<i32> [value=<i64>]
 *   ml_cursor_free cursor=<id> ret=<i32>
 *   ml_xor n=<u64> key=<u8> cap=<u64> ret=<u64>
 *   ml_xor_failed result=<u64> ret=<i32>
 *   ml_hash len=<u64> ret=<u64>
 *   ml_work iters=<u32> ret=<u64>
 *
 * `total=` is present only when ml_counter_add returned 0, `value=` only when
 * ml_cursor_next returned 0. Integers are decimal and signed where the type
 * is signed. A destroy call logs its line before it releases the object, so
 * the order of the destroy lines is the order of destruction.
 */
#ifndef MICROLIB_H
#define MICROLIB_H

#include <stddef.h>
#include <stdint.h>

#define ML_OK 0
#define ML_EMPTY 1
#define ML_PARTIAL 2

/* a + b modulo 2^32, read as two's complement. */
int32_t ml_add(int32_t a, int32_t b);

typedef struct ml_counter ml_counter;

/* A counter holding `start`. NULL on out of memory. */
ml_counter *ml_counter_new(int64_t start);

/*
 * Adds delta to the total. Returns 0 and writes the new total to *total, or
 * returns -1 if the sum does not fit in an int64_t: the total is unchanged
 * and *total is not written.
 */
int32_t ml_counter_add(ml_counter *c, int64_t delta, int64_t *total);

/* 0 if the total is at most limit, else -1. */
int32_t ml_counter_check(ml_counter *c, int32_t limit);

/*
 * The message of the latest ml_counter_add or ml_counter_check on c:
 * "overflow" if it failed in add, "over limit" if it failed in check, else
 * "" (also for a counter that was never used). Never NULL. The pointer is
 * borrowed: it addresses static storage and stays valid after any later
 * call.
 */
const char *ml_counter_errmsg(ml_counter *c);

/* Releases the counter. NULL is accepted and ignored. */
void ml_counter_free(ml_counter *c);

typedef struct ml_cursor ml_cursor;

/*
 * A cursor over three values: the total of c at the time of the call, then
 * that total plus step and plus 2 * step, modulo 2^64. The cursor keeps no
 * reference to c, so it stays usable after c is freed. NULL on out of memory.
 */
ml_cursor *ml_cursor_open(ml_counter *c, int32_t step);

/*
 * Returns ML_OK and writes the next value to *value, or ML_EMPTY when the
 * three values were read: *value is not written, and every later call
 * returns ML_EMPTY too.
 */
int32_t ml_cursor_next(ml_cursor *cur, int64_t *value);

/*
 * Releases the cursor in every case. Returns ML_PARTIAL if values were
 * unread (ML_EMPTY was not required: reading all three values is enough),
 * else ML_OK. NULL is accepted, ignored and returns ML_OK.
 */
int32_t ml_cursor_free(ml_cursor *cur);

/*
 * dst[i] = src[i] ^ key for i < n. Returns n, or SIZE_MAX, without writing
 * dst, if cap < n. src and dst must not overlap; a pointer is not used when
 * n is 0.
 */
size_t ml_xor(const uint8_t *src, size_t n, uint8_t key, uint8_t *dst, size_t cap);

/* 1 if result == SIZE_MAX, else 0. */
int ml_xor_failed(size_t result);

/* FNV-1a, 64 bits, over the bytes of the NUL-terminated string (not including the NUL). */
uint64_t ml_hash(const char *s);

/*
 * Busy work for the crossover benchmark (10.5): iters rounds of a serial
 * dependency chain, so the cost grows linearly with iters and the loop can
 * be neither vectorized nor removed. Starting from x = 0x9e3779b97f4a7c15,
 * each round is x = (x ^ (x >> 29)) * 0xbf58476d1ce4e5b9 + 1 modulo 2^64;
 * the result is the final x. ml_work(0) is the start value.
 */
uint64_t ml_work(uint32_t iters);

#endif
