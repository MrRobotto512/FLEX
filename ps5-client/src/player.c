#include "player.h"

#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/imgutils.h>
#include <libavutil/opt.h>
#include <libswresample/swresample.h>
#include <libswscale/swscale.h>
#include <stdio.h>
#include <stdlib.h>

/* Single-threaded demux/decode loop: audio frames are converted to S16
 * stereo and queued into SDL; video frames wait until the clock (audio, or
 * wall time for silent files) reaches their pts, then go to a YUV texture
 * drawn letterboxed. Seeking in direct files uses avformat_seek_file;
 * transcoded HLS sessions are not seekable, so the caller restarts them. */

#define AUDIO_RATE 48000
#define AUDIO_CH 2
#define BYTES_PER_SEC (AUDIO_RATE * AUDIO_CH * 2)
#define MAX_QUEUED_AUDIO (BYTES_PER_SEC) /* keep ~1 s buffered */
#define SCREEN_W 1920
#define SCREEN_H 1080
#define SEEK_COMMIT_MS 700
#define OSD_MS 3000
#define REPEAT_DELAY_MS 400
#define REPEAT_RATE_MS 150
#define MAX_STASH 600 /* video packets held back while waiting for audio after a seek */

static const char *aspect_names[ASPECT_COUNT] = {"Original", "16:9", "4:3", "2.35:1", "Zoom", "Stretch"};

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
    double sar;   /* pixel aspect of the stream */
    int aspect;   /* ASPECT_* */
    SDL_Rect dst; /* where the picture goes on screen */
    SDL_AudioDeviceID dev;

    double audio_written; /* stream time at the end of the queued audio */
    int resync_audio;     /* the next audio frame sets audio_written */
    int wall_fallback;    /* no audio found after a seek: pace video by wall time */

    /* After a seek (and at the start) video packets can come before any
     * audio from the same moment; the audio clock would then never move and
     * video would wait forever. Video packets are held back until the first
     * audio frame is queued. */
    int priming;
    AVPacket *stash[MAX_STASH];
    int nstash;
    double stash_last; /* stream time of the newest held-back packet */
    double wall_base;     /* silent files: clock = now - wall_base */
    double drop_before;   /* after a seek: skip frames earlier than this stream time */
    double last_pts;      /* stream time of the last presented frame */

    int paused, quit, ended;
    double pending; /* media time the user is seeking to, -1 = none */
    Uint32 seek_at; /* commit the pending seek at this tick */
    double result_seek;
    int toggle_upscale;
    int held;
    Uint32 next_repeat;
    Uint32 osd_until;
} Player;

static double now_s(void) { return SDL_GetTicks() / 1000.0; }

static double clock_s(Player *p) {
    if (p->adec && p->dev && !p->wall_fallback)
        return p->audio_written - (double)SDL_GetQueuedAudioSize(p->dev) / BYTES_PER_SEC;
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
    fprintf(stderr, "trace: %s pts=%.2f clock=%.2f audio_written=%.2f queued=%u\n", what, p->last_pts, clock_s(p),
            p->audio_written, p->dev ? SDL_GetQueuedAudioSize(p->dev) : 0);
}

static int osd_visible(Player *p) { return p->paused || p->pending >= 0 || SDL_GetTicks() < p->osd_until; }
static void show_osd(Player *p) { p->osd_until = SDL_GetTicks() + OSD_MS; }
static int seek_ready(Player *p) { return p->pending >= 0 && SDL_GetTicks() >= p->seek_at; }

/* ---------- OSD ---------- */

static void fmt_time(char *b, size_t n, double s) {
    int t = s < 0 ? 0 : (int)(s + 0.5);
    if (t >= 3600) snprintf(b, n, "%d:%02d:%02d", t / 3600, t / 60 % 60, t % 60);
    else snprintf(b, n, "%d:%02d", t / 60, t % 60);
}

