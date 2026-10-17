/*
 * hqq_platform_win32.c - the Windows implementation of hqq_platform.h.
 *
 * Mirrors the Go reference (internal/shm/shm_windows.go, winnames.go,
 * internal/mpmc/park_windows.go, internal/mmap/mmap_windows.go) and SPEC 8.5,
 * 11.1, 11.4, 11.5:
 *   - a named segment is a paging-file backed file mapping plus four named
 *     kernel semaphores (<object name>.hqq-sem0 .. .hqq-sem3);
 *   - cross-process parking uses those semaphores (one token per wake, F-02);
 *   - unnamed memory parks on WaitOnAddress/WakeByAddressAll (process local).
 *
 * Link libraries (CMake owner): kernel32 and advapi32 (OpenProcessToken,
 * GetTokenInformation, ConvertSidToStringSidW,
 * ConvertStringSecurityDescriptorToSecurityDescriptorW). Everything else is
 * resolved at run time with GetProcAddress, so no further import library is
 * needed: ntdll (NtQuerySection, RtlNtStatusToDosError), KernelBase or
 * api-ms-win-core-synch-l1-2-0.dll (WaitOnAddress, WakeByAddressAll),
 * advapi32 SystemFunction036 (RtlGenRandom), kernel32
 * GetSystemTimePreciseAsFileTime.
 *
 * Requires Windows 7 or newer (WaitOnAddress: Windows 8; without it the
 * unnamed-memory wait degrades to a 1 ms sleep, which the backoff of
 * HQQ_PARK_LOCAL waiters tolerates).
 */
#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#ifndef NOMINMAX
#define NOMINMAX
#endif
#ifndef _WIN32_WINNT
#define _WIN32_WINNT 0x0A00
#endif
#include <windows.h>
#include <sddl.h>

#include <stddef.h>
#include <stdint.h>
#include <string.h>

#include "hqq_internal.h"
#include "hqq_platform.h"

_Static_assert(sizeof(FARPROC) == sizeof(void *), "function pointers are stored in void * slots");

/* ---- Helpers ------------------------------------------------------------------- */

static void set_err(int *os_err, DWORD e) {
    if (os_err != NULL) {
        *os_err = (int)e;
    }
}

/* Milliseconds for a wait, rounded up, at least min_ms, never INFINITE. */
static DWORD ceil_ms(uint64_t ns, DWORD min_ms) {
    uint64_t ms = ns / 1000000u + (ns % 1000000u != 0 ? 1u : 0u);
    if (ms < min_ms) {
        ms = min_ms;
    }
    if (ms >= 0xFFFFFFFFu) {
        ms = 0xFFFFFFFEu; /* INFINITE is 0xFFFFFFFF */
    }
    return (DWORD)ms;
}

static HMODULE module_handle(const wchar_t *name) {
    HMODULE m = GetModuleHandleW(name);
    if (m == NULL) {
        m = LoadLibraryW(name);
    }
    return m;
}

/* Resolves symbol in the first of the given modules that exports it. */
static FARPROC resolve(const wchar_t *const *modules, size_t n, const char *symbol) {
    for (size_t i = 0; i < n; i++) {
        HMODULE m = module_handle(modules[i]);
        FARPROC p = m != NULL ? GetProcAddress(m, symbol) : NULL;
        if (p != NULL) {
            return p;
        }
    }
    return NULL;
}

static FARPROC resolve_one(const wchar_t *module, const char *symbol) {
    return resolve(&module, 1, symbol);
}

static FARPROC resolve_synch(const char *symbol) {
    static const wchar_t *const mods[] = {L"kernelbase.dll", L"api-ms-win-core-synch-l1-2-0.dll", L"kernel32.dll"};
    return resolve(mods, sizeof mods / sizeof mods[0], symbol);
}

/* ---- Time, identity, randomness ---------------------------------------------------- */

