#pragma once
#include <SDL.h>
#include <SDL_ttf.h>

typedef struct {
    const char *url;
    const char *title;
    double offset;   /* media time where this stream starts (HLS sessions started with start=) */
    double initial;  /* seekable streams: media time to start at (resume point) */
    double duration; /* whole item, for the progress bar */
    int seekable;    /* direct file: seek in place; otherwise the caller restarts the stream at seek_to */
    TTF_Font *font;  /* OSD text */
} PlayOpts;

typedef struct {
    double position; /* media time reached; -1 when the stream could not be opened */
    double seek_to;  /* >= 0: the user seeked in a non-seekable stream, restart it here */
    int ended;       /* played to the end */
} PlayResult;

/* Plays one stream until it ends, the user backs out, or (non-seekable
 * streams) the user seeks. Decoding is on the CPU, so the server must have
 * sent the `ps5` profile (<=1080p H.264 + AAC).
 *
 * Controls: Cross pause, Circle stop, D-pad left/right -/+10 s,
 * up/down +/-60 s (hold to repeat; the jump happens 0.7 s after the last
 * press so several presses add up). */
PlayResult player_play(SDL_Renderer *ren, const PlayOpts *o);
