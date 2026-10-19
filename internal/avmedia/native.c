//go:build ffmpeg && cgo

#include "native.h"
#include <libavcodec/avcodec.h>
#include <libavcodec/bsf.h>
#include <libavformat/avformat.h>
#include <libavdevice/avdevice.h>
#include <libavutil/opt.h>
#include <libavutil/imgutils.h>
#include <libavutil/hwcontext.h>
#include <libavutil/pixdesc.h>
#include <libswscale/swscale.h>
#include <libswresample/swresample.h>
#include <stdatomic.h>
#include <stdlib.h>
#include <stdio.h>
#include <string.h>
#include <stdarg.h>
#ifdef _WIN32
#include <windows.h>
// Go passes UTF-8 paths. Search sibling DLL dependencies without changing the
// process-wide DLL directory or depending on the current working directory.
static void *open_windows_library(const char *path) {
    wchar_t wide[4096];
    if (!MultiByteToWideChar(CP_UTF8, MB_ERR_INVALID_CHARS, path, -1, wide, 4096))
        return NULL;
    for (wchar_t *p = wide; *p; p++)
        if (*p == L'/')
            *p = L'\\';
    DWORD flags = LOAD_LIBRARY_SEARCH_DEFAULT_DIRS;
    if (wcschr(wide, L'\\'))
        flags |= LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR;
    return (void *)LoadLibraryExW(wide, NULL, flags);
}
#define LIB_OPEN(path) open_windows_library(path)
#define LIB_SYMBOL(handle, name) ((void *)GetProcAddress((HMODULE)handle, name))
#define LIB_CLOSE(handle) FreeLibrary((HMODULE)handle)
#else
#include <dlfcn.h>
#define LIB_OPEN(path) dlopen(path, RTLD_NOW | RTLD_LOCAL)
#define LIB_SYMBOL(handle, name) dlsym(handle, name)
#define LIB_CLOSE(handle) dlclose(handle)
#endif
#include "symbols.h"

