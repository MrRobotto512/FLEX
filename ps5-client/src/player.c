#include "player.h"

#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/imgutils.h>
#include <libavutil/opt.h>
#include <libswresample/swresample.h>
#include <libswscale/swscale.h>
#include <stdio.h>

/* Single-threaded demux/decode loop: audio frames are converted to S16
 * stereo and queued into SDL; video frames wait until the audio clock reaches
 * their pts, then go to a YUV texture. Good enough to prove the pipeline;
 * threading the decoders is the next step if 1080p stutters. */

#define AUDIO_RATE 48000
#define AUDIO_CH 2
#define BYTES_PER_SEC (AUDIO_RATE * AUDIO_CH * 2)
#define MAX_QUEUED_AUDIO (BYTES_PER_SEC)   /* keep ~1 s buffered */

typedef struct {
    AVFormatContext *fmt;
    AVCodecContext *vdec, *adec;
    int vidx, aidx;
    struct SwsContext *sws;
    SwrContext *swr;
    SDL_Texture *tex;
    SDL_AudioDeviceID dev;
    double audio_written;   /* seconds of audio handed to SDL so far */
    double last_pts;
    int paused, quit;
} Player;

static double audio_clock(Player *p) {
    return p->audio_written - (double)SDL_GetQueuedAudioSize(p->dev) / BYTES_PER_SEC;
}

static void handle_events(Player *p) {
    SDL_Event e;
    while (SDL_PollEvent(&e)) {
        int back = 0, toggle = 0;
        if (e.type == SDL_QUIT) back = 1;
        if (e.type == SDL_CONTROLLERBUTTONDOWN) {
            if (e.cbutton.button == SDL_CONTROLLER_BUTTON_B) back = 1;      /* circle */
            if (e.cbutton.button == SDL_CONTROLLER_BUTTON_A) toggle = 1;    /* cross */
        }
        if (e.type == SDL_KEYDOWN) {
            if (e.key.keysym.sym == SDLK_ESCAPE) back = 1;
            if (e.key.keysym.sym == SDLK_SPACE) toggle = 1;
        }
        if (back) p->quit = 1;
        if (toggle) {
            p->paused = !p->paused;
            SDL_PauseAudioDevice(p->dev, p->paused);
        }
    }
}

static int open_decoder(AVFormatContext *fmt, enum AVMediaType type, AVCodecContext **out) {
    const AVCodec *codec = NULL;
    int idx = av_find_best_stream(fmt, type, -1, -1, &codec, 0);
    if (idx < 0) return -1;
    AVCodecContext *c = avcodec_alloc_context3(codec);
    avcodec_parameters_to_context(c, fmt->streams[idx]->codecpar);
    c->thread_count = 0;  /* let FFmpeg use all cores */
    c->thread_type = FF_THREAD_FRAME | FF_THREAD_SLICE;
    if (avcodec_open2(c, codec, NULL) < 0) {
        avcodec_free_context(&c);
        return -1;
    }
    *out = c;
    return idx;
}

