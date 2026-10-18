/*
 * sqlsrv_port_posix.c - the POSIX side of sqlsrv_port.h: pthreads, sigwait for
 * shutdown requests, realpath(), and the stat family.
 */
#define _POSIX_C_SOURCE 200809L

#include "sqlsrv_port.h"

#include <errno.h>
#include <limits.h>
#include <signal.h>
#include <stdio.h>
#include <sys/stat.h>
#include <time.h>
#include <unistd.h>
#include <stdlib.h>
#include <string.h>

/* ---- Threads ----------------------------------------------------------- */

int sqlsrv_thread_create(sqlsrv_thread *t, sqlsrv_thread_fn fn, void *arg) {
    return pthread_create(t, NULL, fn, arg) == 0 ? 0 : -1;
}

void sqlsrv_thread_join(sqlsrv_thread t) {
    (void)pthread_join(t, NULL);
}

void sqlsrv_thread_detach(sqlsrv_thread t) {
    (void)pthread_detach(t);
}

/* ---- Mutexes and condition variables ----------------------------------- */

int sqlsrv_mutex_init(sqlsrv_mutex *m) {
    return pthread_mutex_init(m, NULL) == 0 ? 0 : -1;
}

void sqlsrv_mutex_destroy(sqlsrv_mutex *m) {
    (void)pthread_mutex_destroy(m);
}

void sqlsrv_mutex_lock(sqlsrv_mutex *m) {
    (void)pthread_mutex_lock(m);
}

void sqlsrv_mutex_unlock(sqlsrv_mutex *m) {
    (void)pthread_mutex_unlock(m);
}

int sqlsrv_cond_init(sqlsrv_cond *c) {
    return pthread_cond_init(c, NULL) == 0 ? 0 : -1;
}

void sqlsrv_cond_destroy(sqlsrv_cond *c) {
    (void)pthread_cond_destroy(c);
}

void sqlsrv_cond_wait(sqlsrv_cond *c, sqlsrv_mutex *m) {
    (void)pthread_cond_wait(c, m);
}

void sqlsrv_cond_timedwait_ms(sqlsrv_cond *c, sqlsrv_mutex *m, long ms) {
    struct timespec ts;
    clock_gettime(CLOCK_REALTIME, &ts);
    ts.tv_nsec += ms * 1000000L;
    if (ts.tv_nsec >= 1000000000L) {
        ts.tv_sec += 1;
        ts.tv_nsec -= 1000000000L;
    }
    (void)pthread_cond_timedwait(c, m, &ts);
}

void sqlsrv_cond_broadcast(sqlsrv_cond *c) {
    (void)pthread_cond_broadcast(c);
}

void sqlsrv_sleep_ms(unsigned long ms) {
    struct timespec pause;
    pause.tv_sec = (time_t)(ms / 1000UL);
    pause.tv_nsec = (long)(ms % 1000UL) * 1000000L;
    (void)nanosleep(&pause, NULL);
}

/* ---- Process ----------------------------------------------------------- */

/* Shutdown: the signals are blocked in the main thread and taken with sigwait(). */
static pthread_t g_main_thread;
static sigset_t g_stop_set;

void sqlsrv_process_init(void) {
    (void)signal(SIGPIPE, SIG_IGN);
}

int sqlsrv_stdin_is_terminal(void) {
    return isatty(STDIN_FILENO) ? 1 : 0;
}

unsigned long sqlsrv_process_id(void) {
    return (unsigned long)getpid();
}

int sqlsrv_shutdown_init(void) {
    sigemptyset(&g_stop_set);
    sigaddset(&g_stop_set, SIGINT);
    sigaddset(&g_stop_set, SIGTERM);
    g_main_thread = pthread_self();
    return pthread_sigmask(SIG_BLOCK, &g_stop_set, NULL) == 0 ? 0 : -1;
}

int sqlsrv_shutdown_wait(void) {
    int sig = 0;
    while (sigwait(&g_stop_set, &sig) != 0) {
        /* Retry on a spurious error. */
    }
    return sig;
}

void sqlsrv_shutdown_request(int sig) {
    /* A blocked signal stays pending for the main thread's sigwait(). */
    (void)pthread_kill(g_main_thread, sig);
}

void sqlsrv_shutdown_finished(void) {
    /* Nothing to release: the process ends by return from main. */
}

/* ---- Files ------------------------------------------------------------- */

int sqlsrv_port_realpath(const char *path, char **out, int *missing) {
    char *real = realpath(path, NULL);
    int err;

    if (real != NULL) {
        *out = real;
        *missing = 0;
        return 0;
    }
    err = errno;
    *missing = err == ENOENT || err == ENOTDIR;
    return -1;
}

int sqlsrv_port_entry_exists(const char *path) {
    struct stat sb;
    return lstat(path, &sb) == 0 ? 1 : 0;
}

int sqlsrv_port_file_exists(const char *path) {
    return access(path, F_OK) == 0 ? 1 : 0;
}

int sqlsrv_port_is_directory(const char *path) {
    struct stat sb;
    return stat(path, &sb) == 0 && S_ISDIR(sb.st_mode) ? 1 : 0;
}

int sqlsrv_port_mkdir(const char *path) {
    return mkdir(path, 0700) == 0 ? 0 : -1;
}

int sqlsrv_port_remove_dir(const char *path) {
    return rmdir(path) == 0 ? 0 : -1;
}

int sqlsrv_port_remove_file(const char *path) {
    return unlink(path) == 0 ? 0 : -1;
}

int sqlsrv_port_create_file(const char *path) {
    FILE *f = fopen(path, "wb");
    if (f == NULL) {
        return -1;
    }
    (void)fclose(f);
    return 0;
}

int sqlsrv_port_symlink_file(const char *target, const char *link) {
    return symlink(target, link) == 0 ? 0 : -1;
}

char *sqlsrv_port_getcwd(void) {
    char buf[PATH_MAX];
    if (getcwd(buf, sizeof buf) == NULL) {
        return NULL;
    }
    return sqlsrv_port_strdup(buf);
}

char *sqlsrv_port_tempdir(void) {
    return sqlsrv_port_strdup("/tmp");
}

char *sqlsrv_port_strdup(const char *s) {
    size_t n = strlen(s);
    char *out = malloc(n + 1);
    if (out != NULL) {
        memcpy(out, s, n + 1);
    }
    return out;
}