uint64_t hqq_plat_mono_ns(void) {
    LARGE_INTEGER f, c;
    uint64_t freq, cnt;
    if (!QueryPerformanceFrequency(&f) || f.QuadPart <= 0 || !QueryPerformanceCounter(&c)) {
        return (uint64_t)GetTickCount64() * 1000000u;
    }
    freq = (uint64_t)f.QuadPart;
    cnt = (uint64_t)c.QuadPart;
    /* cnt * 1e9 / freq without overflowing 64 bits: split off whole seconds. */
    return (cnt / freq) * 1000000000u + (cnt % freq) * 1000000000u / freq;
}

uint64_t hqq_plat_wall_ns(void) {
    typedef VOID(WINAPI * precise_fn)(LPFILETIME);
    static const wchar_t *const mods[] = {L"kernel32.dll"};
    FILETIME ft;
    ULARGE_INTEGER u;
    precise_fn precise;
    FARPROC p = resolve(mods, 1, "GetSystemTimePreciseAsFileTime");

    if (p != NULL) {
        memcpy(&precise, &p, sizeof precise);
        precise(&ft);
    } else {
        GetSystemTimeAsFileTime(&ft);
    }
    u.LowPart = ft.dwLowDateTime;
    u.HighPart = ft.dwHighDateTime;
    /* 100 ns ticks since 1601-01-01 -> ns since 1970-01-01. */
    if (u.QuadPart < 116444736000000000ull) {
        return 0;
    }
    return (u.QuadPart - 116444736000000000ull) * 100u;
}

void hqq_plat_yield(void) {
    (void)SwitchToThread();
}

#ifndef CREATE_WAITABLE_TIMER_HIGH_RESOLUTION
#define CREATE_WAITABLE_TIMER_HIGH_RESOLUTION 0x00000002
#endif

void hqq_plat_sleep_ns(uint64_t ns) {
    HANDLE t;
    if (ns == 0) {
        hqq_plat_yield();
        return;
    }
    /* A high-resolution waitable timer (Windows 10 1803+) honours sub-millisecond
     * delays; elsewhere fall back to Sleep, rounded up to whole milliseconds. */
    t = CreateWaitableTimerExW(NULL, NULL, CREATE_WAITABLE_TIMER_HIGH_RESOLUTION, TIMER_ALL_ACCESS);
    if (t != NULL) {
        LARGE_INTEGER due;
        uint64_t ticks = ns / 100u + (ns % 100u != 0 ? 1u : 0u); /* 100 ns units */
        due.QuadPart = -(LONGLONG)ticks;
        if (SetWaitableTimer(t, &due, 0, NULL, NULL, FALSE)) {
            (void)WaitForSingleObject(t, INFINITE);
            CloseHandle(t);
            return;
        }
        CloseHandle(t);
    }
    {
        uint64_t ms = ns / 1000000u + (ns % 1000000u != 0 ? 1u : 0u);
        while (ms > 0) {
            DWORD step = ms > 0x7FFFFFFFu ? 0x7FFFFFFFu : (DWORD)ms;
            Sleep(step);
            ms -= step;
        }
    }
}

uint64_t hqq_plat_pid(void) {
    return (uint64_t)GetCurrentProcessId();
}

uint64_t hqq_plat_page_size(void) {
    SYSTEM_INFO si;
    GetSystemInfo(&si);
    return si.dwPageSize >= 4096u ? (uint64_t)si.dwPageSize : 4096u;
}

int hqq_plat_random(void *buf, size_t n) {
    typedef BOOLEAN(WINAPI * rtlgenrandom_fn)(PVOID, ULONG); /* SystemFunction036 */
    rtlgenrandom_fn gen;
    FARPROC p = resolve_one(L"advapi32.dll", "SystemFunction036");
    unsigned char *b = buf;

    if (p == NULL) {
        return -1;
    }
    memcpy(&gen, &p, sizeof gen);
    while (n > 0) {
        ULONG chunk = n > 0x40000000u ? 0x40000000u : (ULONG)n;
        if (!gen(b, chunk)) {
            return -1;
        }
        b += chunk;
        n -= chunk;
    }
    return 0;
}

