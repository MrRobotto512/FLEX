/* FLEX PS5 client: find the server, browse Movies and TV as poster grids,
 * open a show's episodes, play.
 *
 * Controls (grid): D-pad moves, L1/R1 switch Movies/TV, Cross plays a movie
 * or opens a show, Triangle re-discovers the server and refreshes, Options
 * installs an update the server offers, holding Circle closes FLEX. Episodes: D-pad
 * up/down, Cross plays, Circle back. Player: Cross pauses, Circle stops. */

#ifdef FLEX_APP
#include "app.h"
#endif
#include <SDL.h>
#include <SDL_image.h>
#include <fcntl.h>
#include <sys/file.h>
#include <sys/stat.h>
#include <unistd.h>
#include <SDL_ttf.h>
#include <libavutil/cpu.h>
#include <libavutil/mem.h>
#include <libavutil/sha.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <strings.h>

#include "blackbox.h"
#include "cJSON.h"
#include "net.h"
#include "player.h"
#include "posters.h"

#ifndef FLEX_VERSION
#define FLEX_VERSION "dev" /* the Makefile passes the git tag */
#endif

#define APP_DIR "/data/homebrew/FLEX"
#define EBOOT APP_DIR "/eboot.elf"
#define SAVED_SERVER APP_DIR "/server.txt"
#define FONT_PATH APP_DIR "/assets/DejaVuSans.ttf"
#define ASPECT_FILE APP_DIR "/aspect.txt"
#define UPSCALE_FILE APP_DIR "/upscale.txt"
#define VOLUME_FILE APP_DIR "/volume.txt"
#define PICTURE_FILE APP_DIR "/picture.txt"
#define LINK_FILE APP_DIR "/link.txt"

/* What this player decodes itself (CPU, any container and audio FFmpeg
 * reads, scaled to the screen with Lanczos); the server plays such files as
 * they are when they fit the link, and converts the rest. HDR still needs
 * the server's tone mapping. */
#define PLAYER_CAPS "h264,hevc,mpeg4,10bit,2160p,anycontainer,anyaudio"
#define SCREEN_W 1920
#define SCREEN_H 1080

/* grid geometry: 6 columns x 2 rows of 2:3 posters at 1080p */
#define MARGIN_X 80
#define GRID_TOP 170
#define CARD_W 240
#define CARD_H 360
#define GAP_X 30
#define ROW_H (CARD_H + 80)
#define COLS ((SCREEN_W - 2 * MARGIN_X + GAP_X) / (CARD_W + GAP_X))
#define ROWS_VISIBLE 2
#define MAX_CACHED_POSTERS 48

/* episode list geometry */
#define EP_TOP 330
#define EP_ROW_H 54
#define EP_ROWS_VISIBLE 13

typedef enum { KIND_MOVIE, KIND_SHOW } Kind;

/* The tabs, in L1/R1 order. Home needs a server with /home; Continue is
 * skipped while empty, and while Home (whose first row it is) is there. */
typedef enum { TAB_HOME, TAB_CONTINUE, TAB_MOVIES, TAB_SHOWS, TAB_SEARCH, TABS } Tab;
static const char *tab_names[TABS] = {"Home", "Continue", "Movies", "TV", "Search"};

/* One poster card: a movie or a show. On the Continue tab a card can stand
 * for an episode: it shows the show's poster and plays episode play_id. */
typedef struct {
    Kind kind; /* whose poster, and what Cross does when episode is 0 */
    long id;
    int episode;
    long play_id;
    char subtitle[160]; /* "S01E02 · Title" */
    char title[200];
    int year, resume_s, runtime_s, watched;
    int seasons, episodes, unwatched; /* shows */
    char poster[128];                 /* API path or "" when the server has no artwork */
    char backdrop[128];               /* home cards: shown behind the details when focused */
    char overview[400], genres[80];   /* home cards */
    char specs[128];                  /* home cards: "1080p · HEVC 10-bit · 5.1 Dolby Digital+" */
    int tmdb_id;                      /* part of the poster's cache name: a new match means a new poster */
    SDL_Texture *tex;                 /* decoded poster, NULL until loaded */
    int tex_state;                    /* 0 = not loaded yet, 1 = loaded, 2 = unavailable, 3 = requested */
} Item;

/* One row of the episode list: a season header or an episode. */
typedef struct {
    int header;
    int season, episode;
    long id;
    char title[200];
    int resume_s, runtime_s, watched;
} EpRow;

typedef struct {
    long id;
    char title[200];
    char overview[600];
    int seasons, episodes, unwatched;
    EpRow *rows;
    int n;
} ShowView;

static const SDL_Color C_FG = {232, 234, 240, 255};
static const SDL_Color C_MUTED = {138, 144, 160, 255};
static const SDL_Color C_ACCENT = {90, 169, 255, 255};
static const SDL_Color C_PANEL = {24, 27, 34, 255};
static const SDL_Color C_BG = {15, 17, 21, 255};
static const SDL_Color C_BLACK = {0, 0, 0, 255};

static char g_host[128] = "";   /* address in use */
static char g_lan[128] = "";    /* last address found by LAN discovery */
static char g_remote[128] = ""; /* the server's Tailscale address, learned at home */
static int g_port = 8484;
static int g_is_remote;
static TTF_Font *g_big, *g_font, *g_small, *g_tiny;

static const char *kind_path(Kind k) { return k == KIND_MOVIE ? "movies" : "shows"; }

/* ---------- server address ----------
 *
 * server.txt holds "lan-host port" and, once seen, a second line with the
 * server's Tailscale address. At home the server is found by broadcast;
 * away (travel router on the tailnet) the Tailscale address is used. */

static void load_saved_server(void) {
    FILE *f = fopen(SAVED_SERVER, "r");
    if (!f) return;
    if (fscanf(f, "%127s %d", g_lan, &g_port) != 2) g_lan[0] = 0;
    if (fscanf(f, "%127s", g_remote) != 1) g_remote[0] = 0;
    fclose(f);
}

static void save_server(void) {
    FILE *f = fopen(SAVED_SERVER, "w");
    if (!f) return;
    fprintf(f, "%s %d\n%s\n", g_lan, g_port, g_remote);
    fclose(f);
}

/* probe asks host for /server; on success it becomes the address in use and
 * the server's Tailscale address (if any) is remembered. */
static int probe(const char *host) {
    char *body = flex_get(host, g_port, "/api/v1/server");
    if (!body) return 0;
    cJSON *j = cJSON_Parse(body);
    free(body);
    cJSON *ts = cJSON_GetObjectItem(j, "tailscale");
    const char *ip = cJSON_GetStringValue(cJSON_GetObjectItem(ts, "ip"));
    if (ip && ip[0]) snprintf(g_remote, sizeof g_remote, "%s", ip);
    cJSON_Delete(j);
    snprintf(g_host, sizeof g_host, "%s", host);
    return 1;
}

static int find_server(void) {
    load_saved_server();
    /* FLEX_SERVER=host:port (via websrv's env=) skips discovery and leaves
     * the saved address alone, for trying a test server */
    const char *fixed = getenv("FLEX_SERVER");
    if (fixed && sscanf(fixed, "%127[^:]:%d", g_lan, &g_port) >= 1) {
        g_is_remote = 0;
        return probe(g_lan) ? 0 : -1;
    }
    char found[128];
    int port = g_port;
    if (flex_discover(found, sizeof found, &port, 1500) == 0) {
        g_port = port;
        snprintf(g_lan, sizeof g_lan, "%s", found);
    }
    g_is_remote = 0;
    if (g_lan[0] && probe(g_lan)) {
        save_server();
        return 0;
    }
    if (g_remote[0] && probe(g_remote)) {
        g_is_remote = 1;
        return 0;
    }
    return -1;
}

static void set_status(char *status, size_t n, int movies, int shows) {
    snprintf(status, n, "%s:%d%s  ·  %d movies  ·  %d shows", g_host, g_port, g_is_remote ? " (remote)" : "", movies,
             shows);
}

/* ---------- per-item picture shape ----------
 * The Square button's choice is remembered per movie / episode in
 * aspect.txt ("m 42 2" = movie 42 plays as 4:3). */

typedef struct {
    char kind; /* 'm' movie, 'e' episode */
    long id;
    int aspect;
} AspectPref;

static AspectPref g_aspects[1024];
static int g_naspects;

static void load_aspects(void) {
    FILE *f = fopen(ASPECT_FILE, "r");
    if (!f) return;
    AspectPref a;
    while (g_naspects < 1024 && fscanf(f, " %c %ld %d", &a.kind, &a.id, &a.aspect) == 3) g_aspects[g_naspects++] = a;
    fclose(f);
}

static int get_aspect(char kind, long id) {
    for (int i = 0; i < g_naspects; i++)
        if (g_aspects[i].kind == kind && g_aspects[i].id == id) return g_aspects[i].aspect;
    return ASPECT_AUTO;
}

static void set_aspect(char kind, long id, int aspect) {
    if (get_aspect(kind, id) == aspect) return;
    int i = 0;
    while (i < g_naspects && !(g_aspects[i].kind == kind && g_aspects[i].id == id)) i++;
    if (aspect == ASPECT_AUTO) {
        if (i < g_naspects) g_aspects[i] = g_aspects[--g_naspects];
    } else if (i < g_naspects) {
        g_aspects[i].aspect = aspect;
    } else if (g_naspects < 1024) {
        g_aspects[g_naspects++] = (AspectPref){kind, id, aspect};
    }
    FILE *f = fopen(ASPECT_FILE, "w");
    if (!f) return;
    for (int j = 0; j < g_naspects; j++) fprintf(f, "%c %ld %d\n", g_aspects[j].kind, g_aspects[j].id, g_aspects[j].aspect);
    fclose(f);
}

/* Upscaling (Triangle while playing) and volume (L1/R1) are one setting for
 * everything, each kept in its own file. */
static int g_upscale, g_volume = 100;

/* Picture adjustments (Options while playing): saturation, brightness,
 * contrast, one setting for everything, kept in picture.txt. */
static int g_picture[PIC_COUNT] = {100, 0, 100};

static void load_picture(void) {
    FILE *f = fopen(PICTURE_FILE, "r");
    if (!f) return;
    int v[PIC_COUNT];
    if (fscanf(f, "%d %d %d", &v[0], &v[1], &v[2]) == PIC_COUNT) memcpy(g_picture, v, sizeof v);
    fclose(f);
}

static void save_picture(void) {
    FILE *f = fopen(PICTURE_FILE, "w");
    if (!f) return;
    fprintf(f, "%d %d %d\n", g_picture[0], g_picture[1], g_picture[2]);
    fclose(f);
}

/* How fast streams reach this console (kbit/s, 0 = not measured yet), kept
 * in link.txt and sent with /play so the server picks the best stream that
 * fits. Direct files show the link itself; a transcode is paced by the
 * server, so it can only raise the estimate. */
static int g_link_kbps;

static void set_link(int kbps) {
    if (kbps <= 0 || kbps == g_link_kbps) return;
    g_link_kbps = kbps;
    FILE *f = fopen(LINK_FILE, "w");
    if (f) {
        fprintf(f, "%d\n", g_link_kbps);
        fclose(f);
    }
    fprintf(stderr, "FLEX: link estimate %d kbit/s\n", g_link_kbps);
}

static void note_link(int kbps, int direct) {
    if (kbps <= 0) return;
    if (direct) set_link(g_link_kbps ? (g_link_kbps + kbps) / 2 : kbps);
    else if (kbps > g_link_kbps) set_link(kbps);
}

static void load_int(const char *path, int *v) {
    FILE *f = fopen(path, "r");
    if (!f) return;
    int n;
    if (fscanf(f, "%d", &n) == 1) *v = n;
    fclose(f);
}

static void save_int(const char *path, int v) {
    FILE *f = fopen(path, "w");
    if (!f) return;
    fprintf(f, "%d\n", v);
    fclose(f);
}

/* An update the server offers (GET /api/v1/client/ps5); empty version = none. */
static struct {
    char version[32], sha256[65], url[128];
    long size;
    struct { /* the app build's files (FLEX_APP) */
        char path[64], sha256[65], url[160];
        long size;
    } files[16];
    int nfiles;
} g_update;

/* ---------- API ---------- */

static int jint(cJSON *o, const char *key) {
    cJSON *v = cJSON_GetObjectItem(o, key);
    return cJSON_IsNumber(v) ? v->valueint : 0;
}

static void jstr(cJSON *o, const char *key, char *dst, size_t n) {
    cJSON *v = cJSON_GetObjectItem(o, key);
    snprintf(dst, n, "%s", cJSON_IsString(v) ? v->valuestring : "");
}

