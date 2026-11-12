/* Startup for FLEX as its own PS5 app (`make app`).
 *
 * A payload gets its start from the payload SDK's crt1.o, which expects to be
 * injected into a running app by a loader. An app is started by the system
 * itself: the loader calls _start with the process parameters (argc, then the
 * argv array) and a teardown function. We set up the C library, run the
 * constructors, call main, and when main returns ask the system to close the
 * app, which takes us back to the home screen. */

#include <fcntl.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>

#include "app.h"
#include "blackbox.h"

typedef void (*hook)(void);

void _init_env(void *params);
int sceSystemServiceLoadExec(const char *path, char *const argv[]);
int main(int argc, char **argv);

extern hook __preinit_array_start[] __attribute__((weak));
extern hook __preinit_array_end[] __attribute__((weak));
extern hook __init_array_start[] __attribute__((weak));
extern hook __init_array_end[] __attribute__((weak));
extern hook __fini_array_start[] __attribute__((weak));
extern hook __fini_array_end[] __attribute__((weak));

static void run(hook *first, hook *last) {
    if (first && last)
        for (; first != last; first++) (*first)();
}

static void fini(void) {
    if (!__fini_array_start || !__fini_array_end) return;
    for (hook *h = __fini_array_end; h != __fini_array_start;) (*--h)();
}

/* The payload SDK's libc.a (linked for emulated TLS and getaddrinfo) routes
 * dlopen and executable mappings through the payload loader in its crt1.o,
 * which an app doesn't have. FLEX loads nothing at run time and never maps
 * code, so these only need to say no. */
void *__dlopen(const char *path, int mode) {
    (void)path, (void)mode;
    return NULL;
}
void *__dlsym(void *handle, const char *name) {
    (void)handle, (void)name;
    return NULL;
}
int __dlclose(void *handle) {
    (void)handle;
    return -1;
}
char *__dlerror(void) { return "no dynamic loading in the FLEX app"; }
int __dladdr(const void *addr, void *info) {
    (void)addr, (void)info;
    return 0;
}
int kernel_mprotect(int pid, intptr_t addr, size_t len, int prot) {
    (void)pid, (void)addr, (void)len, (void)prot;
    return -1;
}

/* System functions the app loader leaves unbound. An app gets only some
 * system modules loaded with it; calls into one that isn't are calls to
 * address 0 (SDL's keyboard setup crashed the first test app that way). SDL's
 * PS5 port sets up the USB keyboard and the system's text-input dialog, which
 * FLEX doesn't use, so the Makefile routes those calls here (--wrap): they go
 * through when the loader bound the function and fail politely when not. */
#define GUARDED(X) X(sceKeyboardInit) X(sceKeyboardOpen) X(sceKeyboardClose) X(sceKeyboardReadState) \
    X(sceImeDialogInit) X(sceImeDialogGetStatus) X(sceImeDialogGetResult) X(sceImeDialogTerm)
#define WRAP(name)                                                                                   \
    extern int __real_##name(long, long, long, long, long, long) __attribute__((weak));             \
    int __wrap_##name(long a, long b, long c, long d, long e, long f) {                              \
        return __real_##name ? __real_##name(a, b, c, d, e, f) : (int)0x80020002; /* not loaded */ \
    }
GUARDED(WRAP)

/* SDL's screen setup: sceVideoOutOpen, then one 64 MB block of screen
 * memory from sceKernelAllocateMainDirectMemory, mapped with
 * sceKernelMapDirectMemory. Each failure is logged to the black box (SDL
 * reports them as "Couldn't find matching render driver"), and a failed
 * allocation is retried the way working apps ask for screen memory. */
size_t sceKernelGetDirectMemorySize(void);
int sceKernelAvailableDirectMemorySize(long start, long end, size_t align, long *phys, size_t *size);
int sceKernelAllocateDirectMemory(long start, long end, size_t len, size_t align, int type, long *phys);
int __real_sceKernelAllocateMainDirectMemory(size_t len, size_t align, int type, long *phys);
int __real_sceKernelMapDirectMemory(void **addr, size_t len, int prot, int flags, long phys, size_t align);
int __real_sceVideoOutOpen(int user, int bus, int index, const void *param);