/* ---- Threads and events ----------------------------------------------------------------- */

struct hqq_plat_thread {
    HANDLE handle;
    void (*fn)(void *);
    void *arg;
};

static DWORD WINAPI thread_main(LPVOID p) {
    struct hqq_plat_thread *t = p;
    t->fn(t->arg);
    return 0;
}

hqq_plat_thread *hqq_plat_thread_start(const hqq_allocator *a, void (*fn)(void *), void *arg, int *os_err) {
    struct hqq_plat_thread *t = hqq_alloc(a, sizeof *t);
    if (t == NULL) {
        set_err(os_err, ERROR_NOT_ENOUGH_MEMORY);
        return NULL;
    }
    t->fn = fn;
    t->arg = arg;
    t->handle = CreateThread(NULL, 0, thread_main, t, 0, NULL);
    if (t->handle == NULL) {
        set_err(os_err, GetLastError());
        hqq_free(a, t, sizeof *t);
        return NULL;
    }
    return t;
}

void hqq_plat_thread_join(const hqq_allocator *a, hqq_plat_thread *t) {
    if (t == NULL) {
        return;
    }
    (void)WaitForSingleObject(t->handle, INFINITE);
    CloseHandle(t->handle);
    hqq_free(a, t, sizeof *t);
}

void hqq_plat_thread_detach(hqq_plat_thread *t) {
    if (t != NULL && t->handle != NULL) {
        CloseHandle(t->handle);
        t->handle = NULL;
    }
}

struct hqq_plat_event {
    HANDLE handle;
};

hqq_plat_event *hqq_plat_event_create(const hqq_allocator *a) {
    struct hqq_plat_event *e = hqq_alloc(a, sizeof *e);
    if (e == NULL) {
        return NULL;
    }
    e->handle = CreateEventW(NULL, TRUE, FALSE, NULL); /* manual reset, unsignalled */
    if (e->handle == NULL) {
        hqq_free(a, e, sizeof *e);
        return NULL;
    }
    return e;
}

void hqq_plat_event_destroy(const hqq_allocator *a, hqq_plat_event *e) {
    if (e == NULL) {
        return;
    }
    CloseHandle(e->handle);
    hqq_free(a, e, sizeof *e);
}

void hqq_plat_event_signal(hqq_plat_event *e) {
    (void)SetEvent(e->handle);
}

void hqq_plat_event_reset(hqq_plat_event *e) {
    (void)ResetEvent(e->handle);
}

int hqq_plat_event_wait(hqq_plat_event *e, uint64_t timeout_ns) {
    return WaitForSingleObject(e->handle, ceil_ms(timeout_ns, 0)) == WAIT_OBJECT_0;
}

struct hqq_plat_mutex {
    SRWLOCK lock;
};

hqq_plat_mutex *hqq_plat_mutex_create(const hqq_allocator *a) {
    struct hqq_plat_mutex *m = hqq_alloc(a, sizeof *m);
    if (m == NULL) {
        return NULL;
    }
    InitializeSRWLock(&m->lock);
    return m;
}

void hqq_plat_mutex_destroy(const hqq_allocator *a, hqq_plat_mutex *m) {
    if (m == NULL) {
        return;
    }
    hqq_free(a, m, sizeof *m);
}

void hqq_plat_mutex_lock(hqq_plat_mutex *m) {
    AcquireSRWLockExclusive(&m->lock);
}

void hqq_plat_mutex_unlock(hqq_plat_mutex *m) {
    ReleaseSRWLockExclusive(&m->lock);
}

/* ---- Named shared memory -------------------------------------------------------------------- */

#define SEM_COUNT 4

struct hqq_shm {
    HANDLE mapping;
    HANDLE sem[SEM_COUNT];
};

