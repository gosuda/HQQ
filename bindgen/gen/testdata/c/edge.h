/* edge: the toy library of the C emitter tests (bindgen/gen/testdata/c/edge.idl). Not thread-safe. */
#ifndef EDGE_H
#define EDGE_H

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

typedef struct edge_dir edge_dir;
typedef struct edge_file edge_file;
typedef struct edge_tag edge_tag;

/* x @0, ok @4, three bytes of padding, y @8. The library fills the padding with 0xAA. */
typedef struct edge_pt {
    int32_t x;
    bool ok;
    int64_t y;
} edge_pt;

void edge_ping(void);
int64_t edge_pings(void);
void edge_reset(void);
/* The destroy log: D closed directory, d refused (locked), F closed file, T freed tag, g guard call. */
const char *edge_log(void);
int32_t edge_frees(void);
void edge_free(void *p);

double edge_mix(int8_t a, uint16_t b, float c, bool d, uint64_t e);
bool edge_flip(bool b);
size_t edge_big(size_t v);
uint32_t edge_key(const uint8_t k[4]);
int64_t edge_sum(const int64_t *v, size_t n);
int32_t edge_count_true(const bool *v, size_t n);
int32_t edge_note(const char *s);

edge_dir *edge_dir_open(int32_t id);
int32_t edge_dir_close(edge_dir *d);
void edge_dir_lock(edge_dir *d, bool on);
int32_t edge_dir_peek(edge_dir *d);
edge_file *edge_file_open(edge_dir *d, const char *name);
int32_t edge_file_close(edge_file *f);
edge_tag *edge_tag_new(void);
void edge_tag_free(edge_tag *t);
int32_t edge_open_pair(edge_dir *d, edge_file **f, edge_tag **t);
int32_t edge_stat(edge_dir *d, int32_t *n, edge_pt *p, int32_t *k, uint8_t tag[4]);
const char *edge_dir_name(edge_dir *d);
int32_t edge_div(int32_t a, int32_t b);
int32_t edge_div_failed(int32_t r);
int32_t edge_blob(edge_dir *d, void **p, size_t *n);
int32_t edge_label(edge_dir *d, char **s);
int32_t edge_read(edge_dir *d, void *dst, size_t cap, size_t *n);
uint8_t edge_iota(int64_t start, int64_t *dst, uint32_t cap);
void edge_two(uint8_t *a, uint32_t ca, uint8_t *b, uint32_t cb, uint32_t *na, uint32_t *nb);
int32_t edge_guard(edge_dir *d, const char *name, int32_t n, int32_t mode);
const char *edge_errmsg(edge_dir *d);

#endif
