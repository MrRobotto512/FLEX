/* FLEX PS5 client: find the server, list movies, play one.
 *
 * Controls: D-pad up/down move, Cross plays, Circle backs out (in the list it
 * quits). While playing: Cross pauses, Circle stops. */

#include <SDL.h>
#include <SDL_ttf.h>
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
#define ROWS 14

typedef struct {
    long id;
    char title[200];
    int year, resume_s, runtime_s, watched;
} Movie;

static char g_host[128] = "";
static int g_port = 8484;

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
              *rt = cJSON_GetObjectItem(m, "runtime_s"), *w = cJSON_GetObjectItem(m, "watched");
        list[i].year = cJSON_IsNumber(y) ? y->valueint : 0;
        list[i].resume_s = cJSON_IsNumber(r) ? r->valueint : 0;
        list[i].runtime_s = cJSON_IsNumber(rt) ? rt->valueint : 0;
        list[i].watched = cJSON_IsTrue(w);
    }
    cJSON_Delete(arr);
    *count = n;
    return list;
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

/* ---------- UI ---------- */

static void draw_text(SDL_Renderer *ren, TTF_Font *font, const char *text, int x, int y, SDL_Color c) {
    SDL_Surface *s = TTF_RenderUTF8_Blended(font, text, c);
    if (!s) return;
    SDL_Texture *t = SDL_CreateTextureFromSurface(ren, s);
    SDL_Rect dst = {x, y, s->w, s->h};
    SDL_RenderCopy(ren, t, NULL, &dst);
    SDL_DestroyTexture(t);
    SDL_FreeSurface(s);
}

static void draw_list(SDL_Renderer *ren, TTF_Font *font, TTF_Font *small, Movie *list, int n, int sel,
                      const char *status) {
    SDL_SetRenderDrawColor(ren, 15, 17, 21, 255);
    SDL_RenderClear(ren);
    draw_text(ren, font, "FLEX", 80, 50, (SDL_Color){232, 234, 240, 255});
    draw_text(ren, small, status, 80, 110, (SDL_Color){138, 144, 160, 255});

    int first = sel - ROWS / 2;
    if (first > n - ROWS) first = n - ROWS;
    if (first < 0) first = 0;
    for (int i = first; i < n && i < first + ROWS; i++) {
        int y = 170 + (i - first) * 60;
        if (i == sel) {
            SDL_SetRenderDrawColor(ren, 90, 169, 255, 255);
            SDL_Rect r = {70, y - 6, SCREEN_W - 140, 54};
            SDL_RenderFillRect(ren, &r);
        }
        char line[256];
        snprintf(line, sizeof line, "%s%s%d%s", list[i].title, list[i].year ? "  (" : "", list[i].year,
                 list[i].year ? ")" : "");
        SDL_Color fg = i == sel ? (SDL_Color){0, 0, 0, 255} : (SDL_Color){232, 234, 240, 255};
        draw_text(ren, font, line, 90, y, fg);
        if (list[i].watched)
            draw_text(ren, small, "watched", SCREEN_W - 260, y + 8, fg);
        else if (list[i].resume_s > 0 && list[i].runtime_s > 0) {
            char pct[32];
            snprintf(pct, sizeof pct, "%d%%", 100 * list[i].resume_s / list[i].runtime_s);
            draw_text(ren, small, pct, SCREEN_W - 260, y + 8, fg);
        }
    }
    if (n == 0) draw_text(ren, font, "No movies. Add folders on the PC and scan.", 90, 200, (SDL_Color){138, 144, 160, 255});
    SDL_RenderPresent(ren);
}