/* Access rights, numerically as in winnames.go. */
#define R_READ_CONTROL 0x00020000u
#define R_SYNCHRONIZE 0x00100000u
#define R_SECTION_QUERY 0x0001u
#define R_MAP_WRITE 0x0002u
#define R_MAP_READ 0x0004u
#define R_SEM_QUERY 0x0001u
#define R_SEM_MODIFY 0x0002u
#define R_SECTION_ALL 0x000F001Fu
#define R_SEM_ALL 0x001F0003u

/* The semaphores are the objects 2*ring (notEmpty) and 2*ring+1 (notFull). */
static const char *const sem_suffix[SEM_COUNT] = {".hqq-sem0", ".hqq-sem1", ".hqq-sem2", ".hqq-sem3"};

#define SUFFIX_LEN 9
#define MAX_OBJECT_NAME 259 /* UTF-16 units, without the NUL */
#define WNAME_CAP 320

int hqq_plat_names_persist(void) {
    return 0;
}

/* -- SDDL ------------------------------------------------------------------------------------ */

typedef struct sddl_buf {
    char *p;
    size_t cap;
    size_t len;
    int overflow;
} sddl_buf;

static void sddl_str(sddl_buf *b, const char *s) {
    size_t n = strlen(s);
    if (b->len + n + 1 > b->cap) {
        b->overflow = 1;
        return;
    }
    memcpy(b->p + b->len, s, n + 1);
    b->len += n;
}

/* "0x" followed by lowercase hex without leading zeros (like %#x of Go's 0x%x). */
static void sddl_hex(sddl_buf *b, uint32_t v) {
    char tmp[16];
    size_t i = sizeof tmp;
    tmp[--i] = '\0';
    do {
        tmp[--i] = "0123456789abcdef"[v & 0xFu];
        v >>= 4;
    } while (v != 0);
    tmp[--i] = 'x';
    tmp[--i] = '0';
    sddl_str(b, tmp + i);
}

static uint32_t rights_for_bits(unsigned bits, int section) {
    uint32_t r = 0;
    if ((bits & 04u) != 0 || (bits & 02u) != 0) {
        r = section ? (R_READ_CONTROL | R_SECTION_QUERY | R_MAP_READ) : (R_READ_CONTROL | R_SYNCHRONIZE | R_SEM_QUERY);
    }
    if ((bits & 02u) != 0) {
        r |= section ? R_MAP_WRITE : R_SEM_MODIFY;
    }
    return r;
}

/* securityDescriptorSDDL of winnames.go. Returns 0 or a Win32 error. */
static DWORD build_sddl(unsigned perm, const char *user_sid, int section, char *out, size_t cap) {
    sddl_buf b = {out, cap, 0, 0};
    uint32_t all = section ? R_SECTION_ALL : R_SEM_ALL;
    unsigned owner = (perm >> 6) & 07u;
    unsigned other = perm & 07u;

    out[0] = '\0';
    sddl_str(&b, "D:P(A;;");
    sddl_hex(&b, all);
    sddl_str(&b, ";;;SY)");
    if (owner != 0) {
        uint32_t mask = (owner & 02u) != 0 ? all : rights_for_bits(owner, section);
        sddl_str(&b, "(A;;");
        sddl_hex(&b, mask);
        sddl_str(&b, ";;;");
        sddl_str(&b, user_sid);
        sddl_str(&b, ")");
    }
    if (other != 0) {
        sddl_str(&b, "(A;;");
        sddl_hex(&b, rights_for_bits(other, section));
        sddl_str(&b, ";;;WD)");
    }
    return b.overflow ? ERROR_INSUFFICIENT_BUFFER : 0;
}