static Item *fetch_items(Kind kind, int *count) {
    *count = 0;
    char path[64];
    snprintf(path, sizeof path, "/api/v1/%s%s", kind_path(kind), kind == KIND_MOVIE ? "?sort=title" : "");
    char *body = flex_get(g_host, g_port, path);
    if (!body) return NULL;
    cJSON *arr = cJSON_Parse(body);
    free(body);
    if (!cJSON_IsArray(arr)) {
        cJSON_Delete(arr);
        return NULL;
    }
    int n = cJSON_GetArraySize(arr);
    Item *list = calloc(n ? n : 1, sizeof *list);
    for (int i = 0; i < n; i++) {
        cJSON *m = cJSON_GetArrayItem(arr, i);
        Item *it = &list[i];
        it->kind = kind;
        it->id = jint(m, "id");
        jstr(m, "title", it->title, sizeof it->title);
        jstr(m, "poster", it->poster, sizeof it->poster);
        it->tmdb_id = jint(m, "tmdb_id");
        it->year = jint(m, "year");
        it->resume_s = jint(m, "resume_s");
        it->runtime_s = jint(m, "runtime_s");
        it->watched = cJSON_IsTrue(cJSON_GetObjectItem(m, "watched"));
        it->seasons = jint(m, "seasons");
        it->episodes = jint(m, "episodes");
        it->unwatched = jint(m, "unwatched");
    }
    cJSON_Delete(arr);
    *count = n;
    return list;
}

/* fetch_continue reads the Continue Watching row: movies stopped part way
 * and the next episode of shows being watched, most recent first. An older
 * server has no /continue; the tab then stays empty. */
static void parse_continue(cJSON *c, Item *it) {
    char s[16];
    jstr(c, "poster_of", s, sizeof s);
    it->kind = strcmp(s, "show") == 0 ? KIND_SHOW : KIND_MOVIE;
    it->id = jint(c, "poster_id");
    jstr(c, "kind", s, sizeof s);
    it->episode = strcmp(s, "episode") == 0;
    it->play_id = jint(c, "id");
    jstr(c, "title", it->title, sizeof it->title);
    jstr(c, "subtitle", it->subtitle, sizeof it->subtitle);
    jstr(c, "poster", it->poster, sizeof it->poster);
    it->tmdb_id = jint(c, "tmdb_id");
    it->resume_s = jint(c, "resume_s");
    it->runtime_s = jint(c, "runtime_s");
    /* the movie's or show's backdrop, for the home screen */
    snprintf(it->backdrop, sizeof it->backdrop, "/api/v1/%s/%ld/backdrop", kind_path(it->kind), it->id);
}

static Item *fetch_continue(int *count) {
    *count = 0;
    char *body = flex_get(g_host, g_port, "/api/v1/continue?limit=24");
    if (!body) return NULL;
    cJSON *arr = cJSON_Parse(body);
    free(body);
    int n = cJSON_IsArray(arr) ? cJSON_GetArraySize(arr) : 0;
    Item *list = calloc(n ? n : 1, sizeof *list);
    for (int i = 0; i < n; i++) parse_continue(cJSON_GetArrayItem(arr, i), &list[i]);
    cJSON_Delete(arr);
    *count = n;
    return list;
}

/* ---------- home ----------
 * GET /api/v1/home: rows of posters (Continue Watching first) under the
 * focused title's backdrop and details. The cards of every row sit in one
 * list, lists[TAB_HOME], so posters and Cross work as on the other tabs; a
 * row is a stretch of that list. */

#define MAX_HOME_ROWS 40
#define HOME_TOP 540 /* the focused row's name; the cards start 40 px below */
#define HCARD_W 180
#define HCARD_H 270
#define HGAP 20
#define HROW_H (40 + HCARD_H + 40)
#define HCOLS ((SCREEN_W - MARGIN_X + HGAP) / (HCARD_W + HGAP)) /* whole cards in view */
#define BACKDROP_X 640                                            /* 1280x720 backdrops, drawn unscaled */

typedef struct {
    char title[96];
    int start, n;   /* the row's cards in the list */
    int col, first; /* focused card, first card in view */
} HomeRow;

static void keep_posters(Item *old, int old_n, Item *fresh, int n);
static void want_poster(Item *m);

static HomeRow g_rows[MAX_HOME_ROWS];
static int g_nrows, g_row;
static SDL_Texture *g_backdrop, *g_fade;
static char g_bd_kind; /* the backdrop wanted / shown: 'b' movie, 'd' show */
static long g_bd_id;
static Uint32 g_moved; /* when the home focus last moved */

static void parse_card(cJSON *c, Item *it) {
    char s[16];
    jstr(c, "kind", s, sizeof s);
    it->kind = strcmp(s, "show") == 0 ? KIND_SHOW : KIND_MOVIE;
    it->id = jint(c, "id");
    if (strcmp(s, "episode") == 0) {
        /* a single episode (the seasonal rows): the show's poster, Cross plays the episode */
        it->kind = KIND_SHOW;
        it->id = jint(c, "show_id");
        it->episode = 1;
        it->play_id = jint(c, "id");
        jstr(c, "subtitle", it->subtitle, sizeof it->subtitle);
    }
    jstr(c, "title", it->title, sizeof it->title);
    jstr(c, "poster", it->poster, sizeof it->poster);
    jstr(c, "backdrop", it->backdrop, sizeof it->backdrop);
    jstr(c, "overview", it->overview, sizeof it->overview);
    jstr(c, "specs", it->specs, sizeof it->specs);
    it->year = jint(c, "year");
    it->resume_s = jint(c, "resume_s");
    it->runtime_s = jint(c, "runtime_s");
    it->seasons = jint(c, "seasons");
    it->watched = cJSON_IsTrue(cJSON_GetObjectItem(c, "watched"));
    cJSON *g;
    int ng = 0;
    cJSON_ArrayForEach(g, cJSON_GetObjectItem(c, "genres")) {
        if (!cJSON_IsString(g) || ng++ == 3) break;
        size_t len = strlen(it->genres);
        snprintf(it->genres + len, sizeof it->genres - len, "%s%s", len ? ", " : "", g->valuestring);
    }
}

/* fetch_home reads the home screen; no rows from a server without /home. */
static Item *fetch_home(int *count) {
    *count = 0;
    g_nrows = 0;
    char *body = flex_get(g_host, g_port, "/api/v1/home");
    if (!body) return NULL;
    cJSON *j = cJSON_Parse(body);
    free(body);
    if (!cJSON_IsObject(j)) {
        cJSON_Delete(j);
        return NULL;
    }
    cJSON *cont = cJSON_GetObjectItem(j, "continue"), *rows = cJSON_GetObjectItem(j, "rows"), *r, *c;
    int total = cJSON_GetArraySize(cont);
    cJSON_ArrayForEach(r, rows) total += cJSON_GetArraySize(cJSON_GetObjectItem(r, "items"));
    Item *list = calloc(total ? total : 1, sizeof *list);
    int n = 0;
    if (cJSON_GetArraySize(cont) > 0) {
        HomeRow *hr = &g_rows[g_nrows++];
        memset(hr, 0, sizeof *hr);
        snprintf(hr->title, sizeof hr->title, "Continue Watching");
        cJSON_ArrayForEach(c, cont) parse_continue(c, &list[n++]);
        hr->n = n;
    }
    cJSON_ArrayForEach(r, rows) {
        if (g_nrows == MAX_HOME_ROWS) break;
        HomeRow *hr = &g_rows[g_nrows];
        memset(hr, 0, sizeof *hr);
        jstr(r, "title", hr->title, sizeof hr->title);
        hr->start = n;
        cJSON_ArrayForEach(c, cJSON_GetObjectItem(r, "items")) parse_card(c, &list[n++]);
        hr->n = n - hr->start;
        if (hr->n) g_nrows++;
    }
    cJSON_Delete(j);
    *count = n;
    return list;
}

/* home_sel is the focused card's place in lists[TAB_HOME]. */
static int home_sel(void) { return g_nrows ? g_rows[g_row].start + g_rows[g_row].col : 0; }

/* refresh_home re-fetches the home screen, keeping posters, the focused row
 * and the place in each row, found by name (a Continue Watching row coming
 * or going moves the others). */
static void refresh_home(Item **list, int *n) {
    HomeRow was[MAX_HOME_ROWS];
    int nwas = g_nrows;
    char focused[96] = "";
    if (g_nrows) snprintf(focused, sizeof focused, "%s", g_rows[g_row].title);
    memcpy(was, g_rows, sizeof was);
    Item *old = *list;
    int old_n = *n;
    *list = fetch_home(n);
    keep_posters(old, old_n, *list, *n);
    g_row = 0;
    for (int i = 0; i < g_nrows; i++) {
        HomeRow *r = &g_rows[i];
        for (int k = 0; k < nwas; k++)
            if (strcmp(was[k].title, r->title) == 0) {
                r->col = was[k].col < r->n ? was[k].col : r->n - 1;
                r->first = was[k].first <= r->col ? was[k].first : r->col;
                break;
            }
        if (strcmp(r->title, focused) == 0) g_row = i;
    }
}

static void move_home(int button) {
    if (!g_nrows) return;
    HomeRow *r = &g_rows[g_row];
    switch (button) {
    case SDL_CONTROLLER_BUTTON_DPAD_LEFT: if (r->col > 0) r->col--; break;
    case SDL_CONTROLLER_BUTTON_DPAD_RIGHT: if (r->col + 1 < r->n) r->col++; break;
    case SDL_CONTROLLER_BUTTON_DPAD_UP: if (g_row > 0) g_row--; break;
    case SDL_CONTROLLER_BUTTON_DPAD_DOWN: if (g_row + 1 < g_nrows) g_row++; break;
    }
    r = &g_rows[g_row];
    if (r->col < r->first) r->first = r->col;
    if (r->col >= r->first + HCOLS) r->first = r->col - HCOLS + 1;
    g_moved = SDL_GetTicks();
}

/* want_backdrop asks for the focused title's backdrop once the focus has
 * rested on it a moment, so scrolling along a row doesn't fetch every one.
 * A title without one keeps the last backdrop up. */
static void want_backdrop(Item *f) {
    if (!f || !f->backdrop[0] || SDL_GetTicks() - g_moved < 200) return;
    char k = f->kind == KIND_MOVIE ? 'b' : 'd';
    if (k == g_bd_kind && f->id == g_bd_id) return;
    g_bd_kind = k;
    g_bd_id = f->id;
    posters_want(k, f->id, f->tmdb_id, f->backdrop);
}

/* load_home_posters wants the posters of the rows on screen (and the row
 * above), the focused row's last so it is served first, and lets go of
 * posters far away once many are held. */
static void load_home_posters(Item *list, int n) {
    if (!list || !g_nrows) return;
    const int order[] = {2, 1, -1, 0};
    for (int k = 0; k < 4; k++) {
        int r = g_row + order[k];
        if (r < 0 || r >= g_nrows) continue;
        HomeRow *hr = &g_rows[r];
        for (int c = hr->first + HCOLS + 1; c >= hr->first; c--)
            if (c < hr->n) want_poster(&list[hr->start + c]);
    }
    int cached = 0;
    for (int i = 0; i < n; i++) cached += list[i].tex != NULL;
    if (cached <= 3 * MAX_CACHED_POSTERS / 2) return;
    int lo = g_rows[g_row > 0 ? g_row - 1 : 0].start;
    int top = g_row + 2 < g_nrows ? g_row + 2 : g_nrows - 1;
    int hi = g_rows[top].start + g_rows[top].n;
    for (int i = 0; i < n; i++)
        if (list[i].tex && (i < lo || i >= hi)) {
            SDL_DestroyTexture(list[i].tex);
            list[i].tex = NULL;
            list[i].tex_state = 0;
        }
}

/* make_fade is drawn over the backdrop: it fades in from the background on
 * the left, where the details are, out again at the bottom, under the rows,
 * and darkens the top under the tabs. */
static SDL_Texture *make_fade(SDL_Renderer *ren) {
    SDL_Surface *s = SDL_CreateRGBSurfaceWithFormat(0, 1280, 720, 32, SDL_PIXELFORMAT_RGBA32);
    if (!s) return NULL;
    for (int y = 0; y < 720; y++) {
        Uint32 *px = (Uint32 *)((Uint8 *)s->pixels + y * s->pitch);
        for (int x = 0; x < 1280; x++) {
            int a = x < 640 ? 255 - x * 255 / 640 : 0;
            int b = y > 420 ? (y - 420) * 255 / 300 : 0;
            int t = y < 150 ? 200 - y * 200 / 150 : 0;
            if (b > a) a = b;
            if (t > a) a = t;
            if (a < 60) a = 60; /* a little darker throughout, for the text */
            px[x] = SDL_MapRGBA(s->format, C_BG.r, C_BG.g, C_BG.b, (Uint8)a);
        }
    }
    SDL_Texture *t = SDL_CreateTextureFromSurface(ren, s);
    SDL_FreeSurface(s);
    if (t) SDL_SetTextureBlendMode(t, SDL_BLENDMODE_BLEND);
    return t;
}

/* keep_posters moves decoded posters from old cards to the same items in a
 * re-fetched list, so a refresh doesn't flash every card blank. */
