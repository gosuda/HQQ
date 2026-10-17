/* edge: the toy library of the C emitter tests. */
#include "edge.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

struct edge_dir {
    int32_t id;
    bool locked;
    const char *err;
};

struct edge_file {
    edge_dir *dir;
    char name[17];
};

struct edge_tag {
    int unused;
};

static char g_log[64];
static size_t g_loglen;
static int64_t g_pings;
static int32_t g_frees;

static void logc(char c) {
    if (g_loglen + 1 < sizeof g_log) {
        g_log[g_loglen++] = c;
        g_log[g_loglen] = '\0';
    }
}

void edge_ping(void) { g_pings++; }
int64_t edge_pings(void) { return g_pings; }

void edge_reset(void) {
    g_loglen = 0;
    g_log[0] = '\0';
    g_pings = 0;
    g_frees = 0;
}

const char *edge_log(void) { return g_log; }
int32_t edge_frees(void) { return g_frees; }

void edge_free(void *p) {
    g_frees++;
    free(p);
}

double edge_mix(int8_t a, uint16_t b, float c, bool d, uint64_t e) {
    return (double)a + (double)b + (double)c + (d ? 1000.0 : 0.0) + (double)e;
}

bool edge_flip(bool b) { return !b; }
size_t edge_big(size_t v) { return v + 1; }

uint32_t edge_key(const uint8_t k[4]) {
    return (uint32_t)k[0] | (uint32_t)k[1] << 8 | (uint32_t)k[2] << 16 | (uint32_t)k[3] << 24;
}

int64_t edge_sum(const int64_t *v, size_t n) {
    int64_t s = 0;
    size_t i;
    for (i = 0; i < n; i++)
        s += v[i];
    return s;
}

int32_t edge_count_true(const bool *v, size_t n) {
    int32_t c = 0;
    size_t i;
    for (i = 0; i < n; i++)
        c += v[i] ? 1 : 0;
    return c;
}

int32_t edge_note(const char *s) { return s == NULL ? -1 : (int32_t)strlen(s); }

edge_dir *edge_dir_open(int32_t id) {
    edge_dir *d;
    if (id < 0)
        return NULL;
    d = (edge_dir *)calloc(1, sizeof *d);
    if (d != NULL) {
        d->id = id;
        d->err = "";
    }
    return d;
}

int32_t edge_dir_close(edge_dir *d) {
    if (d->locked) {
        logc('d');
        return 5;
    }
    logc('D');
    free(d);
    return 0;
}

void edge_dir_lock(edge_dir *d, bool on) { d->locked = on; }
int32_t edge_dir_peek(edge_dir *d) { return d == NULL ? -1 : d->id; }

edge_file *edge_file_open(edge_dir *d, const char *name) {
    edge_file *f;
    if (name[0] == '\0')
        return NULL;
    f = (edge_file *)calloc(1, sizeof *f);
    if (f != NULL) {
        f->dir = d;
        snprintf(f->name, sizeof f->name, "%s", name);
    }
    return f;
}

int32_t edge_file_close(edge_file *f) {
    logc('F');
    free(f);
    return 0;
}

edge_tag *edge_tag_new(void) { return (edge_tag *)calloc(1, sizeof(edge_tag)); }

void edge_tag_free(edge_tag *t) {
    logc('T');
    free(t);
}

int32_t edge_open_pair(edge_dir *d, edge_file **f, edge_tag **t) {
    *f = edge_file_open(d, "pair");
    *t = edge_tag_new();
    return d->id == 2 ? 3 : 0;
}

int32_t edge_stat(edge_dir *d, int32_t *n, edge_pt *p, int32_t *k, uint8_t tag[4]) {
    if (d->id == 3) {
        d->err = "bad stat";
        return 9;
    }
    d->err = "";
    *n = d->id * 2;
    memset(p, 0xAA, sizeof *p);
    p->x = d->id;
    p->ok = true;
    p->y = -(int64_t)d->id;
    *k += 1;
    memcpy(tag, "abcd", 4);
    return 0;
}

const char *edge_dir_name(edge_dir *d) {
    static char buf[16];
    if (d->id == 7)
        return NULL;
    snprintf(buf, sizeof buf, "dir%d", d->id);
    return buf;
}

int32_t edge_div(int32_t a, int32_t b) { return b == 0 ? INT32_MIN : a / b; }
int32_t edge_div_failed(int32_t r) { return r == INT32_MIN; }

int32_t edge_blob(edge_dir *d, void **p, size_t *n) {
    size_t len = d->id == 5 ? 17 : 6;
    char *m = (char *)malloc(len + 1);
    memset(m, 'x', len);
    m[len] = '\0';
    if (len == 6)
        memcpy(m, "blob-", 5);
    *p = m;
    *n = len;
    return d->id == 4 ? 1 : 0;
}

int32_t edge_label(edge_dir *d, char **s) {
    char buf[32];
    snprintf(buf, sizeof buf, "label-%d", d->id);
    *s = strdup(buf);
    return d->id == 4 ? 1 : 0;
}

int32_t edge_read(edge_dir *d, void *dst, size_t cap, size_t *n) {
    size_t want = (size_t)d->id + 3, i;
    if (d->id == 6) {
        d->err = "bad read";
        return 6;
    }
    if (d->id == 7) {
        *n = cap + 1;
        return 0;
    }
    d->err = "";
    *n = want < cap ? want : cap;
    for (i = 0; i < *n; i++)
        ((uint8_t *)dst)[i] = (uint8_t)(0x10 + i);
    return 0;
}

uint8_t edge_iota(int64_t start, int64_t *dst, uint32_t cap) {
    uint32_t n = cap < 5 ? cap : 5, i;
    for (i = 0; i < n; i++)
        dst[i] = start + (int64_t)i;
    return (uint8_t)n;
}

void edge_two(uint8_t *a, uint32_t ca, uint8_t *b, uint32_t cb, uint32_t *na, uint32_t *nb) {
    uint32_t n1 = ca < 3 ? ca : 3, n2 = cb < 2 ? cb : 2, i;
    for (i = 0; i < n1; i++)
        a[i] = (uint8_t)(0xA0 + i);
    for (i = 0; i < n2; i++)
        b[i] = (uint8_t)(0xB0 + i);
    *na = n1;
    *nb = n2;
}

int32_t edge_guard(edge_dir *d, const char *name, int32_t n, int32_t mode) {
    logc('g');
    return d->id * 100000 + n * 1000 + mode * 10 + (name[0] == 'g' ? 1 : 0);
}

const char *edge_errmsg(edge_dir *d) { return d->err; }
