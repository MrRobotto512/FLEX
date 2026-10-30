#include "player.h"

#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/cpu.h>
#include <libavutil/imgutils.h>
#include <libavutil/opt.h>
#include <libswresample/swresample.h>
#include <libswscale/swscale.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

/* Four threads, so a slow network read or a heavy frame never stalls the
 * others:
 *   reader - demuxes ahead into packet queues, up to BUFFER_S of video
 *            (BUFFER_BYTES at most); seeks happen here
 *   video  - decodes into a small queue of frames
 *   audio  - decodes, converts to S16 stereo and fills the ring the audio
 *            device drains; what has played is the clock
 *   main   - input, and shows each frame when the clock reaches it (SDL
 *            rendering stays on this thread)
 * Frames that are already late are skipped, so a picture too heavy for the
 * CPU drops frames instead of dragging the sound along. A seek bumps
 * `serial`: packets and frames from before it are thrown away, and each
 * decoder flushes when it reaches the marker the reader queues after seeking.
 * Transcoded HLS sessions are not seekable, so the caller restarts them. */

#define AUDIO_RATE 48000
#define AUDIO_CH 2
#define BYTES_PER_SEC (AUDIO_RATE * AUDIO_CH * 2)
#define MAX_QUEUED_AUDIO (BYTES_PER_SEC) /* keep ~1 s in the ring */
#define RING_BYTES (2 * BYTES_PER_SEC)
#define VOLUME_STEP 10   /* percent */
#define VOLUME_MAX 200   /* quiet rips (5.1 folded to stereo) need more than 100 */
#define LATE_DROP_S 0.08 /* a frame this far behind the clock is skipped */
#define MAX_DROPS_IN_ROW 6
#define BUFFER_S 30.0           /* read this far ahead of the video decoder */
#define BUFFER_BYTES (64 << 20) /* ...but never hold more than this */
#define FRAME_QUEUE 8           /* decoded frames waiting for their time */
#define AUDIO_DRY_S 1.0         /* video waiting this long on silent audio: pace by wall time */
#define BUFFERING_MS 300        /* no frame for this long: say "Buffering" */
#define REBUFFER_S 3.0          /* after running dry, wait for this much before playing on */
#define GIVE_UP_MS 90000        /* running dry twice within this: the link can't carry the file */
#define SCREEN_W 1920
#define SCREEN_H 1080
#define SEEK_COMMIT_MS 700
#define OSD_MS 3000
#define REPEAT_DELAY_MS 400
#define REPEAT_RATE_MS 150

static const char *aspect_names[ASPECT_COUNT] = {"Original", "16:9", "4:3", "2.35:1", "Zoom", "Stretch"};

/* OSD text is rendered once per change, not once per frame */
typedef struct {
    char text[160];
    SDL_Texture *tex;
    int w, h;
} OsdLine;
enum { OSD_TITLE, OSD_TIME, OSD_STATE, OSD_PERF, OSD_LINES };

/* ---------- queues between the threads ---------- */

enum { MARK_NONE, MARK_FLUSH, MARK_EOF };

typedef struct PktNode {
    AVPacket *pkt; /* NULL for a marker */
    int marker;    /* MARK_* */
    int serial;
    double ts;     /* stream time of the packet */
    struct PktNode *next;
} PktNode;

typedef struct {
    PktNode *head, *tail;
    int bytes;
    double last_ts; /* newest packet in the queue */
    SDL_mutex *mu;
    SDL_cond *cond;
} PktQueue;

typedef struct {
    AVFrame *f[FRAME_QUEUE];
    double pts[FRAME_QUEUE];
    int serial[FRAME_QUEUE];
    int head, count;
    SDL_mutex *mu;
    SDL_cond *cond; /* a slot freed up */
} FrameQueue;

typedef struct {
    const PlayOpts *o;
    SDL_Renderer *ren;
    AVFormatContext *fmt;
    AVCodecContext *vdec, *adec;
    int vidx, aidx;
    double vtb, atb, start; /* stream time bases; container start time (s) */
    struct SwsContext *sws;
    SwrContext *swr;
    SDL_Texture *tex;
    int tex_w, tex_h;
    Uint32 tex_fmt;
    double sar;   /* pixel aspect of the stream */

    /* The video thread scales every frame to the size it takes on screen
     * (Lanczos, on several threads), so a 4K picture becomes a sharp 1080p
     * one, small ones are upscaled well, and the main thread only copies. */
    volatile int src_w, src_h, src_bt709; /* decoded picture, from the video thread */
    int laid_w, laid_h;                   /* what layout() last placed */
    SDL_atomic_t want_w, want_h;          /* size of the scaled frames (0: as decoded) */
    struct SwsContext *vsws;              /* video thread */
    int vsws_key[7];
    int aspect;   /* ASPECT_* */
    SDL_Rect dst; /* where the picture goes on screen */
    SDL_AudioDeviceID dev;

    SDL_Thread *reader, *vthread, *athread;
    SDL_atomic_t abort;  /* every thread stops */
    SDL_atomic_t serial; /* bumped by each seek */
    PktQueue vq, aq;
    FrameQueue fq;

    /* seek request, main -> reader */
    SDL_mutex *seek_mu;
    int seek_req, seek_serial;
    int64_t seek_ts;

    volatile double video_read_ts; /* stream time of the packet the video decoder took last */
    SDL_atomic_t video_eof;        /* serial + 1 once the video decoder drained at the end, else 0 */
    SDL_atomic_t audio_eof;        /* same for audio */
    volatile int want_fast_decode; /* main asks the video thread to skip deblocking */
    double dec_last_pts;           /* video thread: for frames without a timestamp */

    /* Decoded audio waits in a ring the device callback drains (the callback
     * runs with the device locked), so a volume change applies to what plays
     * next rather than to audio queued a second earlier. The ring,
     * audio_written and resync_audio are guarded by the device lock. */
    Uint8 *ring;
    int ring_head, ring_fill;
    int volume; /* percent */

    double audio_written;       /* stream time at the end of the queued audio */
    int resync_audio;           /* the next audio frame sets audio_written */
    volatile int wall_fallback; /* no usable audio: pace video by wall time */
    int wall_started;           /* wall-time pacing anchored to a presented frame */
    Uint32 dry_since;           /* a frame has waited on an empty ring since */

    double wall_base;            /* wall-time pacing: clock = now - wall_base */
    volatile double drop_before; /* after a seek: skip frames earlier than this stream time */
    double last_pts;             /* stream time of the last presented frame */

    int paused, quit, ended, buffering;
    int rebuffering;    /* ran dry: the clock is held until REBUFFER_S is read ahead */
    Uint32 rebuffer_since, last_dry;
    int starved;
    SDL_atomic_t read_eof; /* serial + 1 once the reader hit the end, else 0 */
    double read_bytes, read_secs; /* reader: what came in while it was reading, for the link speed */
    int gate;           /* at the start and after a seek: audio waits for the first frame */
    Uint32 gate_since, gate_opened;
    Uint32 empty_since; /* the frame queue has been empty since */
    double pending;     /* media time the user is seeking to, -1 = none */
    Uint32 seek_at;     /* commit the pending seek at this tick */
    double result_seek;
    int toggle_upscale;
    int held;
    Uint32 next_repeat;
    Uint32 osd_until;
    Uint32 next_buffer_draw;
    OsdLine osd[OSD_LINES];

    /* how playback keeps up, shown on the OSD */
    double decode_acc;         /* video thread: ms in the decoder since the last frame came out */
    double decode_ms, draw_ms; /* moving averages per frame */
    int dropped, drops_in_row;
    Uint32 drop_window; /* start of the 2 s window counting recent drops */
    int drop_count, fast_decode;
    char perf[200];
    Uint32 perf_next;
} Player;