static void present_video(Player *p, SDL_Renderer *ren, AVFrame *f) {
    if (!p->tex) {
        p->tex = SDL_CreateTexture(ren, SDL_PIXELFORMAT_IYUV, SDL_TEXTUREACCESS_STREAMING, f->width, f->height);
    }
    if (f->format == AV_PIX_FMT_YUV420P || f->format == AV_PIX_FMT_YUVJ420P) {
        SDL_UpdateYUVTexture(p->tex, NULL, f->data[0], f->linesize[0], f->data[1], f->linesize[1],
                             f->data[2], f->linesize[2]);
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
    SDL_RenderClear(ren);
    SDL_RenderCopy(ren, p->tex, NULL, NULL);  /* letterboxing: SDL_RenderSetLogicalSize in main */
    SDL_RenderPresent(ren);
}

static void queue_audio(Player *p, AVFrame *f) {
    if (!p->swr) {
        AVChannelLayout out = AV_CHANNEL_LAYOUT_STEREO;
        swr_alloc_set_opts2(&p->swr, &out, AV_SAMPLE_FMT_S16, AUDIO_RATE,
                            &f->ch_layout, f->format, f->sample_rate, 0, NULL);
        swr_init(p->swr);
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

double player_play(SDL_Renderer *ren, const char *url) {
    Player p = {.vidx = -1, .aidx = -1};
    avformat_network_init();

    AVDictionary *opts = NULL;
    av_dict_set(&opts, "headers", "X-Flex-Client: ps5\r\n", 0);
    av_dict_set(&opts, "reconnect", "1", 0);
    if (avformat_open_input(&p.fmt, url, NULL, &opts) < 0) {
        fprintf(stderr, "player: cannot open %s\n", url);
        return -1;
    }
    av_dict_free(&opts);
    if (avformat_find_stream_info(p.fmt, NULL) < 0) goto fail;

    p.vidx = open_decoder(p.fmt, AVMEDIA_TYPE_VIDEO, &p.vdec);
    p.aidx = open_decoder(p.fmt, AVMEDIA_TYPE_AUDIO, &p.adec);
    if (p.vidx < 0) goto fail;
    fprintf(stderr, "player: video %s %dx%d, audio %s\n", p.vdec->codec->name, p.vdec->width, p.vdec->height,
            p.adec ? p.adec->codec->name : "none");

    SDL_AudioSpec want = {.freq = AUDIO_RATE, .format = AUDIO_S16SYS, .channels = AUDIO_CH, .samples = 1024};
    p.dev = SDL_OpenAudioDevice(NULL, 0, &want, NULL, 0);
    if (!p.dev) fprintf(stderr, "player: no audio device: %s\n", SDL_GetError());
    SDL_PauseAudioDevice(p.dev, 0);

    AVPacket *pkt = av_packet_alloc();
    AVFrame *frame = av_frame_alloc();
    double vtb = av_q2d(p.fmt->streams[p.vidx]->time_base);
    double start = p.fmt->start_time == AV_NOPTS_VALUE ? 0 : p.fmt->start_time / (double)AV_TIME_BASE;

    while (!p.quit && av_read_frame(p.fmt, pkt) >= 0) {
        handle_events(&p);
        while (p.paused && !p.quit) {
            handle_events(&p);
            SDL_Delay(20);
        }

        if (pkt->stream_index == p.aidx && p.adec) {
            /* don't run too far ahead of the speaker */
            while (!p.quit && SDL_GetQueuedAudioSize(p.dev) > MAX_QUEUED_AUDIO) {
                handle_events(&p);
                SDL_Delay(5);
            }
            if (avcodec_send_packet(p.adec, pkt) == 0)
                while (avcodec_receive_frame(p.adec, frame) == 0) queue_audio(&p, frame);
        } else if (pkt->stream_index == p.vidx) {
            if (avcodec_send_packet(p.vdec, pkt) == 0) {
                while (avcodec_receive_frame(p.vdec, frame) == 0) {
                    double pts = frame->best_effort_timestamp == AV_NOPTS_VALUE
                                     ? p.last_pts
                                     : frame->best_effort_timestamp * vtb - start;
                    p.last_pts = pts;
                    if (p.adec) {
                        while (!p.quit && audio_clock(&p) < pts) {
                            handle_events(&p);
                            SDL_Delay(2);
                        }
                    } else {
                        SDL_Delay((Uint32)(1000 / 24));
                    }
                    present_video(&p, ren, frame);
                }
            }
        }
        av_packet_unref(pkt);
    }

    av_frame_free(&frame);
    av_packet_free(&pkt);
fail:
    if (p.dev) SDL_CloseAudioDevice(p.dev);
    if (p.tex) SDL_DestroyTexture(p.tex);
    if (p.sws) sws_freeContext(p.sws);
    if (p.swr) swr_free(&p.swr);
    if (p.vdec) avcodec_free_context(&p.vdec);
    if (p.adec) avcodec_free_context(&p.adec);
    avformat_close_input(&p.fmt);
    return p.vidx < 0 ? -1 : p.last_pts;
}