int main(int argc, char **argv) {
    (void)argc;
    (void)argv;
    if (SDL_Init(SDL_INIT_VIDEO | SDL_INIT_AUDIO | SDL_INIT_GAMECONTROLLER) != 0) {
        fprintf(stderr, "SDL_Init: %s\n", SDL_GetError());
        return 1;
    }
    TTF_Init();
    SDL_Window *win = SDL_CreateWindow("FLEX", 0, 0, SCREEN_W, SCREEN_H, SDL_WINDOW_FULLSCREEN);
    SDL_Renderer *ren = SDL_CreateRenderer(win, -1, SDL_RENDERER_ACCELERATED | SDL_RENDERER_PRESENTVSYNC);
    SDL_RenderSetLogicalSize(ren, SCREEN_W, SCREEN_H);
    for (int i = 0; i < SDL_NumJoysticks(); i++)
        if (SDL_IsGameController(i)) SDL_GameControllerOpen(i);

    TTF_Font *font = TTF_OpenFont(FONT_PATH, 36);
    TTF_Font *small = TTF_OpenFont(FONT_PATH, 24);
    if (!font) font = TTF_OpenFont("assets/DejaVuSans.ttf", 36);
    if (!small) small = TTF_OpenFont("assets/DejaVuSans.ttf", 24);
    if (!font || !small) {
        fprintf(stderr, "font missing: put DejaVuSans.ttf in " APP_DIR "/assets\n");
        return 1;
    }

    char status[256] = "Looking for a FLEX server…";
    draw_list(ren, font, small, NULL, 0, 0, status);
    Movie *list = NULL;
    int n = 0, sel = 0, quit = 0;

    if (find_server() == 0) {
        fprintf(stderr, "FLEX: server %s:%d\n", g_host, g_port);
        list = fetch_movies(&n);
        fprintf(stderr, "FLEX: %d movies\n", n);
        snprintf(status, sizeof status, "%s:%d  ·  %d movies", g_host, g_port, n);
    } else {
        fprintf(stderr, "FLEX: no server found (discovery timed out, no saved address)\n");
        snprintf(status, sizeof status, "No server found. Start FLEX on your PC (same network), or put \"host port\" in " SAVED_SERVER);
    }

    while (!quit) {
        draw_list(ren, font, small, list, n, sel, status);
        SDL_Event e;
        while (SDL_PollEvent(&e)) {
            int key = -1;
            if (e.type == SDL_QUIT) quit = 1;
            if (e.type == SDL_CONTROLLERBUTTONDOWN) key = e.cbutton.button;
            if (e.type == SDL_KEYDOWN) {
                switch (e.key.keysym.sym) {
                case SDLK_UP: key = SDL_CONTROLLER_BUTTON_DPAD_UP; break;
                case SDLK_DOWN: key = SDL_CONTROLLER_BUTTON_DPAD_DOWN; break;
                case SDLK_RETURN: key = SDL_CONTROLLER_BUTTON_A; break;
                case SDLK_ESCAPE: key = SDL_CONTROLLER_BUTTON_B; break;
                case SDLK_r: key = SDL_CONTROLLER_BUTTON_Y; break;
                }
            }
            switch (key) {
            case SDL_CONTROLLER_BUTTON_DPAD_UP: if (sel > 0) sel--; break;
            case SDL_CONTROLLER_BUTTON_DPAD_DOWN: if (sel + 1 < n) sel++; break;
            case SDL_CONTROLLER_BUTTON_B: quit = 1; break;
            case SDL_CONTROLLER_BUTTON_Y: /* triangle: retry / refresh */
                free(list);
                list = NULL;
                n = 0;
                if (find_server() == 0) list = fetch_movies(&n);
                snprintf(status, sizeof status, "%s:%d  ·  %d movies", g_host, g_port, n);
                sel = 0;
                break;
            case SDL_CONTROLLER_BUTTON_A:
                if (n > 0) {
                    play_movie(ren, &list[sel]);
                    free(list);
                    list = fetch_movies(&n);  /* refresh resume points */
                    if (sel >= n) sel = n ? n - 1 : 0;
                }
                break;
            }
        }
        SDL_Delay(16);
    }

    free(list);
    TTF_CloseFont(font);
    TTF_CloseFont(small);
    SDL_DestroyRenderer(ren);
    SDL_DestroyWindow(win);
    TTF_Quit();
    SDL_Quit();
    return 0;
}