static double now_s(void) { return SDL_GetTicks() / 1000.0; }

static double ms_since(Uint64 t0) {
    return (double)(SDL_GetPerformanceCounter() - t0) * 1000.0 / (double)SDL_GetPerformanceFrequency();
}

static void average(double *avg, double v) { *avg = *avg > 0 ? *avg * 0.9 + v * 0.1 : v; }

static int aborted(Player *p) { return SDL_AtomicGet(&p->abort); }
static int cur_serial(Player *p) { return SDL_AtomicGet(&p->serial); }

static void pq_init(PktQueue *q) {
    memset(q, 0, sizeof *q);
    q->mu = SDL_CreateMutex();
    q->cond = SDL_CreateCond();
}

static void node_free(PktNode *n) {
    if (n->pkt) av_packet_free(&n->pkt);
    free(n);
}

/* pq_put queues pkt (its data moves into the queue) or, with pkt NULL, a marker. */
static void pq_put(PktQueue *q, AVPacket *pkt, int marker, int serial, double ts) {
    PktNode *n = calloc(1, sizeof *n);
    if (!n) return;
    n->marker = marker;
    n->serial = serial;
    n->ts = ts;
    if (pkt) {
        n->pkt = av_packet_alloc();
        av_packet_move_ref(n->pkt, pkt);
    }
    SDL_LockMutex(q->mu);
    if (q->tail) q->tail->next = n;
    else q->head = n;
    q->tail = n;
    if (n->pkt) {
        q->bytes += n->pkt->size;
        q->last_ts = ts;
    }
    SDL_CondSignal(q->cond);
    SDL_UnlockMutex(q->mu);
}

/* pq_get waits for the next node; NULL when the player is stopping. */
static PktNode *pq_get(Player *p, PktQueue *q) {
    SDL_LockMutex(q->mu);
    while (!q->head && !aborted(p)) SDL_CondWaitTimeout(q->cond, q->mu, 20);
    PktNode *n = aborted(p) ? NULL : q->head;
    if (n) {
        q->head = n->next;
        if (!q->head) q->tail = NULL;
        if (n->pkt) q->bytes -= n->pkt->size;
    }
    SDL_UnlockMutex(q->mu);
    return n;
}

static void pq_clear(PktQueue *q) {
    SDL_LockMutex(q->mu);
    while (q->head) {
        PktNode *n = q->head;
        q->head = n->next;
        node_free(n);
    }
    q->tail = NULL;
    q->bytes = 0;
    SDL_UnlockMutex(q->mu);
}

static int pq_empty(PktQueue *q) {
    SDL_LockMutex(q->mu);
    int e = q->head == NULL;
    SDL_UnlockMutex(q->mu);
    return e;
}

static void pq_destroy(PktQueue *q) {
    if (!q->mu) return;
    pq_clear(q);
    SDL_DestroyMutex(q->mu);
    SDL_DestroyCond(q->cond);
}

static void fq_init(FrameQueue *q) {
    memset(q, 0, sizeof *q);
    for (int i = 0; i < FRAME_QUEUE; i++) q->f[i] = av_frame_alloc();
    q->mu = SDL_CreateMutex();
    q->cond = SDL_CreateCond();
}

/* fq_put moves frame into the queue, waiting for a free slot; frames from
 * before a seek are dropped instead. */
static void fq_put(Player *p, AVFrame *frame, double pts, int serial) {
    FrameQueue *q = &p->fq;
    SDL_LockMutex(q->mu);
    while (q->count == FRAME_QUEUE && !aborted(p) && serial == cur_serial(p))
        SDL_CondWaitTimeout(q->cond, q->mu, 20);
    if (q->count < FRAME_QUEUE && !aborted(p) && serial == cur_serial(p)) {
        int i = (q->head + q->count) % FRAME_QUEUE;
        av_frame_move_ref(q->f[i], frame);
        q->pts[i] = pts;
        q->serial[i] = serial;
        q->count++;
    }
    SDL_UnlockMutex(q->mu);
    av_frame_unref(frame);
}

/* fq_peek returns the oldest frame (main thread only), or NULL. */
static AVFrame *fq_peek(FrameQueue *q, double *pts, int *serial) {
    SDL_LockMutex(q->mu);
    AVFrame *f = NULL;
    if (q->count) {
        f = q->f[q->head];
        *pts = q->pts[q->head];
        *serial = q->serial[q->head];
    }
    SDL_UnlockMutex(q->mu);
    return f;
}

static void fq_pop(FrameQueue *q) {
    SDL_LockMutex(q->mu);
    if (q->count) {
        av_frame_unref(q->f[q->head]);
        q->head = (q->head + 1) % FRAME_QUEUE;
        q->count--;
    }
    SDL_CondSignal(q->cond);
    SDL_UnlockMutex(q->mu);
}

static int fq_count(FrameQueue *q) {
    SDL_LockMutex(q->mu);
    int n = q->count;
    SDL_UnlockMutex(q->mu);
    return n;
}

static void fq_clear(FrameQueue *q) {
    while (fq_count(q)) fq_pop(q);
}

static void fq_destroy(FrameQueue *q) {
    if (!q->mu) return;
    fq_clear(q);
    for (int i = 0; i < FRAME_QUEUE; i++) av_frame_free(&q->f[i]);
    SDL_DestroyMutex(q->mu);
    SDL_DestroyCond(q->cond);
}

/* seconds of video the reader holds ahead of the video decoder */
static double buffered_s(Player *p) {
    SDL_LockMutex(p->vq.mu);
    double b = p->vq.head ? p->vq.last_ts - p->video_read_ts : 0;
    SDL_UnlockMutex(p->vq.mu);
    return b > 0 ? b : 0;
}

/* ---------- audio ring ---------- */

static void scale_s16(Sint16 *dst, const Sint16 *src, int n, int volume) {
    if (volume == 100) {
        memcpy(dst, src, (size_t)n * 2);
        return;
    }
    for (int i = 0; i < n; i++) {
        int v = src[i] * volume / 100;
        dst[i] = (Sint16)(v > 32767 ? 32767 : v < -32768 ? -32768 : v);
    }
}

static void audio_cb(void *userdata, Uint8 *out, int len) {
    Player *p = userdata;
    int n = len < p->ring_fill ? len : p->ring_fill;
    for (int done = 0; done < n;) {
        int chunk = RING_BYTES - p->ring_head;
        if (chunk > n - done) chunk = n - done;
        scale_s16((Sint16 *)(out + done), (const Sint16 *)(p->ring + p->ring_head), chunk / 2, p->volume);
        p->ring_head = (p->ring_head + chunk) % RING_BYTES;
        done += chunk;
    }
    p->ring_fill -= n;
    if (n < len) memset(out + n, 0, (size_t)(len - n)); /* ran dry: silence */
}

/* audio_write adds S16 stereo to the ring, keeping at most MAX_QUEUED_AUDIO
 * there; apts is the stream time of buf. Returns the bytes taken, or -1 when
 * a seek made this audio stale. */
static int audio_write(Player *p, const Uint8 *buf, int len, int serial, double apts) {
    SDL_LockAudioDevice(p->dev);
    if (serial != cur_serial(p)) {
        SDL_UnlockAudioDevice(p->dev);
        return -1;
    }
    if (p->resync_audio) {
        p->audio_written = apts;
        p->resync_audio = 0;
        p->wall_fallback = 0;
    }
    int space = MAX_QUEUED_AUDIO - p->ring_fill;
    if (len > space) len = space > 0 ? space & ~3 : 0;
    int tail = (p->ring_head + p->ring_fill) % RING_BYTES;
    for (int done = 0; done < len;) {
        int chunk = RING_BYTES - tail;
        if (chunk > len - done) chunk = len - done;
        memcpy(p->ring + tail, buf + done, (size_t)chunk);
        tail = (tail + chunk) % RING_BYTES;
        done += chunk;
    }
    p->ring_fill += len;
    p->audio_written += (double)len / BYTES_PER_SEC;
    SDL_UnlockAudioDevice(p->dev);
    return len;
}

