#include "usb.h"

#include <ctype.h>
#include <dirent.h>
#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <strings.h>
#include <sys/stat.h>

#ifndef USB_TEST /* the test on a PC has no FFmpeg */
#include <libavformat/avformat.h>
#endif

#include "blackbox.h"
#ifndef USB_TEST
#include "cJSON.h"
#include "ftpbridge.h"
#include "net.h"
#endif

#ifndef RESUME_FILE
#define RESUME_FILE "/data/homebrew/FLEX/usb-resume.txt"
#endif
#define MAX_DRIVES 8
#ifndef USB_ROOT_FMT
#define USB_ROOT_FMT "/mnt/usb%d" /* where the console mounts drives */
#endif
#define MAX_RESUME 300

static UsbFile *movies;
static int nmovies;
static UsbShow *shows;
static int nshows;
static char last_sig[256] = "?"; /* which drives had which folders at the last scan */

static int is_video(const char *name) {
    static const char *exts[] = {".mkv", ".mp4", ".m4v", ".avi", ".mov", ".ts", ".m2ts", ".webm", ".mpg", ".mpeg", ".wmv"};
    const char *dot = strrchr(name, '.');
    if (!dot || name[0] == '.') return 0; /* no extension, or macOS's "._name" shadows */
    for (size_t i = 0; i < sizeof exts / sizeof *exts; i++)
        if (strcasecmp(dot, exts[i]) == 0) return 1;
    return 0;
}

/* ---------- reading folders ----------
 *
 * As its own app FLEX may open a file on the drive but not list a folder
 * there (opendir: "Operation not permitted"), so when the system says no,
 * the list comes from the homebrew web server that runs on a jailbroken
 * console (websrv, port 8080): GET /fs/<path>/?fmt=json. */

typedef struct {
    char name[256];
    int dir;
} Ent;

#define WEBSRV_PORT 8080
static int via_websrv; /* the last listing came from websrv */

/* url_path writes path with everything but letters, digits and /-._~ escaped. */
static void url_path(const char *path, char *out, size_t size) {
    size_t n = 0;
    for (const unsigned char *c = (const unsigned char *)path; *c && n + 4 < size; c++) {
        if (isalnum(*c) || strchr("/-._~", *c)) out[n++] = (char)*c;
        else n += (size_t)snprintf(out + n, size - n, "%%%02X", *c);
    }
    out[n] = 0;
}

/* list_dir returns the entries of path (malloc'd, *out) or -1. */
static int list_dir(const char *path, Ent **out) {
    Ent *list = NULL;
    int n = 0;
    *out = NULL;
    DIR *d = opendir(path);
    if (d) {
        struct dirent *e;
        while ((e = readdir(d))) {
            if (strcmp(e->d_name, ".") == 0 || strcmp(e->d_name, "..") == 0) continue;
            if (n % 64 == 0) list = realloc(list, (n + 64) * sizeof *list);
            snprintf(list[n].name, sizeof list[n].name, "%s", e->d_name);
            char full[768];
            struct stat st;
            snprintf(full, sizeof full, "%s/%s", path, e->d_name);
            list[n].dir = e->d_type == DT_DIR || (e->d_type != DT_REG && stat(full, &st) == 0 && S_ISDIR(st.st_mode));
            n++;
        }
        closedir(d);
        via_websrv = 0;
        *out = list;
        return n;
    }
#ifndef USB_TEST
    char url[1600] = "/fs";
    url_path(path, url + 3, sizeof url - 16);
    strcat(url, "/?fmt=json");
    char *body = flex_get("127.0.0.1", WEBSRV_PORT, url);
    cJSON *j = body ? cJSON_Parse(body) : NULL;
    free(body);
    if (!cJSON_IsArray(j)) {
        cJSON_Delete(j);
        return -1;
    }
    cJSON *e;
    cJSON_ArrayForEach(e, j) {
        const char *name = cJSON_GetStringValue(cJSON_GetObjectItem(e, "name"));
        const char *mode = cJSON_GetStringValue(cJSON_GetObjectItem(e, "mode"));
        if (!name || strcmp(name, ".") == 0 || strcmp(name, "..") == 0) continue;
        if (n % 64 == 0) list = realloc(list, (n + 64) * sizeof *list);
        snprintf(list[n].name, sizeof list[n].name, "%s", name);
        list[n].dir = mode && mode[0] == 'd';
        n++;
    }
    cJSON_Delete(j);
    via_websrv = 1;
    *out = list;
    return n;
#else
    return -1;
#endif
}

