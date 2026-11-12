#include "blackbox.h"

#include <fcntl.h>
#include <pthread.h>
#include <stdarg.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <sys/resource.h>
#include <sys/sysctl.h>
#include <sys/types.h>
#include <time.h>
#include <unistd.h>

#define BB_DIR "/data/homebrew/FLEX"
#define BB_FILE BB_DIR "/blackbox.txt"
#define BB_MAX_BYTES (8 << 20) /* a very long session stops logging here rather than filling the disk */

/* from the console's libkernel and libc (not in the SDK's headers) */
int sceKernelAvailableFlexibleMemorySize(size_t *size);
int sceKernelConfiguredFlexibleMemorySize(size_t *size);
int malloc_stats_fast(void *managed_size);

static int bb_fd = -1;
static long bb_written;
static volatile int bb_dirty;
static pthread_mutex_t bb_mu = PTHREAD_MUTEX_INITIALIZER;
static struct timespec bb_t0;

static double now_s(void) {
    struct timespec t;
    clock_gettime(CLOCK_MONOTONIC, &t);
    return (t.tv_sec - bb_t0.tv_sec) + (t.tv_nsec - bb_t0.tv_nsec) / 1e9;
}

/* syncer puts what was written on the disk once a second, off the threads
 * that log (a sync can take a while when the console is struggling, which
 * is exactly when the lines matter). */
static void *syncer(void *arg) {
    (void)arg;
    for (;;) {
        sleep(1);
        if (bb_dirty) {
            bb_dirty = 0;
            fsync(bb_fd);
        }
    }
    return NULL;
}

void bb_open(const char *version) {
    clock_gettime(CLOCK_MONOTONIC, &bb_t0);
    rename(BB_DIR "/blackbox.1.txt", BB_DIR "/blackbox.2.txt");
    rename(BB_FILE, BB_DIR "/blackbox.1.txt");
    bb_fd = open(BB_FILE, O_WRONLY | O_CREAT | O_TRUNC | O_APPEND, 0666);
    if (bb_fd >= 0) {
        pthread_t t;
        if (pthread_create(&t, NULL, syncer, NULL) == 0) pthread_detach(t);
    }
    char sys[320];
    bb_system(sys, sizeof sys); /* the first call only sets the starting points */
    int ncpu = 0;
    size_t len = sizeof ncpu;
    if (sysctlbyname("hw.ncpu", &ncpu, &len, NULL, 0) != 0) ncpu = 0;
    unsigned long phys = 0;
    len = sizeof phys;
    if (sysctlbyname("hw.physmem", &phys, &len, NULL, 0) != 0) phys = 0;
    bb_log("FLEX %s started; %d CPUs, %lu MB physical memory; %s", version, ncpu, phys >> 20, sys);
}

void bb_log(const char *fmt, ...) {
    char line[1024];
    int n = snprintf(line, sizeof line, "%8.1f ", now_s());
    va_list ap;
    va_start(ap, fmt);
    int m = vsnprintf(line + n, sizeof line - n, fmt, ap);
    va_end(ap);
    n = m < 0 ? n : n + m;
    if (n > (int)sizeof line - 2) n = sizeof line - 2;
    line[n++] = '\n';
    fwrite(line, 1, n, stderr);
    pthread_mutex_lock(&bb_mu);
    if (bb_fd >= 0 && bb_written < BB_MAX_BYTES) {
        if (write(bb_fd, line, n) > 0) bb_written += n;
        bb_dirty = 1;
    }
    pthread_mutex_unlock(&bb_mu);
}