/* String SID (ASCII) of the user of the process token. Returns 0 or a Win32 error. */
static DWORD current_user_sid(char *out, size_t cap) {
    union {
        TOKEN_USER user;
        BYTE raw[sizeof(TOKEN_USER) + SECURITY_MAX_SID_SIZE + 16];
    } info;
    HANDLE token = NULL;
    DWORD got = 0;
    LPWSTR wsid = NULL;
    DWORD err = 0;
    size_t n = 0;

    if (!OpenProcessToken(GetCurrentProcess(), TOKEN_QUERY, &token)) {
        return GetLastError();
    }
    if (!GetTokenInformation(token, TokenUser, &info, sizeof info, &got)) {
        err = GetLastError();
        CloseHandle(token);
        return err;
    }
    CloseHandle(token);
    if (!ConvertSidToStringSidW(info.user.User.Sid, &wsid)) {
        return GetLastError();
    }
    for (; wsid[n] != 0; n++) {
        if (n + 1 >= cap || wsid[n] > 0x7F) {
            err = ERROR_INSUFFICIENT_BUFFER;
            break;
        }
        out[n] = (char)wsid[n];
    }
    if (err == 0) {
        out[n] = '\0';
    }
    LocalFree(wsid);
    return err;
}

/* A self-relative descriptor + attributes for one object class. */
typedef struct sec_attrs {
    SECURITY_ATTRIBUTES sa;
    PSECURITY_DESCRIPTOR sd;
} sec_attrs;

static DWORD sec_attrs_make(sec_attrs *s, unsigned perm, const char *user_sid, int section) {
    char sddl[512];
    wchar_t wsddl[512];
    DWORD err = build_sddl(perm, user_sid, section, sddl, sizeof sddl);
    size_t i;

    s->sd = NULL;
    if (err != 0) {
        return err;
    }
    for (i = 0; sddl[i] != '\0'; i++) {
        wsddl[i] = (wchar_t)(unsigned char)sddl[i];
    }
    wsddl[i] = 0;
    if (!ConvertStringSecurityDescriptorToSecurityDescriptorW(wsddl, SDDL_REVISION_1, &s->sd, NULL)) {
        s->sd = NULL;
        return GetLastError();
    }
    s->sa.nLength = (DWORD)sizeof s->sa;
    s->sa.lpSecurityDescriptor = s->sd;
    s->sa.bInheritHandle = FALSE;
    return 0;
}

static void sec_attrs_free(sec_attrs *s) {
    if (s->sd != NULL) {
        LocalFree(s->sd);
        s->sd = NULL;
    }
}

/* -- Names ------------------------------------------------------------------------------------ */

/* Portable name -> UTF-16 kernel object name. *wlen excludes the NUL. */
static hqq_status wide_object_name(const char *name, wchar_t *w, size_t cap, size_t *wlen) {
    char obj[1024];
    size_t olen = 0;
    int n;
    hqq_status st = hqq_windows_object_name(name, obj, sizeof obj, &olen);
    if (st != HQQ_OK) {
        return st;
    }
    n = MultiByteToWideChar(CP_UTF8, MB_ERR_INVALID_CHARS, obj, -1, w, (int)cap);
    if (n <= 1 || (size_t)(n - 1) + SUFFIX_LEN > MAX_OBJECT_NAME) {
        return HQQ_ERR_INVALID_NAME;
    }
    *wlen = (size_t)(n - 1);
    return HQQ_OK;
}

/* name + ".hqq-semN" into out (cap >= WNAME_CAP). */
static void sem_name(const wchar_t *base, size_t base_len, int i, wchar_t *out) {
    size_t k;
    memcpy(out, base, base_len * sizeof(wchar_t));
    for (k = 0; k < SUFFIX_LEN; k++) {
        out[base_len + k] = (wchar_t)(unsigned char)sem_suffix[i][k];
    }
    out[base_len + SUFFIX_LEN] = 0;
}

static int is_not_found(DWORD e) {
    return e == ERROR_FILE_NOT_FOUND || e == ERROR_PATH_NOT_FOUND;
}

/* -- Open / close ------------------------------------------------------------------------------- */

static void shm_close_handles(hqq_shm *shm) {
    int i;
    for (i = 0; i < SEM_COUNT; i++) {
        if (shm->sem[i] != NULL) {
            CloseHandle(shm->sem[i]);
            shm->sem[i] = NULL;
        }
    }
    if (shm->mapping != NULL) {
        CloseHandle(shm->mapping);
        shm->mapping = NULL;
    }
}

