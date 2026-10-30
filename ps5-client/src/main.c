/* FLEX PS5 client: find the server, browse Movies and TV as poster grids,
 * open a show's episodes, play.
 *
 * Controls (grid): D-pad moves, L1/R1 switch Movies/TV, Cross plays a movie
 * or opens a show, Triangle re-discovers the server and refreshes, Circle
 * quits. Episodes: D-pad up/down, Cross plays, Circle back. Player: Cross
 * pauses, Circle stops. */

#include <SDL.h>
#include <SDL_image.h>
#include <SDL_ttf.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "cJSON.h"
#include "net.h"
#include "player.h"

#define APP_DIR "/data/homebrew/FLEX"
#define SAVED_SERVER APP_DIR "/server.txt"
#define FONT_PATH APP_DIR "/assets/DejaVuSans.ttf"
#define ASPECT_FILE APP_DIR "/aspect.txt"
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

/* One poster card: a movie or a show. */
typedef struct {
    Kind kind;
    long id;
    char title[200];
    int year, resume_s, runtime_s, watched;
    int seasons, episodes, unwatched; /* shows */
    char poster[128];                 /* API path or "" when the server has no artwork */
    SDL_Texture *tex;                 /* decoded poster, NULL until loaded */
    int tex_state;                    /* 0 = not loaded yet, 1 = loaded, 2 = unavailable */
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
static TTF_Font *g_font, *g_small, *g_tiny;

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

/* refresh_items re-fetches a grid but keeps already decoded posters. */
static void refresh_items(Kind kind, Item **list, int *n) {
    Item *old = *list;
    int old_n = *n;
    Item *fresh = fetch_items(kind, n);
    for (int i = 0; fresh && i < *n; i++)
        for (int j = 0; j < old_n; j++)
            if (old[j].id == fresh[i].id && old[j].tex) {
                fresh[i].tex = old[j].tex;
                fresh[i].tex_state = 1;
                old[j].tex = NULL;
                break;
            }
    for (int j = 0; j < old_n; j++)
        if (old[j].tex) SDL_DestroyTexture(old[j].tex);
    free(old);
    *list = fresh;
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

static void load_poster(SDL_Renderer *ren, Item *m) {
    m->tex_state = 2;
    if (!m->poster[0]) return;
    size_t len = 0;
    char *bytes = flex_http(g_host, g_port, "GET", m->poster, NULL, &len);
    if (!bytes) return;
    SDL_Surface *s = IMG_Load_RW(SDL_RWFromConstMem(bytes, (int)len), 1);
    free(bytes);
    if (!s) {
        fprintf(stderr, "poster %ld: %s\n", m->id, IMG_GetError());
        return;
    }
    m->tex = SDL_CreateTextureFromSurface(ren, s);
    SDL_FreeSurface(s);
    if (m->tex) m->tex_state = 1;
}

/* Decode at most one poster per frame so the UI never stalls; drop cached
 * textures that scrolled far away once the cache gets big. */
static void load_visible_posters(SDL_Renderer *ren, Item *list, int n, int sel, int first_row) {
    int lo = first_row * COLS, hi = (first_row + ROWS_VISIBLE) * COLS;
    if (hi > n) hi = n;
    if (sel >= 0 && sel < n && list[sel].tex_state == 0) {
        load_poster(ren, &list[sel]);
        return;
    }
    for (int i = lo; i < hi; i++)
        if (list[i].tex_state == 0) {
            load_poster(ren, &list[i]);
            break;
        }
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

/* play_item plays a movie ("movies") or an episode ("episodes"), then saves
 * the position. Direct files seek in place; a transcoded HLS session cannot
 * seek, so a seek ends it and a new session starts at the target. */
static void play_item(SDL_Renderer *ren, const char *kind, long id, const char *title, int resume_s, int watched,
                      int runtime_s) {
    double start = watched ? 0 : resume_s, position = -1, duration = runtime_s;
    char kc = kind[0] == 'm' ? 'm' : 'e';
    int aspect = get_aspect(kc, id);
    char path[256];
    for (;;) {
        snprintf(path, sizeof path, "/api/v1/%s/%ld/play?profile=ps5&start=%d", kind, id, (int)start);
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
        char url[512];
        snprintf(url, sizeof url, "http://%s:%d%s", g_host, g_port, rel ? rel : "");
        fprintf(stderr, "FLEX: play \"%s\" via %s from %.0fs: %s\n", title, method ? method : "?", start, url);

        PlayOpts o = {.url = url, .title = title, .offset = hls ? start : 0, .initial = hls ? 0 : start,
                      .duration = duration, .seekable = !hls, .aspect = aspect, .font = g_small};
        PlayResult r = player_play(ren, &o);
        aspect = r.aspect; /* carried across session restarts */
        fprintf(stderr, "FLEX: stopped at %.1fs%s\n", r.position, r.seek_to >= 0 ? " (seek)" : "");

        if (hls && rel) {
            /* /api/v1/hls/<session>/index.m3u8 -> DELETE /api/v1/hls/<session> */
            char session[64] = "";
            sscanf(rel, "/api/v1/hls/%63[^/]", session);
            snprintf(path, sizeof path, "/api/v1/hls/%s", session);
            free(flex_http(g_host, g_port, "DELETE", path, NULL, NULL));
        }
        cJSON_Delete(info);
        if (r.position >= 0) position = r.position;
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
    if (m->kind == KIND_MOVIE) {
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
    char sub[96] = "";
    if (m->kind == KIND_MOVIE) {
        if (m->year) snprintf(sub, sizeof sub, "%d", m->year);
        if (m->watched) snprintf(sub + strlen(sub), sizeof sub - strlen(sub), "%swatched", m->year ? "  ·  " : "");
    } else {
        snprintf(sub, sizeof sub, "%d season%s  ·  %d ep", m->seasons, m->seasons == 1 ? "" : "s", m->episodes);
    }
    if (sub[0]) draw_text(ren, g_tiny, sub, x, y + CARD_H + 42, C_MUTED);
}

static void draw_grid(SDL_Renderer *ren, Kind mode, Item *list, int n, int sel, int first_row, const char *status) {
    SDL_SetRenderDrawColor(ren, C_BG.r, C_BG.g, C_BG.b, 255);
    SDL_RenderClear(ren);
    int x = MARGIN_X;
    x += draw_text(ren, g_font, "FLEX", x, 50, C_FG) + 40;
    x += draw_text(ren, g_font, "Movies", x, 50, mode == KIND_MOVIE ? C_FG : C_MUTED) + 30;
    draw_text(ren, g_font, "TV", x, 50, mode == KIND_SHOW ? C_FG : C_MUTED);
    draw_text(ren, g_tiny, "L1 / R1 switch", SCREEN_W - MARGIN_X - 150, 62, C_MUTED);
    draw_text(ren, g_small, status, MARGIN_X, 108, C_MUTED);

    int lo = first_row * COLS, hi = (first_row + ROWS_VISIBLE) * COLS;
    for (int i = lo; i < n && i < hi; i++) {
        int col = i % COLS, row = i / COLS - first_row;
        draw_card(ren, &list[i], MARGIN_X + col * (CARD_W + GAP_X), GRID_TOP + row * ROW_H, i == sel);
    }
    if (n == 0)
        draw_text(ren, g_font, mode == KIND_MOVIE ? "No movies. Add folders on the PC and scan." : "No TV shows found.",
                  MARGIN_X, GRID_TOP, C_MUTED);

    int rows = (n + COLS - 1) / COLS;
    if (rows > ROWS_VISIBLE) {
        char pos[64];
        snprintf(pos, sizeof pos, "rows %d-%d of %d", first_row + 1,
                 first_row + ROWS_VISIBLE < rows ? first_row + ROWS_VISIBLE : rows, rows);
        draw_text(ren, g_tiny, pos, SCREEN_W - MARGIN_X - 200, 116, C_MUTED);
    }
    SDL_RenderPresent(ren);
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
    SDL_RenderPresent(ren);
}

/* ---------- navigation ---------- */

static void move_sel(int *sel, int *first_row, int n, int button) {
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
    if (row >= *first_row + ROWS_VISIBLE) *first_row = row - ROWS_VISIBLE + 1;
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
    if (SDL_Init(SDL_INIT_VIDEO | SDL_INIT_AUDIO | SDL_INIT_GAMECONTROLLER) != 0) {
        fprintf(stderr, "SDL_Init: %s\n", SDL_GetError());
        return 1;
    }
    if (TTF_Init() != 0) fprintf(stderr, "TTF_Init: %s\n", TTF_GetError());
    if (!(IMG_Init(IMG_INIT_JPG | IMG_INIT_PNG) & IMG_INIT_JPG)) fprintf(stderr, "IMG_Init: %s\n", IMG_GetError());

    SDL_DisplayMode dm;
    fprintf(stderr, "FLEX: video driver %s, %d display(s)\n", SDL_GetCurrentVideoDriver(), SDL_GetNumVideoDisplays());
    if (SDL_GetDesktopDisplayMode(0, &dm) == 0) fprintf(stderr, "FLEX: desktop mode %dx%d @%dHz\n", dm.w, dm.h, dm.refresh_rate);

    /* Same setup as ps5-payload-dev/jtplay, which is known to display on the
     * console: a plain 1920x1080 window and SDL's software renderer (the
     * only render driver this SDL port has). */
    SDL_Window *win = SDL_CreateWindow("FLEX", SDL_WINDOWPOS_CENTERED, SDL_WINDOWPOS_CENTERED, SCREEN_W, SCREEN_H, 0);
    if (!win) {
        fprintf(stderr, "SDL_CreateWindow: %s\n", SDL_GetError());
        return 1;
    }
    SDL_SetHint(SDL_HINT_RENDER_DRIVER, "software");
    SDL_SetHint(SDL_HINT_RENDER_SCALE_QUALITY, "linear");
    SDL_Renderer *ren = SDL_CreateRenderer(win, -1, SDL_RENDERER_SOFTWARE | SDL_RENDERER_PRESENTVSYNC);
    if (!ren) {
        fprintf(stderr, "SDL_CreateRenderer: %s\n", SDL_GetError());
        return 1;
    }
    SDL_SetRenderDrawBlendMode(ren, SDL_BLENDMODE_BLEND);
    for (int i = 0; i < SDL_NumJoysticks(); i++)
        if (SDL_IsGameController(i)) {
            SDL_GameController *gc = SDL_GameControllerOpen(i);
            fprintf(stderr, "FLEX: controller %d: %s\n", i, gc ? SDL_GameControllerName(gc) : SDL_GetError());
        }

    g_font = TTF_OpenFont(FONT_PATH, 36);
    g_small = TTF_OpenFont(FONT_PATH, 22);
    g_tiny = TTF_OpenFont(FONT_PATH, 18);
    if (!g_font) g_font = TTF_OpenFont("assets/DejaVuSans.ttf", 36);
    if (!g_small) g_small = TTF_OpenFont("assets/DejaVuSans.ttf", 22);
    if (!g_tiny) g_tiny = TTF_OpenFont("assets/DejaVuSans.ttf", 18);
    if (!g_font || !g_small || !g_tiny) {
        fprintf(stderr, "font missing: put DejaVuSans.ttf in " APP_DIR "/assets (%s)\n", TTF_GetError());
        return 1;
    }

    load_aspects();
    char status[256] = "Looking for a FLEX server…";
    draw_grid(ren, KIND_MOVIE, NULL, 0, 0, 0, status);
    fprintf(stderr, "FLEX: first frame presented\n");

    Item *lists[2] = {NULL, NULL};
    int counts[2] = {0, 0}, sels[2] = {0, 0}, firsts[2] = {0, 0};
    Kind mode = KIND_MOVIE;
    int quit = 0;
    ShowView sv = {0};
    int in_show = 0, ep_sel = 0, ep_first = 0;

    if (find_server() == 0) {
        fprintf(stderr, "FLEX: server %s:%d%s (tailscale %s)\n", g_host, g_port, g_is_remote ? " via tailscale" : "",
                g_remote[0] ? g_remote : "unknown");
        lists[0] = fetch_items(KIND_MOVIE, &counts[0]);
        lists[1] = fetch_items(KIND_SHOW, &counts[1]);
        fprintf(stderr, "FLEX: %d movies, %d shows\n", counts[0], counts[1]);
        set_status(status, sizeof status, counts[0], counts[1]);
    } else {
        fprintf(stderr, "FLEX: no server found (discovery timed out, saved addresses unreachable)\n");
        snprintf(status, sizeof status, "No server found. Start FLEX on your PC (same network); Triangle retries.");
    }

    /* D-pad auto-repeat while held */
    int held = -1;
    Uint32 next_repeat = 0;

    while (!quit) {
        Item *list = lists[mode];
        int n = counts[mode], *sel = &sels[mode], *first_row = &firsts[mode];

        if (in_show) {
            draw_episodes(ren, &sv, list && *sel < n ? &list[*sel] : NULL, ep_sel, ep_first);
        } else {
            load_visible_posters(ren, list, n, *sel, *first_row);
            draw_grid(ren, mode, list, n, *sel, *first_row, status);
        }

        SDL_Event e;
        while (SDL_PollEvent(&e)) {
            int button = -1;
            if (e.type == SDL_QUIT) quit = 1;
            if (e.type == SDL_CONTROLLERBUTTONDOWN) button = e.cbutton.button;
            if (e.type == SDL_KEYDOWN && !e.key.repeat) button = key_to_button(e.key.keysym.sym);
            if (e.type == SDL_CONTROLLERBUTTONUP && e.cbutton.button == held) held = -1;
            if (e.type == SDL_KEYUP && key_to_button(e.key.keysym.sym) == held) held = -1;
            if (button < 0) continue;

            if (in_show) {
                switch (button) {
                case SDL_CONTROLLER_BUTTON_DPAD_UP: move_ep(&sv, &ep_sel, &ep_first, -1); held = button; next_repeat = SDL_GetTicks() + 350; break;
                case SDL_CONTROLLER_BUTTON_DPAD_DOWN: move_ep(&sv, &ep_sel, &ep_first, 1); held = button; next_repeat = SDL_GetTicks() + 350; break;
                case SDL_CONTROLLER_BUTTON_B: in_show = 0; held = -1; refresh_items(KIND_SHOW, &lists[1], &counts[1]); break;
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
                move_sel(sel, first_row, n, button);
                held = button;
                next_repeat = SDL_GetTicks() + 350;
                continue;
            }
            switch (button) {
            case SDL_CONTROLLER_BUTTON_LEFTSHOULDER:
            case SDL_CONTROLLER_BUTTON_RIGHTSHOULDER:
                mode = mode == KIND_MOVIE ? KIND_SHOW : KIND_MOVIE;
                break;
            case SDL_CONTROLLER_BUTTON_B: quit = 1; break;
            case SDL_CONTROLLER_BUTTON_Y: /* triangle: re-discover and refresh */
                if (find_server() == 0) {
                    refresh_items(KIND_MOVIE, &lists[0], &counts[0]);
                    refresh_items(KIND_SHOW, &lists[1], &counts[1]);
                    set_status(status, sizeof status, counts[0], counts[1]);
                }
                for (int k = 0; k < 2; k++) {
                    if (sels[k] >= counts[k]) sels[k] = counts[k] ? counts[k] - 1 : 0;
                    firsts[k] = 0;
                }
                break;
            case SDL_CONTROLLER_BUTTON_A:
                if (n == 0) break;
                if (mode == KIND_MOVIE) {
                    Item *m = &list[*sel];
                    play_item(ren, "movies", m->id, m->title, m->resume_s, m->watched, m->runtime_s);
                    held = -1;
                    refresh_items(KIND_MOVIE, &lists[0], &counts[0]); /* pick up the new resume point */
                    if (sels[0] >= counts[0]) sels[0] = counts[0] ? counts[0] - 1 : 0;
                } else {
                    ep_sel = fetch_show(list[*sel].id, &sv);
                    ep_first = ep_sel > EP_ROWS_VISIBLE - 2 ? ep_sel - 2 : 0;
                    in_show = sv.n > 0;
                    held = -1;
                }
                break;
            }
        }
        if (held >= 0 && SDL_GetTicks() >= next_repeat) {
            if (in_show) move_ep(&sv, &ep_sel, &ep_first, held == SDL_CONTROLLER_BUTTON_DPAD_UP ? -1 : 1);
            else move_sel(sel, first_row, n, held);
            next_repeat = SDL_GetTicks() + 110;
        }
        SDL_Delay(16);
    }

    free(sv.rows);
    free_items(lists[0], counts[0]);
    free_items(lists[1], counts[1]);
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