static void osd_text(Player *p, const char *text, int x, int y, int right_align) {
    if (!p->o->font || !text[0]) return;
    SDL_Surface *s = TTF_RenderUTF8_Blended(p->o->font, text, (SDL_Color){232, 234, 240, 255});
    if (!s) return;
    SDL_Texture *t = SDL_CreateTextureFromSurface(p->ren, s);
    if (t) {
        SDL_Rect r = {right_align ? x - s->w : x, y, s->w, s->h};
        SDL_RenderCopy(p->ren, t, NULL, &r);
        SDL_DestroyTexture(t);
    }
    SDL_FreeSurface(s);
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
    osd_text(p, p->o->title ? p->o->title : "", 80, SCREEN_H - 140, 0);
    osd_text(p, line, SCREEN_W - 80, SCREEN_H - 140, 1);
    char shape[128];
    snprintf(shape, sizeof shape, "%s%s  ·  Upscale %s  ·  %dx%d", p->paused ? "Paused  ·  " : "",
             aspect_names[p->aspect], p->o->upscaled ? "on" : "off", p->tex_w, p->tex_h);
    osd_text(p, shape, SCREEN_W / 2 - 300, SCREEN_H - 140, 0);

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
    }
    return -1;
}

/* layout places the picture for the current shape. */
static void layout(Player *p) {
    if (!p->tex_w || !p->tex_h) return;
    double ar = p->tex_w * p->sar / p->tex_h;
    switch (p->aspect) {
    case ASPECT_16_9: ar = 16.0 / 9; break;
    case ASPECT_4_3: ar = 4.0 / 3; break;
    case ASPECT_235: ar = 2.35; break;
    case ASPECT_STRETCH: p->dst = (SDL_Rect){0, 0, SCREEN_W, SCREEN_H}; return;
    }
    int w = SCREEN_W, h = (int)(SCREEN_W / ar + 0.5);
    if (p->aspect == ASPECT_ZOOM ? h < SCREEN_H : h > SCREEN_H) {
        h = SCREEN_H;
        w = (int)(SCREEN_H * ar + 0.5);
    }
    p->dst = (SDL_Rect){(SCREEN_W - w) / 2, (SCREEN_H - h) / 2, w, h}; /* ZOOM overflows; the renderer clips */
}

static void set_paused(Player *p, int paused) {
    if (p->paused == paused) return;
    p->paused = paused;
    if (p->dev) SDL_PauseAudioDevice(p->dev, paused);
    if (!paused) p->wall_base = now_s() - p->last_pts;
}

static void step_seek(Player *p, double delta) {
    double base = position(p), t = base + delta, max = p->o->duration > 10 ? p->o->duration - 5 : base;
    if (t > max) t = max;
    if (t < 0) t = 0;
    p->pending = t;
    p->seek_at = SDL_GetTicks() + SEEK_COMMIT_MS;
    show_osd(p);
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
        if (seek_delta(b) != 0) {
            step_seek(p, seek_delta(b));
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
        step_seek(p, seek_delta(p->held));
        p->next_repeat = SDL_GetTicks() + REPEAT_RATE_MS;
    }
}

/* wait_step is the body of every wait loop: input, pause screen, a short
 * sleep. Returns 0 when the wait must end (stop or seek). */
static int wait_step(Player *p) {
    handle_events(p);
    if (p->quit || seek_ready(p)) return 0;
    if (p->paused) {
        redraw(p);
        SDL_Delay(30);
    } else {
        SDL_Delay(2);
    }
    return 1;
}

/* ---------- seeking ---------- */

static void drop_stash(Player *p) {
    for (int i = 0; i < p->nstash; i++) av_packet_free(&p->stash[i]);
    p->nstash = 0;
    p->stash_last = -1e9;
}

/* commit_seek performs the pending seek. Non-seekable streams end here and
 * report the target to the caller. */
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
    int64_t ts = (int64_t)((st + p->start) * AV_TIME_BASE);
    if (avformat_seek_file(p->fmt, -1, INT64_MIN, ts, ts, 0) < 0) {
        fprintf(stderr, "player: seek to %.1fs failed\n", target);
        return;
    }
    avcodec_flush_buffers(p->vdec);
    if (p->adec) avcodec_flush_buffers(p->adec);
    if (p->dev) SDL_ClearQueuedAudio(p->dev);
    drop_stash(p);
    p->priming = p->adec != NULL;
    p->wall_fallback = 0;
    p->audio_written = st;
    p->resync_audio = 1;
    p->drop_before = st;
    p->last_pts = st;
    p->wall_base = now_s() - st;
    set_paused(p, 0);
    show_osd(p);
}

/* ---------- decode ---------- */

