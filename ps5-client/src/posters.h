#pragma once
#include <SDL.h>

/* Poster loading off the UI thread. Worker threads fetch posters from the
 * server (several at a time, so the ~0.2 s round trip over Tailscale
 * overlaps) and keep them in a cache on the console's disk, so each one
 * crosses the network once. Wanted posters are also decoded; the main thread
 * collects the decoded surfaces with posters_take and makes textures. */

void posters_init(void);
void posters_server(const char *host, int port);

/* posters_want asks for a poster to show soon (fetched or read from the
 * cache, then decoded). The newest request is served first, so the rows on
 * screen win over ones scrolled past. */
void posters_want(char kind, long id, int tmdb_id, const char *path);

/* posters_prefetch only puts a poster in the disk cache, after every wanted one. */
void posters_prefetch(char kind, long id, int tmdb_id, const char *path);

/* posters_take hands over one decoded poster (NULL surface: unavailable).
 * Returns 0 when none is ready. The caller frees the surface. */
int posters_take(char *kind, long *id, SDL_Surface **s);