static int audio_queued(Player *p) {
    if (!p->dev) return 0;
    SDL_LockAudioDevice(p->dev);
    int n = p->ring_fill;
    SDL_UnlockAudioDevice(p->dev);
    return n;
}

static double clock_s(Player *p) {
    if (p->adec && p->dev && !p->wall_fallback) {
        SDL_LockAudioDevice(p->dev);
        double c = p->audio_written - (double)p->ring_fill / BYTES_PER_SEC;
        SDL_UnlockAudioDevice(p->dev);
        return c;
    }
    return now_s() - p->wall_base;
}

/* media time of what is on screen (or of the pending seek target) */
static double position(Player *p) {
    if (p->pending >= 0) return p->pending;
    return p->o->offset + p->last_pts;
}

/* trace prints the clocks about once a second when FLEX_PLAYER_TRACE is set
 * (stderr reaches the dev machine through websrv's log relay). */
static void trace(Player *p, const char *what) {
    static int on = -1;
    static Uint32 next;
    if (on < 0) on = getenv("FLEX_PLAYER_TRACE") != NULL;
    if (!on || SDL_GetTicks() < next) return;
    next = SDL_GetTicks() + 1000;
    fprintf(stderr,
            "trace: %s pts=%.2f clock=%.2f queued=%d frames=%d buffer=%.1fs/%dKB decode=%.1fms draw=%.1fms "
            "dropped=%d\n",
            what, p->last_pts, clock_s(p), audio_queued(p), fq_count(&p->fq), buffered_s(p),
            (p->vq.bytes + p->aq.bytes) / 1024, p->decode_ms, p->draw_ms, p->dropped);
}

static int osd_visible(Player *p) {
    return p->paused || p->buffering || p->pending >= 0 || SDL_GetTicks() < p->osd_until;
}
static void show_osd(Player *p) { p->osd_until = SDL_GetTicks() + OSD_MS; }
static int seek_ready(Player *p) { return p->pending >= 0 && SDL_GetTicks() >= p->seek_at; }

/* ---------- OSD ---------- */

static void fmt_time(char *b, size_t n, double s) {
    int t = s < 0 ? 0 : (int)(s + 0.5);
    if (t >= 3600) snprintf(b, n, "%d:%02d:%02d", t / 3600, t / 60 % 60, t % 60);
    else snprintf(b, n, "%d:%02d", t / 60, t % 60);
}

static void osd_text(Player *p, int slot, const char *text, int x, int y, int right_align) {
    OsdLine *l = &p->osd[slot];
    if (!p->o->font || !text[0]) return;
    if (!l->tex || strcmp(l->text, text) != 0) {
        if (l->tex) SDL_DestroyTexture(l->tex);
        l->tex = NULL;
        snprintf(l->text, sizeof l->text, "%s", text);
        SDL_Surface *s = TTF_RenderUTF8_Blended(p->o->font, text, (SDL_Color){232, 234, 240, 255});
        if (!s) return;
        l->tex = SDL_CreateTextureFromSurface(p->ren, s);
        l->w = s->w;
        l->h = s->h;
        SDL_FreeSurface(s);
        if (!l->tex) return;
    }
    SDL_RenderCopy(p->ren, l->tex, NULL, &(SDL_Rect){right_align ? x - l->w : x, y, l->w, l->h});
}

/* perf_text sums up how playback keeps up; refreshed once a second so the
 * OSD isn't re-rendered every frame. With frame threading the decoder's own
 * call time says little, so how full the frame queue stays is shown instead:
 * near FRAME_QUEUE the decoder is ahead, near 0 it can't keep up. */
static const char *perf_text(Player *p) {
    if (!p->perf[0] || SDL_GetTicks() >= p->perf_next) {
        p->perf_next = SDL_GetTicks() + 1000;
        char size[48];
        if (p->src_w == p->tex_w && p->src_h == p->tex_h) snprintf(size, sizeof size, "%dx%d", p->tex_w, p->tex_h);
        else snprintf(size, sizeof size, "%dx%d → %dx%d", p->src_w, p->src_h, p->tex_w, p->tex_h);
        snprintf(p->perf, sizeof p->perf,
                 "%s  ·  frames %d/%d  ·  draw %.0f ms  ·  buffer %.0f s  ·  %d dropped  ·  %d threads%s",
                 size, fq_count(&p->fq), FRAME_QUEUE, p->draw_ms, buffered_s(p), p->dropped,
                 p->vdec ? p->vdec->thread_count : 0, p->fast_decode ? "  ·  fast decode" : "");
    }
    return p->perf;
}

static void draw_osd(Player *p) {
    if (!osd_visible(p)) return;
    SDL_Renderer *r = p->ren;
    SDL_SetRenderDrawBlendMode(r, SDL_BLENDMODE_BLEND);
    SDL_SetRenderDrawColor(r, 0, 0, 0, 170);
    SDL_RenderFillRect(r, &(SDL_Rect){0, SCREEN_H - 160, SCREEN_W, 160});

    double dur = p->o->duration, pos = p->o->offset + p->last_pts;
    char now[32], total[32], line[96];
    fmt_time(now, sizeof now, p->pending >= 0 ? p->pending : pos);
    fmt_time(total, sizeof total, dur);
    snprintf(line, sizeof line, "%s%s / %s", p->pending >= 0 ? "Seek to " : "", now, total);
    osd_text(p, OSD_TITLE, p->o->title ? p->o->title : "", 80, SCREEN_H - 140, 0);
    osd_text(p, OSD_TIME, line, SCREEN_W - 80, SCREEN_H - 140, 1);
    char state[128];
    snprintf(state, sizeof state, "%s%s  ·  Upscale %s  ·  Volume %d%%",
             p->paused ? "Paused  ·  " : p->buffering ? "Buffering…  ·  " : "", aspect_names[p->aspect],
             p->o->upscaled ? "on" : "off", p->volume);
    osd_text(p, OSD_STATE, state, SCREEN_W / 2 - 300, SCREEN_H - 140, 0);
    osd_text(p, OSD_PERF, perf_text(p), SCREEN_W - 80, SCREEN_H - 50, 1);

    if (dur > 0) {
        int x = 80, y = SCREEN_H - 70, w = SCREEN_W - 160;
        double f = pos / dur;
        if (f < 0) f = 0;
        if (f > 1) f = 1;
        SDL_SetRenderDrawColor(r, 255, 255, 255, 60);
        SDL_RenderFillRect(r, &(SDL_Rect){x, y, w, 8});
        SDL_SetRenderDrawColor(r, 90, 169, 255, 255);
        SDL_RenderFillRect(r, &(SDL_Rect){x, y, (int)(w * f), 8});
        if (p->pending >= 0) {
            double t = p->pending / dur;
            if (t > 1) t = 1;
            SDL_SetRenderDrawColor(r, 255, 255, 255, 255);
            SDL_RenderFillRect(r, &(SDL_Rect){x + (int)(w * t) - 3, y - 10, 6, 28});
        }
    }
}

