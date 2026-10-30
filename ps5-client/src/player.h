#pragma once
#include <SDL.h>

/* Plays `url` (direct file or HLS playlist) until it ends or the user backs
 * out. Decoding is on the CPU, so the server must have sent the `ps5`
 * profile (<=1080p H.264 + AAC). Returns the last playback position in
 * seconds relative to the start of the stream, or -1 on error. */
double player_play(SDL_Renderer *ren, const char *url);