static void keep_posters(Item *old, int old_n, Item *fresh, int n) {
    for (int i = 0; fresh && i < n; i++)
        for (int j = 0; j < old_n; j++)
            if (old[j].kind == fresh[i].kind && old[j].id == fresh[i].id && old[j].tex) {
                fresh[i].tex = old[j].tex;
                fresh[i].tex_state = 1;
                old[j].tex = NULL;
                break;
            }
    for (int j = 0; j < old_n; j++)
        if (old[j].tex) SDL_DestroyTexture(old[j].tex);
    free(old);
}

/* refresh_items re-fetches a grid but keeps already decoded posters. */
static void refresh_items(Kind kind, Item **list, int *n) {
    Item *old = *list;
    int old_n = *n;
    *list = fetch_items(kind, n);
    keep_posters(old, old_n, *list, *n);
}

static void refresh_continue(Item **list, int *n) {
    Item *old = *list;
    int old_n = *n;
    *list = fetch_continue(n);
    keep_posters(old, old_n, *list, *n);
}

static void free_items(Item *list, int n) {
    for (int i = 0; i < n; i++)
        if (list[i].tex) SDL_DestroyTexture(list[i].tex);
    free(list);
}

/* fetch_show fills a ShowView with season headers and episodes in order.
 * Returns the row index of the next episode to watch (or 0). */
static int fetch_show(long id, ShowView *sv) {
    free(sv->rows);
    memset(sv, 0, sizeof *sv);
    sv->id = id;
    char path[64];
    snprintf(path, sizeof path, "/api/v1/shows/%ld", id);
    char *body = flex_get(g_host, g_port, path);
    if (!body) return 0;
    cJSON *j = cJSON_Parse(body);
    free(body);
    if (!j) return 0;
    jstr(j, "title", sv->title, sizeof sv->title);
    jstr(j, "overview", sv->overview, sizeof sv->overview);
    sv->seasons = jint(j, "seasons");
    sv->episodes = jint(j, "episodes");
    sv->unwatched = jint(j, "unwatched");
    long next_id = 0;
    cJSON *next = cJSON_GetObjectItem(j, "next_episode");
    if (cJSON_IsObject(next)) next_id = jint(next, "id");

    int cap = 0, focus = 0;
    cJSON *season;
    cJSON_ArrayForEach(season, cJSON_GetObjectItem(j, "season_list")) {
        if (sv->n + 1 > cap) sv->rows = realloc(sv->rows, (cap = cap * 2 + 32) * sizeof *sv->rows);
        EpRow *h = &sv->rows[sv->n++];
        memset(h, 0, sizeof *h);
        h->header = 1;
        h->season = jint(season, "season");
        cJSON *ep;
        cJSON_ArrayForEach(ep, cJSON_GetObjectItem(season, "episodes")) {
            if (sv->n + 1 > cap) sv->rows = realloc(sv->rows, (cap = cap * 2 + 32) * sizeof *sv->rows);
            EpRow *r = &sv->rows[sv->n++];
            memset(r, 0, sizeof *r);
            r->season = jint(ep, "season");
            r->episode = jint(ep, "episode");
            r->id = jint(ep, "id");
            jstr(ep, "title", r->title, sizeof r->title);
            r->resume_s = jint(ep, "resume_s");
            r->runtime_s = jint(ep, "runtime_s");
            r->watched = cJSON_IsTrue(cJSON_GetObjectItem(ep, "watched"));
            if (r->id == next_id || (!focus && !next_id)) focus = sv->n - 1;
        }
    }
    cJSON_Delete(j);
    return focus;
}

static char kind_char(Kind k) { return k == KIND_MOVIE ? 'm' : 's'; }

static void want_poster(Item *m) {
    if (m->tex_state != 0) return;
    if (!m->poster[0]) {
        m->tex_state = 2;
        return;
    }
    posters_want(kind_char(m->kind), m->id, m->tmdb_id, m->poster);
    m->tex_state = 3;
}

/* prefetch_posters fills the console's poster cache in the background. */
static void prefetch_posters(Item *list, int n) {
    for (int i = 0; i < n; i++)
        if (list[i].poster[0]) posters_prefetch(kind_char(list[i].kind), list[i].id, list[i].tmdb_id, list[i].poster);
}

/* take_posters turns the posters the loader finished into textures, for
 * every card waiting for that poster on any tab. */
static void take_posters(SDL_Renderer *ren, Item **lists, int *counts) {
    char kind;
    long id;
    SDL_Surface *s;
    while (posters_take(&kind, &id, &s)) {
        if (kind == 'b' || kind == 'd') { /* a home backdrop: only the latest one wanted counts */
            if (kind == g_bd_kind && id == g_bd_id) {
                if (g_backdrop) SDL_DestroyTexture(g_backdrop);
                g_backdrop = s ? SDL_CreateTextureFromSurface(ren, s) : NULL;
            }
            if (s) SDL_FreeSurface(s);
            continue;
        }
        for (int t = 0; t < TABS; t++)
            for (int i = 0; i < counts[t]; i++) {
                Item *m = &lists[t][i];
                if (kind_char(m->kind) != kind || m->id != id || m->tex_state != 3) continue;
                m->tex = s ? SDL_CreateTextureFromSurface(ren, s) : NULL;
                m->tex_state = m->tex ? 1 : 2;
            }
        if (s) SDL_FreeSurface(s);
    }
}

/* ---------- search ----------
 * An on-screen keyboard filters the movie and show lists already on the
 * console (no server round trip); results are a grid like the others. */

#define KB_ROWS 4
static const char *kb_keys[KB_ROWS - 1] = {"ABCDEFGHIJKLM", "NOPQRSTUVWXYZ", "1234567890"};
static const char *kb_wide[3] = {"Space", "Delete", "Clear"};
static char g_query[64];
static int kb_row, kb_col, kb_focus = 1; /* kb_focus 0: the results */

static int kb_cols(int row) { return row < KB_ROWS - 1 ? (int)strlen(kb_keys[row]) : 3; }

/* contains_ci: ASCII case-insensitive substring match. */
static int contains_ci(const char *hay, const char *needle) {
    size_t n = strlen(needle);
    for (; *hay; hay++)
        if (strncasecmp(hay, needle, n) == 0) return 1;
    return n == 0;
}

/* run_search rebuilds the results tab from the movie and show lists. */
static void run_search(Item **lists, int *counts) {
    free_items(lists[TAB_SEARCH], counts[TAB_SEARCH]);
    lists[TAB_SEARCH] = NULL;
    counts[TAB_SEARCH] = 0;
    if (!g_query[0]) return;
    int cap = counts[TAB_MOVIES] + counts[TAB_SHOWS], n = 0;
    Item *out = calloc(cap ? cap : 1, sizeof *out);
    for (int t = TAB_MOVIES; t <= TAB_SHOWS; t++)
        for (int i = 0; i < counts[t]; i++)
            if (contains_ci(lists[t][i].title, g_query)) {
                out[n] = lists[t][i];
                out[n].tex = NULL; /* its own texture; the poster comes from the disk cache */
                out[n].tex_state = 0;
                n++;
            }
    lists[TAB_SEARCH] = out;
    counts[TAB_SEARCH] = n;
}

static void kb_press(Item **lists, int *counts) {
    size_t len = strlen(g_query);
    if (kb_row < KB_ROWS - 1) {
        if (len + 1 < sizeof g_query) g_query[len] = kb_keys[kb_row][kb_col], g_query[len + 1] = 0;
    } else if (kb_col == 0) {
        if (len && len + 1 < sizeof g_query && g_query[len - 1] != ' ') strcat(g_query, " ");
    } else if (kb_col == 1) {
        if (len) g_query[len - 1] = 0;
    } else {
        g_query[0] = 0;
    }
    run_search(lists, counts);
}

/* Ask the loader for the rows on screen and one row either side (the rows
 * scrolled to next); drop textures that scrolled far away once there are
 * many. Requests are served newest first, so the screen is asked for last. */
static void load_visible_posters(Item *list, int n, int first_row, int rows) {
    int lo = first_row * COLS, hi = (first_row + rows) * COLS;
    if (hi > n) hi = n;
    for (int i = hi; i < hi + COLS && i < n; i++) want_poster(&list[i]);
    for (int i = lo - COLS; i < lo; i++)
        if (i >= 0) want_poster(&list[i]);
    for (int i = hi - 1; i >= lo; i--) want_poster(&list[i]);
    int cached = 0;
    for (int i = 0; i < n; i++) cached += list[i].tex != NULL;
    if (cached > MAX_CACHED_POSTERS)
        for (int i = 0; i < n; i++)
            if (list[i].tex && (i < lo - COLS || i >= hi + COLS)) {
                SDL_DestroyTexture(list[i].tex);
                list[i].tex = NULL;
                list[i].tex_state = 0;
            }
}

/* ---------- the self-test ----------
 * FLEX_BENCH (websrv's env=) or a bench.txt in the app folder, which is
 * removed once read so the next start is a normal one:
 *   movies/60 start=300 secs=30 runs=new,old; movies/56 start=600
 * plays that title from `start` for `secs` once per run and quits; the
 * black box has the figures. A run is settings joined by +: new (as
 * shipped), old (FFmpeg's automatic decoder threads and the full scaler at
 * every size, as before the thread change), t<N> (N decoder threads),
 * buf<N> (N MB read-ahead), lanczos (the full scaler only). Nothing is
 * saved: no progress, no settings. */
#define BENCH_FILE APP_DIR "/bench.txt"

static int read_bench(char *buf, int size) {
    const char *e = getenv("FLEX_BENCH");
    if (e && *e) {
        snprintf(buf, size, "%s", e);
        return 1;
    }
    FILE *f = fopen(BENCH_FILE, "r");
    if (!f) return 0;
    int ok = fgets(buf, size, f) != NULL;
    fclose(f);
    remove(BENCH_FILE);
    buf[strcspn(buf, "\r\n")] = 0;
    return ok && *buf;
}

static void bench_play(SDL_Renderer *ren, const char *kind, long id, int start, int secs, const char *run) {
    char path[320];
    int n = snprintf(path, sizeof path, "/api/v1/%s/%ld/play?profile=ps5&caps=" PLAYER_CAPS "&start=%d&upscale=0", kind,
                     id, start);
    if (g_link_kbps > 0) snprintf(path + n, sizeof path - n, "&max_kbps=%d", g_link_kbps);
    char *body = flex_get(g_host, g_port, path);
    cJSON *info = body ? cJSON_Parse(body) : NULL;
    free(body);
    const char *rel = cJSON_GetStringValue(cJSON_GetObjectItem(info, "url"));
    const char *method = cJSON_GetStringValue(cJSON_GetObjectItem(info, "method"));
    if (!rel) {
        bb_log("bench %s: the server gave no stream for %s", run, path);
        cJSON_Delete(info);
        return;
    }
    int hls = method && strcmp(method, "hls") == 0;
    cJSON *dur = cJSON_GetObjectItem(info, "duration_s");
    char url[512];
    snprintf(url, sizeof url, "http://%s:%d%s", g_host, g_port, rel);
    bb_log("bench %s: %s via %s", run, url, method ? method : "?");
    PlayOpts o = {.url = url, .title = run, .offset = hls ? start : 0, .initial = hls ? 0 : start,
                  .duration = cJSON_IsNumber(dur) && dur->valuedouble > 0 ? dur->valuedouble : start + secs + 60,
                  .seekable = !hls, .aspect = ASPECT_AUTO, .volume = g_volume, .font = g_small, .stop_after_s = secs};
    memcpy(o.picture, g_picture, sizeof o.picture);
    player_play(ren, &o);
    if (hls) {
        char session[64] = "";
        sscanf(rel, "/api/v1/hls/%63[^/]", session);
        snprintf(path, sizeof path, "/api/v1/hls/%s", session);
        free(flex_http(g_host, g_port, "DELETE", path, NULL, NULL));
    }
    cJSON_Delete(info);
}