static hqq_status shm_create(hqq_shm *shm, const wchar_t *wname, size_t wlen, size_t size, unsigned perm,
                             int *os_err) {
    char user_sid[256];
    sec_attrs sec_section = {{0, NULL, FALSE}, NULL};
    sec_attrs sec_sem = {{0, NULL, FALSE}, NULL};
    wchar_t sname[WNAME_CAP];
    hqq_status st = HQQ_ERR_SYSTEM;
    DWORD err;
    uint64_t sz = (uint64_t)size;
    int i;

    err = current_user_sid(user_sid, sizeof user_sid);
    if (err == 0) {
        err = sec_attrs_make(&sec_section, perm, user_sid, 1);
    }
    if (err == 0) {
        err = sec_attrs_make(&sec_sem, perm, user_sid, 0);
    }
    if (err != 0) {
        goto fail;
    }

    /* The semaphores first: whoever can open the mapping then finds them. */
    for (i = 0; i < SEM_COUNT; i++) {
        sem_name(wname, wlen, i, sname);
        SetLastError(0);
        shm->sem[i] = CreateSemaphoreW(&sec_sem.sa, 0, 0x7FFFFFFF, sname);
        if (shm->sem[i] == NULL) {
            err = GetLastError();
            if (err == 0) {
                err = ERROR_INVALID_PARAMETER;
            }
            goto fail;
        }
        if (GetLastError() == ERROR_ALREADY_EXISTS) {
            st = HQQ_ERR_EXISTS;
            goto fail;
        }
    }

    SetLastError(0);
    shm->mapping = CreateFileMappingW(INVALID_HANDLE_VALUE, &sec_section.sa, PAGE_READWRITE,
                                      (DWORD)(sz >> 32), (DWORD)(sz & 0xFFFFFFFFu), wname);
    if (shm->mapping == NULL) {
        err = GetLastError();
        if (err == 0) {
            err = ERROR_INVALID_PARAMETER;
        }
        goto fail;
    }
    if (GetLastError() == ERROR_ALREADY_EXISTS) {
        st = HQQ_ERR_EXISTS;
        goto fail;
    }
    sec_attrs_free(&sec_section);
    sec_attrs_free(&sec_sem);
    return HQQ_OK;

fail:
    sec_attrs_free(&sec_section);
    sec_attrs_free(&sec_sem);
    shm_close_handles(shm);
    if (st == HQQ_ERR_SYSTEM) {
        set_err(os_err, err);
    }
    return st;
}

static hqq_status shm_open_existing(hqq_shm *shm, const wchar_t *wname, size_t wlen, int *os_err) {
    wchar_t sname[WNAME_CAP];
    DWORD err;
    int i;

    /* SECTION_QUERY lets hqq_plat_shm_size read the size of the mapping. */
    shm->mapping = OpenFileMappingW(FILE_MAP_READ | FILE_MAP_WRITE | SECTION_QUERY, FALSE, wname);
    if (shm->mapping == NULL) {
        err = GetLastError();
        goto fail;
    }
    for (i = 0; i < SEM_COUNT; i++) {
        sem_name(wname, wlen, i, sname);
        shm->sem[i] = OpenSemaphoreW(SYNCHRONIZE | SEMAPHORE_MODIFY_STATE, FALSE, sname);
        if (shm->sem[i] == NULL) {
            err = GetLastError();
            goto fail;
        }
    }
    return HQQ_OK;

fail:
    if (err == 0) {
        err = ERROR_INVALID_PARAMETER;
    }
    shm_close_handles(shm);
    if (is_not_found(err)) {
        return HQQ_ERR_NOT_EXIST;
    }
    set_err(os_err, err);
    return HQQ_ERR_SYSTEM;
}

