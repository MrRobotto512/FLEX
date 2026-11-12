/* FLEX's heap when it runs as its own app.
 *
 * The C library's heap in an app turned down an 8 MB request with almost
 * nothing allocated (SDL's first picture), where the same code inside the
 * Homebrew Launcher's process could take a gigabyte. So the app takes a
 * large block of its own memory once and runs one of the C library's own
 * heaps ("mspace") on it: the app's direct memory when there is enough (the
 * screen's 64 MB come from there too, so some is left for it), else its
 * flexible memory. The Makefile routes malloc and friends here (--wrap);
 * whatever doesn't fit goes to the system heap as before, and free hands each
 * block back to the heap it came from, by address. */

#include <pthread.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>

#include "app.h"

#define MB ((size_t)1 << 20)
#define HEAP_MAX (1536 * MB)     /* more than the player ever holds */
#define SCREEN_SPARE (128 * MB)  /* direct memory left for the screen (SDL takes 64 MB) */
#define FLEXIBLE_SPARE (96 * MB) /* flexible memory left for threads and the system libraries */
#define ALIGN (2 * MB)

typedef void *mspace;
mspace sceLibcMspaceCreate(const char *name, void *base, size_t capacity, unsigned flags);
void *sceLibcMspaceMalloc(mspace m, size_t size);
int sceLibcMspaceFree(mspace m, void *p);
void *sceLibcMspaceCalloc(mspace m, size_t n, size_t size);
void *sceLibcMspaceRealloc(mspace m, void *p, size_t size);
int sceLibcMspacePosixMemalign(mspace m, void **out, size_t align, size_t size);
size_t sceLibcMspaceMallocUsableSize(void *p);
int sceLibcMspaceMallocStatsFast(mspace m, void *stats);
size_t malloc_usable_size(void *p);

size_t sceKernelGetDirectMemorySize(void);
int sceKernelAvailableDirectMemorySize(long start, long end, size_t align, long *phys, size_t *size);
int sceKernelAllocateDirectMemory(long start, long end, size_t len, size_t align, int type, long *phys);
int sceKernelReleaseDirectMemory(long phys, size_t len);
int sceKernelMapDirectMemory(void **addr, size_t len, int prot, int flags, long phys, size_t align);
int sceKernelMapNamedFlexibleMemory(void **addr, size_t len, int prot, int flags, const char *name);
int sceKernelAvailableFlexibleMemorySize(size_t *size);

void *__real_malloc(size_t size);
void *__real_calloc(size_t n, size_t size);
void *__real_realloc(void *p, size_t size);
int __real_posix_memalign(void **out, size_t align, size_t size);
void __real_free(void *p);

enum { MEM_WB_ONION = 0, PROT_CPU_RW = 0x3 };

static pthread_once_t once = PTHREAD_ONCE_INIT;
static mspace heap;
static char *base;
static size_t capacity;
static char desc[256];

static int from_direct(size_t *size) {
    size_t total = sceKernelGetDirectMemorySize(), avail = 0;
    long at = 0, phys = 0;
    if (sceKernelAvailableDirectMemorySize(0, (long)total, ALIGN, &at, &avail) != 0) avail = 0;
    size_t want = avail > SCREEN_SPARE ? avail - SCREEN_SPARE : 0;
    if (want > HEAP_MAX) want = HEAP_MAX;
    want &= ~(ALIGN - 1);
    int len = snprintf(desc, sizeof desc, "direct memory %zu MB, %zu MB free", total / MB, avail / MB);
    if (want < 256 * MB) return 0;
    if (sceKernelAllocateDirectMemory(0, (long)total, want, ALIGN, MEM_WB_ONION, &phys) != 0) {
        snprintf(desc + len, sizeof desc - len, "; couldn't take %zu MB of it", want / MB);
        return 0;
    }
    void *addr = NULL;
    if (sceKernelMapDirectMemory(&addr, want, PROT_CPU_RW, 0, phys, ALIGN) != 0) {
        sceKernelReleaseDirectMemory(phys, want);
        snprintf(desc + len, sizeof desc - len, "; couldn't map %zu MB of it", want / MB);
        return 0;
    }
    base = addr;
    *size = want;
    return 1;
}

