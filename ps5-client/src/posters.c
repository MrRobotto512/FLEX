#include "posters.h"

#include <SDL_image.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>

#include "net.h"

#define CACHE_DIR "/data/homebrew/FLEX/cache"
#define WORKERS 4
#define MAX_DONE 256

typedef struct {
    char kind; /* 'm' movie, 's' show */
    long id;
    int tmdb_id;
    char path[128];
    int decode;
} Job;

typedef struct {
    char kind;
    long id;
    SDL_Surface *s;
} Done;

static SDL_mutex *mu;
static SDL_cond *cond;
static char host[128];
static int port;

/* wanted: a stack (newest first); prefetch: a queue */
static Job *wanted, *prefetch;
static int nwanted, capwanted, prefetch_head, nprefetch, capprefetch;
static Done done[MAX_DONE];
static int ndone;
static int fetched, from_cache, failed; /* for the log line when the queues drain */
static int busy;                        /* work was added since that line */

static void cache_path(const Job *j, char *out, size_t n) {
    snprintf(out, n, CACHE_DIR "/%c%ld-%d.img", j->kind, j->id, j->tmdb_id);
}

static int file_exists(const char *p) {
    struct stat st;
    return stat(p, &st) == 0 && st.st_size > 0;
}

static char *read_file(const char *p, size_t *len) {
    FILE *f = fopen(p, "rb");
    if (!f) return NULL;
    fseek(f, 0, SEEK_END);
    long n = ftell(f);
    fseek(f, 0, SEEK_SET);
    char *b = n > 0 ? malloc(n) : NULL;
    if (b && fread(b, 1, n, f) != (size_t)n) {
        free(b);
        b = NULL;
    }
    fclose(f);
    *len = b ? (size_t)n : 0;
    return b;
}

/* write_cache stores bytes under a per-worker temporary name first, so a
 * half-written file is never mistaken for a poster. */
static void write_cache(const char *p, const char *bytes, size_t len, int worker) {
    char tmp[256];
    snprintf(tmp, sizeof tmp, "%s.%d.part", p, worker);
    FILE *f = fopen(tmp, "wb");
    if (!f) return;
    int ok = fwrite(bytes, 1, len, f) == len;
    if (fclose(f) != 0) ok = 0;
    if (ok) rename(tmp, p);
    else remove(tmp);
}

static void push_done(char kind, long id, SDL_Surface *s) {
    SDL_LockMutex(mu);
    if (ndone < MAX_DONE) done[ndone++] = (Done){kind, id, s};
    else if (s) SDL_FreeSurface(s); /* the UI is far behind; it asks again later */
    SDL_UnlockMutex(mu);
}

static int worker(void *arg) {
    int me = (int)(intptr_t)arg;
    for (;;) {
        SDL_LockMutex(mu);
        while (!nwanted && prefetch_head == nprefetch) SDL_CondWait(cond, mu);
        Job j = nwanted ? wanted[--nwanted] : prefetch[prefetch_head++];
        if (prefetch_head == nprefetch) prefetch_head = nprefetch = 0;
        char h[128];
        snprintf(h, sizeof h, "%s", host);
        int p = port;
        SDL_UnlockMutex(mu);

        char file[192];
        cache_path(&j, file, sizeof file);
        if (!j.decode && file_exists(file)) continue; /* already cached */

        size_t len = 0;
        int how = 0; /* 1 cache, 2 network, 3 failed */
        char *bytes = read_file(file, &len);
        if (bytes) {
            how = 1;
        } else if (h[0] && (bytes = flex_http(h, p, "GET", j.path, NULL, &len)) && len > 0) {
            write_cache(file, bytes, len, me);
            how = 2;
        } else {
            free(bytes);
            bytes = NULL;
            how = 3;
        }
        if (j.decode) {
            SDL_Surface *s = bytes ? IMG_Load_RW(SDL_RWFromConstMem(bytes, (int)len), 1) : NULL;
            if (bytes && !s) {
                fprintf(stderr, "poster %c%ld: %s\n", j.kind, j.id, IMG_GetError());
                remove(file); /* damaged: fetch it again next time */
            }
            push_done(j.kind, j.id, s);
        }
        free(bytes);

        SDL_LockMutex(mu);
        from_cache += how == 1;
        fetched += how == 2;
        failed += how == 3;
        int report = busy && !nwanted && prefetch_head == nprefetch;
        if (report) busy = 0;
        int f = fetched, c = from_cache, x = failed;
        SDL_UnlockMutex(mu);
        if (report) fprintf(stderr, "FLEX: posters idle: %d downloaded, %d read from cache, %d unavailable\n", f, c, x);
    }
    return 0;
}

void posters_init(void) {
    mkdir(CACHE_DIR, 0777);
    mu = SDL_CreateMutex();
    cond = SDL_CreateCond();
    for (int i = 0; i < WORKERS; i++) SDL_DetachThread(SDL_CreateThread(worker, "flex-posters", (void *)(intptr_t)i));
}

void posters_server(const char *h, int p) {
    SDL_LockMutex(mu);
    snprintf(host, sizeof host, "%s", h);
    port = p;
    SDL_UnlockMutex(mu);
}

static void add(Job **q, int *n, int *cap, Job j) {
    if (*n == *cap) {
        Job *g = realloc(*q, (*cap = *cap * 2 + 64) * sizeof **q);
        if (!g) return;
        *q = g;
    }
    (*q)[(*n)++] = j;
    busy = 1;
}

static Job job(char kind, long id, int tmdb_id, const char *path, int decode) {
    Job j = {.kind = kind, .id = id, .tmdb_id = tmdb_id, .decode = decode};
    snprintf(j.path, sizeof j.path, "%s", path);
    return j;
}

void posters_want(char kind, long id, int tmdb_id, const char *path) {
    SDL_LockMutex(mu);
    add(&wanted, &nwanted, &capwanted, job(kind, id, tmdb_id, path, 1));
    SDL_CondSignal(cond);
    SDL_UnlockMutex(mu);
}

void posters_prefetch(char kind, long id, int tmdb_id, const char *path) {
    SDL_LockMutex(mu);
    add(&prefetch, &nprefetch, &capprefetch, job(kind, id, tmdb_id, path, 0));
    SDL_CondSignal(cond);
    SDL_UnlockMutex(mu);
}

int posters_take(char *kind, long *id, SDL_Surface **s) {
    SDL_LockMutex(mu);
    int got = ndone > 0;
    if (got) {
        Done d = done[--ndone];
        *kind = d.kind;
        *id = d.id;
        *s = d.s;
    }
    SDL_UnlockMutex(mu);
    return got;
}