struct bm_session {
    AVCodecContext *codec;
    AVBSFContext *bsf;
    int passthrough;
    AVFormatContext *input;
    AVFrame *frame, *converted, *uploaded;
    AVPacket *packet;
    struct SwsContext *scale;
    struct SwrContext *resample;
    AVBufferRef *device;
    atomic_int cancelled;
    int audio, stream, vaapi;
    int64_t pts;
};
static int error_code(char *err, size_t len, const char *op, int code) {
    char detail[128];
    av_strerror(code, detail, sizeof(detail));
    snprintf(err, len, "%s: %s (%d)", op, detail, code);
    return -1;
}
static void *libraries[6];
static int loaded;
static void log_callback(void *, int, const char *, va_list);
static _Thread_local int media_log_suppressed;
int bm_load(const char *dir, char *err, size_t len) {
    if (loaded)
        return 0;
    const char *names[] = {"avutil", "swresample", "swscale", "avcodec", "avformat", "avdevice"};
    const int majors[] = {LIBAVUTIL_VERSION_MAJOR,   LIBSWRESAMPLE_VERSION_MAJOR,
                          LIBSWSCALE_VERSION_MAJOR,  LIBAVCODEC_VERSION_MAJOR,
                          LIBAVFORMAT_VERSION_MAJOR, LIBAVDEVICE_VERSION_MAJOR};
    for (int i = 0; i < 6; i++) {
        char file[128], path[4096];
#ifdef _WIN32
        snprintf(file, sizeof(file), "%s-%d.dll", names[i], majors[i]);
#elif __APPLE__
        snprintf(file, sizeof(file), "lib%s.%d.dylib", names[i], majors[i]);
#else
        snprintf(file, sizeof(file), "lib%s.so.%d", names[i], majors[i]);
#endif
        snprintf(path, sizeof(path), "%s%s%s", dir, *dir ? "/" : "", file);
        libraries[i] = LIB_OPEN(path);
        if (!libraries[i]) {
#ifdef _WIN32
            snprintf(err, len, "cannot load %s (Windows error %lu; check dependent DLLs)", path,
                     (unsigned long)GetLastError());
#else
            const char *detail = dlerror();
            snprintf(err, len, "cannot load %s: %s", path,
                     detail ? detail : "unknown dynamic loader error");
#endif
            goto fail;
        }
    }
#define RESOLVE(name, lib)                                                                         \
    do {                                                                                           \
        *(void **)(&p_##name) = LIB_SYMBOL(libraries[lib], #name);                                 \
        if (!p_##name) {                                                                           \
            snprintf(err, len, "missing %s", #name);                                               \
            goto fail;                                                                             \
        }                                                                                          \
    } while (0);
    BM_SYMBOLS(RESOLVE)
#undef RESOLVE
    const unsigned versions[] = {avutil_version(),  swresample_version(), swscale_version(),
                                 avcodec_version(), avformat_version(),   avdevice_version()};
    for (int i = 0; i < 6; i++)
        if ((versions[i] >> 16) != (unsigned)majors[i]) {
            snprintf(err, len, "incompatible FFmpeg library ABI: %s requires major %d, got %u",
                     names[i], majors[i], versions[i] >> 16);
            goto fail;
        }
    av_log_set_callback(log_callback);
    avdevice_register_all();
    loaded = 1;
    return 0;
fail:
    for (int i = 5; i >= 0; i--)
        if (libraries[i]) {
            LIB_CLOSE(libraries[i]);
            libraries[i] = NULL;
        }
    return -1;
}
static bm_session *alloc_session(void) {
    bm_session *s = calloc(1, sizeof(*s));
    if (!s)
        return NULL;
    s->frame = av_frame_alloc();
    s->converted = av_frame_alloc();
    s->uploaded = av_frame_alloc();
    s->packet = av_packet_alloc();
    if (!s->frame || !s->converted || !s->uploaded || !s->packet) {
        bm_close(s);
        return NULL;
    }
    return s;
}
static int cancelled(void *opaque) {
    return atomic_load(&((bm_session *)opaque)->cancelled);
}
void bm_interrupt(bm_session *s) {
    atomic_store(&s->cancelled, 1);
}
void bm_close(bm_session *s) {
    if (!s)
        return;
    if (s->input)
        avformat_close_input(&s->input);
    av_bsf_free(&s->bsf);
    avcodec_free_context(&s->codec);
    av_frame_free(&s->frame);
    av_frame_free(&s->converted);
    av_frame_free(&s->uploaded);
    av_packet_free(&s->packet);
    av_buffer_unref(&s->device);
    sws_freeContext(s->scale);
    swr_free(&s->resample);
    free(s);
}
static int open_codec(bm_session *s, const AVCodec *codec, AVDictionary **opts, char *err,
                      size_t len) {
    if (!codec || !s->codec)
        return error_code(err, len, "codec unavailable", AVERROR_ENCODER_NOT_FOUND);
    int result = avcodec_open2(s->codec, codec, opts);
    if (result < 0)
        return error_code(err, len, "avcodec_open2", result);
    return 0;
}
bm_session *bm_encoder(const char *name, const char *device, int w, int h, int fps_num, int fps_den,
                       int bitrate, int audio_ms, char *err, size_t len) {
    bm_session *s = alloc_session();
    if (!s)
        return NULL;
    const AVCodec *codec = avcodec_find_encoder_by_name(name);
    if (!codec) {
        error_code(err, len, name, AVERROR_ENCODER_NOT_FOUND);
        goto fail;
    }
    s->codec = avcodec_alloc_context3(codec);
    if (!s->codec)
        goto fail;
    AVCodecContext *c = s->codec;
    AVDictionary *opts = NULL;
    c->thread_count = 1;
    c->flags |= AV_CODEC_FLAG_LOW_DELAY;
    if (audio_ms) {
        s->audio = 1;
        c->sample_rate = 48000;
        c->time_base = (AVRational){1, 48000};
        c->bit_rate = bitrate * 1000;
        av_channel_layout_default(&c->ch_layout, 2);
        c->sample_fmt = AV_SAMPLE_FMT_S16;
        av_dict_set(&opts, "application", "lowdelay", 0);
        av_dict_set(&opts, "vbr", "off", 0);
        char duration[16];
        snprintf(duration, sizeof(duration), "%d", audio_ms);
        av_dict_set(&opts, "frame_duration", duration, 0);
    } else {
        int fps = (int)(((int64_t)fps_num + fps_den - 1) / fps_den);
        c->width = w;
        c->height = h;
        c->time_base = (AVRational){fps_den, fps_num};
        c->framerate = (AVRational){fps_num, fps_den};
        if (bitrate > 0)
            c->bit_rate = bitrate * 1000;
        c->gop_size = fps;
        c->max_b_frames = 0;
        c->pix_fmt = AV_PIX_FMT_YUV420P;
        c->color_range = AVCOL_RANGE_MPEG;
        c->colorspace = AVCOL_SPC_BT709;
        c->color_primaries = AVCOL_PRI_BT709;
        c->color_trc = AVCOL_TRC_BT709;
        if (!strcmp(name, "h264_nvenc") || !strcmp(name, "av1_nvenc")) {
            c->pix_fmt = AV_PIX_FMT_NV12;
            av_dict_set(&opts, "preset", "p1", 0);
            av_dict_set(&opts, "tune", "ull", 0);
            av_dict_set(&opts, "rc", "cbr", 0);
            av_dict_set(&opts, "delay", "0", 0);
            av_dict_set(&opts, "zerolatency", "1", 0);
            av_dict_set(&opts, "rc-lookahead", "0", 0);
            av_dict_set(&opts, "forced-idr", "1", 0);
            // NVENC exposes its baseline profile under this name; disable CABAC explicitly.
            if (c->codec_id == AV_CODEC_ID_H264) {
                av_dict_set(&opts, "profile", "baseline", 0);
                av_dict_set(&opts, "coder", "cavlc", 0);
            } else
                c->profile = AV_PROFILE_AV1_MAIN;
        } else if (!strcmp(name, "h264_vaapi") || !strcmp(name, "av1_vaapi")) {
            int result =
                av_hwdevice_ctx_create(&s->device, AV_HWDEVICE_TYPE_VAAPI, device, NULL, 0);
            if (result < 0) {
                error_code(err, len, "VAAPI device", result);
                goto failopts;
            }
            c->pix_fmt = AV_PIX_FMT_VAAPI;
            c->hw_frames_ctx = av_hwframe_ctx_alloc(s->device);
            if (!c->hw_frames_ctx)
                goto failopts;
            AVHWFramesContext *frames = (AVHWFramesContext *)c->hw_frames_ctx->data;
            frames->format = AV_PIX_FMT_VAAPI;
            frames->sw_format = AV_PIX_FMT_NV12;
            frames->width = w;
            frames->height = h;
            frames->initial_pool_size = 4;
            result = av_hwframe_ctx_init(c->hw_frames_ctx);
            if (result < 0) {
                error_code(err, len, "VAAPI frames", result);
                goto failopts;
            }
            s->vaapi = 1;
            av_dict_set(&opts, "async_depth", "1", 0);
            av_dict_set(&opts, "rc_mode", "CBR", 0);
            av_dict_set(&opts, "profile",
                        c->codec_id == AV_CODEC_ID_AV1 ? "main" : "constrained_baseline", 0);
        } else if (strstr(name, "videotoolbox")) {
            c->pix_fmt = AV_PIX_FMT_NV12;
            av_dict_set(&opts, "realtime", "1", 0);
            av_dict_set(&opts, "allow_sw", "0", 0);
            // Match the native VideoToolbox profile for Windows DXVA compatibility.
            av_dict_set(&opts, "profile",
                        c->codec_id == AV_CODEC_ID_H264 ? "constrained_baseline" : "main", 0);
        } else if (!strcmp(name, "av1_amf")) {
            c->pix_fmt = AV_PIX_FMT_NV12;
            c->profile = AV_PROFILE_AV1_MAIN;
            av_dict_set(&opts, "usage", "ultralowlatency", 0);
            av_dict_set(&opts, "preset", "speed", 0);
            av_dict_set(&opts, "rc", "cbr", 0);
            av_dict_set(&opts, "preanalysis", "0", 0);
            av_dict_set(&opts, "header_insertion_mode", "frame", 0);
            // AMF otherwise turns forced I pictures into non-recovery intra-only frames.
            av_dict_set(&opts, "forced_idr", "1", 0);
            av_dict_set(&opts, "async_depth", "1", 0);
        } else if (!strcmp(name, "av1_qsv")) {
            c->pix_fmt = AV_PIX_FMT_NV12;
            c->profile = AV_PROFILE_AV1_MAIN;
            if (bitrate > 0) {
                c->rc_max_rate = c->bit_rate;
                c->rc_buffer_size = (int)(c->bit_rate * fps_den / fps_num);
            }
            av_dict_set(&opts, "preset", "veryfast", 0);
            av_dict_set(&opts, "async_depth", "1", 0);
            av_dict_set(&opts, "low_delay_brc", "1", 0);
        } else if (!strcmp(name, "h264_amf")) {
            c->pix_fmt = AV_PIX_FMT_NV12;
            av_dict_set(&opts, "usage", "ultralowlatency", 0);
            av_dict_set(&opts, "quality", "speed", 0);
            av_dict_set(&opts, "rc", "cbr", 0);
            av_dict_set(&opts, "profile", "constrained_baseline", 0);
            av_dict_set(&opts, "preanalysis", "0", 0);
            av_dict_set(&opts, "bf", "0", 0);
            av_dict_set(&opts, "header_spacing", "1", 0);
            av_dict_set(&opts, "forced_idr", "1", 0);
            av_dict_set(&opts, "async_depth", "1", 0);
        } else if (!strcmp(name, "h264_qsv")) {
            c->pix_fmt = AV_PIX_FMT_NV12;
            av_dict_set(&opts, "preset", "veryfast", 0);
            av_dict_set(&opts, "async_depth", "1", 0);
            av_dict_set(&opts, "look_ahead", "0", 0);
            av_dict_set(&opts, "forced_idr", "1", 0);
            av_dict_set(&opts, "low_delay_brc", "1", 0);
            // QSV uses Media SDK profile values, not AV_PROFILE values.
            // MFX_PROFILE_AVC_CONSTRAINED_BASELINE = 66 | (0x100 << 1).
            av_dict_set(&opts, "profile", "578", 0);
            av_dict_set(&opts, "cavlc", "1", 0);
            c->profile = AV_PROFILE_H264_CONSTRAINED_BASELINE;
            if (bitrate > 0) {
                c->rc_max_rate = c->bit_rate;
                c->rc_buffer_size = (int)(c->bit_rate * fps_den / fps_num);
            }
        } else if (!strcmp(name, "libx264")) {
            // x264 baseline with progressive input and no B frames emits Constrained Baseline.
            av_dict_set(&opts, "preset", "ultrafast", 0);
            av_dict_set(&opts, "tune", "zerolatency", 0);
            av_dict_set(&opts, "profile", "baseline", 0);
            av_dict_set(&opts, "x264-params", "scenecut=0:bframes=0:repeat-headers=1", 0);
        } else if (!strcmp(name, "mjpeg")) {
            c->pix_fmt = AV_PIX_FMT_YUVJ420P;
            c->color_range = AVCOL_RANGE_JPEG;
            c->flags &= ~AV_CODEC_FLAG_LOW_DELAY;
        }
    }
    if (open_codec(s, codec, &opts, err, len) < 0)
        goto failopts;
    if (audio_ms && c->frame_size != audio_ms * 48) {
        snprintf(err, len, "Opus frame duration mismatch");
        goto failopts;
    }
    av_dict_free(&opts);
    return s;
failopts:
    av_dict_free(&opts);
fail:
    bm_close(s);
    return NULL;
}
bm_session *bm_decoder(const char *name, char *err, size_t len) {
    bm_session *s = alloc_session();
    if (!s)
        return NULL;
    const AVCodec *codec = avcodec_find_decoder_by_name(name);
    if (!codec) {
        error_code(err, len, name, AVERROR_DECODER_NOT_FOUND);
        goto fail;
    }
    s->codec = avcodec_alloc_context3(codec);
    if (!s->codec)
        goto fail;
    s->codec->thread_count = 1;
    s->codec->flags |= AV_CODEC_FLAG_LOW_DELAY;
    if (open_codec(s, codec, NULL, err, len) < 0)
        goto fail;
    return s;
fail:
    bm_close(s);
    return NULL;
}
bm_session *bm_audio_decoder(const char *name, const uint8_t *config, size_t size, int rate,
                             int channels, char *err, size_t len) {
    bm_session *s = alloc_session();
    if (!s)
        return NULL;
    s->audio = 1;
    const AVCodec *codec = avcodec_find_decoder_by_name(name);
    if (!codec || codec->type != AVMEDIA_TYPE_AUDIO) {
        error_code(err, len, name, AVERROR_DECODER_NOT_FOUND);
        goto fail;
    }
    s->codec = avcodec_alloc_context3(codec);
    if (!s->codec)
        goto fail;
    s->codec->thread_count = 1;
    s->codec->sample_rate = rate;
    av_channel_layout_default(&s->codec->ch_layout, channels);
    if (size) {
        s->codec->extradata = av_mallocz(size + AV_INPUT_BUFFER_PADDING_SIZE);
        if (!s->codec->extradata)
            goto fail;
        memcpy(s->codec->extradata, config, size);
        s->codec->extradata_size = (int)size;
    }
    if (open_codec(s, codec, NULL, err, len) < 0)
        goto fail;
    return s;
fail:
    bm_close(s);
    return NULL;
}
bm_session *bm_source(const char *format, const char *url, const char *options, int audio,
                      const char *passthrough, char *err, size_t len) {
    bm_session *s = alloc_session();
    if (!s)
        return NULL;
    s->audio = audio;
    s->input = avformat_alloc_context();
    if (!s->input)
        goto fail;
    s->input->interrupt_callback = (AVIOInterruptCB){cancelled, s};
    if (!strcmp(format, "dshow") && *passthrough) {
        s->input->video_codec_id =
            !strcmp(passthrough, "hevc") ? AV_CODEC_ID_HEVC : AV_CODEC_ID_H264;
    }
    // DirectShow's blocking read ignores AVIOInterruptCB. Poll its nonblocking
    // mode so unplugged/silent devices cannot prevent session teardown.
    if (!strcmp(format, "dshow"))
        s->input->flags |= AVFMT_FLAG_NONBLOCK;
    const AVInputFormat *fmt = av_find_input_format(format);
    if (!fmt) {
        snprintf(err, len, "input format %s unavailable", format);
        goto fail;
    }
    AVDictionary *opts = NULL;
    char *copy = strdup(options), *cursor = copy;
    while (cursor && *cursor) {
        char *next = strchr(cursor, '\n');
        if (next)
            *next++ = 0;
        char *eq = strchr(cursor, '=');
        if (eq) {
            *eq++ = 0;
            av_dict_set(&opts, cursor, eq, 0);
        }
        cursor = next;
    }
    free(copy);
    int result = avformat_open_input(&s->input, url, fmt, &opts);
    av_dict_free(&opts);
    if (result < 0) {
        error_code(err, len, "open capture", result);
        goto fail;
    }
    // Device demuxers supply codec parameters during open; avoid probing several
    // captured frames just to infer stream information already available.
    const AVCodec *codec = NULL;
    result = av_find_best_stream(s->input, audio ? AVMEDIA_TYPE_AUDIO : AVMEDIA_TYPE_VIDEO, -1, -1,
                                 NULL, 0);
    if (result < 0) {
        error_code(err, len, "capture stream", result);
        goto fail;
    }
    s->stream = result;
    AVStream *stream = s->input->streams[s->stream];
    enum AVCodecID want = !strcmp(passthrough, "h264")   ? AV_CODEC_ID_H264
                          : !strcmp(passthrough, "hevc") ? AV_CODEC_ID_HEVC
                                                         : AV_CODEC_ID_NONE;
    if (want != AV_CODEC_ID_NONE && stream->codecpar->codec_id == want) {
        const AVBitStreamFilter *filter =
            av_bsf_get_by_name(want == AV_CODEC_ID_H264 ? "h264_mp4toannexb" : "hevc_mp4toannexb");
        if (!filter) {
            snprintf(err, len, "Annex-B bitstream filter unavailable");
            goto fail;
        }
        result = av_bsf_alloc(filter, &s->bsf);
        if (result >= 0)
            result = avcodec_parameters_copy(s->bsf->par_in, stream->codecpar);
        if (result >= 0) {
            s->bsf->time_base_in = stream->time_base;
            result = av_bsf_init(s->bsf);
        }
        if (result < 0) {
            error_code(err, len, "passthrough setup", result);
            goto fail;
        }
        s->passthrough = want == AV_CODEC_ID_H264 ? 1 : 2;
        return s;
    }
    codec = avcodec_find_decoder(stream->codecpar->codec_id);
    if (!codec) {
        snprintf(err, len, "capture decoder unavailable");
        goto fail;
    }
    s->codec = avcodec_alloc_context3(codec);
    if (!s->codec)
        goto fail;
    result = avcodec_parameters_to_context(s->codec, s->input->streams[s->stream]->codecpar);
    if (result < 0) {
        error_code(err, len, "capture codec parameters", result);
        goto fail;
    }
    s->codec->thread_count = 1;
    if (open_codec(s, codec, NULL, err, len) < 0)
        goto fail;
    return s;
fail:
    bm_close(s);
    return NULL;
}
// YUVJ has the same plane layout as YUV; express its full range explicitly.
// Passing YUVJ to swscale also defeats its cache after internal normalization.
static enum AVPixelFormat scale_format(enum AVPixelFormat format, int *full_range) {
    switch (format) {
    case AV_PIX_FMT_YUVJ420P:
        format = AV_PIX_FMT_YUV420P;
        break;
    case AV_PIX_FMT_YUVJ422P:
        format = AV_PIX_FMT_YUV422P;
        break;
    case AV_PIX_FMT_YUVJ444P:
        format = AV_PIX_FMT_YUV444P;
        break;
    case AV_PIX_FMT_YUVJ440P:
        format = AV_PIX_FMT_YUV440P;
        break;
    case AV_PIX_FMT_YUVJ411P:
        format = AV_PIX_FMT_YUV411P;
        break;
    default:
        return format;
    }
    *full_range = 1;
    return format;
}
static int convert_frame(bm_session *s, uint8_t **out, int *size, int *w, int *h, char *err,
                         size_t len) {
    AVFrame *f = s->frame;
    if (s->audio) {
        if (!s->resample) {
            AVChannelLayout stereo = AV_CHANNEL_LAYOUT_STEREO;
            int result = swr_alloc_set_opts2(&s->resample, &stereo, AV_SAMPLE_FMT_S16, 48000,
                                             &f->ch_layout, f->format, f->sample_rate, 0, NULL);
            if (result >= 0)
                result = swr_init(s->resample);
            if (result < 0)
                return error_code(err, len, "audio conversion", result);
        }
        int count = swr_get_out_samples(s->resample, f->nb_samples);
        if (count < 0 || count > 480000)
            return error_code(err, len, "audio size", AVERROR(EINVAL));
        *out = malloc((size_t)count * 4);
        if (!*out)
            return -1;
        uint8_t *planes[] = {*out};
        int result = swr_convert(s->resample, planes, count, (const uint8_t **)f->extended_data,
                                 f->nb_samples);
        if (result < 0)
            return error_code(err, len, "audio conversion", result);
        *size = result * 4;
        *w = *h = 0;
        return 0;
    }
    *w = f->width;
    *h = f->height;
    if (*w < 1 || *h < 1 || *w > 8192 || *h > 8192)
        return error_code(err, len, "invalid frame size", AVERROR(EINVAL));
    *size = *w * *h * 4;
    *out = malloc(*size);
    if (!*out)
        return -1;
    int full_range = f->color_range == AVCOL_RANGE_JPEG;
    enum AVPixelFormat input = scale_format(f->format, &full_range);
    s->scale = sws_getCachedContext(s->scale, *w, *h, input, *w, *h, AV_PIX_FMT_BGRA,
                                    SWS_FAST_BILINEAR, NULL, NULL, NULL);
    if (!s->scale)
        return error_code(err, len, "pixel conversion", AVERROR(EINVAL));
    const int *coeff = sws_getCoefficients(f->colorspace);
    int result =
        sws_setColorspaceDetails(s->scale, coeff, full_range, coeff, 1, 0, 1 << 16, 1 << 16);
    if (result < 0)
        return error_code(err, len, "pixel color range", result);
    uint8_t *planes[] = {*out, NULL, NULL, NULL};
    int pitches[] = {*w * 4, 0, 0, 0};
    result =
        sws_scale(s->scale, (const uint8_t *const *)f->data, f->linesize, 0, *h, planes, pitches);
    if (result < 0)
        return error_code(err, len, "pixel conversion", result);
    return 0;
}
int bm_decode(bm_session *s, const uint8_t *data, size_t size, uint8_t **out, int *outsize, int *w,
              int *h, char *err, size_t len) {
    *out = NULL;
    *outsize = 0;
    if (size > 32 * 1024 * 1024)
        return error_code(err, len, "packet too large", AVERROR(EINVAL));
    av_packet_unref(s->packet);
    int result = av_new_packet(s->packet, (int)size);
    if (result < 0)
        return error_code(err, len, "allocate packet", result);
    memcpy(s->packet->data, data, size);
    result = avcodec_send_packet(s->codec, s->packet);
    av_packet_unref(s->packet);
    if (result < 0)
        return error_code(err, len, "decode send", result);
    av_frame_unref(s->frame);
    result = avcodec_receive_frame(s->codec, s->frame);
    if (result == AVERROR(EAGAIN))
        return 0;
    if (result < 0)
        return error_code(err, len, "decode receive", result);
    return convert_frame(s, out, outsize, w, h, err, len);
}
void bm_source_rate(bm_session *s, int *num, int *den) {
    AVStream *st = s->input->streams[s->stream];
    AVRational rate = st->avg_frame_rate.num ? st->avg_frame_rate : st->r_frame_rate;
    *num = rate.num;
    *den = rate.den;
}
int bm_source_codec(bm_session *s) {
    return s->passthrough;
}
int bm_read(bm_session *s, uint8_t **out, int *size, int *w, int *h, char *err, size_t len) {
    *out = NULL;
    *size = 0;
    for (;;) {
        if (cancelled(s))
            return 1;
        if (s->passthrough) {
            av_packet_unref(s->packet);
            int filtered = av_bsf_receive_packet(s->bsf, s->packet);
            if (filtered == 0) {
                *w = s->bsf->par_out->width;
                *h = s->bsf->par_out->height;
                *out = malloc(s->packet->size);
                if (!*out)
                    return -1;
                *size = s->packet->size;
                memcpy(*out, s->packet->data, *size);
                return 0;
            }
            if (filtered != AVERROR(EAGAIN))
                return filtered == AVERROR_EOF
                           ? 1
                           : error_code(err, len, "passthrough filter", filtered);
        }
        av_frame_unref(s->frame);
        int result = s->passthrough ? AVERROR(EAGAIN) : avcodec_receive_frame(s->codec, s->frame);
        if (result == 0)
            return convert_frame(s, out, size, w, h, err, len);
        if (result != AVERROR(EAGAIN) && result != AVERROR_EOF)
            return error_code(err, len, "capture decode", result);
        av_packet_unref(s->packet);
        result = av_read_frame(s->input, s->packet);
        if (result == AVERROR(EAGAIN)) {
#ifdef _WIN32
            Sleep(1);
            continue;
#else
            return 0;
#endif
        }
        if (result == AVERROR_EOF || cancelled(s))
            return 1;
        if (result < 0)
            return error_code(err, len, "capture read", result);
        if (s->packet->stream_index != s->stream)
            continue;
        result = s->passthrough ? av_bsf_send_packet(s->bsf, s->packet)
                                : avcodec_send_packet(s->codec, s->packet);
        if (result < 0)
            return error_code(err, len, "capture submit", result);
    }
}
int bm_encode(bm_session *s, const uint8_t *data, size_t size, int w, int h, const char *pixfmt,
              int idr, uint8_t **out, int *outsize, char *err, size_t len) {
    *out = NULL;
    *outsize = 0;
    AVCodecContext *c = s->codec;
    AVFrame *f = s->converted;
    av_frame_unref(f);
    f->format = s->vaapi ? AV_PIX_FMT_NV12 : c->pix_fmt;
    if (s->audio) {
        if (size != (size_t)c->frame_size * 4)
            return error_code(err, len, "invalid PCM frame size", AVERROR(EINVAL));
        f->format = c->sample_fmt;
        f->sample_rate = c->sample_rate;
        f->nb_samples = c->frame_size;
        av_channel_layout_copy(&f->ch_layout, &c->ch_layout);
        int result = av_frame_get_buffer(f, 0);
        if (result < 0)
            return error_code(err, len, "audio buffer", result);
        memcpy(f->data[0], data, size);
    } else {
        enum AVPixelFormat input = av_get_pix_fmt(pixfmt);
        if (input == AV_PIX_FMT_NONE || w < 1 || h < 1 || w > 8192 || h > 8192)
            return error_code(err, len, "invalid input pixels", AVERROR(EINVAL));
        int expected = av_image_get_buffer_size(input, w, h, 1);
        if (expected < 0 || (size_t)expected != size)
            return error_code(err, len, "invalid pixel buffer size", AVERROR(EINVAL));
        uint8_t *planes[4];
        int pitches[4];
        av_image_fill_arrays(planes, pitches, data, input, w, h, 1);
        f->width = c->width;
        f->height = c->height;
        f->color_range = c->color_range;
        f->colorspace = c->colorspace;
        int result = av_frame_get_buffer(f, 32);
        if (result < 0)
            return error_code(err, len, "video buffer", result);
        // Fit and center the source, retaining even chroma offsets.
        int dw = c->width, dh = c->height;
        if ((int64_t)w * dh > (int64_t)h * dw)
            dh = (int)((int64_t)h * dw / w) & ~1;
        else
            dw = (int)((int64_t)w * dh / h) & ~1;
        if (dw < 2)
            dw = 2;
        if (dh < 2)
            dh = 2;
        ptrdiff_t lines[4] = {f->linesize[0], f->linesize[1], f->linesize[2], f->linesize[3]};
        av_image_fill_black(f->data, lines, f->format, c->color_range, c->width, c->height);
        int x = ((c->width - dw) / 2) & ~1, y = ((c->height - dh) / 2) & ~1;
        uint8_t *dest[4] = {f->data[0] + y * f->linesize[0] + x, NULL, NULL, NULL};
        if (f->format == AV_PIX_FMT_NV12)
            dest[1] = f->data[1] + (y / 2) * f->linesize[1] + x;
        else {
            dest[1] = f->data[1] + (y / 2) * f->linesize[1] + x / 2;
            dest[2] = f->data[2] + (y / 2) * f->linesize[2] + x / 2;
        }
        int src_range = input == AV_PIX_FMT_BGRA, dst_range = c->color_range == AVCOL_RANGE_JPEG;
        enum AVPixelFormat scale_input = scale_format(input, &src_range),
                           scale_output = scale_format(f->format, &dst_range);
        s->scale = sws_getCachedContext(s->scale, w, h, scale_input, dw, dh, scale_output,
                                        SWS_FAST_BILINEAR, NULL, NULL, NULL);
        if (!s->scale)
            return error_code(err, len, "scale context", AVERROR(EINVAL));
        const int *src_coeff = sws_getCoefficients(SWS_CS_DEFAULT),
                  *dst_coeff = sws_getCoefficients(SWS_CS_ITU709);
        result = sws_setColorspaceDetails(s->scale, src_coeff, src_range, dst_coeff, dst_range, 0,
                                          1 << 16, 1 << 16);
        if (result < 0)
            return error_code(err, len, "scale color range", result);
        result =
            sws_scale(s->scale, (const uint8_t *const *)planes, pitches, 0, h, dest, f->linesize);
        if (result < 0)
            return error_code(err, len, "scale", result);
    }
    f->pts = s->pts;
    s->pts += s->audio ? f->nb_samples : 1;
    if (idr) {
        f->pict_type = AV_PICTURE_TYPE_I;
        f->flags |= AV_FRAME_FLAG_KEY;
    }
    AVFrame *send = f;
    if (s->vaapi) {
        av_frame_unref(s->uploaded);
        int result = av_hwframe_get_buffer(c->hw_frames_ctx, s->uploaded, 0);
        if (result >= 0)
            result = av_hwframe_transfer_data(s->uploaded, f, 0);
        if (result >= 0)
            result = av_frame_copy_props(s->uploaded, f);
        if (result < 0)
            return error_code(err, len, "VAAPI upload", result);
        send = s->uploaded;
    }
    int result = avcodec_send_frame(c, send);
    if (result < 0)
        return error_code(err, len, "encode send", result);
    av_packet_unref(s->packet);
    result = avcodec_receive_packet(c, s->packet);
    if (result == AVERROR(EAGAIN))
        return 0;
    if (result < 0)
        return error_code(err, len, "encode receive", result);
    *outsize = s->packet->size;
    *out = malloc(*outsize);
    if (!*out)
        return -1;
    memcpy(*out, s->packet->data, *outsize);
    return 0;
}
// AVFoundation and DirectShow expose device enumeration through its list_devices option.
// A thread-local collector avoids swallowing logs from other active sessions.
static _Thread_local char *device_log;
static _Thread_local size_t device_log_size;
static void log_callback(void *avcl, int level, const char *format, va_list args) {
    if (media_log_suppressed)
        return; // Media URLs can contain bearer tokens.
    if (device_log) {
        char line[2048];
        vsnprintf(line, sizeof(line), format, args);
        size_t n = strlen(line);
        if (device_log_size + n < 65535) {
            memcpy(device_log + device_log_size, line, n + 1);
            device_log_size += n;
        }
    } else
        av_log_default_callback(avcl, level, format, args);
}
char *bm_devices(const char *format) {
    const AVInputFormat *fmt = av_find_input_format(format);
    if (!fmt)
        return NULL;
    device_log = calloc(1, 65536);
    if (!device_log)
        return NULL;
    device_log_size = 0;
    AVDictionary *opts = NULL;
    av_dict_set(&opts, "list_devices", "true", 0);
    AVFormatContext *context = NULL;
    avformat_open_input(&context, "", fmt, &opts);
    avformat_close_input(&context);
    av_dict_free(&opts);
    char *result = device_log;
    device_log = NULL;
    device_log_size = 0;
    return result;
}

// URL media is demuxed once. Preserve compressed H.264/AAC so platform
// decoders remain the first choice and both streams share one source clock.
struct bm_media {
    AVFormatContext *input;
    AVBSFContext *filters[2];
    AVPacket *packet;
    int streams[2], pending, eof;
    uint8_t *audio_config;
    int audio_config_size;
    int hls_fmp4;
    AVPacket *preroll[4096];
    int preroll_count, preroll_index;
    size_t preroll_bytes;
    char *url;
    atomic_int cancelled;
};
int bm_media_available(void) {
    return avio_find_protocol_name("http://localhost/") &&
           avio_find_protocol_name("https://localhost/") && av_find_input_format("mov") &&
           av_find_input_format("hls") && av_find_input_format("mpegts") &&
           av_bsf_get_by_name("h264_mp4toannexb") && av_bsf_get_by_name("aac_adtstoasc");
}
static int media_cancelled(void *opaque) {
    return atomic_load(&((bm_media *)opaque)->cancelled);
}
bm_media *bm_media_alloc(void) {
    bm_media *m = calloc(1, sizeof(*m));
    if (!m)
        return NULL;
    m->streams[0] = m->streams[1] = -1;
    m->pending = -1;
    m->packet = av_packet_alloc();
    if (!m->packet) {
        free(m);
        return NULL;
    }
    return m;
}
void bm_media_interrupt(bm_media *m) {
    atomic_store(&m->cancelled, 1);
}
static void media_clear_preroll(bm_media *m) {
    for (int i = m->preroll_index; i < m->preroll_count; i++)
        av_packet_free(&m->preroll[i]);
    m->preroll_index = m->preroll_count = 0;
    m->preroll_bytes = 0;
}
static void media_close_impl(bm_media *m) {
    if (!m)
        return;
    avformat_close_input(&m->input);
    av_bsf_free(&m->filters[0]);
    av_bsf_free(&m->filters[1]);
    media_clear_preroll(m);
    av_packet_free(&m->packet);
    free(m->audio_config);
    free(m->url);
    free(m);
}
static int media_open_impl(bm_media *m, const char *url, char *err, size_t len) {
    if (!m->url) {
        m->url = strdup(url);
        if (!m->url)
            return -1;
    }
    m->input = avformat_alloc_context();
    if (!m->input)
        return -1;
    m->input->interrupt_callback = (AVIOInterruptCB){media_cancelled, m};
    AVDictionary *opts = NULL;
    // This also restricts redirects, HLS segments and key URIs. Never open local
    // files or arbitrary FFmpeg protocols from a sender-supplied playlist.
    av_dict_set(&opts, "protocol_whitelist", "http,https,tcp,tls,crypto", 0);
    av_dict_set(&opts, "rw_timeout", "10000000", 0);
    av_dict_set(&opts, "tls_verify", "1", 0);
    // FCUP proxy paths and signed CDN segment URLs may have no file suffix.
    // Resource access remains restricted by the protocol whitelist above.
    av_dict_set(&opts, "extension_picky", "0", 0);
    int result = avformat_open_input(&m->input, url, NULL, &opts);
    av_dict_free(&opts);
    if (result < 0)
        return error_code(err, len, "open media", result);
    result = avformat_find_stream_info(m->input, NULL);
    if (result < 0)
        return error_code(err, len, "probe media", result);
    m->streams[0] = av_find_best_stream(m->input, AVMEDIA_TYPE_VIDEO, -1, -1, NULL, 0);
    m->streams[1] = av_find_best_stream(m->input, AVMEDIA_TYPE_AUDIO, -1, m->streams[0], NULL, 0);
    if (m->streams[0] < 0 && m->streams[1] < 0) {
        snprintf(err, len, "media has no supported streams");
        return -1;
    }
    for (int i = 0; i < 2; i++) {
        if (m->streams[i] < 0)
            continue;
        AVStream *st = m->input->streams[m->streams[i]];
        if (st->codecpar->codec_id != (i ? AV_CODEC_ID_AAC : AV_CODEC_ID_H264)) {
            snprintf(err, len, "URL media requires H.264 video and AAC audio");
            return -1;
        }
        const AVBitStreamFilter *filter =
            av_bsf_get_by_name(i ? "aac_adtstoasc" : "h264_mp4toannexb");
        if (!filter) {
            snprintf(err, len, "media bitstream filter unavailable");
            return -1;
        }
        result = av_bsf_alloc(filter, &m->filters[i]);
        if (result >= 0)
            result = avcodec_parameters_copy(m->filters[i]->par_in, st->codecpar);
        if (result >= 0) {
            m->filters[i]->time_base_in = st->time_base;
            result = av_bsf_init(m->filters[i]);
        }
        if (result < 0)
            return error_code(err, len, "media filter setup", result);
    }
    m->hls_fmp4 = !strcmp(m->input->iformat->name, "hls") && m->streams[0] >= 0 &&
                  m->input->streams[m->streams[0]]->codecpar->extradata_size > 0 &&
                  m->input->streams[m->streams[0]]->codecpar->extradata[0] == 1;
    return 0;
}
int64_t bm_media_duration(bm_media *m) {
    return m->input->duration == AV_NOPTS_VALUE ? 0 : m->input->duration;
}
static int64_t media_time(bm_media *m, int i, int64_t value) {
    if (value == AV_NOPTS_VALUE)
        return INT64_MIN;
    AVRational tb = m->filters[i]->time_base_out;
    int64_t origin = m->input->start_time == AV_NOPTS_VALUE ? 0 : m->input->start_time;
    return (int64_t)((long double)value * tb.num * 1000000 / tb.den) - origin;
}
static int media_read_impl(bm_media *m, uint8_t **out, int *size, int *w, int *h, int *audio,
                           int64_t *pts, int64_t *dts, uint8_t **config, int *configsize, int *rate,
                           int *channels, char *err, size_t len) {
    *out = NULL;
    *size = 0;
    *config = NULL;
    *configsize = 0;
    for (;;) {
        if (media_cancelled(m))
            return 1;
        if (m->pending >= 0) {
            int i = m->pending;
            av_packet_unref(m->packet);
            int result = av_bsf_receive_packet(m->filters[i], m->packet);
            if (result == 0) {
                AVCodecParameters *par = m->filters[i]->par_out;
                if (i) {
                    const uint8_t *data = par->extradata;
                    int count = par->extradata_size;
                    for (int n = 0; n < m->packet->side_data_elems; n++)
                        if (m->packet->side_data[n].type == AV_PKT_DATA_NEW_EXTRADATA) {
                            data = m->packet->side_data[n].data;
                            count = (int)m->packet->side_data[n].size;
                            break;
                        }
                    if (count > 65536)
                        return error_code(err, len, "audio config size", AVERROR(EINVAL));
                    if (count > 0) {
                        uint8_t *copy = malloc(count);
                        if (!copy)
                            return -1;
                        memcpy(copy, data, count);
                        free(m->audio_config);
                        m->audio_config = copy;
                        m->audio_config_size = count;
                    }
                }
                *audio = i;
                *w = par->width;
                *h = par->height;
                *rate = par->sample_rate;
                *channels = par->ch_layout.nb_channels;
                *pts = media_time(m, i, m->packet->pts);
                *dts = media_time(m, i, m->packet->dts);
                if (m->packet->size <= 0 || m->packet->size > 32 * 1024 * 1024 ||
                    par->extradata_size > 65536)
                    return error_code(err, len, "media packet size", AVERROR(EINVAL));
                *out = malloc(m->packet->size);
                if (!*out)
                    return -1;
                *size = m->packet->size;
                memcpy(*out, m->packet->data, *size);
                if (i && m->audio_config_size) {
                    *config = malloc(m->audio_config_size);
                    if (!*config) {
                        free(*out);
                        *out = NULL;
                        return -1;
                    }
                    *configsize = m->audio_config_size;
                    memcpy(*config, m->audio_config, *configsize);
                }
                return 0;
            }
            m->pending = -1;
            if (result != AVERROR(EAGAIN) && result != AVERROR_EOF)
                return error_code(err, len, "media filter", result);
        }
        if (m->eof && m->preroll_index == m->preroll_count)
            return 1;
        av_packet_unref(m->packet);
        int result;
        if (m->preroll_index < m->preroll_count) {
            AVPacket *packet = m->preroll[m->preroll_index++];
            av_packet_move_ref(m->packet, packet);
            av_packet_free(&packet);
            result = 0;
            if (m->preroll_index == m->preroll_count)
                media_clear_preroll(m);
        } else
            result = av_read_frame(m->input, m->packet);
        if (result == AVERROR_EOF) {
            m->eof = 1;
            return 1;
        }
        if (result < 0)
            return error_code(err, len, "media read", result);
        for (int i = 0; i < 2; i++)
            if (m->packet->stream_index == m->streams[i]) {
                result = av_bsf_send_packet(m->filters[i], m->packet);
                if (result < 0)
                    return error_code(err, len, "media submit", result);
                m->pending = i;
                break;
            }
    }
}
static int media_seek_impl(bm_media *m, int64_t position, char *err, size_t len) {
    if (!m->input)
        return error_code(err, len, "media closed", AVERROR(EIO));
    if (m->hls_fmp4) {
        // FFmpeg 7's HLS/MOV sub-demuxer cannot reliably reset fragment sample
        // cursors via read_seek. Reopen and retain only the preceding GOP instead.
        // This costs earlier segment reads, but never returns undecodable deltas.
        media_clear_preroll(m);
        avformat_close_input(&m->input);
        for (int i = 0; i < 2; i++)
            av_bsf_free(&m->filters[i]);
        av_packet_unref(m->packet);
        m->pending = -1;
        m->eof = 1;
        int result = media_open_impl(m, m->url, err, len);
        if (result < 0)
            return result;
        m->eof = 0;
        int64_t origin = m->input->start_time == AV_NOPTS_VALUE ? 0 : m->input->start_time;
        int started = 0;
        for (;;) {
            if (media_cancelled(m))
                return error_code(err, len, "media seek cancelled", AVERROR_EXIT);
            av_packet_unref(m->packet);
            result = av_read_frame(m->input, m->packet);
            if (result == AVERROR_EOF) {
                m->eof = 1;
                break;
            }
            if (result < 0)
                return error_code(err, len, "media preroll read", result);
            int video = m->packet->stream_index == m->streams[0];
            AVRational tb = m->input->streams[m->packet->stream_index]->time_base;
            int64_t stamp = m->packet->dts == AV_NOPTS_VALUE ? m->packet->pts : m->packet->dts;
            if (stamp == AV_NOPTS_VALUE)
                return error_code(err, len, "media preroll timestamp", AVERROR_INVALIDDATA);
            int64_t us = (int64_t)((long double)stamp * tb.num * 1000000 / tb.den) - origin;
            if (video && (m->packet->flags & AV_PKT_FLAG_KEY) && (us <= position || !started)) {
                media_clear_preroll(m);
                started = 1;
            }
            if (!started || (!video && m->packet->stream_index != m->streams[1]))
                continue;
            if (m->packet->size < 0 || m->preroll_count == 4096 ||
                m->preroll_bytes + (size_t)m->packet->size > 64 * 1024 * 1024) {
                media_clear_preroll(m);
                return error_code(err, len, "media seek preroll exceeds budget", AVERROR(ENOMEM));
            }
            AVPacket *copy = av_packet_clone(m->packet);
            if (!copy)
                return -1;
            m->preroll[m->preroll_count++] = copy;
            m->preroll_bytes += m->packet->size;
            if (video && us >= position)
                break;
        }
        av_packet_unref(m->packet);
        return 0;
    }
    int64_t origin = m->input->start_time == AV_NOPTS_VALUE ? 0 : m->input->start_time;
    int result =
        avformat_seek_file(m->input, -1, INT64_MIN, position + origin, position + origin, 0);
    if (result < 0)
        return error_code(err, len, "media seek", result);
    for (int i = 0; i < 2; i++)
        if (m->filters[i])
            av_bsf_flush(m->filters[i]);
    av_packet_unref(m->packet);
    m->pending = -1;
    m->eof = 0;
    return 0;
}

int bm_media_open(bm_media *m, const char *url, char *err, size_t len) {
    media_log_suppressed = 1;
    int result = media_open_impl(m, url, err, len);
    media_log_suppressed = 0;
    return result;
}
int bm_media_read(bm_media *m, uint8_t **out, int *size, int *w, int *h, int *audio, int64_t *pts,
                  int64_t *dts, uint8_t **config, int *configsize, int *rate, int *channels,
                  char *err, size_t len) {
    media_log_suppressed = 1;
    int result = media_read_impl(m, out, size, w, h, audio, pts, dts, config, configsize, rate,
                                 channels, err, len);
    media_log_suppressed = 0;
    return result;
}
int bm_media_seek(bm_media *m, int64_t position, char *err, size_t len) {
    media_log_suppressed = 1;
    int result = media_seek_impl(m, position, err, len);
    media_log_suppressed = 0;
    return result;
}
void bm_media_close(bm_media *m) {
    media_log_suppressed = 1;
    media_close_impl(m);
    media_log_suppressed = 0;
}