/* redraw shows the current frame (letterboxed) plus the OSD. */
static void redraw(Player *p) {
    SDL_SetRenderDrawColor(p->ren, 0, 0, 0, 255);
    SDL_RenderClear(p->ren);
    if (p->tex) SDL_RenderCopy(p->ren, p->tex, NULL, &p->dst);
    draw_osd(p);
    SDL_RenderPresent(p->ren);
}

/* ---------- input ---------- */

static double seek_delta(int button) {
    switch (button) {
    case SDL_CONTROLLER_BUTTON_DPAD_LEFT: return -10;
    case SDL_CONTROLLER_BUTTON_DPAD_RIGHT: return 10;
    case SDL_CONTROLLER_BUTTON_DPAD_UP: return 60;
    case SDL_CONTROLLER_BUTTON_DPAD_DOWN: return -60;
    }
    return 0;
}

static int volume_delta(int button) {
    switch (button) {
    case SDL_CONTROLLER_BUTTON_LEFTSHOULDER: return -VOLUME_STEP;
    case SDL_CONTROLLER_BUTTON_RIGHTSHOULDER: return VOLUME_STEP;
    }
    return 0;
}

static int key_button(SDL_Keycode k) {
    switch (k) {
    case SDLK_LEFT: return SDL_CONTROLLER_BUTTON_DPAD_LEFT;
    case SDLK_RIGHT: return SDL_CONTROLLER_BUTTON_DPAD_RIGHT;
    case SDLK_UP: return SDL_CONTROLLER_BUTTON_DPAD_UP;
    case SDLK_DOWN: return SDL_CONTROLLER_BUTTON_DPAD_DOWN;
    case SDLK_SPACE: case SDLK_RETURN: return SDL_CONTROLLER_BUTTON_A;
    case SDLK_ESCAPE: return SDL_CONTROLLER_BUTTON_B;
    case SDLK_TAB: return SDL_CONTROLLER_BUTTON_X;
    case SDLK_u: return SDL_CONTROLLER_BUTTON_Y;
    case SDLK_MINUS: return SDL_CONTROLLER_BUTTON_LEFTSHOULDER;
    case SDLK_EQUALS: return SDL_CONTROLLER_BUTTON_RIGHTSHOULDER;
    }
    return -1;
}

/* layout places the picture for the current shape and tells the video
 * thread the size to scale frames to. */
static void layout(Player *p) {
    p->laid_w = p->src_w;
    p->laid_h = p->src_h;
    if (!p->src_w || !p->src_h) return;
    double ar = p->src_w * p->sar / p->src_h;
    int w = SCREEN_W, h;
    switch (p->aspect) {
    case ASPECT_16_9: ar = 16.0 / 9; break;
    case ASPECT_4_3: ar = 4.0 / 3; break;
    case ASPECT_235: ar = 2.35; break;
    }
    if (p->aspect == ASPECT_STRETCH) {
        h = SCREEN_H;
    } else {
        h = (int)(SCREEN_W / ar + 0.5);
        if (p->aspect == ASPECT_ZOOM ? h < SCREEN_H : h > SCREEN_H) {
            h = SCREEN_H;
            w = (int)(SCREEN_H * ar + 0.5);
        }
    }
    p->dst = (SDL_Rect){(SCREEN_W - w) / 2, (SCREEN_H - h) / 2, w, h}; /* ZOOM overflows; the renderer clips */
    SDL_AtomicSet(&p->want_w, (w < 2 * SCREEN_W ? w : 2 * SCREEN_W) & ~1);
    SDL_AtomicSet(&p->want_h, (h < 2 * SCREEN_H ? h : 2 * SCREEN_H) & ~1);
}

/* audio_run plays the device unless the user paused or the gate is shut. */
static void audio_run(Player *p) {
    if (p->dev) SDL_PauseAudioDevice(p->dev, p->paused || p->gate);
}

static void set_paused(Player *p, int paused) {
    if (p->paused == paused) return;
    p->paused = paused;
    audio_run(p);
    if (!paused) p->wall_base = now_s() - p->last_pts;
}

/* shut_gate holds the audio (and so the clock) until a video frame is
 * ready. After a seek the video decoder starts at the keyframe before the
 * target and has to decode up to it, while the audio is there at once;
 * letting the clock run meanwhile would make the first frames late. */
static void shut_gate(Player *p) {
    p->gate = 1;
    p->gate_since = SDL_GetTicks();
    audio_run(p);
}

static void step_seek(Player *p, double delta) {
    double base = position(p), t = base + delta, max = p->o->duration > 10 ? p->o->duration - 5 : base;
    if (t > max) t = max;
    if (t < 0) t = 0;
    p->pending = t;
    p->seek_at = SDL_GetTicks() + SEEK_COMMIT_MS;
    show_osd(p);
}

static void change_volume(Player *p, int delta) {
    int v = p->volume + delta;
    if (v < 0) v = 0;
    if (v > VOLUME_MAX) v = VOLUME_MAX;
    if (p->dev) SDL_LockAudioDevice(p->dev);
    p->volume = v;
    if (p->dev) SDL_UnlockAudioDevice(p->dev);
    show_osd(p);
    if (p->paused) redraw(p);
}

/* repeat_press acts on a button that repeats while held (seek, volume);
 * returns 0 for any other button. */
static int repeat_press(Player *p, int b) {
    if (seek_delta(b) != 0) step_seek(p, seek_delta(b));
    else if (volume_delta(b) != 0) change_volume(p, volume_delta(b));
    else return 0;
    return 1;
}

static void handle_events(Player *p) {
    SDL_Event e;
    while (SDL_PollEvent(&e)) {
        int b = -1;
        if (e.type == SDL_QUIT) p->quit = 1;
        if (e.type == SDL_CONTROLLERBUTTONDOWN) b = e.cbutton.button;
        if (e.type == SDL_KEYDOWN && !e.key.repeat) b = key_button(e.key.keysym.sym);
        if ((e.type == SDL_CONTROLLERBUTTONUP && e.cbutton.button == p->held) ||
            (e.type == SDL_KEYUP && key_button(e.key.keysym.sym) == p->held))
            p->held = -1;
        if (b < 0) continue;
        show_osd(p);
        if (repeat_press(p, b)) {
            p->held = b;
            p->next_repeat = SDL_GetTicks() + REPEAT_DELAY_MS;
        } else if (b == SDL_CONTROLLER_BUTTON_B) {
            p->quit = 1;
        } else if (b == SDL_CONTROLLER_BUTTON_A) {
            set_paused(p, !p->paused);
        } else if (b == SDL_CONTROLLER_BUTTON_Y) {
            p->toggle_upscale = 1;
            p->quit = 1;
        } else if (b == SDL_CONTROLLER_BUTTON_X) {
            p->aspect = (p->aspect + 1) % ASPECT_COUNT;
            layout(p);
            if (p->paused) redraw(p);
        }
    }
    if (p->held >= 0 && SDL_GetTicks() >= p->next_repeat) {
        repeat_press(p, p->held);
        p->next_repeat = SDL_GetTicks() + REPEAT_RATE_MS;
    }
}

/* ---------- seeking ---------- */

/* commit_seek performs the pending seek: everything queued becomes stale and
 * the reader seeks. Non-seekable streams end here and report the target to
 * the caller. */