static int named(const char *name, const char *const *list) {
    for (; *list; list++)
        if (strcasecmp(name, *list) == 0) return 1;
    return 0;
}
static const char *const movie_dirs[] = {"movies", "movie", "films", "film", NULL};
static const char *const tv_dirs[] = {"tv", "tv shows", "tvshows", "shows", "series", NULL};

/* clean_title turns a file or folder name into a title: the extension goes,
 * dots and underscores become spaces, and what follows the year or the
 * first release tag ("1080p", "BluRay", "x264"...) is dropped. */
static void clean_title(const char *name, int strip_ext, char *out, size_t size) {
    char s[256];
    snprintf(s, sizeof s, "%s", name);
    char *dot = strrchr(s, '.');
    if (strip_ext && dot) *dot = 0;
    for (char *c = s; *c; c++)
        if (*c == '.' || *c == '_') *c = ' ';
    static const char *tags[] = {"2160p", "1080p", "1080i", "720p", "576p", "480p", "bluray", "blu-ray", "brrip", "bdrip",
                                 "webrip", "web-dl", "webdl", "hdtv", "dvdrip", "x264", "x265", "h264", "h265", "hevc",
                                 "xvid", "remux", "uhd", "hdr", NULL};
    char *cut = NULL;
    for (char *c = s; *c; c++) {
        if (c == s || c[-1] != ' ') continue; /* tags and years start a word, and not the first */
        if ((c[0] == '1' || c[0] == '2') && isdigit((unsigned char)c[1]) && isdigit((unsigned char)c[2]) &&
            isdigit((unsigned char)c[3]) && (c[4] == 0 || c[4] == ' ' || c[4] == ')') && (c[0] == '2' || c[1] == '9')) {
            cut = c + 4; /* keep the year */
            break;
        }
        if (c[0] == '(' || c[0] == '[') continue;
        for (const char **t = tags; *t; t++)
            if (strncasecmp(c, *t, strlen(*t)) == 0) cut = c;
        if (cut) break;
    }
    if (cut) *cut = 0;
    size_t len = strlen(s);
    while (len && (s[len - 1] == ' ' || s[len - 1] == '-' || s[len - 1] == '(' || s[len - 1] == '[')) s[--len] = 0;
    snprintf(out, size, "%s", s[0] ? s : name);
}

/* episode_numbers finds S01E02 or 1x02 in a name. */
static void episode_numbers(const char *name, int *season, int *episode) {
    *season = *episode = 0;
    for (const char *c = name; *c; c++) {
        int s, e, n = 0;
        if ((c[0] == 's' || c[0] == 'S') && sscanf(c + 1, "%2d%*1[eE]%3d%n", &s, &e, &n) == 2 && n) {
            *season = s, *episode = e;
            return;
        }
        if (isdigit((unsigned char)c[0]) && (c == name || !isdigit((unsigned char)c[-1])) &&
            sscanf(c, "%2d%*1[xX]%3d%n", &s, &e, &n) == 2 && n) {
            *season = s, *episode = e;
            return;
        }
    }
}

typedef void (*found_fn)(const char *path, const char *name, void *arg);

/* walk calls fn for every video file under dir, depth folders deep. */
static void walk(const char *dir, int depth, found_fn fn, void *arg) {
    Ent *list;
    int n = list_dir(dir, &list);
    for (int i = 0; i < n; i++) {
        char path[512];
        if (list[i].name[0] == '.' || snprintf(path, sizeof path, "%s/%s", dir, list[i].name) >= (int)sizeof path) continue;
        if (list[i].dir) {
            if (depth > 0) walk(path, depth - 1, fn, arg);
        } else if (is_video(list[i].name)) {
            fn(path, list[i].name, arg);
        }
    }
    free(list);
}

static void add_movie(const char *path, const char *name, void *arg) {
    const char *folder = arg; /* the movie's own folder, when it is the only video in it */
    if (nmovies % 64 == 0) movies = realloc(movies, (nmovies + 64) * sizeof *movies);
    UsbFile *m = &movies[nmovies++];
    memset(m, 0, sizeof *m);
    snprintf(m->path, sizeof m->path, "%s", path);
    if (folder) clean_title(folder, 0, m->title, sizeof m->title);
    else clean_title(name, 1, m->title, sizeof m->title);
}

static void count_one(const char *path, const char *name, void *arg) {
    (void)path, (void)name;
    ++*(int *)arg;
}

