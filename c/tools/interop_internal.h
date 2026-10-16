/*
 * interop_internal.h - internals shared by the C interop helper hqq_interop
 * (conformance/INTEROP_CLI.md). Not installed.
 *
 *   hqq_interop.c      CLI, catalogue (list), result line, barrier, payload, error kinds, main
 *   interop_std.c      Standard scenarios (X-01 .. X-21)     - ihx_std_scenarios[]
 *   interop_adv.c      Advanced scenarios (X-A01 .. X-A10)   - ihx_adv_scenarios[]
 *
 * Every scenario is a table entry: the catalogue (`list`) is generated from the
 * table, `run` calls the scenario function. The scenario function returns IHX_OK
 * or IHX_FAIL (a check failed or an error occurred; e->error_kind is set), or
 * IHX_SKIP (exit 4), and may also exit via ihx_crash() at a crash point.
 */
#ifndef HQQ_INTEROP_INTERNAL_H
#define HQQ_INTEROP_INTERNAL_H

#include <stddef.h>
#include <stdint.h>

#include "hqq/hqq.h"

#ifdef __cplusplus
extern "C" {
#endif

/* Scenario outcomes returned by a run function. */
#define IHX_OK 0
#define IHX_FAIL 1
#define IHX_SKIP 4

/* Exit codes (INTEROP_CLI.md section 3). */
#define IHX_EXIT_OK 0
#define IHX_EXIT_FAILED 1
#define IHX_EXIT_USAGE 2
#define IHX_EXIT_TIMEOUT 3
#define IHX_EXIT_SKIPPED 4
#define IHX_EXIT_INTERNAL 5

#define IHX_MAX_CHECKS 64
#define IHX_NAME_MAX 128
#define IHX_DETAIL_MAX 256
#define IHX_ERROR_MAX 256

/* One assertion made by a role (result line "checks"). */
typedef struct ihx_check_rec {
    char name[IHX_NAME_MAX];
    int ok;
    char detail[IHX_DETAIL_MAX];
} ihx_check_rec;

/* Parsed `run` flags (INTEROP_CLI.md section 2). */
typedef struct ihx_config {
    const char *scenario;
    const char *variant;     /* "" = first variant */
    int step;                /* 1-based */
    int role;                /* 0 = creator (Primary), 1 = opener (Secondary) */
    const char *name;
    const char *sync_dir;
    const char *impl;        /* label of this helper, default "c" */
    const char *peer_impl;
    uint64_t buffer_count;   /* default 4 */
    uint64_t buffer_size;    /* default 256 */
    uint64_t size;           /* 0 = buffer_size */
    uint64_t messages;       /* default 100 */
    uint64_t seed;           /* default 1 */
    int64_t timeout_ns;      /* default 20 s */
    const char *crash_after; /* "" = none */
    int64_t heartbeat_ns;    /* 0 = library default, < 0 disabled */
    int64_t peer_timeout_ns; /* 0 = library default */
    int64_t attach_timeout_ns;
    int spin;                /* --wait spin */
    int long_sizes;          /* --long */
    uint64_t max_message;    /* Advanced MaxMessageSize, 0 = default */
    uint64_t max_reassembly; /* Advanced MaxReassemblyBytes, 0 = 4 x max_message */
    uint64_t queue_size;     /* Advanced IncomingQueueSize, 0 = buffer_count */
    const char *extra_args[8]; /* step args of the role, e.g. "--crash-after" (already parsed) */
} ihx_config;

/* Per-process state of one scenario run. */
typedef struct ihx_env {
    ihx_config cfg;
    uint64_t start_mono_ns;
    uint64_t deadline_mono_ns; /* hard process deadline */
    int failed;                /* a check failed */
    int crashed;               /* crash point injected: the result line was printed with crashed=true */
    const char *error_kind;    /* canonical error_kind name, "" = none */
    char error[IHX_ERROR_MAX];
    ihx_check_rec checks[IHX_MAX_CHECKS];
    int nchecks;
    uint64_t sent;
    uint64_t received;
    int meta_status_set;       /* meta.status (ResponseError) */
    uint64_t meta_status;
    char meta[512];            /* extra "key":value pairs of meta, may be empty */
} ihx_env;

/* Scenario tables ---------------------------------------------------------- */

/* One step of a variant: the roles started together and their expectations. */
typedef struct ihx_step {
    int nroles;                /* 1 or 2 */
    const char *roles[2];      /* "creator", "opener" */
    const char *expect[2];     /* per role: "ok", "crash", "ok|crash", "skip" */
    const char *args[2];      /* per role: extra flags as one string, e.g. "--crash-after claim", or NULL */
} ihx_step;

/* One variant: its name and parameters (the driver passes them as flags). A
 * zero field is absent from the catalogue ("absent keys keep the helper defaults"). */
typedef struct ihx_variant {
    const char *name;          /* unique, non-empty */
    uint64_t buffer_count;
    uint64_t buffer_size;
    uint64_t size;
    uint64_t messages;
    uint64_t seed;
    int64_t heartbeat_ms;      /* 0 = absent */
    int64_t peer_timeout_ms;   /* 0 = absent */
    int64_t attach_timeout_ms; /* 0 = absent */
    uint64_t max_message;      /* 0 = absent */
    uint64_t max_reassembly;   /* 0 = absent */
    uint64_t queue_size;       /* 0 = absent */
    int nsteps;
    ihx_step steps[4];
} ihx_variant;

typedef int (*ihx_run_fn)(ihx_env *e);

typedef struct ihx_scenario {
    const char *id;            /* "X-01", "X-A04" */
    const char *title;
    const char *kind;          /* "standard", "advanced", "acceptor" */
    const char *platforms;     /* "any", "windows", "linux,darwin" (comma separated GOOS names) */
    int timeout_s;             /* per-process deadline the driver uses */
    int nvariants;
    const ihx_variant *variants;
    ihx_run_fn run;
} ihx_scenario;

/* Tables defined by interop_std.c and interop_adv.c; each ends with a NULL id. */
extern const ihx_scenario ihx_std_scenarios[];
extern const ihx_scenario ihx_adv_scenarios[];

/* Services provided by hqq_interop.c --------------------------------------- */

/* Records a check. A failed check marks the run failed (and the result CheckFailed). */
void ihx_check(ihx_env *e, const char *name, int ok, const char *fmt, ...);
/* Records a check of a hqq_status against `want` (HQQ_OK or a base status with hqq_error_is). */
void ihx_check_status(ihx_env *e, const char *name, hqq_status got, hqq_status want);
/* Sets the error of the role (error_kind from the status, message free text). Returns IHX_FAIL. */
int ihx_fail_status(ihx_env *e, hqq_status st, const char *fmt, ...);
/* Sets error_kind directly (for example "CheckFailed" or "Unknown") and the message. Returns IHX_FAIL. */
int ihx_fail(ihx_env *e, const char *kind, const char *fmt, ...);
/* Canonical error_kind of a status (INTEROP_CLI.md section 4.1). */
const char *ihx_error_kind_of(hqq_status st);

/* Barrier (INTEROP_CLI.md section 6). Returns IHX_OK or IHX_FAIL (ErrTimeout). */
int ihx_barrier(ihx_env *e, const char *phase);
/* Sleeps ns in slices, returns early when the process deadline passes. */
void ihx_sleep_ns(ihx_env *e, uint64_t ns);
/* Monotonic now (ns) and remaining time to the process deadline (ns, may be <= 0). */
uint64_t ihx_now_ns(void);
int64_t ihx_remaining_ns(const ihx_env *e);
/* Crash point: when point equals --crash-after, prints the result line with
 * crashed=true, flushes and SIGKILLs the process (never returns in that case). */
void ihx_crash_point(ihx_env *e, const char *point);
/* True when this process was asked to crash at `point`. */
int ihx_crash_is(const ihx_env *e, const char *point);

/* Deterministic payloads (INTEROP_CLI.md section 5). */
void ihx_payload(uint64_t seed, uint64_t seq, uint8_t *out, size_t size);
/* 0 when buf == Payload(seed, seq, size), else non-zero. */
int ihx_payload_verify(uint64_t seed, uint64_t seq, const uint8_t *buf, size_t size);
/* CRC-32 IEEE, as used by the payload layout (size >= 16). */
uint32_t ihx_crc32(const uint8_t *p, size_t n);

/* Segment name helpers for the peer/attach scenarios. */
int ihx_name_is_valid(const char *s);

/* Scenario role helpers. */
const char *ihx_role_name(const ihx_env *e);
const char *ihx_peer_role_name(const ihx_env *e);

/* JSON-escapes `in` into out (cap bytes; no surrounding quotes), returns out. */
const char *ihx_json_escape(const char *in, char *out, size_t cap);

/* Result meta (the diagnostic "meta" object of the result line). */
void ihx_meta_u64(ihx_env *e, const char *key, uint64_t v);
void ihx_meta_bool(ihx_env *e, const char *key, int v);
void ihx_meta_str(ihx_env *e, const char *key, const char *v);

/* GOOS name of this build ("linux", "darwin", "windows", "freebsd", ...) and the
 * platform test of a scenario ("any" or comma-separated GOOS names). */
const char *ihx_goos(void);
int ihx_platform_ok(const char *platforms);

/* Raw producer on the transmit ring of this role (SPEC 7.4): ring 0 for the
 * creator, ring 1 for the opener. Maps cfg.name with cfg.buffer_count x
 * cfg.buffer_size. words[0] is the opcode word, words[1..7] the operands;
 * payload (may be NULL) is copied to the slot buffer, len <= buffer_size. The
 * wait for a free slot is bounded by the process deadline (ErrTimeout). */
typedef struct ihx_raw ihx_raw;
int ihx_raw_open(ihx_env *e, ihx_raw **out);
int ihx_raw_put(ihx_env *e, ihx_raw *r, const uint64_t words[8], const void *payload, size_t len);
void ihx_raw_close(ihx_raw *r);

/* Superblock words of a named segment, read without writing to it. */
typedef struct ihx_view {
    uint64_t magic, state, major, epoch, init_token, creator_token;
    uint32_t page_crc; /* CRC-32 of the whole first page */
} ihx_view;
int ihx_observe(ihx_env *e, const char *name, ihx_view *v);

/* Raw initializer of the crash scenarios (X-04 initializing, X-08): creates the
 * segment and runs SPEC 5 by hand until --crash-after claim|format|publish|zeroing,
 * where it dies. Returns IHX_SKIP for any other point. */
int ihx_raw_init_victim(ihx_env *e);

#ifdef __cplusplus
} /* extern "C" */
#endif

#endif /* HQQ_INTEROP_INTERNAL_H */