static void commit_seek(Player *p) {
    double target = p->pending;
    p->pending = -1;
    p->held = -1;
    if (!p->o->seekable) {
        p->result_seek = target;
        p->quit = 1;
        return;
    }
    double st = target - p->o->offset;
    p->drop_before = st;
    if (p->dev) SDL_LockAudioDevice(p->dev);
    SDL_AtomicIncRef(&p->serial);
    p->ring_fill = 0;
    p->audio_written = st;
    p->resync_audio = 1;
    if (p->dev) SDL_UnlockAudioDevice(p->dev);

    SDL_LockMutex(p->seek_mu);
    p->seek_ts = (int64_t)((st + p->start) * AV_TIME_BASE);
    p->seek_serial = cur_serial(p);
    p->seek_req = 1;
    SDL_UnlockMutex(p->seek_mu);
    fq_clear(&p->fq);

    p->wall_fallback = 0;
    p->wall_started = 0;
    p->dry_since = 0;
    p->rebuffering = 0;
    p->last_pts = st;
    p->wall_base = now_s() - st;
    shut_gate(p);
    set_paused(p, 0);
    show_osd(p);
}

/* ---------- threads ---------- */

static int interrupt_cb(void *opaque) { return aborted(opaque); }

static double packet_ts(AVPacket *pkt, double tb, double start) {
    int64_t ts = pkt->pts != AV_NOPTS_VALUE ? pkt->pts : pkt->dts;
    return ts == AV_NOPTS_VALUE ? 0 : ts * tb - start;
}

static int buffer_full(Player *p) {
    if (p->vq.bytes + p->aq.bytes > BUFFER_BYTES) return 1;
    return buffered_s(p) > BUFFER_S;
}

static int reader_thread(void *arg) {
    Player *p = arg;
    AVPacket *pkt = av_packet_alloc();
    int serial = 0, eof = 0;
    while (!aborted(p)) {
        SDL_LockMutex(p->seek_mu);
        int seek = p->seek_req;
        int64_t ts = p->seek_ts;
        if (seek) {
            serial = p->seek_serial;
            p->seek_req = 0;
        }
        SDL_UnlockMutex(p->seek_mu);
        if (seek) {
            if (avformat_seek_file(p->fmt, -1, INT64_MIN, ts, ts, 0) < 0)
                fprintf(stderr, "player: seek to %.1fs failed\n", ts / (double)AV_TIME_BASE - p->start);
            pq_clear(&p->vq);
            pq_clear(&p->aq);
            pq_put(&p->vq, NULL, MARK_FLUSH, serial, 0);
            if (p->adec) pq_put(&p->aq, NULL, MARK_FLUSH, serial, 0);
            eof = 0;
            SDL_AtomicSet(&p->read_eof, 0);
            continue;
        }
        if (eof || buffer_full(p)) {
            SDL_Delay(10);
            continue;
        }
        Uint64 t0 = SDL_GetPerformanceCounter();
        int ret = av_read_frame(p->fmt, pkt);
        if (ret < 0) {
            if (aborted(p)) break;
            if (ret != AVERROR_EOF)
                fprintf(stderr, "player: read failed (%s), treating it as the end\n", av_err2str(ret));
            pq_put(&p->vq, NULL, MARK_EOF, serial, 0);
            if (p->adec) pq_put(&p->aq, NULL, MARK_EOF, serial, 0);
            eof = 1;
            SDL_AtomicSet(&p->read_eof, serial + 1);
            continue;
        }
        /* time spent waiting in here is the link; with the buffer full the reader idles and isn't counted */
        p->read_secs += ms_since(t0) / 1000.0;
        p->read_bytes += pkt->size;
        if (pkt->stream_index == p->vidx) pq_put(&p->vq, pkt, MARK_NONE, serial, packet_ts(pkt, p->vtb, p->start));
        else if (pkt->stream_index == p->aidx && p->adec)
            pq_put(&p->aq, pkt, MARK_NONE, serial, packet_ts(pkt, p->atb, p->start));
        av_packet_unref(pkt);
    }
    av_packet_free(&pkt);
    return 0;
}

/* matrix: the YUV->RGB coefficients a frame was made with. Untagged video
 * follows the usual rule: HD is BT.709, SD is BT.601. */
static int matrix(const AVFrame *f) {
    switch (f->colorspace) {
    case AVCOL_SPC_BT709: return SWS_CS_ITU709;
    case AVCOL_SPC_BT470BG:
    case AVCOL_SPC_SMPTE170M: return SWS_CS_ITU601;
    case AVCOL_SPC_SMPTE240M: return SWS_CS_SMPTE240M;
    case AVCOL_SPC_FCC: return SWS_CS_FCC;
    case AVCOL_SPC_BT2020_NCL:
    case AVCOL_SPC_BT2020_CL: return SWS_CS_BT2020;
    default: return f->height >= 720 ? SWS_CS_ITU709 : SWS_CS_ITU601;
    }
}

static int full_range(const AVFrame *f) {
    return f->color_range == AVCOL_RANGE_JPEG || f->format == AV_PIX_FMT_YUVJ420P || f->format == AV_PIX_FMT_YUVJ422P ||
           f->format == AV_PIX_FMT_YUVJ444P;
}

/* to_display returns the frame ready for the screen: at the size it takes
 * there (Lanczos on several threads, so 4K comes out as a sharp 1080p
 * picture and SD/720p are upscaled better than the renderer's bilinear
 * stretch) and already in RGB, converted with the frame's own matrix and
 * range rather than whatever SDL would infer from the texture, and off the
 * main thread. (Checked with FLEX_DUMP_FRAME: the result matches a textbook
 * decode of the file pixel for pixel.) out is scratch space. */
static AVFrame *to_display(Player *p, AVFrame *in, AVFrame *out) {
    if (in->width != p->src_w || in->height != p->src_h) {
        p->sar = in->sample_aspect_ratio.num && in->sample_aspect_ratio.den ? av_q2d(in->sample_aspect_ratio) : 1.0;
        p->src_bt709 = matrix(in) == SWS_CS_ITU709;
        p->src_h = in->height;
        p->src_w = in->width; /* last: the main thread lays out when it changes */
    }
    int w = SDL_AtomicGet(&p->want_w), h = SDL_AtomicGet(&p->want_h);
    if (w <= 0 || h <= 0) w = in->width, h = in->height; /* not laid out yet */

    int key[7] = {in->width, in->height, in->format, w, h, matrix(in), full_range(in)};
    if (!p->vsws || memcmp(key, p->vsws_key, sizeof key) != 0) {
        sws_freeContext(p->vsws);
        p->vsws = sws_alloc_context();
        av_opt_set_int(p->vsws, "srcw", in->width, 0);
        av_opt_set_int(p->vsws, "srch", in->height, 0);
        av_opt_set_int(p->vsws, "src_format", in->format, 0);
        av_opt_set_int(p->vsws, "dstw", w, 0);
        av_opt_set_int(p->vsws, "dsth", h, 0);
        av_opt_set_int(p->vsws, "dst_format", AV_PIX_FMT_BGRA, 0); /* SDL's ARGB8888 in memory order */
        /* full-resolution chroma both ways: sharper colour edges than the default */
        av_opt_set_int(p->vsws, "sws_flags",
                       SWS_LANCZOS | SWS_ACCURATE_RND | SWS_FULL_CHR_H_INT | SWS_FULL_CHR_H_INP, 0);
        av_opt_set_int(p->vsws, "threads", av_cpu_count() > 1 ? av_cpu_count() / 2 : 4, 0);
        if (sws_init_context(p->vsws, NULL, NULL) < 0) {
            fprintf(stderr, "player: cannot scale %dx%d to %dx%d\n", in->width, in->height, w, h);
            sws_freeContext(p->vsws);
            p->vsws = NULL;
            return in;
        }
        sws_setColorspaceDetails(p->vsws, sws_getCoefficients(matrix(in)), full_range(in),
                                 sws_getCoefficients(SWS_CS_DEFAULT), 1, 0, 1 << 16, 1 << 16);
        memcpy(p->vsws_key, key, sizeof key);
        static const char *names[] = {[SWS_CS_ITU709] = "BT.709", [SWS_CS_FCC] = "FCC", [SWS_CS_ITU601] = "BT.601",
                                      [SWS_CS_SMPTE240M] = "SMPTE 240M", [SWS_CS_BT2020] = "BT.2020"};
        fprintf(stderr, "player: %dx%d %s (%s, %s range) to %dx%d RGB (Lanczos)\n", in->width, in->height,
                av_get_pix_fmt_name(in->format), names[matrix(in)] ? names[matrix(in)] : "?",
                full_range(in) ? "full" : "limited", w, h);
    }
    out->format = AV_PIX_FMT_BGRA;
    out->width = w;
    out->height = h;
    if (sws_scale_frame(p->vsws, out, in) < 0) {
        av_frame_unref(out);
        return in;
    }
    av_frame_copy_props(out, in);
    return out;
}

