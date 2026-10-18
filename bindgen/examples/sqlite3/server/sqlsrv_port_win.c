/*
 * sqlsrv_port_win.c - the Windows side of sqlsrv_port.h: CreateThread, SRWLOCK
 * and CONDITION_VARIABLE, a console control handler for shutdown, binary
 * standard streams, and GetFinalPathNameByHandleW for canonical paths.
 *
 * Paths are UTF-8 at this interface (SQLite takes UTF-8 on Windows as well);
 * the Win32 calls take UTF-16, converted here.
 */
#include "sqlsrv_port.h"

#include <fcntl.h>
#include <io.h>
#include <signal.h>
#include <stdio.h>
#include <wchar.h>
#include <stdlib.h>
#include <string.h>

#ifndef SIGBREAK
#define SIGBREAK 21
#endif

/* How long a close or shutdown event handler waits for the shutdown (ms). Windows
 * allows a console process about 5 seconds, then ends it. */
#define CLOSE_WAIT_MS 4000u

/* ---- UTF-8 <-> UTF-16 -------------------------------------------------- */

static wchar_t *to_wide(const char *s) {
    int n = MultiByteToWideChar(CP_UTF8, MB_ERR_INVALID_CHARS, s, -1, NULL, 0);
    wchar_t *w;
    if (n <= 0) {
        return NULL;
    }
    w = malloc((size_t)n * sizeof *w);
    if (w == NULL) {
        return NULL;
    }
    if (MultiByteToWideChar(CP_UTF8, MB_ERR_INVALID_CHARS, s, -1, w, n) <= 0) {
        free(w);
        return NULL;
    }
    return w;
}

static char *to_utf8(const wchar_t *w) {
    int n = WideCharToMultiByte(CP_UTF8, 0, w, -1, NULL, 0, NULL, NULL);
    char *s;
    if (n <= 0) {
        return NULL;
    }
    s = malloc((size_t)n);
    if (s == NULL) {
        return NULL;
    }
    if (WideCharToMultiByte(CP_UTF8, 0, w, -1, s, n, NULL, NULL) <= 0) {
        free(s);
        return NULL;
    }
    return s;
}

/* ---- Threads ----------------------------------------------------------- */

typedef struct thread_start {
    sqlsrv_thread_fn fn;
    void *arg;
} thread_start;

static DWORD WINAPI thread_trampoline(LPVOID p) {
    thread_start st = *(thread_start *)p;
    free(p);
    (void)st.fn(st.arg);
    return 0;
}

int sqlsrv_thread_create(sqlsrv_thread *t, sqlsrv_thread_fn fn, void *arg) {
    thread_start *st = malloc(sizeof *st);
    HANDLE h;
    if (st == NULL) {
        return -1;
    }
    st->fn = fn;
    st->arg = arg;
    h = CreateThread(NULL, 0, thread_trampoline, st, 0, NULL);
    if (h == NULL) {
        free(st);
        return -1;
    }
    *t = h;
    return 0;
}

void sqlsrv_thread_join(sqlsrv_thread t) {
    (void)WaitForSingleObject(t, INFINITE);
    (void)CloseHandle(t);
}

void sqlsrv_thread_detach(sqlsrv_thread t) {
    (void)CloseHandle(t);
}

/* ---- Mutexes and condition variables ----------------------------------- */

int sqlsrv_mutex_init(sqlsrv_mutex *m) {
    InitializeSRWLock(m);
    return 0;
}

void sqlsrv_mutex_destroy(sqlsrv_mutex *m) {
    (void)m; /* SRWLOCK needs no cleanup */
}

void sqlsrv_mutex_lock(sqlsrv_mutex *m) {
    AcquireSRWLockExclusive(m);
}

void sqlsrv_mutex_unlock(sqlsrv_mutex *m) {
    ReleaseSRWLockExclusive(m);
}

int sqlsrv_cond_init(sqlsrv_cond *c) {
    InitializeConditionVariable(c);
    return 0;
}

void sqlsrv_cond_destroy(sqlsrv_cond *c) {
    (void)c; /* CONDITION_VARIABLE needs no cleanup */
}

void sqlsrv_cond_wait(sqlsrv_cond *c, sqlsrv_mutex *m) {
    (void)SleepConditionVariableSRW(c, m, INFINITE, 0);
}

void sqlsrv_cond_timedwait_ms(sqlsrv_cond *c, sqlsrv_mutex *m, long ms) {
    (void)SleepConditionVariableSRW(c, m, (DWORD)ms, 0);
}

void sqlsrv_cond_broadcast(sqlsrv_cond *c) {
    WakeAllConditionVariable(c);
}

void sqlsrv_sleep_ms(unsigned long ms) {
    Sleep((DWORD)ms);
}

/* ---- Process ----------------------------------------------------------- */

