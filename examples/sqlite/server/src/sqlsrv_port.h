/*
 * sqlsrv_port.h - the platform layer of the server: threads, mutexes and
 * condition variables, sleeping, shutdown requests, process setup, and the
 * file-system queries the database policy needs.
 *
 * Exactly one implementation is compiled: sqlsrv_port_posix.c (pthreads,
 * sigwait, realpath) or sqlsrv_port_win.c (CreateThread, SRWLOCK,
 * CONDITION_VARIABLE, a console control handler, GetFinalPathNameByHandleW).
 * The protocol and the session logic never name a platform API.
 */
#ifndef SQLSRV_PORT_H
#define SQLSRV_PORT_H

#include <stddef.h>

#if defined(_WIN32)
#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#ifndef NOMINMAX
#define NOMINMAX
#endif
#ifndef _WIN32_WINNT
#define _WIN32_WINNT 0x0600 /* Windows Vista: SRWLOCK, condition variables, GetFinalPathNameByHandleW */
#endif
#include <windows.h>
typedef HANDLE sqlsrv_thread;
typedef SRWLOCK sqlsrv_mutex;
typedef CONDITION_VARIABLE sqlsrv_cond;
#else
#include <pthread.h>
typedef pthread_t sqlsrv_thread;
typedef pthread_mutex_t sqlsrv_mutex;
typedef pthread_cond_t sqlsrv_cond;
#endif

#ifdef __cplusplus
extern "C" {
#endif

/* ---- Threads ----------------------------------------------------------- */

typedef void *(*sqlsrv_thread_fn)(void *arg);

/* Returns 0, or -1 when the thread cannot be started. */
int sqlsrv_thread_create(sqlsrv_thread *t, sqlsrv_thread_fn fn, void *arg);
/* Waits for the thread to finish and releases it. */
void sqlsrv_thread_join(sqlsrv_thread t);
/* Releases a thread that is never joined; it runs on its own. */
void sqlsrv_thread_detach(sqlsrv_thread t);

/* ---- Mutexes and condition variables ----------------------------------- */

/* init returns 0, or -1. A mutex is not recursive. */
int sqlsrv_mutex_init(sqlsrv_mutex *m);
void sqlsrv_mutex_destroy(sqlsrv_mutex *m);
void sqlsrv_mutex_lock(sqlsrv_mutex *m);
void sqlsrv_mutex_unlock(sqlsrv_mutex *m);

int sqlsrv_cond_init(sqlsrv_cond *c);
void sqlsrv_cond_destroy(sqlsrv_cond *c);
/* Callers loop on their predicate: wakeups may be spurious. */
void sqlsrv_cond_wait(sqlsrv_cond *c, sqlsrv_mutex *m);
/* Waits at most ms milliseconds (the mutex is held again on return). */
void sqlsrv_cond_timedwait_ms(sqlsrv_cond *c, sqlsrv_mutex *m, long ms);
void sqlsrv_cond_broadcast(sqlsrv_cond *c);

void sqlsrv_sleep_ms(unsigned long ms);

/* ---- Process ----------------------------------------------------------- */

/*
 * Standard streams: binary mode on Windows (the protocol text lines must not
 * gain CR characters, and the lifeline reads bytes); SIGPIPE ignored on POSIX.
 * Call first in main().
 */
void sqlsrv_process_init(void);
/* 1 when standard input is a terminal (a console on Windows), 0 otherwise. */
int sqlsrv_stdin_is_terminal(void);
unsigned long sqlsrv_process_id(void);

/*
 * Shutdown requests. SIGINT and SIGTERM (POSIX), or the console control
 * events CTRL_C, CTRL_BREAK, CTRL_CLOSE, CTRL_SHUTDOWN (Windows), and the
 * parent lifeline all end in one request, which the main thread waits for.
 *
 * sqlsrv_shutdown_init: main thread, before any other thread starts. 0, or -1.
 * sqlsrv_shutdown_wait: blocks until a request; returns its signal number
 *   (SIGINT, SIGBREAK, or SIGTERM).
 * sqlsrv_shutdown_request: any thread (also a lifeline callback). The first
 *   request wins; later ones are ignored.
 * sqlsrv_shutdown_finished: main thread, once the shutdown is complete. It
 *   releases a close-event handler that waits for it.
 */
int sqlsrv_shutdown_init(void);
int sqlsrv_shutdown_wait(void);
void sqlsrv_shutdown_request(int sig);
void sqlsrv_shutdown_finished(void);

/* ---- Files ------------------------------------------------------------- */

/*
 * Canonical absolute form of an existing path (symbolic links resolved, UTF-8,
 * free() it). Returns 0, or -1 with *missing set to 1 when the path or one of
 * its parents does not exist, and to 0 for any other failure.
 */
int sqlsrv_port_realpath(const char *path, char **out, int *missing);
/* 1 when the entry exists; a final symbolic link is not followed (lstat). */
int sqlsrv_port_entry_exists(const char *path);
/* 1 when the path exists (links followed). */
int sqlsrv_port_file_exists(const char *path);
int sqlsrv_port_is_directory(const char *path);
/* Each returns 0, or -1. */
int sqlsrv_port_mkdir(const char *path);
int sqlsrv_port_remove_dir(const char *path);
int sqlsrv_port_remove_file(const char *path);
int sqlsrv_port_create_file(const char *path);
int sqlsrv_port_symlink_file(const char *target, const char *link);
/* Malloc'd copy of a string (free() it), or NULL. Defined by each platform file. */
char *sqlsrv_port_strdup(const char *s);
/* Malloc'd strings (free() them), or NULL. */
char *sqlsrv_port_getcwd(void);
char *sqlsrv_port_tempdir(void);

#ifdef __cplusplus
}
#endif

#endif /* SQLSRV_PORT_H */