static void decode_video(Player *p, AVPacket *pkt, AVFrame *frame, AVFrame *scaled, int serial) {
    if (p->want_fast_decode && !p->fast_decode) {
        p->fast_decode = 1;
        p->vdec->skip_loop_filter = AVDISCARD_ALL;
    }
    Uint64 t0 = SDL_GetPerformanceCounter();
    if (avcodec_send_packet(p->vdec, pkt) != 0 && pkt) return;
    for (;;) {
        int got = avcodec_receive_frame(p->vdec, frame) == 0;
        p->decode_acc += ms_since(t0);
        if (!got) break;
        average(&p->decode_ms, p->decode_acc);
        p->decode_acc = 0;
        double pts = frame->best_effort_timestamp == AV_NOPTS_VALUE ? p->dec_last_pts
                                                                    : frame->best_effort_timestamp * p->vtb - p->start;
        p->dec_last_pts = pts;
        if (pts >= p->drop_before - 0.05) {
            AVFrame *d = to_display(p, frame, scaled);
            fq_put(p, d, pts, serial);
            if (d != frame) av_frame_unref(frame);
        } else {
            av_frame_unref(frame); /* decoding up to a seek target */
        }
        t0 = SDL_GetPerformanceCounter(); /* waiting for a free slot isn't decoding */
    }
}

static int video_thread(void *arg) {
    Player *p = arg;
    AVFrame *frame = av_frame_alloc(), *scaled = av_frame_alloc();
    PktNode *n;
    while ((n = pq_get(p, &p->vq))) {
        if (n->marker == MARK_FLUSH) {
            avcodec_flush_buffers(p->vdec);
        } else if (n->marker == MARK_EOF) {
            decode_video(p, NULL, frame, scaled, n->serial); /* drain */
            SDL_AtomicSet(&p->video_eof, n->serial + 1);
        } else if (n->serial == cur_serial(p)) {
            p->video_read_ts = n->ts;
            decode_video(p, n->pkt, frame, scaled, n->serial);
        }
        node_free(n);
    }
    av_frame_free(&frame);
    av_frame_free(&scaled);
    sws_freeContext(p->vsws);
    return 0;
}

static void queue_audio(Player *p, AVFrame *f, double apts, int serial) {
    if (!p->swr) {
        AVChannelLayout out = AV_CHANNEL_LAYOUT_STEREO;
        swr_alloc_set_opts2(&p->swr, &out, AV_SAMPLE_FMT_S16, AUDIO_RATE, &f->ch_layout, f->format, f->sample_rate, 0,
                            NULL);
        swr_init(p->swr);
    }
    int out_samples = swr_get_out_samples(p->swr, f->nb_samples);
    uint8_t *buf = av_malloc(out_samples * AUDIO_CH * 2);
    int n = swr_convert(p->swr, &buf, out_samples, (const uint8_t **)f->extended_data, f->nb_samples);
    /* the ring holds ~1 s; wait for the speaker rather than run ahead */
    for (int len = n > 0 ? n * AUDIO_CH * 2 : 0, done = 0; done < len && !aborted(p);) {
        int w = audio_write(p, buf + done, len - done, serial, apts + (double)done / BYTES_PER_SEC);
        if (w < 0) break;
        done += w;
        if (done < len) SDL_Delay(5);
    }
    av_free(buf);
}

static int audio_thread(void *arg) {
    Player *p = arg;
    AVFrame *frame = av_frame_alloc();
    PktNode *n;
    double next = 0; /* for frames without a timestamp */
    while ((n = pq_get(p, &p->aq))) {
        if (n->marker == MARK_FLUSH) {
            avcodec_flush_buffers(p->adec);
            swr_free(&p->swr);
        } else if (n->marker == MARK_EOF) {
            SDL_AtomicSet(&p->audio_eof, n->serial + 1);
        } else if (n->serial == cur_serial(p) && avcodec_send_packet(p->adec, n->pkt) == 0) {
            while (avcodec_receive_frame(p->adec, frame) == 0) {
                double apts = frame->best_effort_timestamp == AV_NOPTS_VALUE
                                  ? next
                                  : frame->best_effort_timestamp * p->atb - p->start;
                next = apts + (double)frame->nb_samples / (frame->sample_rate ? frame->sample_rate : AUDIO_RATE);
                if (apts >= p->drop_before - 0.02) queue_audio(p, frame, apts, n->serial);
                av_frame_unref(frame); /* earlier ones are before the seek target */
            }
        }
        node_free(n);
    }
    av_frame_free(&frame);
    return 0;
}

/* ---------- decode setup ---------- */

static int open_decoder(AVFormatContext *fmt, enum AVMediaType type, AVCodecContext **out) {
    const AVCodec *codec = NULL;
    int idx = av_find_best_stream(fmt, type, -1, -1, &codec, 0);
    if (idx < 0) return -1;
    AVCodecContext *c = avcodec_alloc_context3(codec);
    avcodec_parameters_to_context(c, fmt->streams[idx]->codecpar);
    /* all cores; if FFmpeg can't count the PS5's, still use several */
    c->thread_count = av_cpu_count() > 1 ? 0 : 6;
    c->thread_type = FF_THREAD_FRAME | FF_THREAD_SLICE;
    if (avcodec_open2(c, codec, NULL) < 0) {
        avcodec_free_context(&c);
        return -1;
    }
    *out = c;
    return idx;
}

static void upload_frame(Player *p, AVFrame *f) {
    if (p->src_w != p->laid_w || p->src_h != p->laid_h) layout(p); /* a new picture size */
    int rgb = f->format == AV_PIX_FMT_BGRA; /* what to_display makes; YUV only if it failed */
    Uint32 fmt = rgb ? SDL_PIXELFORMAT_ARGB8888 : SDL_PIXELFORMAT_IYUV;
    if (!p->tex || p->tex_w != f->width || p->tex_h != f->height || p->tex_fmt != fmt) {
        if (p->tex) SDL_DestroyTexture(p->tex);
        if (!rgb) SDL_SetYUVConversionMode(p->src_bt709 ? SDL_YUV_CONVERSION_BT709 : SDL_YUV_CONVERSION_BT601);
        p->tex = SDL_CreateTexture(p->ren, fmt, SDL_TEXTUREACCESS_STREAMING, f->width, f->height);
        p->tex_w = f->width;
        p->tex_h = f->height;
        p->tex_fmt = fmt;
    }
    if (rgb) {
        SDL_UpdateTexture(p->tex, NULL, f->data[0], f->linesize[0]);
    } else if (f->format == AV_PIX_FMT_YUV420P || f->format == AV_PIX_FMT_YUVJ420P) {
        SDL_UpdateYUVTexture(p->tex, NULL, f->data[0], f->linesize[0], f->data[1], f->linesize[1], f->data[2],
                             f->linesize[2]);
    } else {
        /* unexpected pixel format (10-bit etc.): convert */
        p->sws = sws_getCachedContext(p->sws, f->width, f->height, f->format, f->width, f->height,
                                      AV_PIX_FMT_YUV420P, SWS_BILINEAR, NULL, NULL, NULL);
        uint8_t *dst[4];
        int dstls[4];
        av_image_alloc(dst, dstls, f->width, f->height, AV_PIX_FMT_YUV420P, 32);
        sws_scale(p->sws, (const uint8_t *const *)f->data, f->linesize, 0, f->height, dst, dstls);
        SDL_UpdateYUVTexture(p->tex, NULL, dst[0], dstls[0], dst[1], dstls[1], dst[2], dstls[2]);
        av_freep(&dst[0]);
    }
}

