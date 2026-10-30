#include "player.h"

#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/imgutils.h>
#include <libavutil/opt.h>
#include <libswresample/swresample.h>
#include <libswscale/swscale.h>
#include <stdio.h>

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
    SDL_Rect dst; /* letterboxed video rect */
    SDL_AudioDeviceID dev;

    double audio_written; /* stream time at the end of the queued audio */
    int resync_audio;     /* the next audio frame sets audio_written */
    double wall_base;     /* silent files: clock = now - wall_base */
    double drop_before;   /* after a seek: skip frames earlier than this stream time */
    double last_pts;      /* stream time of the last presented frame */

    int paused, quit, ended;
    double pending; /* media time the user is seeking to, -1 = none */
    Uint32 seek_at; /* commit the pending seek at this tick */
    double result_seek;
    int held;
    Uint32 next_repeat;
    Uint32 osd_until;
} Player;

static double now_s(void) { return SDL_GetTicks() / 1000.0; }

static double clock_s(Player *p) {
    if (p->adec && p->dev) return p->audio_written - (double)SDL_GetQueuedAudioSize(p->dev) / BYTES_PER_SEC;
    return now_s() - p->wall_base;
}

/* media time of what is on screen (or of the pending seek target) */
static double position(Player *p) {
    if (p->pending >= 0) return p->pending;
    return p->o->offset + p->last_pts;
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
    if (p->paused) osd_text(p, "Paused", SCREEN_W / 2 - 40, SCREEN_H - 140, 0);

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
    }
    return -1;
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
        /* letterbox to the screen, honouring non-square pixels */
        double sar = f->sample_aspect_ratio.num && f->sample_aspect_ratio.den ? av_q2d(f->sample_aspect_ratio) : 1.0;
        double ar = f->width * sar / f->height;
        int w = SCREEN_W, h = (int)(SCREEN_W / ar + 0.5);
        if (h > SCREEN_H) {
            h = SCREEN_H;
            w = (int)(SCREEN_H * ar + 0.5);
        }
        p->dst = (SDL_Rect){(SCREEN_W - w) / 2, (SCREEN_H - h) / 2, w, h};
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
        while (clock_s(p) < pts - 0.005)
            if (!wait_step(p)) {
                stop = 1;
                break;
            }
        if (stop) {
            av_frame_unref(frame);
            continue; /* keep draining the decoder; the frames are dropped */
        }
        p->last_pts = pts;
        upload_frame(p, frame);
        redraw(p);
    }
}

PlayResult player_play(SDL_Renderer *ren, const PlayOpts *o) {
    Player p = {.o = o, .ren = ren, .vidx = -1, .aidx = -1, .pending = -1, .result_seek = -1, .held = -1,
                .resync_audio = 1, .drop_before = -1e9};
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
        if (pkt->stream_index == p.aidx && p.adec) audio_packet(&p, pkt, frame);
        else if (pkt->stream_index == p.vidx) video_packet(&p, pkt, frame);
        av_packet_unref(pkt);
    }
    if (p.ended) /* let the last second of audio play out */
        while (p.dev && SDL_GetQueuedAudioSize(p.dev) > 0)
            if (!wait_step(&p)) break;

    av_frame_free(&frame);
    av_packet_free(&pkt);
    res.position = position(&p);
    res.seek_to = p.result_seek;
    res.ended = p.ended;
done:
    if (p.dev) SDL_CloseAudioDevice(p.dev);
    if (p.tex) SDL_DestroyTexture(p.tex);
    if (p.sws) sws_freeContext(p.sws);
    if (p.swr) swr_free(&p.swr);
    if (p.vdec) avcodec_free_context(&p.vdec);
    if (p.adec) avcodec_free_context(&p.adec);
    avformat_close_input(&p.fmt);
    return res;
}