static HANDLE g_stop_event;       /* manual reset: set by the first shutdown request */
static HANDLE g_done_event;       /* manual reset: set when the shutdown is complete */
static volatile LONG g_stop_sig;  /* the signal number of the first request */

static BOOL WINAPI on_console_ctrl(DWORD type) {
    switch (type) {
    case CTRL_C_EVENT:
        sqlsrv_shutdown_request(SIGINT);
        return TRUE;
    case CTRL_BREAK_EVENT:
        sqlsrv_shutdown_request(SIGBREAK);
        return TRUE;
    case CTRL_CLOSE_EVENT:
    case CTRL_SHUTDOWN_EVENT:
        sqlsrv_shutdown_request(SIGTERM);
        /* The process ends when this handler returns, so keep it alive until the
         * server has shut down (or the wait runs out, before Windows' own limit). */
        (void)WaitForSingleObject(g_done_event, CLOSE_WAIT_MS);
        return TRUE;
    default:
        return FALSE;
    }
}

void sqlsrv_process_init(void) {
    /* Binary mode: no CR is added to the ready line, and the lifeline reads bytes. */
    if (_fileno(stdin) >= 0) {
        (void)_setmode(_fileno(stdin), _O_BINARY);
    }
    if (_fileno(stdout) >= 0) {
        (void)_setmode(_fileno(stdout), _O_BINARY);
    }
    if (_fileno(stderr) >= 0) {
        (void)_setmode(_fileno(stderr), _O_BINARY);
    }
}

int sqlsrv_stdin_is_terminal(void) {
    DWORD mode = 0;
    HANDLE h = GetStdHandle(STD_INPUT_HANDLE);
    return h != NULL && h != INVALID_HANDLE_VALUE && GetConsoleMode(h, &mode) != 0 ? 1 : 0;
}

unsigned long sqlsrv_process_id(void) {
    return (unsigned long)GetCurrentProcessId();
}

int sqlsrv_shutdown_init(void) {
    g_stop_event = CreateEventW(NULL, TRUE, FALSE, NULL);
    g_done_event = CreateEventW(NULL, TRUE, FALSE, NULL);
    if (g_stop_event == NULL || g_done_event == NULL) {
        return -1;
    }
    /* Without a console the registration may fail; the other request paths still work. */
    (void)SetConsoleCtrlHandler(on_console_ctrl, TRUE);
    return 0;
}

int sqlsrv_shutdown_wait(void) {
    (void)WaitForSingleObject(g_stop_event, INFINITE);
    return (int)g_stop_sig;
}

void sqlsrv_shutdown_request(int sig) {
    (void)InterlockedCompareExchange(&g_stop_sig, (LONG)sig, 0);
    if (g_stop_event != NULL) {
        (void)SetEvent(g_stop_event);
    }
}

void sqlsrv_shutdown_finished(void) {
    if (g_done_event != NULL) {
        (void)SetEvent(g_done_event);
    }
}

/* ---- Files ------------------------------------------------------------- */

static int is_drive_letter(wchar_t c) {
    return (c >= L'A' && c <= L'Z') || (c >= L'a' && c <= L'z');
}

/*
 * The final path of an open handle, as a UTF-8 string. Only local drive paths
 * (X:\...) are accepted: UNC and other forms are refused (the policy then sees
 * a failure, which is fail-closed).
 */
static int final_path(HANDLE h, char **out) {
    DWORD n = GetFinalPathNameByHandleW(h, NULL, 0, VOLUME_NAME_DOS);
    wchar_t *w;
    DWORD got;
    char *u;

    if (n == 0) {
        return -1;
    }
    w = malloc((size_t)(n + 1) * sizeof *w);
    if (w == NULL) {
        return -1;
    }
    got = GetFinalPathNameByHandleW(h, w, n + 1, VOLUME_NAME_DOS);
    if (got == 0 || got > n || got < 7 || wcsncmp(w, L"\\\\?\\", 4) != 0 || !is_drive_letter(w[4]) ||
        w[5] != L':' || w[6] != L'\\') {
        free(w);
        return -1;
    }
    w[got] = L'\0';
    u = to_utf8(w + 4); /* drops the \\?\ prefix */
    free(w);
    if (u == NULL) {
        return -1;
    }
    *out = u;
    return 0;
}

int sqlsrv_port_realpath(const char *path, char **out, int *missing) {
    wchar_t *w = to_wide(path);
    HANDLE h;
    DWORD err;
    int rc;

    *missing = 0;
    if (w == NULL) {
        return -1;
    }
    /* Backup semantics open directories too; symbolic links are followed. */
    h = CreateFileW(w, FILE_READ_ATTRIBUTES, FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE, NULL,
                    OPEN_EXISTING, FILE_FLAG_BACKUP_SEMANTICS, NULL);
    if (h == INVALID_HANDLE_VALUE) {
        err = GetLastError();
        *missing = err == ERROR_FILE_NOT_FOUND || err == ERROR_PATH_NOT_FOUND;
        free(w);
        return -1;
    }
    free(w);
    rc = final_path(h, out);
    (void)CloseHandle(h);
    return rc;
}

