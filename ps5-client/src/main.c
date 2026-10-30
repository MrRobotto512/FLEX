/* FLEX PS5 client: find the server, show the library as a poster grid, play.
 *
 * Controls: D-pad moves, Cross plays, Triangle re-discovers the server and
 * refreshes, Circle quits. While playing: Cross pauses, Circle stops. */

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

typedef struct {
    long id;
    char title[200];
    int year, resume_s, runtime_s, watched;
    char poster[128];  /* API path or "" when the server has no artwork */
    SDL_Texture *tex;  /* decoded poster, NULL until loaded */
    int tex_state;     /* 0 = not loaded yet, 1 = loaded, 2 = unavailable */
} Movie;

static const SDL_Color C_FG = {232, 234, 240, 255};
static const SDL_Color C_MUTED = {138, 144, 160, 255};
static const SDL_Color C_ACCENT = {90, 169, 255, 255};
static const SDL_Color C_PANEL = {24, 27, 34, 255};
static const SDL_Color C_BG = {15, 17, 21, 255};

static char g_host[128] = "";
static int g_port = 8484;
static TTF_Font *g_font, *g_small, *g_tiny;

/* ---------- server address ---------- */

static int load_saved_server(void) {
    FILE *f = fopen(SAVED_SERVER, "r");
    if (!f) return -1;
    int ok = fscanf(f, "%127s %d", g_host, &g_port) == 2;
    fclose(f);
    return ok ? 0 : -1;
}

static void save_server(void) {
    FILE *f = fopen(SAVED_SERVER, "w");
    if (!f) return;
    fprintf(f, "%s %d\n", g_host, g_port);
    fclose(f);
}

static int find_server(void) {
    if (flex_discover(g_host, sizeof g_host, &g_port, 1500) == 0) {
        save_server();
        return 0;
    }
    /* no answer on the LAN: fall back to the remembered (e.g. Tailscale) address */
    return load_saved_server();
}

/* ---------- API ---------- */

static Movie *fetch_movies(int *count) {
    *count = 0;
    char *body = flex_get(g_host, g_port, "/api/v1/movies?sort=title");
    if (!body) return NULL;
    cJSON *arr = cJSON_Parse(body);
    free(body);
    if (!cJSON_IsArray(arr)) {
        cJSON_Delete(arr);
        return NULL;
    }
    int n = cJSON_GetArraySize(arr);
    Movie *list = calloc(n ? n : 1, sizeof *list);
    for (int i = 0; i < n; i++) {
        cJSON *m = cJSON_GetArrayItem(arr, i);
        list[i].id = (long)cJSON_GetObjectItem(m, "id")->valuedouble;
        snprintf(list[i].title, sizeof list[i].title, "%s", cJSON_GetStringValue(cJSON_GetObjectItem(m, "title")));
        cJSON *y = cJSON_GetObjectItem(m, "year"), *r = cJSON_GetObjectItem(m, "resume_s"),
              *rt = cJSON_GetObjectItem(m, "runtime_s"), *w = cJSON_GetObjectItem(m, "watched"),
              *p = cJSON_GetObjectItem(m, "poster");
        list[i].year = cJSON_IsNumber(y) ? y->valueint : 0;
        list[i].resume_s = cJSON_IsNumber(r) ? r->valueint : 0;
        list[i].runtime_s = cJSON_IsNumber(rt) ? rt->valueint : 0;
        list[i].watched = cJSON_IsTrue(w);
        if (cJSON_IsString(p)) snprintf(list[i].poster, sizeof list[i].poster, "%s", p->valuestring);
    }
    cJSON_Delete(arr);
    *count = n;
    return list;
}