static void run_bench(SDL_Renderer *ren, const char *spec) {
    char kind[16] = "movies", runs[160] = "new,old";
    long id = 0;
    int start = 300, secs = 30;
    const char *s;
    while (*spec == ' ') spec++; /* "...; movies/56" */
    sscanf(spec, "%15[^/]/%ld", kind, &id);
    if ((s = strstr(spec, "start="))) start = atoi(s + 6);
    if ((s = strstr(spec, "secs="))) secs = atoi(s + 5);
    if ((s = strstr(spec, "runs="))) sscanf(s + 5, "%159s", runs);
    bb_log("bench: %s/%ld from %d s, %d s per run, runs %s", kind, id, start, secs, runs);
    char *save = NULL;
    for (char *run = strtok_r(runs, ",", &save); run; run = strtok_r(NULL, ",", &save)) {
        unsetenv("FLEX_DEC_THREADS");
        unsetenv("FLEX_SWS");
        unsetenv("FLEX_BUFFER_MB");
        char parts[64], *save2 = NULL;
        snprintf(parts, sizeof parts, "%s", run);
        for (char *t = strtok_r(parts, "+", &save2); t; t = strtok_r(NULL, "+", &save2)) {
            if (strcmp(t, "old") == 0) {
                setenv("FLEX_DEC_THREADS", "0", 1);
                setenv("FLEX_SWS", "lanczos", 1);
            } else if (strcmp(t, "lanczos") == 0) {
                setenv("FLEX_SWS", "lanczos", 1);
            } else if (t[0] == 't' && t[1] >= '0' && t[1] <= '9') {
                setenv("FLEX_DEC_THREADS", t + 1, 1);
            } else if (strncmp(t, "buf", 3) == 0) {
                setenv("FLEX_BUFFER_MB", t + 3, 1);
            }
        }
        bench_play(ren, kind, id, start, secs, run);
        SDL_Delay(3000); /* memory settles between runs */
        bb_every(0, "bench: after the run");
    }
}

/* play_item plays a movie ("movies") or an episode ("episodes"), then saves
 * the position. Direct files seek in place; a transcoded HLS session cannot
 * seek, so a seek ends it and a new session starts at the target. */
static void play_item(SDL_Renderer *ren, const char *kind, long id, const char *title, int resume_s, int watched,
                      int runtime_s) {
    double start = watched ? 0 : resume_s, position = -1, duration = runtime_s;
    char kc = kind[0] == 'm' ? 'm' : 'e';
    int aspect = get_aspect(kc, id);
    char path[256];
    /* FLEX_PLAY_PROFILE=direct (via websrv's env=) plays files as they are, for measuring the decoder */
    const char *profile = getenv("FLEX_PLAY_PROFILE") ? getenv("FLEX_PLAY_PROFILE") : "ps5";
    int no_direct = 0;  /* the file ran dry on this link: ask for a stream sized to it instead */
    int step_downs = 0; /* a conversion ran dry: ask for a smaller one (twice at most) */
    for (;;) {
        int n = snprintf(path, sizeof path, "/api/v1/%s/%ld/play?profile=%s&caps=" PLAYER_CAPS "%s&start=%d&upscale=%d",
                         kind, id, profile, no_direct ? ",nodirect" : "", (int)start, g_upscale);
        if (g_link_kbps > 0) snprintf(path + n, sizeof path - n, "&max_kbps=%d", g_link_kbps);
        char *body = flex_get(g_host, g_port, path);
        if (!body) break;
        cJSON *info = cJSON_Parse(body);
        free(body);
        if (!info) break;

        const char *rel = cJSON_GetStringValue(cJSON_GetObjectItem(info, "url"));
        const char *method = cJSON_GetStringValue(cJSON_GetObjectItem(info, "method"));
        cJSON *dur = cJSON_GetObjectItem(info, "duration_s");
        if (cJSON_IsNumber(dur) && dur->valuedouble > 0) duration = dur->valuedouble;
        int hls = method && strcmp(method, "hls") == 0;
        int upscaled = cJSON_IsTrue(cJSON_GetObjectItem(info, "upscaled"));
        char url[512];
        snprintf(url, sizeof url, "http://%s:%d%s", g_host, g_port, rel ? rel : "");
        bb_log("FLEX: play \"%s\" via %s from %.0fs: %s", title, method ? method : "?", start, url);

        PlayOpts o = {.url = url, .title = title, .offset = hls ? start : 0, .initial = hls ? 0 : start,
                      .duration = duration, .seekable = !hls, .aspect = aspect, .upscaled = upscaled,
                      .volume = g_volume, .may_give_up = hls ? step_downs < 2 : !no_direct, .font = g_small};
        memcpy(o.picture, g_picture, sizeof o.picture);
        PlayResult r = player_play(ren, &o);
        /* an original file shows the link even when it ran dry; a conversion
         * that ran dry only shows how fast the server made it */
        if (!r.starved || !hls) note_link(r.link_kbps, !hls);
        aspect = r.aspect; /* carried across session restarts */
        if (r.volume != g_volume) {
            g_volume = r.volume;
            save_int(VOLUME_FILE, g_volume);
        }
        if (memcmp(r.picture, g_picture, sizeof g_picture) != 0) {
            memcpy(g_picture, r.picture, sizeof g_picture);
            save_picture();
        }
        bb_log("FLEX: stopped at %.1fs%s", r.position, r.seek_to >= 0 ? " (seek)" : "");

        if (hls && rel) {
            /* /api/v1/hls/<session>/index.m3u8 -> DELETE /api/v1/hls/<session> */
            char session[64] = "";
            sscanf(rel, "/api/v1/hls/%63[^/]", session);
            snprintf(path, sizeof path, "/api/v1/hls/%s", session);
            free(flex_http(g_host, g_port, "DELETE", path, NULL, NULL));
        }
        cJSON_Delete(info);
        if (r.position >= 0) position = r.position;
        if (r.starved) { /* carry on from here with a stream the link can carry */
            if (hls) {
                /* the conversion came out fatter than the link (an encoder that
                 * overshoots): ask for a quarter less. Not from what the reader
                 * measured: a conversion arrives as fast as the server makes it,
                 * so that speed says nothing about the link (a starting one read
                 * at 0.9 Mbit/s on a 26 Mbit/s link). */
                int was = g_link_kbps ? g_link_kbps : 8000;
                set_link(was * 3 / 4 > 4000 ? was * 3 / 4 : 4000);
                step_downs++;
            } else {
                no_direct = 1;
            }
            start = r.position > 0 ? r.position : start;
            continue;
        }
        if (r.toggle_upscale) { /* restart where we are with the other setting */
            g_upscale = !g_upscale;
            save_int(UPSCALE_FILE, g_upscale);
            start = r.position > 0 ? r.position : start;
            continue;
        }
        if (r.seek_to < 0) break;
        start = r.seek_to;
    }
    set_aspect(kc, id, aspect);
    if (position >= 0) {
        char json[128];
        snprintf(json, sizeof json, "{\"position_s\":%d,\"duration_s\":%d}", (int)position, (int)duration);
        snprintf(path, sizeof path, "/api/v1/%s/%ld/progress", kind, id);
        free(flex_http(g_host, g_port, "PUT", path, json, NULL));
    }
}

/* ---------- drawing ---------- */

static void fill(SDL_Renderer *ren, SDL_Rect r, SDL_Color c) {
    SDL_SetRenderDrawColor(ren, c.r, c.g, c.b, c.a);
    SDL_RenderFillRect(ren, &r);
}

static int draw_text(SDL_Renderer *ren, TTF_Font *font, const char *text, int x, int y, SDL_Color c) {
    static int reported;
    if (!text[0]) return 0;
    SDL_Surface *s = TTF_RenderUTF8_Blended(font, text, c);
    if (!s) {
        if (!reported++) fprintf(stderr, "TTF_RenderUTF8_Blended: %s\n", TTF_GetError());
        return 0;
    }
    SDL_Texture *t = SDL_CreateTextureFromSurface(ren, s);
    if (!t) {
        if (!reported++) fprintf(stderr, "SDL_CreateTextureFromSurface: %s\n", SDL_GetError());
        SDL_FreeSurface(s);
        return 0;
    }
    SDL_Rect dst = {x, y, s->w, s->h};
    SDL_RenderCopy(ren, t, NULL, &dst);
    int w = s->w;
    SDL_DestroyTexture(t);
    SDL_FreeSurface(s);
    return w;
}

/* draw_text_fit truncates with an ellipsis so the text fits in max_w pixels. */
static void draw_text_fit(SDL_Renderer *ren, TTF_Font *font, const char *text, int x, int y, int max_w, SDL_Color c) {
    int w = 0, h = 0;
    if (TTF_SizeUTF8(font, text, &w, &h) == 0 && w <= max_w) {
        draw_text(ren, font, text, x, y, c);
        return;
    }
    char buf[256];
    size_t len = strlen(text);
    if (len >= sizeof buf - 4) len = sizeof buf - 4;
    while (len > 0) {
        /* back up to a UTF-8 character boundary */
        do len--; while (len > 0 && (text[len] & 0xC0) == 0x80);
        memcpy(buf, text, len);
        strcpy(buf + len, "\xE2\x80\xA6"); /* … */
        if (TTF_SizeUTF8(font, buf, &w, &h) == 0 && w <= max_w) break;
    }
    draw_text(ren, font, buf, x, y, c);
}

/* draw_wrapped renders multi-line text inside a box; centred when `center`. */
static int draw_wrapped(SDL_Renderer *ren, TTF_Font *font, const char *text, SDL_Rect box, SDL_Color c, int center) {
    if (!text[0]) return 0;
    SDL_Surface *s = TTF_RenderUTF8_Blended_Wrapped(font, text, c, box.w - (center ? 24 : 0));
    if (!s) return 0;
    int h = s->h;
    SDL_Texture *t = SDL_CreateTextureFromSurface(ren, s);
    if (t) {
        SDL_Rect dst = {box.x + (center ? (box.w - s->w) / 2 : 0), box.y + (center ? (box.h - s->h) / 2 : 0), s->w, s->h};
        if (dst.y < box.y) dst.y = box.y;
        SDL_Rect clip = box;
        SDL_RenderSetClipRect(ren, &clip);
        SDL_RenderCopy(ren, t, NULL, &dst);
        SDL_RenderSetClipRect(ren, NULL);
        SDL_DestroyTexture(t);
    }
    SDL_FreeSurface(s);
    return h;
}

static void draw_card(SDL_Renderer *ren, Item *m, int x, int y, int selected) {
    SDL_Rect card = {x, y, CARD_W, CARD_H};
    if (selected) fill(ren, (SDL_Rect){x - 6, y - 6, CARD_W + 12, CARD_H + 12}, C_ACCENT);
    if (m->tex) {
        SDL_RenderCopy(ren, m->tex, NULL, &card);
    } else {
        fill(ren, card, C_PANEL);
        draw_wrapped(ren, g_small, m->title, card, C_MUTED, 1);
    }
    if (m->kind == KIND_MOVIE || m->episode) {
        if (!m->watched && m->resume_s > 0 && m->runtime_s > 0) {
            int pct = 100 * m->resume_s / m->runtime_s;
            if (pct > 100) pct = 100;
            fill(ren, (SDL_Rect){x + 10, y + CARD_H - 16, CARD_W - 20, 6}, (SDL_Color){0, 0, 0, 160});
            fill(ren, (SDL_Rect){x + 10, y + CARD_H - 16, (CARD_W - 20) * pct / 100, 6}, C_ACCENT);
        }
    } else if (m->unwatched > 0) {
        /* unwatched count badge */
        char n[16];
        snprintf(n, sizeof n, "%d", m->unwatched);
        int w = 0, h = 0;
        TTF_SizeUTF8(g_tiny, n, &w, &h);
        fill(ren, (SDL_Rect){x + CARD_W - w - 26, y + 10, w + 16, h + 4}, C_ACCENT);
        draw_text(ren, g_tiny, n, x + CARD_W - w - 18, y + 12, C_BLACK);
    }
    draw_text_fit(ren, g_small, m->title, x, y + CARD_H + 10, CARD_W, selected ? C_FG : C_MUTED);
    char sub[160] = "";
    if (m->subtitle[0]) {
        snprintf(sub, sizeof sub, "%s", m->subtitle);
    } else if (m->episode) {
        snprintf(sub, sizeof sub, "next episode");
    } else if (m->kind == KIND_MOVIE) {
        if (m->year) snprintf(sub, sizeof sub, "%d", m->year);
        if (m->watched) snprintf(sub + strlen(sub), sizeof sub - strlen(sub), "%swatched", m->year ? "  ·  " : "");
    } else {
        snprintf(sub, sizeof sub, "%d season%s  ·  %d ep", m->seasons, m->seasons == 1 ? "" : "s", m->episodes);
    }
    if (sub[0]) draw_text_fit(ren, g_tiny, sub, x, y + CARD_H + 42, CARD_W, C_MUTED);
}

/* the search tab: query and keyboard on top, one row of results below */
#define KB_TOP GRID_TOP
#define KB_KEY 64
#define SEARCH_GRID_TOP (KB_TOP + KB_ROWS * (KB_KEY + 8) + 40)

static int rows_visible(Tab tab) { return tab == TAB_SEARCH ? 1 : ROWS_VISIBLE; }
static int grid_top(Tab tab) { return tab == TAB_SEARCH ? SEARCH_GRID_TOP : GRID_TOP; }