hqq_status hqq_plat_shm_open(const hqq_allocator *a, const char *name, size_t size, int create, unsigned perm,
                             hqq_shm **out, int *os_err) {
    wchar_t wname[WNAME_CAP];
    size_t wlen = 0;
    hqq_shm *shm;
    hqq_status st;

    *out = NULL;
    st = wide_object_name(name, wname, sizeof wname / sizeof wname[0], &wlen);
    if (st != HQQ_OK) {
        return st;
    }
    shm = hqq_alloc(a, sizeof *shm);
    if (shm == NULL) {
        return HQQ_ERR_NO_MEMORY;
    }
    memset(shm, 0, sizeof *shm);

    st = create ? shm_create(shm, wname, wlen, size, perm, os_err) : shm_open_existing(shm, wname, wlen, os_err);
    if (st != HQQ_OK) {
        hqq_free(a, shm, sizeof *shm);
        return st;
    }
    *out = shm;
    return HQQ_OK;
}

void hqq_plat_shm_close(const hqq_allocator *a, hqq_shm *shm) {
    if (shm == NULL) {
        return;
    }
    shm_close_handles(shm);
    hqq_free(a, shm, sizeof *shm);
}

/* -- Size (NtQuerySection) ------------------------------------------------------------------------- */

/* SECTION_BASIC_INFORMATION from winternl.h. */
typedef struct hqq_section_basic_information {
    PVOID BaseAddress;
    ULONG AllocationAttributes;
    LARGE_INTEGER MaximumSize;
} hqq_section_basic_information;

int64_t hqq_plat_shm_size(hqq_shm *shm, int *os_err) {
    typedef LONG(NTAPI * nt_query_section_fn)(HANDLE, int, PVOID, SIZE_T, PSIZE_T);
    typedef ULONG(NTAPI * nt_status_to_dos_fn)(LONG);
    nt_query_section_fn query;
    hqq_section_basic_information info;
    SIZE_T returned = 0;
    LONG status;
    FARPROC p = resolve_one(L"ntdll.dll", "NtQuerySection");

    if (p == NULL) {
        set_err(os_err, ERROR_PROC_NOT_FOUND);
        return -1;
    }
    memcpy(&query, &p, sizeof query);
    memset(&info, 0, sizeof info);
    status = query(shm->mapping, 0 /* SectionBasicInformation */, &info, sizeof info, &returned);
    if (((uint32_t)status >> 30) == 3u) { /* NT_ERROR */
        nt_status_to_dos_fn to_dos;
        FARPROC q = resolve_one(L"ntdll.dll", "RtlNtStatusToDosError");
        ULONG dos = 0;
        if (q != NULL) {
            memcpy(&to_dos, &q, sizeof to_dos);
            dos = to_dos(status);
        }
        if (dos == 0 || dos == 0xFFFFFFFFu) {
            dos = (ULONG)status; /* no Win32 mapping: report the NTSTATUS itself */
        }
        set_err(os_err, dos);
        return -1;
    }
    return (int64_t)info.MaximumSize.QuadPart;
}

/* -- Mapping ----------------------------------------------------------------------------------------- */

hqq_status hqq_plat_shm_map(hqq_shm *shm, size_t size, void **out, int *os_err) {
    void *p = MapViewOfFile(shm->mapping, FILE_MAP_READ | FILE_MAP_WRITE, 0, 0, size);
    if (p == NULL) {
        DWORD e = GetLastError();
        set_err(os_err, e != 0 ? e : ERROR_INVALID_PARAMETER);
        return HQQ_ERR_SYSTEM;
    }
    *out = p;
    return HQQ_OK;
}

void hqq_plat_shm_unmap(hqq_shm *shm, void *p, size_t size) {
    (void)shm;
    (void)size;
    if (p != NULL) {
        (void)UnmapViewOfFile(p);
    }
}

/* -- Identity, names ------------------------------------------------------------------------------------ */

hqq_status hqq_plat_shm_name_refers_to_self(hqq_shm *shm, int *known, int *same) {
    (void)shm;
    *known = 0;
    *same = 0;
    return HQQ_OK;
}

