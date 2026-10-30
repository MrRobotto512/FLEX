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
    int aspect;      /* ASPECT_* to start with */
    int upscaled;    /* the server is upscaling this stream (shown in the OSD) */
    int volume;      /* percent, 0..200 */
    int may_give_up; /* the file itself: stop if the link can't keep up, so the caller asks for a conversion */
    TTF_Font *font;  /* OSD text */
} PlayOpts;

/* Picture shapes, cycled with Square. AUTO honours the file's own aspect
 * ratio; the forced ratios fix files with wrong metadata; ZOOM fills the
 * screen and crops; STRETCH fills it and distorts. */
enum { ASPECT_AUTO, ASPECT_16_9, ASPECT_4_3, ASPECT_235, ASPECT_ZOOM, ASPECT_STRETCH, ASPECT_COUNT };

typedef struct {
    double position; /* media time reached; -1 when the stream could not be opened */
    double seek_to;  /* >= 0: the user seeked in a non-seekable stream, restart it here */
    int ended;       /* played to the end */
    int aspect;      /* ASPECT_* in use when playback stopped */
    int toggle_upscale; /* Triangle: restart at `position` with upscaling switched */
    int volume;      /* percent in use when playback stopped */
    int link_kbps;   /* how fast the stream came in while the reader was busy; 0 = not measured */
    int starved;     /* may_give_up: ran dry repeatedly; restart at `position` with a stream sized to the link */
} PlayResult;

/* Plays one stream until it ends, the user backs out, or (non-seekable
 * streams) the user seeks. Decoding is on the CPU, so the server must have
 * sent the `ps5` profile (<=1080p H.264 + AAC).
 *
 * Controls: Cross pause, Circle stop, Square picture shape, Triangle
 * server upscaling on/off, L1/R1 volume -/+10 %, D-pad
 * left/right -/+10 s, up/down +/-60 s (hold to repeat; the jump happens
 * 0.7 s after the last press so several presses add up). */
PlayResult player_play(SDL_Renderer *ren, const PlayOpts *o);