static void add_episode(const char *path, const char *name, void *arg) {
    UsbShow *s = arg;
    if (s->n % 64 == 0) s->eps = realloc(s->eps, (s->n + 64) * sizeof *s->eps);
    UsbFile *f = &s->eps[s->n++];
    memset(f, 0, sizeof *f);
    snprintf(f->path, sizeof f->path, "%s", path);
    clean_title(name, 1, f->title, sizeof f->title);
    episode_numbers(name, &f->season, &f->episode);
    if (!f->season) { /* "Season 2/whatever.mkv" */
        for (const char *c = path; (c = strchr(c, '/'));) {
            int n;
            if (strncasecmp(++c, "season", 6) == 0 && sscanf(c + 6, "%d", &n) == 1) f->season = n;
        }
    }
}

static int by_title(const void *a, const void *b) { return strcasecmp(((const UsbFile *)a)->title, ((const UsbFile *)b)->title); }
static int by_show(const void *a, const void *b) { return strcasecmp(((const UsbShow *)a)->title, ((const UsbShow *)b)->title); }
static int by_episode(const void *a, const void *b) {
    const UsbFile *x = a, *y = b;
    if (x->season != y->season) return x->season - y->season;
    if (x->episode != y->episode) return x->episode - y->episode;
    return strcasecmp(x->path, y->path);
}

static UsbShow *new_show(const char *title) {
    if (nshows % 32 == 0) shows = realloc(shows, (nshows + 32) * sizeof *shows);
    UsbShow *s = &shows[nshows++];
    memset(s, 0, sizeof *s);
    snprintf(s->title, sizeof s->title, "%s", title);
    return s;
}

static void scan_movies(const char *dir) {
    Ent *list;
    int n = list_dir(dir, &list);
    for (int i = 0; i < n; i++) {
        char path[512];
        if (list[i].name[0] == '.' || snprintf(path, sizeof path, "%s/%s", dir, list[i].name) >= (int)sizeof path) continue;
        if (list[i].dir) {
            int videos = 0;
            walk(path, 1, count_one, &videos);
            walk(path, 1, add_movie, videos == 1 ? list[i].name : NULL);
        } else if (is_video(list[i].name)) {
            add_movie(path, list[i].name, NULL);
        }
    }
    free(list);
}

static void scan_tv(const char *dir) {
    UsbShow loose = {0}; /* files straight in the TV folder */
    Ent *list;
    int n = list_dir(dir, &list);
    for (int i = 0; i < n; i++) {
        char path[512];
        if (list[i].name[0] == '.' || snprintf(path, sizeof path, "%s/%s", dir, list[i].name) >= (int)sizeof path) continue;
        if (list[i].dir) {
            UsbShow s = {0};
            walk(path, 2, add_episode, &s);
            if (s.n == 0) continue;
            char title[200];
            clean_title(list[i].name, 0, title, sizeof title);
            UsbShow *dst = new_show(title);
            dst->eps = s.eps, dst->n = s.n;
        } else if (is_video(list[i].name)) {
            add_episode(path, list[i].name, &loose);
        }
    }
    free(list);
    if (loose.n) {
        UsbShow *dst = new_show("Other episodes");
        dst->eps = loose.eps, dst->n = loose.n;
    }
}

/* each_library calls fn for the Movies and TV folders at the top of every
 * drive (the console mounts them as /mnt/usb0 ... /mnt/usb7). */
static void each_library(void (*fn)(const char *dir, int tv, void *arg), void *arg) {
    for (int i = 0; i < MAX_DRIVES; i++) {
        char root[128];
        snprintf(root, sizeof root, USB_ROOT_FMT, i);
        Ent *list;
        int n = list_dir(root, &list);
        for (int k = 0; k < n; k++) {
            int tv = named(list[k].name, tv_dirs);
            if (!list[k].dir || (!tv && !named(list[k].name, movie_dirs))) continue;
            char path[512];
            snprintf(path, sizeof path, "%s/%s", root, list[k].name);
            fn(path, tv, arg);
        }
        free(list);
        if (n < 0 && i == 0) break; /* no way to list drives at all */
    }
}

static void sig_one(const char *dir, int tv, void *arg) {
    (void)tv;
    char *sig = arg;
    size_t len = strlen(sig);
    snprintf(sig + len, sizeof last_sig - len, "%s;", dir);
}

int usb_changed(void) {
    char sig[sizeof last_sig] = "";
    each_library(sig_one, sig);
    return strcmp(sig, last_sig) != 0;
}

static void scan_one(const char *dir, int tv, void *arg) {
    (void)arg;
    if (tv) scan_tv(dir);
    else scan_movies(dir);
}