static int open_decoder(AVFormatContext *fmt, enum AVMediaType type, AVCodecContext **out) {
    const AVCodec *codec = NULL;
    int idx = av_find_best_stream(fmt, type, -1, -1, &codec, 0);
    if (idx < 0) return -1;
    AVCodecContext *c = avcodec_alloc_context3(codec);
    avcodec_parameters_to_context(c, fmt->streams[idx]->codecpar);
    c->thread_count = 0; /* let FFmpeg use all cores */
    c->thread_type = FF_THREAD_FRAME | FF_THREAD_SLICE;
    if (avcodec_open2(c, codec, NULL) < 0) {
        avcodec_free_context(&c);
        return -1;
    }
    *out = c;
    return idx;
}

static void upload_frame(Player *p, AVFrame *f) {
    if (!p->tex || p->tex_w != f->width || p->tex_h != f->height) {
        if (p->tex) SDL_DestroyTexture(p->tex);
        p->tex = SDL_CreateTexture(p->ren, SDL_PIXELFORMAT_IYUV, SDL_TEXTUREACCESS_STREAMING, f->width, f->height);
        p->tex_w = f->width;
        p->tex_h = f->height;
        p->sar = f->sample_aspect_ratio.num && f->sample_aspect_ratio.den ? av_q2d(f->sample_aspect_ratio) : 1.0;
        layout(p);
    }
    if (f->format == AV_PIX_FMT_YUV420P || f->format == AV_PIX_FMT_YUVJ420P) {
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

static void queue_audio(Player *p, AVFrame *f, double apts) {
    if (!p->swr) {
        AVChannelLayout out = AV_CHANNEL_LAYOUT_STEREO;
        swr_alloc_set_opts2(&p->swr, &out, AV_SAMPLE_FMT_S16, AUDIO_RATE, &f->ch_layout, f->format, f->sample_rate, 0,
                            NULL);
        swr_init(p->swr);
    }
    if (p->resync_audio) {
        p->audio_written = apts;
        p->resync_audio = 0;
        p->wall_fallback = 0;
    }
    int out_samples = swr_get_out_samples(p->swr, f->nb_samples);
    uint8_t *buf = av_malloc(out_samples * AUDIO_CH * 2);
    int n = swr_convert(p->swr, &buf, out_samples, (const uint8_t **)f->extended_data, f->nb_samples);
    if (n > 0) {
        SDL_QueueAudio(p->dev, buf, n * AUDIO_CH * 2);
        p->audio_written += (double)n / AUDIO_RATE;
    }
    av_free(buf);
}

static void audio_packet(Player *p, AVPacket *pkt, AVFrame *frame) {
    /* don't run too far ahead of the speaker */
    while (SDL_GetQueuedAudioSize(p->dev) > MAX_QUEUED_AUDIO)
        if (!wait_step(p)) return;
    if (avcodec_send_packet(p->adec, pkt) != 0) return;
    while (avcodec_receive_frame(p->adec, frame) == 0) {
        double apts = frame->best_effort_timestamp == AV_NOPTS_VALUE ? p->audio_written
                                                                     : frame->best_effort_timestamp * p->atb - p->start;
        if (apts < p->drop_before - 0.02) continue; /* still before the seek target */
        queue_audio(p, frame, apts);
    }
}

static void video_packet(Player *p, AVPacket *pkt, AVFrame *frame) {
    if (avcodec_send_packet(p->vdec, pkt) != 0) return;
    while (avcodec_receive_frame(p->vdec, frame) == 0) {
        double pts = frame->best_effort_timestamp == AV_NOPTS_VALUE ? p->last_pts
                                                                    : frame->best_effort_timestamp * p->vtb - p->start;
        if (pts < p->drop_before - 0.05) continue; /* decoding up to the seek target */
        int stop = 0;
        while (clock_s(p) < pts - 0.005) {
            if (!wait_step(p)) {
                stop = 1;
                break;
            }
            /* audio ran dry: its clock can't move until more packets are
             * read, so show the frame now instead of waiting forever */
            if (!p->paused && p->dev && !p->wall_fallback && SDL_GetQueuedAudioSize(p->dev) == 0) break;
        }
        if (stop) {
            av_frame_unref(frame);
            continue; /* keep draining the decoder; the frames are dropped */
        }
        p->last_pts = pts;
        upload_frame(p, frame);
        redraw(p);
        trace(p, "frame");
    }
}

/* end_priming replays the video packets held back while waiting for audio. */
static void end_priming(Player *p, AVFrame *frame) {
    p->priming = 0;
    int n = p->nstash;
    p->nstash = 0;
    p->stash_last = -1e9;
    for (int i = 0; i < n; i++) {
        video_packet(p, p->stash[i], frame);
        av_packet_free(&p->stash[i]);
    }
}

PlayResult player_play(SDL_Renderer *ren, const PlayOpts *o) {
    Player p = {.o = o, .ren = ren, .vidx = -1, .aidx = -1, .pending = -1, .result_seek = -1, .held = -1,
                .resync_audio = 1, .drop_before = -1e9, .stash_last = -1e9, .sar = 1, .aspect = o->aspect};
    if (p.aspect < 0 || p.aspect >= ASPECT_COUNT) p.aspect = ASPECT_AUTO;
    PlayResult res = {.position = -1, .seek_to = -1};
    avformat_network_init();

    AVDictionary *opts = NULL;
    av_dict_set(&opts, "headers", "X-Flex-Client: ps5\r\n", 0);
    av_dict_set(&opts, "reconnect", "1", 0);
    av_dict_set(&opts, "reconnect_streamed", "1", 0);
    av_dict_set(&opts, "rw_timeout", "30000000", 0); /* us */
    if (avformat_open_input(&p.fmt, o->url, NULL, &opts) < 0) {
        av_dict_free(&opts);
        fprintf(stderr, "player: cannot open %s\n", o->url);
        return res;
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

    SDL_AudioSpec want = {.freq = AUDIO_RATE, .format = AUDIO_S16SYS, .channels = AUDIO_CH, .samples = 1024};
    if (p.adec) {
        p.dev = SDL_OpenAudioDevice(NULL, 0, &want, NULL, 0);
        if (!p.dev) fprintf(stderr, "player: no audio device (%s); using wall clock\n", SDL_GetError());
        else SDL_PauseAudioDevice(p.dev, 0);
    }
    if (!p.dev && p.adec) avcodec_free_context(&p.adec);
    p.wall_base = now_s();
    p.priming = p.adec != NULL;

    if (o->seekable && o->initial > 1) { /* resume: seek before the first frame */
        p.pending = o->initial;
        p.seek_at = 0;
    }
    show_osd(&p);

    AVPacket *pkt = av_packet_alloc();
    AVFrame *frame = av_frame_alloc();
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
        if (av_read_frame(p.fmt, pkt) < 0) {
            p.ended = 1;
            break;
        }
        if (pkt->stream_index == p.aidx && p.adec) {
            audio_packet(&p, pkt, frame);
            /* release the held-back video once audio covers it, so replaying
             * it doesn't drain the audio queue */
            if (p.priming && !p.resync_audio && p.audio_written >= p.stash_last) end_priming(&p, frame);
        } else if (pkt->stream_index == p.vidx) {
            if (p.priming && p.nstash < MAX_STASH) {
                int64_t ts = pkt->pts != AV_NOPTS_VALUE ? pkt->pts : pkt->dts;
                if (ts != AV_NOPTS_VALUE) p.stash_last = ts * p.vtb - p.start;
                p.stash[p.nstash++] = av_packet_clone(pkt);
            } else {
                if (p.priming) { /* no audio anywhere near: pace by wall time until some shows up */
                    fprintf(stderr, "player: no audio after %d video packets, pacing by wall clock\n", p.nstash);
                    p.wall_fallback = 1;
                    p.wall_base = now_s() - (p.drop_before > 0 ? p.drop_before : 0);
                    end_priming(&p, frame);
                }
                video_packet(&p, pkt, frame);
            }
        }
        av_packet_unref(pkt);
    }
    if (p.ended) /* let the last second of audio play out */
        while (p.dev && SDL_GetQueuedAudioSize(p.dev) > 0)
            if (!wait_step(&p)) break;

    drop_stash(&p);
    av_frame_free(&frame);
    av_packet_free(&pkt);
    res.position = position(&p);
    res.seek_to = p.result_seek;
    res.ended = p.ended;
    res.toggle_upscale = p.toggle_upscale;
done:
    res.aspect = p.aspect;
    if (p.dev) SDL_CloseAudioDevice(p.dev);
    if (p.tex) SDL_DestroyTexture(p.tex);
    if (p.sws) sws_freeContext(p.sws);
    if (p.swr) swr_free(&p.swr);
    if (p.vdec) avcodec_free_context(&p.vdec);
    if (p.adec) avcodec_free_context(&p.adec);
    avformat_close_input(&p.fmt);
    return res;
}