int __wrap_sceKernelAllocateMainDirectMemory(size_t len, size_t align, int type, long *phys) {
    int r = __real_sceKernelAllocateMainDirectMemory(len, align, type, phys);
    if (r == 0) return 0;
    size_t pool = sceKernelGetDirectMemorySize(), avail = 0;
    long at = 0;
    sceKernelAvailableDirectMemorySize(0, (long)pool, align, &at, &avail);
    int again = sceKernelAllocateDirectMemory(0, (long)pool, len, align, type, phys);
    bb_log("FLEX: screen memory: %zu MB from main direct memory failed (0x%08x); direct memory %zu MB, %zu MB "
           "free; asking directly %s (0x%08x)",
           len >> 20, (unsigned)r, pool >> 20, avail >> 20, again == 0 ? "worked" : "failed", (unsigned)again);
    return again;
}

int __wrap_sceKernelMapDirectMemory(void **addr, size_t len, int prot, int flags, long phys, size_t align) {
    int r = __real_sceKernelMapDirectMemory(addr, len, prot, flags, phys, align);
    if (r != 0) bb_log("FLEX: mapping %zu MB of screen memory failed (0x%08x)", len >> 20, (unsigned)r);
    return r;
}

int __wrap_sceVideoOutOpen(int user, int bus, int index, const void *param) {
    int h = __real_sceVideoOutOpen(user, bus, index, param);
    if (h < 0) bb_log("FLEX: sceVideoOutOpen failed (0x%08x)", (unsigned)h);
    return h;
}

/* The rest of what FLEX calls in the system modules, for app_unbound. */
#define CHECKED(X)                                                                                      \
    X(sceAudioOutInit) X(sceAudioOutOpen) X(sceAudioOutOutput)                        \
    X(sceVideoOutRegisterBuffers2) X(sceVideoOutSubmitFlip) X(sceVideoOutAddFlipEvent) X(scePadInit)    \
    X(scePadOpen) X(scePadReadState) X(scePadGetHandle) X(scePadSetLightBar) X(sceUserServiceInitialize) \
    X(sceUserServiceGetForegroundUser) X(sceUserServiceGetLoginUserIdList)                              \
    X(sceSystemServiceHideSplashScreen) X(sceNetPoolCreate)               \
    X(sceNetResolverCreate) X(sceNetResolverStartNtoa) X(sceNetErrnoLoc)                                \
    X(sceKernelCreateEqueue)          \
    X(sceKernelAvailableFlexibleMemorySize)
#define DECLARE(name) extern void name(void) __attribute__((weak));
CHECKED(DECLARE)

/* app_unbound lists the system functions FLEX uses that the loader left
 * unbound ("" when all are bound), for the black box. */
void app_unbound(char *out, size_t size) {
    size_t used = 0;
    out[0] = 0;
#define REPORT(name, addr)                                                         \
    if (!(addr) && used + sizeof(#name) + 1 < size) {                              \
        used += (size_t)snprintf(out + used, size - used, "%s%s", used ? " " : "", #name); \
    }
#define REPORT_CHECKED(name) REPORT(name, name)
#define REPORT_GUARDED(name) REPORT(name, __real_##name)
    CHECKED(REPORT_CHECKED)
    GUARDED(REPORT_GUARDED)
}

/* An app's stdout and stderr go nowhere anyone can read; FLEX's messages go
 * to a file next to the black box instead (the previous run's stays as
 * app-log.1.txt). */
#define APP_LOG "/data/homebrew/FLEX/app-log"
static void capture_output(void) {
    rename(APP_LOG ".txt", APP_LOG ".1.txt");
    /* In an app the C library's stderr isn't file descriptor 2 (pointing 2
     * at a file caught nothing), so swap the streams themselves. */
    FILE *f = fopen(APP_LOG ".txt", "w");
    if (!f) return;
    setvbuf(f, NULL, _IONBF, 0);
    stdout = f;
    stderr = f;
}

__attribute__((visibility("default"), noreturn)) void _start(void *params, hook teardown) {
    int argc = *(int *)params;
    char **argv = (char **)((uint8_t *)params + sizeof(uint64_t));
    _init_env(params);
    if (teardown) atexit(teardown);
    atexit(fini);
    run(__preinit_array_start, __preinit_array_end);
    run(__init_array_start, __init_array_end);
    capture_output();
    int status = main(argc, argv);
    fprintf(stderr, "FLEX: main returned %d, closing the app\n", status);
    sceSystemServiceLoadExec("exit", NULL); /* closes the app; returns only if refused */
    exit(status);
}