void usb_scan(void) {
    for (int i = 0; i < nshows; i++) free(shows[i].eps);
    free(shows);
    free(movies);
    shows = NULL, movies = NULL, nshows = nmovies = 0;
    last_sig[0] = 0;
    each_library(sig_one, last_sig);
    each_library(scan_one, NULL);
    if (nmovies) qsort(movies, nmovies, sizeof *movies, by_title);
    if (nshows) qsort(shows, nshows, sizeof *shows, by_show);
    for (int i = 0; i < nshows; i++) qsort(shows[i].eps, shows[i].n, sizeof *shows[i].eps, by_episode);
    if (last_sig[0] || nmovies || nshows) bb_log("usb: %s %d movies, %d shows", last_sig, nmovies, nshows);
    static int told; /* once: how the first drive looks from in here */
    if (!told++) {
        char root[128];
        snprintf(root, sizeof root, USB_ROOT_FMT, 0);
        Ent *list;
        int n = list_dir(root, &list);
        free(list);
        if (n < 0) bb_log("usb: %s can't be listed, by the system (%s) or through websrv", root, strerror(errno));
        else bb_log("usb: %s has %d entries, listed %s", root, n, via_websrv ? "through websrv" : "by the system");
    }
}

int usb_movie_count(void) { return nmovies; }
const UsbFile *usb_movie(int i) { return i >= 0 && i < nmovies ? &movies[i] : NULL; }
int usb_show_count(void) { return nshows; }
const UsbShow *usb_show(int i) { return i >= 0 && i < nshows ? &shows[i] : NULL; }

/* usb_url: what the player should open for a file on the drive, and whether
 * it can seek in it. The file itself when the app may read it. Else the
 * console's FTP server reads it for us, behind a local web address
 * (ftpbridge.c); and if that can't start, websrv serves it from the start
 * only (it ignores byte ranges, which also breaks MP4s whose index is at
 * the end). */
int usb_url(const char *path, char *out, size_t size) {
    FILE *f = fopen(path, "rb");
    if (f) {
        fclose(f);
        snprintf(out, size, "%s", path);
        return 1;
    }
    int err = errno, seekable = 1, n;
#ifndef USB_TEST
    int port = ftpbridge_port("127.0.0.1", 2121, 0);
#else
    int port = 0;
#endif
    if (port) {
        n = snprintf(out, size, "http://127.0.0.1:%d", port);
    } else {
        n = snprintf(out, size, "http://127.0.0.1:%d/fs", WEBSRV_PORT);
        seekable = 0;
    }
    url_path(path, out + n, size - (size_t)n);
    bb_log("usb: can't open %s itself (%s), playing it through %s", path, strerror(err),
           seekable ? "the console's FTP server" : "websrv, without seeking");
    return seekable;
}

/* ---------- resume points: "seconds<TAB>path" a line, newest last ---------- */

typedef struct {
    int pos;
    char path[512];
} Resume;

static int load_resume(Resume *r) {
    FILE *f = fopen(RESUME_FILE, "r");
    if (!f) return 0;
    int n = 0;
    char line[600];
    while (n < MAX_RESUME && fgets(line, sizeof line, f)) {
        char *tab = strchr(line, '\t');
        if (!tab) continue;
        line[strcspn(line, "\r\n")] = 0;
        r[n].pos = atoi(line);
        snprintf(r[n].path, sizeof r[n].path, "%s", tab + 1);
        n++;
    }
    fclose(f);
    return n;
}

int usb_resume(const char *path) {
    Resume *r = malloc(MAX_RESUME * sizeof *r);
    if (!r) return 0;
    int n = load_resume(r), pos = 0;
    for (int i = 0; i < n; i++)
        if (strcmp(r[i].path, path) == 0) pos = r[i].pos;
    free(r);
    return pos;
}

void usb_set_resume(const char *path, double position, double duration) {
    Resume *r = malloc(MAX_RESUME * sizeof *r);
    if (!r) return;
    int n = load_resume(r);
    int keep = position > 30 && (duration <= 0 || position < duration * 0.95);
    FILE *f = fopen(RESUME_FILE, "w");
    if (f) {
        /* drop the oldest when full, and this file's old line */
        for (int i = n >= MAX_RESUME ? 1 : 0; i < n; i++)
            if (strcmp(r[i].path, path) != 0) fprintf(f, "%d\t%s\n", r[i].pos, r[i].path);
        if (keep) fprintf(f, "%d\t%s\n", (int)position, path);
        fclose(f);
    }
    free(r);
}

#ifndef USB_TEST
double usb_duration(const char *path) {
    AVFormatContext *fmt = NULL;
    double d = 0;
    if (avformat_open_input(&fmt, path, NULL, NULL) < 0) return 0;
    if (fmt->duration <= 0) avformat_find_stream_info(fmt, NULL);
    if (fmt->duration > 0) d = (double)fmt->duration / AV_TIME_BASE;
    avformat_close_input(&fmt);
    return d;
}
#endif