static void draw_keyboard(SDL_Renderer *ren, int nresults) {
    char line[128];
    snprintf(line, sizeof line, "%s%s", g_query[0] ? g_query : "Type a title", kb_focus ? "_" : "");
    draw_text_fit(ren, g_font, line, MARGIN_X, KB_TOP, 700, g_query[0] ? C_FG : C_MUTED);
    if (g_query[0]) snprintf(line, sizeof line, "%d found  ·  Down to the results", nresults);
    else snprintf(line, sizeof line, "Cross types  ·  Square deletes");
    draw_text_fit(ren, g_small, line, MARGIN_X, KB_TOP + 60, 700, C_MUTED);
    int x0 = MARGIN_X + 760;
    for (int r = 0; r < KB_ROWS; r++)
        for (int c = 0; c < kb_cols(r); c++) {
            int wide = r == KB_ROWS - 1, kw = wide ? 3 * KB_KEY + 16 : KB_KEY;
            SDL_Rect k = {x0 + c * (kw + 8), KB_TOP + r * (KB_KEY + 8), kw, KB_KEY};
            int on = kb_focus && r == kb_row && c == kb_col;
            fill(ren, k, on ? C_ACCENT : C_PANEL);
            char label[8];
            if (wide) snprintf(label, sizeof label, "%s", kb_wide[c]);
            else snprintf(label, sizeof label, "%c", kb_keys[r][c]);
            int w = 0, h = 0;
            TTF_SizeUTF8(g_small, label, &w, &h);
            draw_text(ren, g_small, label, k.x + (k.w - w) / 2, k.y + (k.h - h) / 2, on ? C_BLACK : C_FG);
        }
}

/* tab_shown: Home needs rows; Continue needs items and is a row of Home when that is there. */
static int tab_shown(Tab t, const int *counts) {
    if (t == TAB_HOME) return counts[TAB_HOME] > 0;
    if (t == TAB_CONTINUE) return counts[TAB_CONTINUE] > 0 && counts[TAB_HOME] == 0;
    return 1;
}

/* draw_header: the tabs, the server line and an offered update. */
static void draw_header(SDL_Renderer *ren, Tab tab, const char *status, const int *counts) {
    int x = MARGIN_X;
    x += draw_text(ren, g_font, "FLEX", x, 50, C_FG) + 40;
    for (int t = 0; t < TABS; t++)
        if (tab_shown((Tab)t, counts))
            x += draw_text(ren, g_font, tab_names[t], x, 50, t == (int)tab ? C_FG : C_MUTED) + 30;
    draw_text(ren, g_tiny, "L1 / R1 switch  ·  Square search", SCREEN_W - MARGIN_X - 290, 62, C_MUTED);
    draw_text(ren, g_tiny, "v" FLEX_VERSION, SCREEN_W - MARGIN_X - 290, 36, C_MUTED);
    int sw = draw_text(ren, g_small, status, MARGIN_X, 108, C_MUTED);
    if (g_update.version[0]) {
        char up[96];
        snprintf(up, sizeof up, "Update %s ready  ·  press Options to install", g_update.version);
        draw_text(ren, g_small, up, MARGIN_X + sw + 40, 108, C_ACCENT);
    }
}

/* ---------- hold Circle to close ----------
 *
 * A press of Circle on the main screens used to close FLEX at once, and so
 * did every stray press. Now it has to be held: a toast at the bottom fills
 * up while it is, letting go early cancels (the toast stays a moment so the
 * hint can be read). Circle on the episode list and in the player still goes
 * back at once. */

#define HOLD_EXIT_MS 1500
static Uint32 g_exit_since; /* when Circle went down; 0: not held */
static Uint32 g_exit_hint;  /* after an early release: keep the toast until then */

static void hold_exit_start(void) { g_exit_since = SDL_GetTicks() | 1; }

static void hold_exit_release(void) {
    if (!g_exit_since) return;
    g_exit_since = 0;
    g_exit_hint = SDL_GetTicks() + 1200;
}

/* hold_exit_done: Circle has been held long enough. */
static int hold_exit_done(void) { return g_exit_since && SDL_GetTicks() - g_exit_since >= HOLD_EXIT_MS; }

static void draw_exit_toast(SDL_Renderer *ren) {
    Uint32 now = SDL_GetTicks();
    double frac = 0;
    if (g_exit_since) frac = (double)(now - g_exit_since) / HOLD_EXIT_MS;
    else if (now >= g_exit_hint) return;
    if (frac > 1) frac = 1;
    const char *text = "Hold \u25CB to close FLEX";
    int tw = 0, th = 0;
    TTF_SizeUTF8(g_small, text, &tw, &th);
    int w = tw + 96 > 520 ? tw + 96 : 520, h = 92, x = (SCREEN_W - w) / 2, y = SCREEN_H - h - 64;
    fill(ren, (SDL_Rect){x, y, w, h}, (SDL_Color){C_PANEL.r, C_PANEL.g, C_PANEL.b, 240});
    fill(ren, (SDL_Rect){x, y, w, 2}, C_ACCENT);
    draw_text(ren, g_small, text, (SCREEN_W - tw) / 2, y + 16, C_FG);
    int bx = x + 40, bw = w - 80, by = y + h - 28;
    fill(ren, (SDL_Rect){bx, by, bw, 8}, (SDL_Color){58, 64, 78, 255});
    fill(ren, (SDL_Rect){bx, by, (int)(bw * frac), 8}, C_ACCENT);
}

/* present shows the frame, with the close toast on top while it is up. */
static void present(SDL_Renderer *ren) {
    draw_exit_toast(ren);
    SDL_RenderPresent(ren);
}

static void draw_home_card(SDL_Renderer *ren, Item *m, int x, int y, int selected) {
    SDL_Rect card = {x, y, HCARD_W, HCARD_H};
    if (selected) fill(ren, (SDL_Rect){x - 5, y - 5, HCARD_W + 10, HCARD_H + 10}, C_ACCENT);
    if (m->tex) {
        SDL_RenderCopy(ren, m->tex, NULL, &card);
    } else {
        fill(ren, card, C_PANEL);
        draw_wrapped(ren, g_tiny, m->title, card, C_MUTED, 1);
    }
    if ((m->kind == KIND_MOVIE || m->episode) && !m->watched && m->resume_s > 0 && m->runtime_s > 0) {
        int pct = 100 * m->resume_s / m->runtime_s;
        if (pct > 100) pct = 100;
        fill(ren, (SDL_Rect){x + 8, y + HCARD_H - 14, HCARD_W - 16, 5}, (SDL_Color){0, 0, 0, 160});
        fill(ren, (SDL_Rect){x + 8, y + HCARD_H - 14, (HCARD_W - 16) * pct / 100, 5}, C_ACCENT);
    }
}

/* draw_home: the focused title's backdrop and details on top, its row and
 * the next one below. */
static void draw_home(SDL_Renderer *ren, Item *list, int n, const char *status, const int *counts) {
    SDL_SetRenderDrawColor(ren, C_BG.r, C_BG.g, C_BG.b, 255);
    SDL_RenderClear(ren);
    if (g_backdrop) {
        SDL_Rect d = {BACKDROP_X, 0, 1280, 720};
        SDL_RenderCopy(ren, g_backdrop, NULL, &d);
        if (g_fade) SDL_RenderCopy(ren, g_fade, NULL, &d);
    }
    draw_header(ren, TAB_HOME, status, counts);
    Item *f = n && g_nrows ? &list[home_sel()] : NULL;
    if (f) {
        draw_text(ren, g_tiny, g_rows[g_row].title, MARGIN_X, 180, C_ACCENT);
        draw_text_fit(ren, g_big, f->title, MARGIN_X, 204, SCREEN_W - 2 * MARGIN_X, C_FG);
        char meta[256] = "";
        if (f->episode) {
            snprintf(meta, sizeof meta, "%s", f->subtitle[0] ? f->subtitle : "next episode");
        } else {
            if (f->year) snprintf(meta, sizeof meta, "%d", f->year);
            if (f->genres[0]) snprintf(meta + strlen(meta), sizeof meta - strlen(meta), "%s%s", meta[0] ? "  ·  " : "", f->genres);
            if (f->kind == KIND_SHOW && f->seasons)
                snprintf(meta + strlen(meta), sizeof meta - strlen(meta), "  ·  %d season%s", f->seasons, f->seasons == 1 ? "" : "s");
            else if (f->runtime_s)
                snprintf(meta + strlen(meta), sizeof meta - strlen(meta), "  ·  %d min", (f->runtime_s + 30) / 60);
            if (f->watched) snprintf(meta + strlen(meta), sizeof meta - strlen(meta), "  ·  watched");
        }
        draw_text_fit(ren, g_small, meta, MARGIN_X, 284, 1100, C_MUTED);
        int top = 326; /* the file's details (resolution, HDR, codec, audio), when the server sends them */
        if (f->specs[0]) {
            draw_text_fit(ren, g_tiny, f->specs, MARGIN_X, 318, 1100, C_ACCENT);
            top = 350;
        }
        draw_wrapped(ren, g_small, f->overview, (SDL_Rect){MARGIN_X, top, 900, 476 - top}, C_FG, 0);
    }
    for (int k = 0; k < 2 && g_row + k < g_nrows; k++) {
        HomeRow *r = &g_rows[g_row + k];
        int y = HOME_TOP + k * HROW_H;
        draw_text(ren, g_small, r->title, MARGIN_X, y, k == 0 ? C_FG : C_MUTED);
        for (int c = r->first; c < r->n && c <= r->first + HCOLS; c++)
            draw_home_card(ren, &list[r->start + c], MARGIN_X + (c - r->first) * (HCARD_W + HGAP), y + 40,
                           k == 0 && c == r->col);
    }
    if (g_row + 2 < g_nrows || g_row > 0) {
        char pos[48];
        snprintf(pos, sizeof pos, "row %d of %d", g_row + 1, g_nrows);
        draw_text(ren, g_tiny, pos, SCREEN_W - MARGIN_X - 120, HOME_TOP + 4, C_MUTED);
    }
    present(ren);
}

static void draw_grid(SDL_Renderer *ren, Tab tab, Item *list, int n, int sel, int first_row, const char *status,
                      const int *counts) {
    SDL_SetRenderDrawColor(ren, C_BG.r, C_BG.g, C_BG.b, 255);
    SDL_RenderClear(ren);
    draw_header(ren, tab, status, counts);
    if (tab == TAB_SEARCH) draw_keyboard(ren, n);

    int rv = rows_visible(tab), top = grid_top(tab), focus = tab != TAB_SEARCH || !kb_focus;
    int lo = first_row * COLS, hi = (first_row + rv) * COLS;
    for (int i = lo; i < n && i < hi; i++) {
        int col = i % COLS, row = i / COLS - first_row;
        draw_card(ren, &list[i], MARGIN_X + col * (CARD_W + GAP_X), top + row * ROW_H, focus && i == sel);
    }
    const char *empty[TABS] = {"", "Nothing to continue.", "No movies. Add folders on the PC and scan.",
                               "No TV shows found.", g_query[0] ? "Nothing matches." : ""};
    if (n == 0 && empty[tab][0]) draw_text(ren, g_font, empty[tab], MARGIN_X, top, C_MUTED);

    int rows = (n + COLS - 1) / COLS;
    if (rows > rv) {
        char pos[64];
        snprintf(pos, sizeof pos, "rows %d-%d of %d", first_row + 1, first_row + rv < rows ? first_row + rv : rows,
                 rows);
        draw_text(ren, g_tiny, pos, SCREEN_W - MARGIN_X - 200, top - 54, C_MUTED);
    }
    present(ren);
}

static void draw_episodes(SDL_Renderer *ren, ShowView *sv, Item *show, int sel, int first) {
    SDL_SetRenderDrawColor(ren, C_BG.r, C_BG.g, C_BG.b, 255);
    SDL_RenderClear(ren);
    int tx = MARGIN_X;
    if (show && show->tex) {
        SDL_Rect p = {MARGIN_X, 50, 150, 225};
        SDL_RenderCopy(ren, show->tex, NULL, &p);
        tx += 150 + 30;
    }
    draw_text(ren, g_font, sv->title, tx, 50, C_FG);
    char meta[160];
    snprintf(meta, sizeof meta, "%d season%s  ·  %d episodes  ·  %d unwatched        Circle: back", sv->seasons,
             sv->seasons == 1 ? "" : "s", sv->episodes, sv->unwatched);
    draw_text(ren, g_small, meta, tx, 104, C_MUTED);
    draw_wrapped(ren, g_tiny, sv->overview, (SDL_Rect){tx, 140, SCREEN_W - tx - MARGIN_X, 130}, C_MUTED, 0);

    for (int i = first; i < sv->n && i < first + EP_ROWS_VISIBLE; i++) {
        EpRow *r = &sv->rows[i];
        int y = EP_TOP + (i - first) * EP_ROW_H;
        if (r->header) {
            char h[32];
            snprintf(h, sizeof h, r->season == 0 ? "SPECIALS" : "SEASON %d", r->season);
            draw_text(ren, g_tiny, h, MARGIN_X, y + 18, C_MUTED);
            continue;
        }
        if (i == sel) fill(ren, (SDL_Rect){MARGIN_X - 10, y, SCREEN_W - 2 * MARGIN_X + 20, EP_ROW_H - 4}, C_ACCENT);
        SDL_Color fg = i == sel ? C_BLACK : C_FG, mu = i == sel ? C_BLACK : C_MUTED;
        char num[16];
        snprintf(num, sizeof num, "S%02dE%02d", r->season, r->episode);
        draw_text(ren, g_small, num, MARGIN_X, y + 12, mu);
        draw_text_fit(ren, g_small, r->title, MARGIN_X + 130, y + 12, SCREEN_W - 2 * MARGIN_X - 330, fg);
        char state[32] = "";
        if (r->watched) snprintf(state, sizeof state, "watched");
        else if (r->resume_s > 0 && r->runtime_s > 0) snprintf(state, sizeof state, "%d%%", 100 * r->resume_s / r->runtime_s);
        else if (r->runtime_s > 0) snprintf(state, sizeof state, "%d min", (r->runtime_s + 30) / 60);
        int w = 0, hh = 0;
        TTF_SizeUTF8(g_small, state, &w, &hh);
        draw_text(ren, g_small, state, SCREEN_W - MARGIN_X - w, y + 12, r->watched && i != sel ? C_ACCENT : mu);
    }
    present(ren);
}