/* refresh_list re-fetches the library but keeps already decoded posters. */
static void refresh_list(Movie **list, int *n) {
    Movie *old = *list;
    int old_n = *n;
    Movie *fresh = fetch_movies(n);
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

static void free_list(Movie *list, int n) {
    for (int i = 0; i < n; i++)
        if (list[i].tex) SDL_DestroyTexture(list[i].tex);
    free(list);
}

static void load_poster(SDL_Renderer *ren, Movie *m) {
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
static void load_visible_posters(SDL_Renderer *ren, Movie *list, int n, int sel, int first_row) {
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

static void play_movie(SDL_Renderer *ren, Movie *m) {
    int start = m->watched ? 0 : m->resume_s;
    char path[256];
    snprintf(path, sizeof path, "/api/v1/movies/%ld/play?profile=ps5&start=%d", m->id, start);
    char *body = flex_get(g_host, g_port, path);
    if (!body) return;
    cJSON *info = cJSON_Parse(body);
    free(body);
    if (!info) return;

    const char *rel = cJSON_GetStringValue(cJSON_GetObjectItem(info, "url"));
    const char *method = cJSON_GetStringValue(cJSON_GetObjectItem(info, "method"));
    cJSON *dur = cJSON_GetObjectItem(info, "duration_s");
    char url[512];
    snprintf(url, sizeof url, "http://%s:%d%s", g_host, g_port, rel ? rel : "");
    fprintf(stderr, "FLEX: play \"%s\" via %s: %s\n", m->title, method ? method : "?", url);

    double pos = player_play(ren, url);
    fprintf(stderr, "FLEX: stopped at %.1fs\n", pos);

    if (pos >= 0) {
        /* HLS sessions start at `start`, direct files at 0 */
        int position = (int)pos + (method && strcmp(method, "hls") == 0 ? start : 0);
        char json[128];
        snprintf(json, sizeof json, "{\"position_s\":%d,\"duration_s\":%d}", position,
                 cJSON_IsNumber(dur) ? (int)dur->valuedouble : m->runtime_s);
        snprintf(path, sizeof path, "/api/v1/movies/%ld/progress", m->id);
        free(flex_http(g_host, g_port, "PUT", path, json, NULL));
    }
    if (method && strcmp(method, "hls") == 0 && rel) {
        /* /api/v1/hls/<session>/index.m3u8 -> DELETE /api/v1/hls/<session> */
        char session[64] = "";
        sscanf(rel, "/api/v1/hls/%63[^/]", session);
        snprintf(path, sizeof path, "/api/v1/hls/%s", session);
        free(flex_http(g_host, g_port, "DELETE", path, NULL, NULL));
    }
    cJSON_Delete(info);
}

/* ---------- drawing ---------- */

static void fill(SDL_Renderer *ren, SDL_Rect r, SDL_Color c) {
    SDL_SetRenderDrawColor(ren, c.r, c.g, c.b, c.a);
    SDL_RenderFillRect(ren, &r);
}

static int draw_text(SDL_Renderer *ren, TTF_Font *font, const char *text, int x, int y, SDL_Color c) {
    static int reported;
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

/* draw_wrapped centres multi-line text inside a box (poster placeholders). */
static void draw_wrapped(SDL_Renderer *ren, TTF_Font *font, const char *text, SDL_Rect box, SDL_Color c) {
    SDL_Surface *s = TTF_RenderUTF8_Blended_Wrapped(font, text, c, box.w - 24);
    if (!s) return;
    SDL_Texture *t = SDL_CreateTextureFromSurface(ren, s);
    if (t) {
        SDL_Rect dst = {box.x + (box.w - s->w) / 2, box.y + (box.h - s->h) / 2, s->w, s->h};
        if (dst.y < box.y) dst.y = box.y;
        SDL_RenderCopy(ren, t, NULL, &dst);
        SDL_DestroyTexture(t);
    }
    SDL_FreeSurface(s);
}

static void draw_card(SDL_Renderer *ren, Movie *m, int x, int y, int selected) {
    SDL_Rect card = {x, y, CARD_W, CARD_H};
    if (selected) fill(ren, (SDL_Rect){x - 6, y - 6, CARD_W + 12, CARD_H + 12}, C_ACCENT);
    if (m->tex) {
        SDL_RenderCopy(ren, m->tex, NULL, &card);
    } else {
        fill(ren, card, C_PANEL);
        draw_wrapped(ren, g_small, m->title, card, C_MUTED);
    }
    if (!m->watched && m->resume_s > 0 && m->runtime_s > 0) {
        int pct = 100 * m->resume_s / m->runtime_s;
        if (pct > 100) pct = 100;
        fill(ren, (SDL_Rect){x + 10, y + CARD_H - 16, CARD_W - 20, 6}, (SDL_Color){0, 0, 0, 160});
        fill(ren, (SDL_Rect){x + 10, y + CARD_H - 16, (CARD_W - 20) * pct / 100, 6}, C_ACCENT);
    }
    draw_text_fit(ren, g_small, m->title, x, y + CARD_H + 10, CARD_W, selected ? C_FG : C_MUTED);
    char sub[64] = "";
    if (m->year) snprintf(sub, sizeof sub, "%d", m->year);
    if (m->watched) snprintf(sub + strlen(sub), sizeof sub - strlen(sub), "%swatched", m->year ? "  ·  " : "");
    if (sub[0]) draw_text(ren, g_tiny, sub, x, y + CARD_H + 42, C_MUTED);
}

static void draw_grid(SDL_Renderer *ren, Movie *list, int n, int sel, int first_row, const char *status) {
    SDL_SetRenderDrawColor(ren, C_BG.r, C_BG.g, C_BG.b, 255);
    SDL_RenderClear(ren);
    draw_text(ren, g_font, "FLEX", MARGIN_X, 50, C_FG);
    draw_text(ren, g_small, status, MARGIN_X, 108, C_MUTED);

    int lo = first_row * COLS, hi = (first_row + ROWS_VISIBLE) * COLS;
    for (int i = lo; i < n && i < hi; i++) {
        int col = i % COLS, row = i / COLS - first_row;
        draw_card(ren, &list[i], MARGIN_X + col * (CARD_W + GAP_X), GRID_TOP + row * ROW_H, i == sel);
    }
    if (n == 0) draw_text(ren, g_font, "No movies. Add folders on the PC and scan.", MARGIN_X, GRID_TOP, C_MUTED);

    /* scroll hint */
    int rows = (n + COLS - 1) / COLS;
    if (rows > ROWS_VISIBLE) {
        char pos[64];
        snprintf(pos, sizeof pos, "rows %d-%d of %d", first_row + 1, first_row + ROWS_VISIBLE < rows ? first_row + ROWS_VISIBLE : rows, rows);
        draw_text(ren, g_tiny, pos, SCREEN_W - MARGIN_X - 200, 116, C_MUTED);
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

    /* --- diagnostics: what SDL sees on this console --- */
    SDL_DisplayMode dm;
    fprintf(stderr, "FLEX: video driver %s, %d display(s)\n", SDL_GetCurrentVideoDriver(), SDL_GetNumVideoDisplays());
    if (SDL_GetDesktopDisplayMode(0, &dm) == 0) fprintf(stderr, "FLEX: desktop mode %dx%d @%dHz\n", dm.w, dm.h, dm.refresh_rate);
    fprintf(stderr, "FLEX: %d audio device(s), %d joystick(s)\n", SDL_GetNumAudioDevices(0), SDL_NumJoysticks());

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
    {
        SDL_RendererInfo ri;
        int w = 0, h = 0;
        SDL_GetRendererInfo(ren, &ri);
        SDL_GetRendererOutputSize(ren, &w, &h);
        fprintf(stderr, "FLEX: renderer %s, output %dx%d\n", ri.name, w, h);
    }
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

    char status[256] = "Looking for a FLEX server…";
    draw_grid(ren, NULL, 0, 0, 0, status);
    fprintf(stderr, "FLEX: first frame presented\n");

    Movie *list = NULL;
    int n = 0, sel = 0, first_row = 0, quit = 0;
    if (find_server() == 0) {
        fprintf(stderr, "FLEX: server %s:%d\n", g_host, g_port);
        list = fetch_movies(&n);
        fprintf(stderr, "FLEX: %d movies\n", n);
        snprintf(status, sizeof status, "%s:%d  ·  %d movies", g_host, g_port, n);
    } else {
        fprintf(stderr, "FLEX: no server found (discovery timed out, no saved address)\n");
        snprintf(status, sizeof status, "No server found. Start FLEX on your PC (same network), or put \"host port\" in " SAVED_SERVER);
    }

    /* D-pad auto-repeat while held */
    int held = -1;
    Uint32 next_repeat = 0;

    while (!quit) {
        load_visible_posters(ren, list, n, sel, first_row);
        draw_grid(ren, list, n, sel, first_row, status);

        SDL_Event e;
        while (SDL_PollEvent(&e)) {
            int button = -1;
            if (e.type == SDL_QUIT) quit = 1;
            if (e.type == SDL_CONTROLLERBUTTONDOWN) button = e.cbutton.button;
            if (e.type == SDL_KEYDOWN && !e.key.repeat) button = key_to_button(e.key.keysym.sym);
            if (e.type == SDL_CONTROLLERBUTTONUP && e.cbutton.button == held) held = -1;
            if (e.type == SDL_KEYUP && key_to_button(e.key.keysym.sym) == held) held = -1;
            if (button < 0) continue;

            if (is_dpad(button)) {
                move_sel(&sel, &first_row, n, button);
                held = button;
                next_repeat = SDL_GetTicks() + 350;
                continue;
            }
            switch (button) {
            case SDL_CONTROLLER_BUTTON_B: quit = 1; break;
            case SDL_CONTROLLER_BUTTON_Y: /* triangle: re-discover and refresh */
                if (find_server() == 0) {
                    refresh_list(&list, &n);
                    snprintf(status, sizeof status, "%s:%d  ·  %d movies", g_host, g_port, n);
                }
                if (sel >= n) sel = n ? n - 1 : 0;
                first_row = 0;
                break;
            case SDL_CONTROLLER_BUTTON_A:
                if (n > 0) {
                    play_movie(ren, &list[sel]);
                    held = -1;
                    refresh_list(&list, &n); /* pick up the new resume point */
                    if (sel >= n) sel = n ? n - 1 : 0;
                }
                break;
            }
        }
        if (held >= 0 && SDL_GetTicks() >= next_repeat) {
            move_sel(&sel, &first_row, n, held);
            next_repeat = SDL_GetTicks() + 110;
        }
        SDL_Delay(16);
    }

    free_list(list, n);
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