/* only ever called from the main thread, so the starting points need no lock */
void bb_system(char *buf, int size) {
    static double last_t, last_cpu;
    static long last_minflt, last_majflt, last_cp[5];
    static int have_cp;
    double t = now_s();
    char cpu[24] = "?", faults[40] = "?", console[24] = "?", heap[64] = "?", freemem[64] = "?";
    long rss = -1;

    struct rusage ru;
    if (getrusage(RUSAGE_SELF, &ru) == 0) {
        double c = ru.ru_utime.tv_sec + ru.ru_utime.tv_usec / 1e6 + ru.ru_stime.tv_sec + ru.ru_stime.tv_usec / 1e6;
        if (last_t > 0 && t - last_t > 0.05) {
            /* 100% is one CPU thread's worth */
            snprintf(cpu, sizeof cpu, "%.0f%%", (c - last_cpu) / (t - last_t) * 100);
            snprintf(faults, sizeof faults, "%ld/%ld", ru.ru_minflt - last_minflt, ru.ru_majflt - last_majflt);
        }
        last_cpu = c;
        last_minflt = ru.ru_minflt;
        last_majflt = ru.ru_majflt;
        rss = ru.ru_maxrss / 1024; /* KB -> MB */
    }
    last_t = t;

    /* the whole console: user, nice, system, interrupt, idle ticks over all CPUs */
    long cp[5];
    size_t len = sizeof cp;
    if (sysctlbyname("kern.cp_time", cp, &len, NULL, 0) == 0 && len == sizeof cp) {
        long busy = 0, total = 0;
        for (int i = 0; i < 5; i++) {
            long d = cp[i] - last_cp[i];
            total += d;
            if (i != 4) busy += d;
        }
        if (have_cp && total > 0) snprintf(console, sizeof console, "%.0f%%", busy * 100.0 / total);
        memcpy(last_cp, cp, sizeof cp);
        have_cp = 1;
    }

    /* what this app holds (malloc: in use / taken from the system) and how
     * much more the system will let it have (its flexible memory) */
    union {
        struct {
            uint16_t size, version;
            uint32_t reserved;
            size_t max_system, cur_system, max_inuse, cur_inuse;
        } s;
        char pad[256]; /* room in case the system's struct is bigger */
    } mm;
    memset(&mm, 0, sizeof mm);
    mm.s.size = sizeof mm.s;
    mm.s.version = 1;
    size_t avail = 0, conf = 0;
    int have_mm = malloc_stats_fast(&mm) == 0;
    int have_flex = sceKernelAvailableFlexibleMemorySize(&avail) == 0;
    if (sceKernelConfiguredFlexibleMemorySize(&conf) != 0) conf = 0;
    if (have_mm || have_flex) {
        char a[24] = "?", b[24] = "?";
        if (have_mm) snprintf(a, sizeof a, "%zu/%zu", mm.s.cur_inuse >> 20, mm.s.cur_system >> 20);
        if (have_flex) snprintf(b, sizeof b, "%zu/%zu", avail >> 20, conf >> 20);
        snprintf(heap, sizeof heap, "%s MB, may still take %s MB", a, b);
    }

    /* the console's free and inactive (reclaimable) memory */
    unsigned freec, inact, page;
    size_t ulen = sizeof freec;
    if (sysctlbyname("vm.stats.vm.v_free_count", &freec, &ulen, NULL, 0) == 0 &&
        sysctlbyname("vm.stats.vm.v_page_size", &page, &(size_t){sizeof page}, NULL, 0) == 0) {
        if (sysctlbyname("vm.stats.vm.v_inactive_count", &inact, &(size_t){sizeof inact}, NULL, 0) != 0) inact = 0;
        snprintf(freemem, sizeof freemem, "%llu MB (+%llu inactive)", (unsigned long long)freec * page >> 20,
                 (unsigned long long)inact * page >> 20);
    }

    snprintf(buf, size, "cpu %s, console %s, faults %s, rss %ld MB, heap %s, free %s", cpu, console, faults, rss, heap,
             freemem);
}

int bb_heap_mb(long *inuse, long *system) {
    union {
        struct {
            uint16_t size, version;
            uint32_t reserved;
            size_t max_system, cur_system, max_inuse, cur_inuse;
        } s;
        char pad[256];
    } mm;
    memset(&mm, 0, sizeof mm);
    mm.s.size = sizeof mm.s;
    mm.s.version = 1;
    if (malloc_stats_fast(&mm) != 0) return -1;
    *inuse = (long)(mm.s.cur_inuse >> 20);
    *system = (long)(mm.s.cur_system >> 20);
    return 0;
}

void bb_every(unsigned ms, const char *what) {
    static double next;
    double t = now_s();
    if (t < next) return;
    next = t + ms / 1000.0;
    char sys[320];
    bb_system(sys, sizeof sys);
    bb_log("%s: %s", what, sys);
}