/* dump_frame: with FLEX_DUMP_FRAME set, 5 s into playback, saves the frame as
 * the player converted it (frame.bmp) and the screen as the renderer drew
 * it (screen.bmp) in the app folder, to compare colours with the source. */
static void dump_frame(Player *p, AVFrame *f) {
    static int on = -1, done;
    if (on < 0) on = getenv("FLEX_DUMP_FRAME") != NULL;
    if (!on || done || f->format != AV_PIX_FMT_BGRA || SDL_GetTicks() - p->gate_opened < 5000) return;
    done = 1;
    SDL_Surface *s = SDL_CreateRGBSurfaceWithFormatFrom(f->data[0], f->width, f->height, 32, f->linesize[0],
                                                        SDL_PIXELFORMAT_ARGB8888);
    if (s) {
        SDL_SaveBMP(s, "/data/homebrew/FLEX/frame.bmp");
        SDL_FreeSurface(s);
    }
    redraw(p); /* draw it, then read back what the renderer made of it */
    SDL_Surface *scr = SDL_CreateRGBSurfaceWithFormat(0, SCREEN_W, SCREEN_H, 32, SDL_PIXELFORMAT_ARGB8888);
    if (scr && SDL_RenderReadPixels(p->ren, NULL, SDL_PIXELFORMAT_ARGB8888, scr->pixels, scr->pitch) == 0)
        SDL_SaveBMP(scr, "/data/homebrew/FLEX/screen.bmp");
    if (scr) SDL_FreeSurface(scr);
    SDL_RendererInfo ri;
    SDL_GetRendererInfo(p->ren, &ri);
    fprintf(stderr, "player: dumped the frame at pts %.3f (renderer %s, window format %s)\n", p->last_pts, ri.name,
            SDL_GetPixelFormatName(SDL_GetWindowPixelFormat(SDL_RenderGetWindow(p->ren))));
}

/* note_drop: when frames keep coming late, have the video thread stop
 * deblocking (about a quarter of H.264 decoding time) for the rest of this
 * stream. */
static void note_drop(Player *p) {
    p->dropped++;
    p->drops_in_row++;
    Uint32 now = SDL_GetTicks();
    if (now - p->drop_window > 2000) {
        p->drop_window = now;
        p->drop_count = 0;
    }
    /* drops right after the start or a seek are the pipeline filling, not a slow decoder */
    if (++p->drop_count >= 5 && !p->want_fast_decode && now - p->gate_opened > 3000) {
        p->want_fast_decode = 1;
        fprintf(stderr, "player: falling behind (%d frames queued, draw %.1f ms), skipping the deblocking filter\n",
                fq_count(&p->fq), p->draw_ms);
    }
}

/* audio_dry: the clock can't move because no audio is coming (the stream
 * ran out of it, or has none near here); after AUDIO_DRY_S, video is paced
 * by wall time until the next seek. */
static int audio_dry(Player *p, double pts) {
    if (!p->dev || p->wall_fallback || audio_queued(p) > 0 || !pq_empty(&p->aq)) {
        p->dry_since = 0;
        return 0;
    }
    if (!p->dry_since) p->dry_since = SDL_GetTicks();
    if (SDL_GetTicks() - p->dry_since < AUDIO_DRY_S * 1000) return 0;
    fprintf(stderr, "player: no audio for %.1fs at %.1fs, pacing by wall clock\n", AUDIO_DRY_S, pts);
    p->wall_fallback = 1;
    p->wall_started = 0;
    return 1;
}

/* show_next presents the oldest decoded frame once the clock reaches it.
 * Returns 0 when there is nothing to show yet. */
static int show_next(Player *p) {
    double pts;
    int serial;
    AVFrame *f = fq_peek(&p->fq, &pts, &serial);
    if (!f) {
        /* nothing will come (end of stream) or the decoder is stuck: don't hold the sound forever */
        if (p->gate && (SDL_AtomicGet(&p->video_eof) == cur_serial(p) + 1 ||
                        (!p->rebuffering && SDL_GetTicks() - p->gate_since > 5000))) {
            p->gate = 0;
            audio_run(p);
        }
        return 0;
    }
    if (serial != cur_serial(p)) { /* from before a seek */
        fq_pop(&p->fq);
        return 1;
    }
    if (p->gate && p->rebuffering) { /* ran dry: play on once there is a cushion again */
        if (buffered_s(p) < REBUFFER_S && !buffer_full(p) && SDL_AtomicGet(&p->read_eof) != cur_serial(p) + 1)
            return 0;
        fprintf(stderr, "player: buffered again after %.1f s\n", (SDL_GetTicks() - p->rebuffer_since) / 1000.0);
        p->rebuffering = 0;
        p->buffering = 0;
        p->gate = 0; /* the frame on screen is the one before the gap; carry on from it */
        p->gate_opened = SDL_GetTicks();
        p->wall_started = 0;
        audio_run(p);
    }
    if (p->gate) { /* the first frame is here: put it up, then start the clock at it */
        if (p->dev) {
            SDL_LockAudioDevice(p->dev);
            if (p->resync_audio) p->audio_written = pts; /* no audio decoded yet */
            SDL_UnlockAudioDevice(p->dev);
        }
        p->last_pts = pts;
        upload_frame(p, f);
        fq_pop(&p->fq);
        redraw(p);
        p->gate = 0;
        p->gate_opened = SDL_GetTicks();
        p->wall_started = 0;
        audio_run(p);
        return 1;
    }
    int wall = !(p->adec && p->dev) || p->wall_fallback;
    if (wall && !p->wall_started) { /* anchor wall-time pacing to this frame */
        p->wall_base = now_s() - pts;
        p->wall_started = 1;
    }
    if (clock_s(p) < pts - 0.005 && !audio_dry(p, pts)) return 0;
    p->dry_since = 0;
    p->last_pts = pts;
    if (p->tex && clock_s(p) - pts > LATE_DROP_S && p->drops_in_row < MAX_DROPS_IN_ROW) {
        note_drop(p); /* behind the clock: skip drawing this one to catch up */
        fq_pop(&p->fq);
        return 1;
    }
    p->drops_in_row = 0;
    Uint64 d0 = SDL_GetPerformanceCounter();
    upload_frame(p, f);
    dump_frame(p, f);
    fq_pop(&p->fq);
    redraw(p);
    average(&p->draw_ms, ms_since(d0));
    trace(p, "frame");
    return 1;
}

/* finished: the decoders reached the end and every frame and sample played. */
static int finished(Player *p) {
    int serial = cur_serial(p) + 1;
    if (SDL_AtomicGet(&p->video_eof) != serial || fq_count(&p->fq)) return 0;
    if (!p->adec || !p->dev) return 1;
    return SDL_AtomicGet(&p->audio_eof) == serial && audio_queued(p) == 0;
}

/* note_waiting: nothing was due. With frames queued that is the clock; with
 * none for a while it is the network (or the decoder), so say so. */