/* ---------- updates ----------
 *
 * The server keeps the newest PS5 build of the latest FLEX release (it
 * fetches it from GitHub; this HTTP client has no TLS) and describes it at
 * /api/v1/client/ps5. Options downloads it next to eboot.elf, checks its
 * SHA-256, swaps it in (the old one stays as eboot.elf.bak) and asks websrv
 * to start it, which also ends this copy. */

/* version_parse reads "1.2.3" (or "1.2"); 0 for anything else, e.g. a dev build. */
static int version_parse(const char *v, int out[3]) {
    out[0] = out[1] = out[2] = 0;
    if (*v == 'v') v++;
    for (int i = 0; i < 3; i++) {
        char *end;
        long n = strtol(v, &end, 10);
        if (end == v || n < 0) return 0;
        out[i] = (int)n;
        if (*end == 0) return 1;
        if (*end != '.' || i == 2) return 0;
        v = end + 1;
    }
    return 0;
}

/* version_newer: a is a release newer than b (any release beats a dev build). */
static int version_newer(const char *a, const char *b) {
    int va[3], vb[3];
    if (!version_parse(a, va)) return 0;
    if (!version_parse(b, vb)) return 1;
    for (int i = 0; i < 3; i++)
        if (va[i] != vb[i]) return va[i] > vb[i];
    return 0;
}

static void check_update(void) {
    memset(&g_update, 0, sizeof g_update);
    char *body = flex_get(g_host, g_port, "/api/v1/client/ps5"); /* 404: no build, or an older server */
    if (!body) return;
    cJSON *j = cJSON_Parse(body);
    free(body);
    char version[32];
    jstr(j, "version", version, sizeof version);
#ifdef FLEX_APP
    /* the app updates from the app build's files: an older server, or a
     * release without them, offers nothing */
    cJSON *file, *files = cJSON_GetObjectItem(cJSON_GetObjectItem(j, "app"), "files");
    cJSON_ArrayForEach(file, files) {
        char path[64];
        jstr(file, "path", path, sizeof path);
        /* sce_sys is copied to the home screen when the tile is installed;
         * a new one there would change nothing until it is reinstalled */
        if (!path[0] || strncmp(path, "sce_sys/", 8) == 0 || strstr(path, "..") ||
            g_update.nfiles == (int)(sizeof g_update.files / sizeof g_update.files[0]))
            continue;
        typeof(g_update.files[0]) *f = &g_update.files[g_update.nfiles++];
        snprintf(f->path, sizeof f->path, "%s", path);
        jstr(file, "sha256", f->sha256, sizeof f->sha256);
        jstr(file, "url", f->url, sizeof f->url);
        cJSON *size = cJSON_GetObjectItem(file, "size");
        f->size = cJSON_IsNumber(size) ? (long)size->valuedouble : 0;
    }
    if (!g_update.nfiles) version[0] = 0;
#endif
    if (version_newer(version, FLEX_VERSION)) {
        snprintf(g_update.version, sizeof g_update.version, "%s", version);
        jstr(j, "sha256", g_update.sha256, sizeof g_update.sha256);
        jstr(j, "url", g_update.url, sizeof g_update.url);
        cJSON *size = cJSON_GetObjectItem(j, "size");
        g_update.size = cJSON_IsNumber(size) ? (long)size->valuedouble : 0;
        fprintf(stderr, "FLEX: update %s available (running %s)\n", g_update.version, FLEX_VERSION);
    }
    cJSON_Delete(j);
}

/* draw_message fills the screen with one or two centred lines and a bar
 * (frac < 0: no bar). */
/* test_pattern (FLEX_TEST_PATTERN set): labelled colour bars, each drawn
 * twice, with a plain fill (as the menus are) and from an ARGB8888 texture
 * (as video frames are), to see on the TV what the output path does to
 * colours. Waits for a button. */
static void test_pattern(SDL_Renderer *ren) {
    static const struct {
        const char *name;
        Uint8 r, g, b;
    } bars[] = {{"Red", 255, 0, 0},      {"Green", 0, 255, 0},     {"Blue", 0, 0, 255},     {"Yellow", 255, 255, 0},
                {"Cyan", 0, 255, 255},   {"Magenta", 255, 0, 255}, {"Pink", 255, 105, 180}, {"Skin", 224, 172, 105},
                {"Orange", 255, 128, 0}, {"Grey", 128, 128, 128},  {"White", 255, 255, 255}};
    int n = sizeof bars / sizeof bars[0], bw = (SCREEN_W - 2 * MARGIN_X) / n;
    SDL_SetRenderDrawColor(ren, 0, 0, 0, 255);
    SDL_RenderClear(ren);
    draw_text(ren, g_font, "FLEX colour test", MARGIN_X, 60, C_FG);
    draw_text(ren, g_small, "Top row: plain fill (menus)   ·   Bottom row: texture (video)   ·   any button continues",
              MARGIN_X, 120, C_MUTED);
    Uint32 *px = malloc(sizeof(Uint32) * bw * 300);
    for (int i = 0; i < n; i++) {
        int x = MARGIN_X + i * bw;
        fill(ren, (SDL_Rect){x, 200, bw - 10, 300}, (SDL_Color){bars[i].r, bars[i].g, bars[i].b, 255});
        SDL_Texture *t = SDL_CreateTexture(ren, SDL_PIXELFORMAT_ARGB8888, SDL_TEXTUREACCESS_STREAMING, bw - 10, 300);
        for (int k = 0; px && k < (bw - 10) * 300; k++)
            px[k] = 0xFF000000u | (Uint32)bars[i].r << 16 | (Uint32)bars[i].g << 8 | bars[i].b;
        if (t && px) {
            SDL_UpdateTexture(t, NULL, px, (bw - 10) * 4);
            SDL_RenderCopy(ren, t, NULL, &(SDL_Rect){x, 540, bw - 10, 300});
        }
        if (t) SDL_DestroyTexture(t);
        draw_text(ren, g_tiny, bars[i].name, x, 860, C_FG);
    }
    free(px);
    SDL_RenderPresent(ren);
    fprintf(stderr, "FLEX: showing the colour test pattern\n");
    SDL_Delay(500);
    for (SDL_Event e;;)
        if (SDL_WaitEventTimeout(&e, 100) && (e.type == SDL_CONTROLLERBUTTONDOWN || e.type == SDL_KEYDOWN || e.type == SDL_QUIT))
            break;
}

static void draw_message(SDL_Renderer *ren, const char *line1, const char *line2, double frac) {
    SDL_SetRenderDrawColor(ren, C_BG.r, C_BG.g, C_BG.b, 255);
    SDL_RenderClear(ren);
    int w = 0, h = 0;
    TTF_SizeUTF8(g_font, line1, &w, &h);
    draw_text(ren, g_font, line1, (SCREEN_W - w) / 2, SCREEN_H / 2 - 80, C_FG);
    if (line2 && line2[0]) {
        TTF_SizeUTF8(g_small, line2, &w, &h);
        draw_text(ren, g_small, line2, (SCREEN_W - w) / 2, SCREEN_H / 2 - 20, C_MUTED);
    }
    if (frac >= 0) {
        int bw = 800, x = (SCREEN_W - bw) / 2, y = SCREEN_H / 2 + 40;
        fill(ren, (SDL_Rect){x, y, bw, 10}, C_PANEL);
        fill(ren, (SDL_Rect){x, y, (int)(bw * (frac > 1 ? 1 : frac)), 10}, C_ACCENT);
    }
    SDL_RenderPresent(ren);
}

static void download_progress(long done, long total, void *arg) {
    static Uint32 next;
    if (SDL_GetTicks() < next) return; /* redraw ~10x a second */
    next = SDL_GetTicks() + 100;
    char line[96];
    snprintf(line, sizeof line, "%.1f of %.1f MB", done / 1048576.0, total / 1048576.0);
    draw_message(arg, "Downloading the update…", line, total > 0 ? (double)done / total : 0);
    SDL_PumpEvents(); /* keep the system from thinking the app hung */
}

static int file_sha256(const char *path, char hex[65]) {
    FILE *f = fopen(path, "rb");
    struct AVSHA *sha = av_sha_alloc();
    if (!f || !sha || av_sha_init(sha, 256) < 0) {
        if (f) fclose(f);
        av_free(sha);
        return -1;
    }
    unsigned char buf[1 << 16], digest[32];
    size_t n;
    while ((n = fread(buf, 1, sizeof buf, f)) > 0) av_sha_update(sha, buf, n);
    fclose(f);
    av_sha_final(sha, digest);
    av_free(sha);
    for (int i = 0; i < 32; i++) snprintf(hex + 2 * i, 3, "%02x", digest[i]);
    return 0;
}

/* show_notice explains how the update ended and waits for a button. */
static void show_notice(SDL_Renderer *ren, const char *title, const char *why) {
    fprintf(stderr, "FLEX: update: %s: %s\n", title, why);
    draw_message(ren, title, why, -1);
    SDL_Delay(500);
    for (SDL_Event e;;) {
        if (SDL_WaitEventTimeout(&e, 100) && (e.type == SDL_CONTROLLERBUTTONDOWN || e.type == SDL_KEYDOWN ||
                                              e.type == SDL_QUIT))
            break;
    }
}

#ifdef FLEX_APP
/* The app's folder as Shadow Mount+ installed it; the app sees it as
 * /app0 too, but only read-only. */
#define APP_HOME "/data/homebrew/" FLEX_APP_ID

/* install_update (app): fetch each file of the new build that differs from
 * the app's own into <file>.new, check them all, then swap them in (the old
 * ones stay as .bak). The running eboot.bin can be replaced: the console
 * keeps the old one for this run. FLEX then closes, to be opened again. */
static void install_update(SDL_Renderer *ren, int *quit) {
    const char *failed = "The update did not install";
    char path[192], tmp[200], bak[200], hex[65];
    int todo[16], n = 0;
    FILE *own = fopen(APP_HOME "/eboot.bin", "rb");
    if (!own) {
        show_notice(ren, failed,
                    "FLEX isn't in " APP_HOME ". Copy the new folder from FLEX-ps5-app.zip there instead.");
        return;
    }
    fclose(own);
    fprintf(stderr, "FLEX: installing %s (app, %d files)\n", g_update.version, g_update.nfiles);
    for (int i = 0; i < g_update.nfiles; i++) {
        snprintf(path, sizeof path, APP_HOME "/%s", g_update.files[i].path);
        if (file_sha256(path, hex) == 0 && strcmp(hex, g_update.files[i].sha256) == 0) continue;
        todo[n++] = i;
    }
    for (int k = 0; k < n; k++) {
        typeof(g_update.files[0]) *f = &g_update.files[todo[k]];
        snprintf(tmp, sizeof tmp, APP_HOME "/%s.new", f->path);
        char *slash = strrchr(tmp, '/');
        if (slash && slash > tmp + sizeof APP_HOME) { /* a folder the old build didn't have */
            *slash = 0;
            mkdir(tmp, 0777);
            *slash = '/';
        }
        draw_message(ren, "Downloading the update…", f->path, 0);
        long got = flex_download(g_host, g_port, f->url, tmp, download_progress, ren);
        if (got < 0 || (f->size && got != f->size) || file_sha256(tmp, hex) != 0 || strcmp(hex, f->sha256) != 0) {
            for (int j = 0; j <= k; j++) {
                snprintf(tmp, sizeof tmp, APP_HOME "/%s.new", g_update.files[todo[j]].path);
                remove(tmp);
            }
            show_notice(ren, failed, got < 0 ? "The download from the server was cut off. Try again with Options."
                                             : "A downloaded file is damaged (checksum mismatch). Try again with Options.");
            return;
        }
    }
    for (int k = 0; k < n; k++) {
        const char *rel = g_update.files[todo[k]].path;
        snprintf(path, sizeof path, APP_HOME "/%s", rel);
        snprintf(tmp, sizeof tmp, "%s.new", path);
        snprintf(bak, sizeof bak, "%s.bak", path);
        remove(bak);
        int had = rename(path, bak) == 0;
        if (rename(tmp, path) != 0) {
            if (had) rename(bak, path);
            for (int j = 0; j < k; j++) { /* put back the ones already swapped */
                snprintf(path, sizeof path, APP_HOME "/%s", g_update.files[todo[j]].path);
                snprintf(bak, sizeof bak, "%s.bak", path);
                rename(bak, path);
            }
            for (int j = k; j < n; j++) {
                snprintf(tmp, sizeof tmp, APP_HOME "/%s.new", g_update.files[todo[j]].path);
                remove(tmp);
            }
            show_notice(ren, failed, "Could not replace the app's files in " APP_HOME ".");
            return;
        }
    }
    char line[128];
    snprintf(line, sizeof line, "Version %s is installed. FLEX closes now: open it again from its tile.",
             g_update.version);
    bb_log("FLEX: updated to %s (%d of %d files changed)", g_update.version, n, g_update.nfiles);
    show_notice(ren, "Update installed", line);
    *quit = 1;
}
#else
/* install_update returns only if the update failed or websrv could not
 * restart FLEX; *quit is set when the new version is installed. */
