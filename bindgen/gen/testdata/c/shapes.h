/* The library declarations of shapes.idl. Only declarations: the glue generated from shapes.idl is compiled, not run. */
#ifndef SHAPES_H
#define SHAPES_H

#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

typedef struct sh_obj sh_obj;
typedef struct sh_sub sh_sub;

typedef struct sh_pair {
    uint8_t a;
    uint32_t b;
} sh_pair;

typedef struct sh_wide {
    sh_pair p;
    bool flags[3];
    uint64_t q;
    float f;
} sh_wide;

uint16_t sh_scalars(int8_t a, int16_t b, uint16_t c, uint32_t d, int64_t e, float f, double g, bool h, size_t k, long long m);
int32_t sh_status_neg(int32_t a);
bool sh_ret_bool(bool a);
float sh_ret_f32(void);
long sh_ret_long(void);
int32_t sh_in_fixed(const bool a[4], const sh_pair b[2], const int16_t c[3]);
int32_t sh_in_var(const sh_pair *a, uint32_t na, const bool *b, size_t nb, const double *c, int32_t nc);
int32_t sh_in_hooked(const uint8_t a[4], const void *b, int32_t nb, const double *c, int32_t nc, const char *s, int32_t *k);
void sh_no_args(void);
int32_t sh_out_big(uint8_t a[2000], sh_pair *b, bool *c, double *d, sh_pair e[2]);
void sh_out_arr(int32_t *a, int32_t ca, const sh_pair **b, uint16_t *nb);
int32_t sh_out_ret_arr(sh_obj *o, void *buf, uint32_t cap);
const void *sh_ret_bytes(sh_obj *o, uint32_t *n);
const int32_t *sh_ret_arr(sh_obj *o, uint32_t *n);
const char *sh_ret_str_free(void);
void sh_free(void *p);
int32_t sh_detail_ret(sh_obj *o);
const char *sh_msg(sh_obj *o, int32_t r);
sh_obj *sh_pred_obj(int32_t n);
const char *sh_msg2(sh_obj *r);
int32_t sh_bad(sh_obj *r);
sh_pair sh_ret_struct(int32_t a);
void sh_inout(int32_t *a, bool *b, double *c);
int32_t sh_off(const char *s, const char **t, const char **u);
int32_t sh_off_n(const char *s, int32_t n, const char **t);
sh_obj *sh_obj_new(void);
sh_sub *sh_sub_new(sh_obj *a, int32_t b);
int32_t sh_obj_free(sh_obj *o);
void sh_sub_free(sh_sub *o);
int32_t sh_two(sh_obj *a, sh_obj *b);

#endif