static void note_waiting(Player *p) {
    Uint32 now = SDL_GetTicks();
    if (!p->rebuffering) {
        if (fq_count(&p->fq) || SDL_AtomicGet(&p->video_eof) == cur_serial(p) + 1) {
            p->empty_since = 0;
            p->buffering = 0;
            return;
        }
        if (!p->empty_since) p->empty_since = now;
        p->buffering = now - p->empty_since >= BUFFERING_MS;
        /* the network can't keep up: hold the sound and the clock instead of
         * playing on and dropping every late frame. Only when the read-ahead
         * is really empty (no frames but packets waiting is the decoder
         * catching up), and not in the first seconds after a start or seek,
         * when the pipeline is still filling. */
        if (p->buffering && !p->gate && SDL_AtomicGet(&p->read_eof) != cur_serial(p) + 1 && buffered_s(p) < 0.5 &&
            now - p->gate_opened > 5000) {
            fprintf(stderr, "player: ran dry at %.1fs, buffering %.0f s\n", p->last_pts, REBUFFER_S);
            /* a file whose peaks the link can't carry (its average says nothing about
             * an action scene): hand back so the caller asks for a converted stream */
            if (p->o->may_give_up && p->last_dry && now - p->last_dry < GIVE_UP_MS) {
                fprintf(stderr, "player: the link can't keep up with this file, asking for a converted stream\n");
                p->starved = 1;
                p->quit = 1;
            }
            p->last_dry = now;
            p->rebuffering = 1;
            p->rebuffer_since = now;
            shut_gate(p);
        }
    }
    if (p->buffering && now >= p->next_buffer_draw) {
        p->next_buffer_draw = now + 250;
        redraw(p);
    }
}

PlayResult player_play(SDL_Renderer *ren, const PlayOpts *o) {
    Player p = {.o = o, .ren = ren, .vidx = -1, .aidx = -1, .pending = -1, .result_seek = -1, .held = -1,
                .resync_audio = 1, .drop_before = -1e9, .sar = 1, .aspect = o->aspect, .volume = o->volume};
    if (p.aspect < 0 || p.aspect >= ASPECT_COUNT) p.aspect = ASPECT_AUTO;
    if (p.volume < 0 || p.volume > VOLUME_MAX) p.volume = 100;
    PlayResult res = {.position = -1, .seek_to = -1};
    pq_init(&p.vq);
    pq_init(&p.aq);
    fq_init(&p.fq);
    p.seek_mu = SDL_CreateMutex();
    avformat_network_init();

    AVDictionary *opts = NULL;
    av_dict_set(&opts, "headers", "X-Flex-Client: ps5\r\n", 0);
    av_dict_set(&opts, "reconnect", "1", 0);
    av_dict_set(&opts, "reconnect_streamed", "1", 0);
    av_dict_set(&opts, "rw_timeout", "30000000", 0); /* us */
    p.fmt = avformat_alloc_context();
    p.fmt->interrupt_callback = (AVIOInterruptCB){interrupt_cb, &p};
    if (avformat_open_input(&p.fmt, o->url, NULL, &opts) < 0) {
        av_dict_free(&opts);
        fprintf(stderr, "player: cannot open %s\n", o->url);
        goto done;
    }
    av_dict_free(&opts);
    if (avformat_find_stream_info(p.fmt, NULL) < 0) goto done;

    p.vidx = open_decoder(p.fmt, AVMEDIA_TYPE_VIDEO, &p.vdec);
    p.aidx = open_decoder(p.fmt, AVMEDIA_TYPE_AUDIO, &p.adec);
    if (p.vidx < 0) goto done;
    p.vtb = av_q2d(p.fmt->streams[p.vidx]->time_base);
    if (p.aidx >= 0) p.atb = av_q2d(p.fmt->streams[p.aidx]->time_base);
    p.start = p.fmt->start_time == AV_NOPTS_VALUE ? 0 : p.fmt->start_time / (double)AV_TIME_BASE;
    fprintf(stderr, "player: video %s %dx%d, audio %s, %s\n", p.vdec->codec->name, p.vdec->width, p.vdec->height,
            p.adec ? p.adec->codec->name : "none", o->seekable ? "seekable" : "restart to seek");
    { /* lay out from the stream's own size, so even the first frames come scaled */
        AVRational sar = p.fmt->streams[p.vidx]->codecpar->sample_aspect_ratio;
        p.sar = sar.num && sar.den ? av_q2d(sar) : 1.0;
        p.src_w = p.vdec->width;
        p.src_h = p.vdec->height;
        layout(&p);
    }

    SDL_AudioSpec want = {.freq = AUDIO_RATE, .format = AUDIO_S16SYS, .channels = AUDIO_CH, .samples = 1024,
                          .callback = audio_cb, .userdata = &p};
    if (p.adec && (p.ring = malloc(RING_BYTES))) {
        p.dev = SDL_OpenAudioDevice(NULL, 0, &want, NULL, 0); /* opens paused; the gate starts it */
        if (!p.dev) fprintf(stderr, "player: no audio device (%s); using wall clock\n", SDL_GetError());
    }
    if (!p.dev && p.adec) avcodec_free_context(&p.adec);
    p.wall_base = now_s();
    shut_gate(&p);

    if (o->seekable && o->initial > 1) { /* resume: seek before the first frame */
        p.pending = o->initial;
        p.seek_at = 0;
    }
    show_osd(&p);

    p.reader = SDL_CreateThread(reader_thread, "flex-reader", &p);
    p.vthread = SDL_CreateThread(video_thread, "flex-video", &p);
    if (p.adec) p.athread = SDL_CreateThread(audio_thread, "flex-audio", &p);

    while (!p.quit) {
        handle_events(&p);
        if (seek_ready(&p)) {
            commit_seek(&p);
            continue;
        }
        if (p.paused) {
            redraw(&p);
            SDL_Delay(30);
            continue;
        }
        if (show_next(&p)) continue;
        if (finished(&p)) {
            p.ended = 1;
            break;
        }
        note_waiting(&p);
        SDL_Delay(2);
    }

    res.position = position(&p);
    res.seek_to = p.result_seek;
    res.ended = p.ended;
    res.toggle_upscale = p.toggle_upscale;
    res.starved = p.starved;
done:
    SDL_AtomicSet(&p.abort, 1);
    SDL_WaitThread(p.reader, NULL);
    if (p.read_secs >= 3) { /* enough reading to say how fast the link is */
        res.link_kbps = (int)(p.read_bytes * 8 / p.read_secs / 1000);
        fprintf(stderr, "player: link %d kbit/s (%.0f MB in %.1f s of reading)\n", res.link_kbps,
                p.read_bytes / 1048576, p.read_secs);
    }
    SDL_WaitThread(p.vthread, NULL);
    SDL_WaitThread(p.athread, NULL);
    res.aspect = p.aspect;
    res.volume = p.volume;
    if (p.dev) SDL_CloseAudioDevice(p.dev);
    free(p.ring);
    pq_destroy(&p.vq);
    pq_destroy(&p.aq);
    fq_destroy(&p.fq);
    SDL_DestroyMutex(p.seek_mu);
    for (int i = 0; i < OSD_LINES; i++)
        if (p.osd[i].tex) SDL_DestroyTexture(p.osd[i].tex);
    if (p.tex) SDL_DestroyTexture(p.tex);
    if (p.sws) sws_freeContext(p.sws);
    if (p.swr) swr_free(&p.swr);
    if (p.vdec) avcodec_free_context(&p.vdec);
    if (p.adec) avcodec_free_context(&p.adec);
    avformat_close_input(&p.fmt);
    return res;
}