hqq_status hqq_plat_shm_peek(const char *name, size_t page_size, uint64_t *state, uint64_t *creator_token,
                             int *found) {
    (void)name;
    (void)page_size;
    *state = 0;
    *creator_token = 0;
    *found = 0;
    return HQQ_OK;
}

hqq_status hqq_plat_shm_delete(hqq_shm *shm) {
    (void)shm;
    return HQQ_OK; /* names vanish with their last handle */
}

hqq_status hqq_plat_unlink(const char *name) {
    wchar_t w[WNAME_CAP];
    size_t wlen = 0;
    return wide_object_name(name, w, sizeof w / sizeof w[0], &wlen); /* validation only */
}

/* ---- Parking ------------------------------------------------------------------------------------------------ */

/*
 * Named segments (shm != NULL): handle[0] / handle[1] are the notEmpty / notFull
 * semaphores of the ring (borrowed from the hqq_shm), kind = HQQ_PARK_SHARED.
 * Unnamed memory: kind = HQQ_PARK_LOCAL; handle[0] / handle[1] hold the
 * resolved WaitOnAddress / WakeByAddressAll function pointers (NULL if the
 * system lacks them).
 */
typedef BOOL(WINAPI *wait_on_address_fn)(volatile VOID *, PVOID, SIZE_T, DWORD);
typedef VOID(WINAPI *wake_by_address_all_fn)(PVOID);

void hqq_plat_wait_init(hqq_plat_wait *w, hqq_shm *shm, int ring_index, uint32_t flags) {
    (void)flags;
    w->mode = 0;
    if (shm != NULL) {
        w->handle[0] = shm->sem[2 * ring_index];
        w->handle[1] = shm->sem[2 * ring_index + 1];
        w->kind = HQQ_PARK_SHARED;
    } else {
        FARPROC wait = resolve_synch("WaitOnAddress");
        FARPROC wake = resolve_synch("WakeByAddressAll");
        if (wait == NULL || wake == NULL) {
            wait = NULL;
            wake = NULL;
        }
        memcpy(&w->handle[0], &wait, sizeof wait);
        memcpy(&w->handle[1], &wake, sizeof wake);
        w->kind = HQQ_PARK_LOCAL;
    }
}

static void ring_wait(void *ctx, int side, _Atomic uint32_t *word, uint32_t expect, uint64_t timeout_ns) {
    hqq_plat_wait *w = ctx;
    DWORD ms = ceil_ms(timeout_ns, 1);

    if (w->kind == HQQ_PARK_SHARED) {
        HANDLE h = w->handle[side != 0 ? 1 : 0];
        if (h == NULL || WaitForSingleObject(h, ms) == WAIT_FAILED) {
            Sleep(1); /* no usable semaphore: bounded polling, the caller re-checks */
        }
    } else if (w->kind == HQQ_PARK_LOCAL) {
        wait_on_address_fn wait;
        memcpy(&wait, &w->handle[0], sizeof wait);
        if (wait != NULL) {
            (void)wait((volatile VOID *)word, &expect, sizeof expect, ms);
        } else {
            Sleep(1); /* no WaitOnAddress: bounded polling, the caller re-checks */
        }
    }
}

static void ring_wake(void *ctx, int side, _Atomic uint32_t *word, uint32_t waiters) {
    hqq_plat_wait *w = ctx;
    (void)waiters;

    if (w->kind == HQQ_PARK_SHARED) {
        HANDLE h = w->handle[side != 0 ? 1 : 0];
        if (h != NULL) {
            /* One token per wake per side (FINDINGS F-02); errors (too many posts) ignored. */
            (void)ReleaseSemaphore(h, 1, NULL);
        }
    } else if (w->kind == HQQ_PARK_LOCAL) {
        wake_by_address_all_fn wake;
        memcpy(&wake, &w->handle[1], sizeof wake);
        if (wake != NULL) {
            wake((PVOID)word);
        }
    }
}

const hqq_ring_ops *hqq_plat_ring_ops(void) {
    static const hqq_ring_ops ops = {ring_wait, ring_wake};
    return &ops;
}