static int from_flexible(size_t *size) {
    size_t avail = 0;
    if (sceKernelAvailableFlexibleMemorySize(&avail) != 0) avail = 0;
    size_t want = avail > FLEXIBLE_SPARE ? avail - FLEXIBLE_SPARE : 0;
    if (want > HEAP_MAX) want = HEAP_MAX;
    want &= ~(ALIGN - 1);
    size_t len = strlen(desc);
    snprintf(desc + len, sizeof desc - len, "; flexible memory %zu MB free", avail / MB);
    void *addr = NULL;
    if (want < 64 * MB || sceKernelMapNamedFlexibleMemory(&addr, want, PROT_CPU_RW, 0, "FLEX heap") != 0) return 0;
    base = addr;
    *size = want;
    return 1;
}

static void heap_init(void) {
    size_t size = 0;
    const char *from = from_direct(&size) ? "direct" : from_flexible(&size) ? "flexible" : NULL;
    size_t len = strlen(desc);
    if (from) heap = sceLibcMspaceCreate("FLEX heap", base, size, 0);
    if (heap) {
        capacity = size;
        snprintf(desc + len, sizeof desc - len, "; FLEX's heap: %zu MB of %s memory", size / MB, from);
    } else {
        base = NULL;
        snprintf(desc + len, sizeof desc - len, "; no heap of its own (%s), using the system's",
                 from ? "the C library refused it" : "no memory to spare");
    }
}

static inline int ours(const void *p) { return heap && (const char *)p >= base && (const char *)p < base + capacity; }

void *__wrap_malloc(size_t size) {
    pthread_once(&once, heap_init);
    void *p = heap ? sceLibcMspaceMalloc(heap, size ? size : 1) : NULL;
    return p ? p : __real_malloc(size);
}

void *__wrap_calloc(size_t n, size_t size) {
    pthread_once(&once, heap_init);
    void *p = heap ? sceLibcMspaceCalloc(heap, n ? n : 1, size ? size : 1) : NULL;
    return p ? p : __real_calloc(n, size);
}

int __wrap_posix_memalign(void **out, size_t align, size_t size) {
    pthread_once(&once, heap_init);
    if (heap && sceLibcMspacePosixMemalign(heap, out, align, size ? size : 1) == 0) return 0;
    return __real_posix_memalign(out, align, size);
}

void __wrap_free(void *p) {
    if (!p) return;
    if (ours(p)) sceLibcMspaceFree(heap, p);
    else __real_free(p);
}

void *__wrap_realloc(void *p, size_t size) {
    if (!p) return __wrap_malloc(size);
    if (size == 0) {
        __wrap_free(p);
        return NULL;
    }
    void *q;
    size_t had;
    if (ours(p)) {
        if ((q = sceLibcMspaceRealloc(heap, p, size))) return q;
        if (!(q = __real_malloc(size))) return NULL; /* ours is full: move it to the system heap */
        had = sceLibcMspaceMallocUsableSize(p);
    } else {
        if ((q = __real_realloc(p, size))) return q;
        if (!(q = __wrap_malloc(size))) return NULL; /* the system heap said no: move it into ours */
        had = malloc_usable_size(p);
    }
    memcpy(q, p, had < size ? had : size);
    __wrap_free(p);
    return q;
}

const char *app_heap_desc(void) {
    pthread_once(&once, heap_init);
    return desc;
}

int app_heap_mb(long *inuse, long *system, long *cap) {
    union {
        struct {
            uint16_t size, version;
            uint32_t reserved;
            size_t max_system, cur_system, max_inuse, cur_inuse;
        } s;
        char pad[64];
    } mm;
    if (!heap) return -1;
    memset(&mm, 0, sizeof mm);
    mm.s.size = sizeof mm.s;
    mm.s.version = 1;
    if (sceLibcMspaceMallocStatsFast(heap, &mm) != 0) return -1;
    *inuse = (long)(mm.s.cur_inuse / MB);
    *system = (long)(mm.s.cur_system / MB);
    *cap = (long)(capacity / MB);
    return 0;
}