static void install_update(SDL_Renderer *ren, int *quit) {
    const char *tmp = EBOOT ".new", *bak = EBOOT ".bak";
    char hex[65];
    fprintf(stderr, "FLEX: installing %s from %s\n", g_update.version, g_update.url);
    draw_message(ren, "Downloading the update…", "", 0);
    long n = flex_download(g_host, g_port, g_update.url, tmp, download_progress, ren);
    const char *failed = "The update did not install";
    if (n < 0) {
        show_notice(ren, failed, "The download from the server was cut off. Try again with Options.");
        return;
    }
    if ((g_update.size && n != g_update.size) || file_sha256(tmp, hex) != 0 || strcmp(hex, g_update.sha256) != 0) {
        remove(tmp);
        show_notice(ren, failed, "The downloaded file is damaged (checksum mismatch). Try again with Options.");
        return;
    }
    remove(bak);
    if (rename(EBOOT, bak) != 0 || rename(tmp, EBOOT) != 0) {
        rename(bak, EBOOT); /* put the running version back */
        remove(tmp);
        show_notice(ren, failed, "Could not replace " EBOOT ".");
        return;
    }
    char line[96];
    snprintf(line, sizeof line, "Version %s is installed. Restarting FLEX…", g_update.version);
    draw_message(ren, "Update installed", line, -1);
    fprintf(stderr, "FLEX: updated to %s, asking websrv to restart it\n", g_update.version);
    fflush(stderr);
    /* websrv ends this app, then starts the new one; if it answers instead,
     * it isn't running or refused */
    free(flex_get("127.0.0.1", 8080, "/hbldr?path=" EBOOT "&pipe=0&daemon=0"));
    SDL_Delay(4000);
    show_notice(ren, "Update installed", "FLEX could not restart itself. Start it again from the Homebrew Launcher.");
    *quit = 1;
}
#endif

/* ---------- navigation ---------- */

static void move_sel(int *sel, int *first_row, int n, int button, int rows) {
    if (n == 0) return;
    int s = *sel;
    switch (button) {
    case SDL_CONTROLLER_BUTTON_DPAD_LEFT: if (s > 0) s--; break;
    case SDL_CONTROLLER_BUTTON_DPAD_RIGHT: if (s + 1 < n) s++; break;
    case SDL_CONTROLLER_BUTTON_DPAD_UP: if (s - COLS >= 0) s -= COLS; break;
    case SDL_CONTROLLER_BUTTON_DPAD_DOWN:
        if (s + COLS < n) s += COLS;
        else if (s / COLS < (n - 1) / COLS) s = n - 1;
        break;
    }
    *sel = s;
    int row = s / COLS;
    if (row < *first_row) *first_row = row;
    if (row >= *first_row + rows) *first_row = row - rows + 1;
}

/* move_kb moves the keyboard cursor; returns 0 when Down leaves the keyboard. */
static int move_kb(int button) {
    switch (button) {
    case SDL_CONTROLLER_BUTTON_DPAD_LEFT: kb_col = kb_col > 0 ? kb_col - 1 : kb_cols(kb_row) - 1; break;
    case SDL_CONTROLLER_BUTTON_DPAD_RIGHT: kb_col = (kb_col + 1) % kb_cols(kb_row); break;
    case SDL_CONTROLLER_BUTTON_DPAD_UP:
        if (kb_row > 0) kb_row--;
        break;
    case SDL_CONTROLLER_BUTTON_DPAD_DOWN:
        if (kb_row == KB_ROWS - 1) return 0;
        kb_row++;
        break;
    }
    /* the wide bottom keys: map the column across */
    if (kb_row == KB_ROWS - 1 && kb_col > 2) kb_col = kb_col * 3 / 13;
    else if (kb_col >= kb_cols(kb_row)) kb_col = kb_cols(kb_row) - 1;
    return 1;
}

/* next_tab steps L1/R1 through the tabs that are shown. */
static Tab next_tab(Tab t, int dir, const int *counts) {
    do t = (Tab)((t + dir + TABS) % TABS);
    while (!tab_shown(t, counts));
    return t;
}

/* move_ep steps over season headers and keeps the selection on screen. */
static void move_ep(ShowView *sv, int *sel, int *first, int dir) {
    int i = *sel;
    do i += dir; while (i >= 0 && i < sv->n && sv->rows[i].header);
    if (i < 0 || i >= sv->n) return;
    *sel = i;
    if (i < *first + 1) *first = i > 0 && sv->rows[i - 1].header ? i - 1 : i;
    if (i >= *first + EP_ROWS_VISIBLE) *first = i - EP_ROWS_VISIBLE + 1;
    if (*first < 0) *first = 0;
}

static int is_dpad(int b) {
    return b == SDL_CONTROLLER_BUTTON_DPAD_LEFT || b == SDL_CONTROLLER_BUTTON_DPAD_RIGHT ||
           b == SDL_CONTROLLER_BUTTON_DPAD_UP || b == SDL_CONTROLLER_BUTTON_DPAD_DOWN;
}

static int key_to_button(SDL_Keycode k) {
    switch (k) {
    case SDLK_UP: return SDL_CONTROLLER_BUTTON_DPAD_UP;
    case SDLK_DOWN: return SDL_CONTROLLER_BUTTON_DPAD_DOWN;
    case SDLK_LEFT: return SDL_CONTROLLER_BUTTON_DPAD_LEFT;
    case SDLK_RIGHT: return SDL_CONTROLLER_BUTTON_DPAD_RIGHT;
    case SDLK_RETURN: return SDL_CONTROLLER_BUTTON_A;
    case SDLK_ESCAPE: return SDL_CONTROLLER_BUTTON_B;
    case SDLK_r: return SDL_CONTROLLER_BUTTON_Y;
    case SDLK_o: return SDL_CONTROLLER_BUTTON_START;
    case SDLK_BACKSPACE: return SDL_CONTROLLER_BUTTON_X;
    case SDLK_q: return SDL_CONTROLLER_BUTTON_LEFTSHOULDER;
    case SDLK_e: return SDL_CONTROLLER_BUTTON_RIGHTSHOULDER;
    }
    return -1;
}