int sqlsrv_port_entry_exists(const char *path) {
    wchar_t *w = to_wide(path);
    HANDLE h;
    if (w == NULL) {
        return 0;
    }
    /* OPEN_REPARSE_POINT opens a symbolic link itself, even a dangling one. */
    h = CreateFileW(w, FILE_READ_ATTRIBUTES, FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE, NULL,
                    OPEN_EXISTING, FILE_FLAG_BACKUP_SEMANTICS | FILE_FLAG_OPEN_REPARSE_POINT, NULL);
    free(w);
    if (h == INVALID_HANDLE_VALUE) {
        return 0;
    }
    (void)CloseHandle(h);
    return 1;
}

int sqlsrv_port_file_exists(const char *path) {
    wchar_t *w = to_wide(path);
    DWORD attr;
    if (w == NULL) {
        return 0;
    }
    attr = GetFileAttributesW(w);
    free(w);
    return attr != INVALID_FILE_ATTRIBUTES ? 1 : 0;
}

int sqlsrv_port_is_directory(const char *path) {
    wchar_t *w = to_wide(path);
    DWORD attr;
    if (w == NULL) {
        return 0;
    }
    attr = GetFileAttributesW(w);
    free(w);
    return attr != INVALID_FILE_ATTRIBUTES && (attr & FILE_ATTRIBUTE_DIRECTORY) != 0 ? 1 : 0;
}

/* Runs a Win32 path operation on the UTF-16 form of path; returns 0 or -1. */
static int with_wide(const char *path, BOOL (WINAPI *op)(LPCWSTR)) {
    wchar_t *w = to_wide(path);
    int ok;
    if (w == NULL) {
        return -1;
    }
    ok = op(w) ? 0 : -1;
    free(w);
    return ok;
}

int sqlsrv_port_mkdir(const char *path) {
    wchar_t *w = to_wide(path);
    int ok;
    if (w == NULL) {
        return -1;
    }
    /* The new directory inherits its ACL from the parent. */
    ok = CreateDirectoryW(w, NULL) ? 0 : -1;
    free(w);
    return ok;
}

int sqlsrv_port_remove_dir(const char *path) {
    return with_wide(path, RemoveDirectoryW);
}

int sqlsrv_port_remove_file(const char *path) {
    return with_wide(path, DeleteFileW);
}

int sqlsrv_port_create_file(const char *path) {
    wchar_t *w = to_wide(path);
    HANDLE h;
    if (w == NULL) {
        return -1;
    }
    h = CreateFileW(w, GENERIC_WRITE, 0, NULL, CREATE_ALWAYS, FILE_ATTRIBUTE_NORMAL, NULL);
    free(w);
    if (h == INVALID_HANDLE_VALUE) {
        return -1;
    }
    (void)CloseHandle(h);
    return 0;
}

int sqlsrv_port_symlink_file(const char *target, const char *link) {
    wchar_t *wt = to_wide(target);
    wchar_t *wl = to_wide(link);
    int ok = -1;
    if (wt != NULL && wl != NULL) {
        ok = CreateSymbolicLinkW(wl, wt, 0) ? 0 : -1;
    }
    free(wt);
    free(wl);
    return ok;
}

char *sqlsrv_port_getcwd(void) {
    DWORD n = GetCurrentDirectoryW(0, NULL);
    wchar_t *w;
    char *u;
    if (n == 0) {
        return NULL;
    }
    w = malloc((size_t)(n + 1) * sizeof *w);
    if (w == NULL) {
        return NULL;
    }
    if (GetCurrentDirectoryW(n + 1, w) == 0) {
        free(w);
        return NULL;
    }
    u = to_utf8(w);
    free(w);
    return u;
}

char *sqlsrv_port_tempdir(void) {
    wchar_t buf[MAX_PATH + 1];
    DWORD n = GetTempPathW((DWORD)(sizeof buf / sizeof buf[0]), buf);
    if (n == 0 || n >= sizeof buf / sizeof buf[0]) {
        return NULL;
    }
    /* GetTempPath ends with a backslash; the callers add their own separator. */
    while (n > 0 && (buf[n - 1] == L'\\' || buf[n - 1] == L'/')) {
        buf[--n] = L'\0';
    }
    return to_utf8(buf);
}

char *sqlsrv_port_strdup(const char *s) {
    size_t n = strlen(s);
    char *out = malloc(n + 1);
    if (out != NULL) {
        memcpy(out, s, n + 1);
    }
    return out;
}