int main(int argc, char **argv) {
    (void)argc;
    (void)argv;
    /* stdout/stderr may be a pipe back to the dev machine (websrv pipe=1);
     * when that side hangs up we must not die on the next log line */
    signal(SIGPIPE, SIG_IGN);
#ifdef FLEX_APP
    mkdir(APP_DIR, 0777); /* settings, cache and the black box, shared with the Homebrew Launcher version */
#endif
    /* One FLEX at a time: two fighting over the screen froze the whole
     * console. The lock goes with the process, however it ends; after an
     * update websrv is still ending the old copy as this one starts, so it
     * gets 10 s to go. Nothing has touched the screen yet. */
    int lock = open(APP_DIR "/running.lock", O_RDWR | O_CREAT, 0666), tries = 0;
    while (lock >= 0 && flock(lock, LOCK_EX | LOCK_NB) != 0 && ++tries <= 20) usleep(500 * 1000);
    if (tries > 20) {
        fprintf(stderr, "FLEX: another FLEX is already running; this one quits\n");
        return 0;
    }
#ifdef FLEX_APP
    bb_open(FLEX_VERSION " (app)");
    {
        char unbound[512];
        app_unbound(unbound, sizeof unbound);
        bb_log("FLEX: system functions the app loader left unbound: %s", unbound[0] ? unbound : "none");
        bb_log("FLEX: %s", app_heap_desc());
    }
#else
    bb_open(FLEX_VERSION);
#endif
    if (SDL_Init(SDL_INIT_VIDEO | SDL_INIT_AUDIO | SDL_INIT_GAMECONTROLLER) != 0) {
        bb_log("FLEX: SDL_Init failed: %s", SDL_GetError());
        return 1;
    }
    if (TTF_Init() != 0) fprintf(stderr, "TTF_Init: %s\n", TTF_GetError());
    if (!(IMG_Init(IMG_INIT_JPG | IMG_INIT_PNG) & IMG_INIT_JPG)) fprintf(stderr, "IMG_Init: %s\n", IMG_GetError());

    SDL_DisplayMode dm;
    bb_log("FLEX: video driver %s, %d display(s)", SDL_GetCurrentVideoDriver(), SDL_GetNumVideoDisplays());
    if (SDL_GetDesktopDisplayMode(0, &dm) == 0) fprintf(stderr, "FLEX: desktop mode %dx%d @%dHz\n", dm.w, dm.h, dm.refresh_rate);
    for (int i = 0, n = SDL_GetNumDisplayModes(0); i < n; i++)
        if (SDL_GetDisplayMode(0, i, &dm) == 0) fprintf(stderr, "FLEX: display mode %d: %dx%d @%dHz\n", i, dm.w, dm.h, dm.refresh_rate);
    bb_log("FLEX: %d CPU threads (SDL sees %d); decoding uses 3-8 of them by picture size", av_cpu_count(),
           SDL_GetCPUCount());

    /* Same setup as ps5-payload-dev/jtplay, which is known to display on the
     * console: a plain 1920x1080 window and SDL's software renderer (the
     * only render driver this SDL port has). */
    SDL_Window *win = SDL_CreateWindow("FLEX", SDL_WINDOWPOS_CENTERED, SDL_WINDOWPOS_CENTERED, SCREEN_W, SCREEN_H, 0);
    if (!win) {
        bb_log("FLEX: SDL_CreateWindow failed: %s", SDL_GetError());
        return 1;
    }
    SDL_SetHint(SDL_HINT_RENDER_DRIVER, "software");
    SDL_SetHint(SDL_HINT_RENDER_SCALE_QUALITY, "linear");
    SDL_Renderer *ren = SDL_CreateRenderer(win, -1, SDL_RENDERER_SOFTWARE | SDL_RENDERER_PRESENTVSYNC);
    if (!ren) {
        bb_log("FLEX: SDL_CreateRenderer failed: %s", SDL_GetError());
        return 1;
    }
    bb_log("FLEX: window and renderer ready");
    SDL_SetRenderDrawBlendMode(ren, SDL_BLENDMODE_BLEND);
    for (int i = 0; i < SDL_NumJoysticks(); i++)
        if (SDL_IsGameController(i)) {
            SDL_GameController *gc = SDL_GameControllerOpen(i);
            fprintf(stderr, "FLEX: controller %d: %s\n", i, gc ? SDL_GameControllerName(gc) : SDL_GetError());
        }

    g_big = TTF_OpenFont(FONT_PATH, 56);
    g_font = TTF_OpenFont(FONT_PATH, 36);
    g_small = TTF_OpenFont(FONT_PATH, 22);
    g_tiny = TTF_OpenFont(FONT_PATH, 18);
#ifdef FLEX_APP
#define LOCAL_FONT "/app0/assets/DejaVuSans.ttf" /* inside the app's own folder */
#else
#define LOCAL_FONT "assets/DejaVuSans.ttf"
#endif
    if (!g_big) g_big = TTF_OpenFont(LOCAL_FONT, 56);
    if (!g_font) g_font = TTF_OpenFont(LOCAL_FONT, 36);
    if (!g_small) g_small = TTF_OpenFont(LOCAL_FONT, 22);
    if (!g_tiny) g_tiny = TTF_OpenFont(LOCAL_FONT, 18);
    if (!g_big || !g_font || !g_small || !g_tiny) {
        bb_log("FLEX: font missing: put DejaVuSans.ttf in " APP_DIR "/assets (%s)", TTF_GetError());
        return 1;
    }

    posters_init();
    load_aspects();
    load_int(UPSCALE_FILE, &g_upscale);
    load_int(VOLUME_FILE, &g_volume);
    load_picture();
    load_int(LINK_FILE, &g_link_kbps);
    if (getenv("FLEX_TEST_PATTERN")) test_pattern(ren);
    g_fade = make_fade(ren);
    char status[256] = "Looking for a FLEX server…";
    int none[TABS] = {0};
    draw_grid(ren, TAB_MOVIES, NULL, 0, 0, 0, status, none);
    fprintf(stderr, "FLEX: first frame presented\n");

    Item *lists[TABS] = {NULL};
    int counts[TABS] = {0}, sels[TABS] = {0}, firsts[TABS] = {0};
    Tab tab = TAB_MOVIES;
    int quit = 0;
    ShowView sv = {0};
    int in_show = 0, ep_sel = 0, ep_first = 0;

    int found = find_server() == 0;
    char bench[256];
    if (found && read_bench(bench, sizeof bench)) {
        char *save = NULL; /* several titles: separated by ; */
        for (char *one = strtok_r(bench, ";", &save); one; one = strtok_r(NULL, ";", &save)) run_bench(ren, one);
        bb_log("FLEX: self-test done, quitting");
        quit = 1;
    } else if (found) {
        bb_log("FLEX: server %s:%d%s (tailscale %s)", g_host, g_port, g_is_remote ? " via tailscale" : "",
               g_remote[0] ? g_remote : "unknown");
        lists[TAB_MOVIES] = fetch_items(KIND_MOVIE, &counts[TAB_MOVIES]);
        lists[TAB_SHOWS] = fetch_items(KIND_SHOW, &counts[TAB_SHOWS]);
        lists[TAB_CONTINUE] = fetch_continue(&counts[TAB_CONTINUE]);
        lists[TAB_HOME] = fetch_home(&counts[TAB_HOME]);
        fprintf(stderr, "FLEX: %d movies, %d shows, %d to continue, %d home rows\n", counts[TAB_MOVIES],
                counts[TAB_SHOWS], counts[TAB_CONTINUE], g_nrows);
        set_status(status, sizeof status, counts[TAB_MOVIES], counts[TAB_SHOWS]);
        if (counts[TAB_HOME]) tab = TAB_HOME;
        else if (counts[TAB_CONTINUE]) tab = TAB_CONTINUE;
        posters_server(g_host, g_port);
        prefetch_posters(lists[TAB_MOVIES], counts[TAB_MOVIES]);
        prefetch_posters(lists[TAB_SHOWS], counts[TAB_SHOWS]);
        check_update();
    } else {
        fprintf(stderr, "FLEX: no server found (discovery timed out, saved addresses unreachable)\n");
        snprintf(status, sizeof status, "No server found. Start FLEX on your PC (same network); Triangle retries.");
    }

    /* D-pad auto-repeat while held */
    int held = -1;
    Uint32 next_repeat = 0;

    while (!quit) {
        bb_every(5000, "grid");
        Item *list = lists[tab];
        int n = counts[tab], *sel = &sels[tab], *first_row = &firsts[tab];
        int typing = tab == TAB_SEARCH && kb_focus;

        take_posters(ren, lists, counts);
        if (in_show) {
            draw_episodes(ren, &sv, list && *sel < n ? &list[*sel] : NULL, ep_sel, ep_first);
        } else if (tab == TAB_HOME) {
            load_home_posters(list, n);
            want_backdrop(n ? &list[home_sel()] : NULL);
            draw_home(ren, list, n, status, counts);
        } else {
            load_visible_posters(list, n, *first_row, rows_visible(tab));
            draw_grid(ren, tab, list, n, *sel, *first_row, status, counts);
        }

        SDL_Event e;
        while (SDL_PollEvent(&e)) {
            int button = -1;
            if (e.type == SDL_QUIT) quit = 1;
            if (e.type == SDL_CONTROLLERBUTTONDOWN) button = e.cbutton.button;
            if (e.type == SDL_KEYDOWN && !e.key.repeat) button = key_to_button(e.key.keysym.sym);
            if (e.type == SDL_CONTROLLERBUTTONUP && e.cbutton.button == held) held = -1;
            if (e.type == SDL_KEYUP && key_to_button(e.key.keysym.sym) == held) held = -1;
            if ((e.type == SDL_CONTROLLERBUTTONUP && e.cbutton.button == SDL_CONTROLLER_BUTTON_B) ||
                (e.type == SDL_KEYUP && key_to_button(e.key.keysym.sym) == SDL_CONTROLLER_BUTTON_B))
                hold_exit_release();
            if (button < 0) continue;

            if (in_show) {
                switch (button) {
                case SDL_CONTROLLER_BUTTON_DPAD_UP: move_ep(&sv, &ep_sel, &ep_first, -1); held = button; next_repeat = SDL_GetTicks() + 350; break;
                case SDL_CONTROLLER_BUTTON_DPAD_DOWN: move_ep(&sv, &ep_sel, &ep_first, 1); held = button; next_repeat = SDL_GetTicks() + 350; break;
                case SDL_CONTROLLER_BUTTON_B:
                    in_show = 0;
                    held = -1;
                    refresh_items(KIND_SHOW, &lists[TAB_SHOWS], &counts[TAB_SHOWS]);
                    refresh_continue(&lists[TAB_CONTINUE], &counts[TAB_CONTINUE]);
                    if (tab == TAB_HOME) {
                        refresh_home(&lists[TAB_HOME], &counts[TAB_HOME]);
                        sels[TAB_HOME] = home_sel();
                    }
                    break;
                case SDL_CONTROLLER_BUTTON_A:
                    if (ep_sel < sv.n && !sv.rows[ep_sel].header) {
                        EpRow r = sv.rows[ep_sel];
                        char title[260];
                        snprintf(title, sizeof title, "%s S%02dE%02d %s", sv.title, r.season, r.episode, r.title);
                        play_item(ren, "episodes", r.id, title, r.resume_s, r.watched, r.runtime_s);
                        held = -1;
                        long keep = r.id;
                        fetch_show(sv.id, &sv);
                        for (int i = 0; i < sv.n; i++)
                            if (sv.rows[i].id == keep) ep_sel = i;
                    }
                    break;
                }
                continue;
            }

            if (is_dpad(button)) {
                if (tab == TAB_HOME) {
                    move_home(button);
                    *sel = home_sel();
                } else if (typing) {
                    if (!move_kb(button) && n > 0) kb_focus = 0; /* Down off the keyboard: the results */
                } else if (tab == TAB_SEARCH && button == SDL_CONTROLLER_BUTTON_DPAD_UP && *sel < COLS) {
                    kb_focus = 1; /* Up off the top row: back to the keyboard */
                } else {
                    move_sel(sel, first_row, n, button, rows_visible(tab));
                }
                held = button;
                next_repeat = SDL_GetTicks() + 350;
                continue;
            }
            switch (button) {
            case SDL_CONTROLLER_BUTTON_LEFTSHOULDER:
            case SDL_CONTROLLER_BUTTON_RIGHTSHOULDER:
                tab = next_tab(tab, button == SDL_CONTROLLER_BUTTON_LEFTSHOULDER ? -1 : 1, counts);
                if (tab == TAB_SEARCH && !counts[TAB_SEARCH]) kb_focus = 1;
                break;
            case SDL_CONTROLLER_BUTTON_X: /* square: search, or delete while typing */
                if (typing) {
                    size_t len = strlen(g_query);
                    if (len) g_query[len - 1] = 0;
                    run_search(lists, counts);
                    sels[TAB_SEARCH] = firsts[TAB_SEARCH] = 0;
                } else {
                    tab = TAB_SEARCH;
                    kb_focus = 1;
                }
                break;
            case SDL_CONTROLLER_BUTTON_B:
                if (tab == TAB_SEARCH && !kb_focus) kb_focus = 1; /* results -> keyboard -> Movies */
                else if (tab == TAB_SEARCH) tab = TAB_MOVIES;
                else hold_exit_start(); /* closes once held long enough (below) */
                break;
            case SDL_CONTROLLER_BUTTON_START: /* options: install the offered update */
                if (g_update.version[0]) install_update(ren, &quit);
                held = -1;
                break;
            case SDL_CONTROLLER_BUTTON_Y: /* triangle: re-discover and refresh */
                if (find_server() == 0) {
                    refresh_items(KIND_MOVIE, &lists[TAB_MOVIES], &counts[TAB_MOVIES]);
                    refresh_items(KIND_SHOW, &lists[TAB_SHOWS], &counts[TAB_SHOWS]);
                    refresh_continue(&lists[TAB_CONTINUE], &counts[TAB_CONTINUE]);
                    refresh_home(&lists[TAB_HOME], &counts[TAB_HOME]);
                    run_search(lists, counts);
                    set_status(status, sizeof status, counts[TAB_MOVIES], counts[TAB_SHOWS]);
                    posters_server(g_host, g_port);
                    prefetch_posters(lists[TAB_MOVIES], counts[TAB_MOVIES]);
                    prefetch_posters(lists[TAB_SHOWS], counts[TAB_SHOWS]);
                    check_update();
                }
                for (int k = 0; k < TABS; k++) {
                    if (sels[k] >= counts[k]) sels[k] = counts[k] ? counts[k] - 1 : 0;
                    firsts[k] = 0;
                }
                sels[TAB_HOME] = home_sel();
                if (!tab_shown(tab, counts)) tab = counts[TAB_HOME] ? TAB_HOME : TAB_MOVIES;
                break;
            case SDL_CONTROLLER_BUTTON_A:
                if (typing) {
                    kb_press(lists, counts);
                    sels[TAB_SEARCH] = firsts[TAB_SEARCH] = 0;
                    break;
                }
                if (n == 0) break;
                Item m = list[*sel]; /* a copy: the lists are re-fetched below */
                if (m.episode || m.kind == KIND_MOVIE) {
                    char title[380];
                    if (m.episode) snprintf(title, sizeof title, "%s %s", m.title, m.subtitle);
                    else snprintf(title, sizeof title, "%s", m.title);
                    play_item(ren, m.episode ? "episodes" : "movies", m.episode ? m.play_id : m.id, title, m.resume_s,
                              m.watched, m.runtime_s);
                    held = -1;
                    /* pick up the new resume points */
                    refresh_items(KIND_MOVIE, &lists[TAB_MOVIES], &counts[TAB_MOVIES]);
                    refresh_continue(&lists[TAB_CONTINUE], &counts[TAB_CONTINUE]);
                    if (m.episode) refresh_items(KIND_SHOW, &lists[TAB_SHOWS], &counts[TAB_SHOWS]);
                    if (tab == TAB_HOME) {
                        int from_continue = g_nrows && strcmp(g_rows[g_row].title, "Continue Watching") == 0;
                        refresh_home(&lists[TAB_HOME], &counts[TAB_HOME]);
                        /* what was just played is first in Continue Watching now */
                        if (from_continue && g_nrows) g_rows[g_row].col = g_rows[g_row].first = 0;
                    }
                    if (tab == TAB_SEARCH) run_search(lists, counts);
                    for (int k = 0; k < TABS; k++)
                        if (sels[k] >= counts[k]) sels[k] = counts[k] ? counts[k] - 1 : 0;
                    if (tab == TAB_CONTINUE) sels[tab] = firsts[tab] = 0; /* what was just played is first now */
                    sels[TAB_HOME] = home_sel();
                    if (!tab_shown(tab, counts)) tab = counts[TAB_HOME] ? TAB_HOME : TAB_MOVIES;
                } else {
                    ep_sel = fetch_show(m.id, &sv);
                    ep_first = ep_sel > EP_ROWS_VISIBLE - 2 ? ep_sel - 2 : 0;
                    in_show = sv.n > 0;
                    held = -1;
                }
                break;
            }
        }
        if (held >= 0 && SDL_GetTicks() >= next_repeat) {
            if (in_show) move_ep(&sv, &ep_sel, &ep_first, held == SDL_CONTROLLER_BUTTON_DPAD_UP ? -1 : 1);
            else if (tab == TAB_HOME) {
                move_home(held);
                sels[TAB_HOME] = home_sel();
            } else if (tab == TAB_SEARCH && kb_focus) move_kb(held);
            else move_sel(&sels[tab], &firsts[tab], counts[tab], held, rows_visible(tab));
            next_repeat = SDL_GetTicks() + 110;
        }
        if (hold_exit_done()) quit = 1;
        SDL_Delay(16);
    }

    bb_log("FLEX: quit");
    free(sv.rows);
    for (int t = 0; t < TABS; t++) free_items(lists[t], counts[t]);
    if (g_backdrop) SDL_DestroyTexture(g_backdrop);
    if (g_fade) SDL_DestroyTexture(g_fade);
    TTF_CloseFont(g_big);
    TTF_CloseFont(g_font);
    TTF_CloseFont(g_small);
    TTF_CloseFont(g_tiny);
    SDL_DestroyRenderer(ren);
    SDL_DestroyWindow(win);
    IMG_Quit();
    TTF_Quit();
    SDL_Quit();
    return 0;
}
