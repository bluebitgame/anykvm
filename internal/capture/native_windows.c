//go:build windows && cgo

#define COBJMACROS
#define WINVER 0x0a00
#include <windows.h>
#include <d3d11.h>
#include <d3d10.h>
#include <dxgi1_2.h>
#include <mfapi.h>
#include <mfidl.h>
#include <mfreadwrite.h>
#include <mferror.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <math.h>
#include "native_windows.h"
#define DROP(t, p)                                                                                 \
    do {                                                                                           \
        if (p) {                                                                                   \
            t##_Release(p);                                                                        \
            p = NULL;                                                                              \
        }                                                                                          \
    } while (0)
#define TRY(x)                                                                                     \
    do {                                                                                           \
        hr = (x);                                                                                  \
        if (FAILED(hr))                                                                            \
            goto done;                                                                             \
    } while (0)
#define LIMIT (128u * 1024u * 1024u)

// The asynchronous reader keeps capture callbacks independent of Go threads.
// A bounded audio queue preserves packet order; video retains only the latest sample.
typedef struct sample_node {
    IMFSample *sample;
    struct sample_node *next;
} sample_node;
typedef struct {
    IMFSourceReaderCallback iface;
    LONG refs;
    CRITICAL_SECTION lock;
    IMFSourceReader *reader; // borrowed, cleared under lock before reader release
    sample_node *first, *last;
    unsigned count;
    int audio, stop;
    HRESULT status;
} reader_callback;
struct bk_win_capture {
    int audio, screen, width, height, rotation, hide, rate_num, rate_den, kind;
    IMFMediaSource *source;
    IMFSourceReader *reader;
    reader_callback *callback;
    int mf_started;
    LONG stride;
    char input_format[40];
    ID3D11Device *device;
    ID3D11DeviceContext *context;
    IDXGIOutput1 *output;
    IDXGIOutputDuplication *duplication;
    ID3D11Texture2D *shared;
    HANDLE handle;
    DXGI_OUTPUT_DESC output_desc;
    DXGI_ADAPTER_DESC1 adapter_desc;
    int64_t last_present;
};
struct bk_win_frame {
    bk_win_timing timing;
    int width, height, rotation, kind;
    IMFMediaBuffer *borrowed;
    IMF2DBuffer *borrowed2d;
    int borrow_lock;
    LONG stride;
    IMFSample *sample;
    ID3D11Texture2D *texture;
    ID3D11Device *device;
    ID3D11DeviceContext *context;
    HANDLE handle;
};
static void error_text(char *out, size_t size, const char *where, HRESULT hr) {
    snprintf(out, size, "%s failed (HRESULT 0x%08lx)", where, (unsigned long)hr);
}
static char *utf8(const wchar_t *value) {
    int n = WideCharToMultiByte(CP_UTF8, 0, value, -1, NULL, 0, NULL, NULL);
    char *s = n > 0 ? malloc(n) : NULL;
    if (s)
        WideCharToMultiByte(CP_UTF8, 0, value, -1, s, n, NULL, NULL);
    return s;
}
static HRESULT enumerate(int audio, IMFActivate ***devices, UINT32 *count) {
    IMFAttributes *attrs = NULL;
    HRESULT hr = MFCreateAttributes(&attrs, 1);
    if (SUCCEEDED(hr))
        hr = IMFAttributes_SetGUID(attrs, &MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE,
                                   audio ? &MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE_AUDCAP_GUID
                                         : &MF_DEVSOURCE_ATTRIBUTE_SOURCE_TYPE_VIDCAP_GUID);
    if (SUCCEEDED(hr))
        hr = MFEnumDeviceSources(attrs, devices, count);
    DROP(IMFAttributes, attrs);
    return hr;
}

// Activation and metadata enumeration never request samples or start streaming.
static HRESULT activate_media(const char *id, int audio, IMFMediaSource **source) {
    IMFActivate **devices = NULL;
    UINT32 count = 0;
    HRESULT hr = enumerate(audio, &devices, &count);
    if (FAILED(hr))
        return hr;
    hr = HRESULT_FROM_WIN32(ERROR_NOT_FOUND);
    char *end = NULL;
    unsigned long index = strtoul(id, &end, 10);
    int numeric = (*id && end && !*end);
    for (UINT32 i = 0; i < count; i++) {
        wchar_t *name = NULL;
        UINT32 length = 0;
        if (SUCCEEDED(IMFActivate_GetAllocatedString(
                devices[i], &MF_DEVSOURCE_ATTRIBUTE_FRIENDLY_NAME, &name, &length))) {
            char *name8 = utf8(name);
            CoTaskMemFree(name);
            int matches =
                (!*id && i == 0) || (numeric && i == index) || (name8 && !strcmp(name8, id));
            free(name8);
            if (matches) {
                hr = IMFActivate_ActivateObject(devices[i], &IID_IMFMediaSource, (void **)source);
                break;
            }
        }
    }
    for (UINT32 i = 0; i < count; i++)
        IMFActivate_Release(devices[i]);
    CoTaskMemFree(devices);
    return hr;
}
static void media_format(IMFMediaType *type, const GUID *subtype, char *name, size_t size) {
    const char *known = NULL;
    if (IsEqualGUID(subtype, &MFVideoFormat_NV12)) {
        UINT32 range = 0;
        IMFMediaType_GetUINT32(type, &MF_MT_VIDEO_NOMINAL_RANGE, &range);
        known = range == MFNominalRange_0_255 ? "nv12_full" : "nv12";
    } else if (IsEqualGUID(subtype, &MFVideoFormat_YUY2))
        known = "yuyv422";
    else if (IsEqualGUID(subtype, &MFVideoFormat_UYVY))
        known = "uyvy422";
    else if (IsEqualGUID(subtype, &MFVideoFormat_MJPG))
        known = "mjpeg";
    else if (IsEqualGUID(subtype, &MFVideoFormat_H264))
        known = "h264";
    else if (IsEqualGUID(subtype, &MFVideoFormat_ARGB32))
        known = "bgra";
    else if (IsEqualGUID(subtype, &MFVideoFormat_RGB32))
        known = "bgr0";
    else if (IsEqualGUID(subtype, &MFVideoFormat_RGB24))
        known = "bgr24";
    else if (IsEqualGUID(subtype, &MFVideoFormat_I420))
        known = "i420";
    else if (IsEqualGUID(subtype, &MFVideoFormat_IYUV))
        known = "iyuv";
    else if (IsEqualGUID(subtype, &MFVideoFormat_YV12))
        known = "yv12";
    else if (IsEqualGUID(subtype, &MFVideoFormat_P010))
        known = "p010";
    else if (IsEqualGUID(subtype, &MFVideoFormat_P016))
        known = "p016";
    else if (IsEqualGUID(subtype, &MFVideoFormat_Y210))
        known = "y210";
    else if (IsEqualGUID(subtype, &MFVideoFormat_Y216))
        known = "y216";
    else if (IsEqualGUID(subtype, &MFVideoFormat_v210))
        known = "v210";
    else if (IsEqualGUID(subtype, &MFVideoFormat_v216))
        known = "v216";
    if (known) {
        snprintf(name, size, "%s", known);
        return;
    }
    // Standard MF subtypes preserve their native FourCC/numeric format code.
    GUID base = {0, 0x0000, 0x0010, {0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71}};
    base.Data1 = subtype->Data1;
    if (IsEqualGUID(subtype, &base)) {
        snprintf(name, size, "fourcc_%08lx", (unsigned long)subtype->Data1);
        return;
    }
    snprintf(name, size, "guid_%08lx%04x%04x%02x%02x%02x%02x%02x%02x%02x%02x",
             (unsigned long)subtype->Data1, subtype->Data2, subtype->Data3, subtype->Data4[0],
             subtype->Data4[1], subtype->Data4[2], subtype->Data4[3], subtype->Data4[4],
             subtype->Data4[5], subtype->Data4[6], subtype->Data4[7]);
}
typedef struct {
    UINT64 size, rate, min_rate, max_rate;
    GUID subtype;
    char format[40];
} media_mode;
static double media_fps(UINT64 value) {
    return (UINT32)value ? (double)(value >> 32) / (UINT32)value : 0;
}
static int read_mode(IMFMediaType *type, media_mode *mode) {
    memset(mode, 0, sizeof(*mode));
    if (FAILED(IMFMediaType_GetUINT64(type, &MF_MT_FRAME_SIZE, &mode->size)) ||
        FAILED(IMFMediaType_GetUINT64(type, &MF_MT_FRAME_RATE, &mode->rate)) ||
        FAILED(IMFMediaType_GetGUID(type, &MF_MT_SUBTYPE, &mode->subtype)))
        return 0;
    if (!(mode->size >> 32) || !(UINT32)mode->size || (mode->size >> 32) > 65535 ||
        (UINT32)mode->size > 65535 || !(mode->rate >> 32) || (mode->rate >> 32) > INT_MAX ||
        !(UINT32)mode->rate || (UINT32)mode->rate > INT_MAX)
        return 0;
    mode->min_rate = mode->max_rate = mode->rate;
    UINT64 min = 0, max = 0;
    if (SUCCEEDED(IMFMediaType_GetUINT64(type, &MF_MT_FRAME_RATE_RANGE_MIN, &min)) &&
        SUCCEEDED(IMFMediaType_GetUINT64(type, &MF_MT_FRAME_RATE_RANGE_MAX, &max)) &&
        media_fps(min) > 0 && media_fps(max) >= media_fps(min)) {
        mode->min_rate = min;
        mode->max_rate = max;
    }
    media_format(type, &mode->subtype, mode->format, sizeof(mode->format));
    return 1;
}
char *bk_win_modes(const char *id, char *error, size_t capacity) {
    IMFMediaSource *source = NULL;
    IMFSourceReader *reader = NULL;
    IMFMediaType *type = NULL;
    char *result = NULL;
    size_t used = 0;
    HRESULT com = CoInitializeEx(NULL, COINIT_MULTITHREADED),
            hr = MFStartup(MF_VERSION, MFSTARTUP_FULL);
    int started = SUCCEEDED(hr);
    if (FAILED(hr))
        goto done;
    TRY(activate_media(id, 0, &source));
    TRY(MFCreateSourceReaderFromMediaSource(source, NULL, &reader));
    result = malloc(3);
    if (!result) {
        hr = E_OUTOFMEMORY;
        goto done;
    }
    result[used++] = '[';
    result[used] = 0;
    for (DWORD i = 0;; i++) {
        hr = IMFSourceReader_GetNativeMediaType(reader, MF_SOURCE_READER_FIRST_VIDEO_STREAM, i,
                                                &type);
        if (hr == MF_E_NO_MORE_TYPES) {
            hr = S_OK;
            break;
        }
        if (FAILED(hr))
            goto done;
        media_mode mode;
        if (read_mode(type, &mode)) {
            char row[256];
            int n = snprintf(
                row, sizeof(row),
                "%s{\"width\":%u,\"height\":%u,\"pixelFormat\":\"%s\",\"minFPS\":%.12g,\"maxFPS\":%.12g}",
                used > 1 ? "," : "", (UINT32)(mode.size >> 32), (UINT32)mode.size, mode.format,
                media_fps(mode.min_rate), media_fps(mode.max_rate));
            if (n < 0 || (size_t)n >= sizeof(row) || used + (size_t)n + 2 > LIMIT) {
                hr = E_OUTOFMEMORY;
                goto done;
            }
            char *next = realloc(result, used + (size_t)n + 2);
            if (!next) {
                hr = E_OUTOFMEMORY;
                goto done;
            }
            result = next;
            memcpy(result + used, row, (size_t)n);
            used += (size_t)n;
            result[used] = 0;
        }
        DROP(IMFMediaType, type);
    }
    result[used++] = ']';
    result[used] = 0;
done:
    DROP(IMFMediaType, type);
    DROP(IMFSourceReader, reader);
    if (source)
        IMFMediaSource_Shutdown(source);
    DROP(IMFMediaSource, source);
    if (started)
        MFShutdown();
    if (SUCCEEDED(com))
        CoUninitialize();
    if (FAILED(hr)) {
        free(result);
        result = NULL;
        error_text(error, capacity, "Media Foundation mode enumeration", hr);
    }
    return result;
}
// Select the camera input before installing a decoder/color converter. This is
// independent of the NV12, compressed MJPEG, or RGB32 consumer output type.
static HRESULT select_video(bk_win_capture *c, int width, int height, double fps,
                            const char *format, media_mode *selected) {
    IMFMediaType *type = NULL, *chosen = NULL;
    IMFSourceReaderEx *extended = NULL;
    media_mode current = {0}, mode = {0};
    double best = HUGE_VAL;
    HRESULT hr = IMFSourceReader_GetNativeMediaType(c->reader, MF_SOURCE_READER_FIRST_VIDEO_STREAM,
                                                    MF_SOURCE_READER_CURRENT_TYPE_INDEX, &type);
    if (SUCCEEDED(hr) && !read_mode(type, &current))
        memset(&current, 0, sizeof(current));
    DROP(IMFMediaType, type);
    if (width <= 0 && height <= 0 && fps <= 0 && !*format && current.size) {
        *selected = current;
        return S_OK;
    }
    for (DWORD i = 0;; i++) {
        hr = IMFSourceReader_GetNativeMediaType(c->reader, MF_SOURCE_READER_FIRST_VIDEO_STREAM, i,
                                                &type);
        if (hr == MF_E_NO_MORE_TYPES) {
            hr = S_OK;
            break;
        }
        if (FAILED(hr))
            goto done;
        if (read_mode(type, &mode) && (width <= 0 || (UINT32)(mode.size >> 32) == (UINT32)width) &&
            (height <= 0 || (UINT32)mode.size == (UINT32)height) &&
            (!*format || !strcmp(format, mode.format)) &&
            (fps <= 0 || (fps >= media_fps(mode.min_rate) - 0.001 &&
                          fps <= media_fps(mode.max_rate) + 0.001))) {
            double score = fabs(media_fps(mode.rate) - media_fps(current.rate));
            if (current.size && mode.size != current.size)
                score += 1000000;
            if (current.size && !IsEqualGUID(&mode.subtype, &current.subtype))
                score += 1000;
            if (score < best) {
                DROP(IMFMediaType, chosen);
                chosen = type;
                IMFMediaType_AddRef(chosen);
                *selected = mode;
                best = score;
            }
        }
        DROP(IMFMediaType, type);
    }
    if (!chosen) {
        hr = MF_E_INVALIDMEDIATYPE;
        goto done;
    }
    if (fps > 0 && fabs(media_fps(selected->rate) - fps) > 0.001) {
        UINT32 num = (UINT32)llround(fps * 1000000), den = 1000000;
        selected->rate = ((UINT64)num << 32) | den;
        TRY(IMFMediaType_SetUINT64(chosen, &MF_MT_FRAME_RATE, selected->rate));
    }
    TRY(IMFSourceReader_QueryInterface(c->reader, &IID_IMFSourceReaderEx, (void **)&extended));
    DWORD flags = 0;
    TRY(IMFSourceReaderEx_SetNativeMediaType(extended, MF_SOURCE_READER_FIRST_VIDEO_STREAM, chosen,
                                             &flags));
done:
    DROP(IMFMediaType, type);
    DROP(IMFMediaType, chosen);
    DROP(IMFSourceReaderEx, extended);
    return hr;
}
static HRESULT verify_video(bk_win_capture *c, const media_mode *selected) {
    IMFMediaType *type = NULL;
    media_mode actual = {0};
    HRESULT hr = IMFSourceReader_GetNativeMediaType(c->reader, MF_SOURCE_READER_FIRST_VIDEO_STREAM,
                                                    MF_SOURCE_READER_CURRENT_TYPE_INDEX, &type);
    if (SUCCEEDED(hr) && (!read_mode(type, &actual) || actual.size != selected->size ||
                          strcmp(actual.format, selected->format) ||
                          fabs(media_fps(actual.rate) - media_fps(selected->rate)) > 0.001))
        hr = MF_E_INVALIDMEDIATYPE;
    DROP(IMFMediaType, type);
    return hr;
}
static void clear_samples(reader_callback *cb) {
    while (cb->first) {
        sample_node *n = cb->first;
        cb->first = n->next;
        IMFSample_Release(n->sample);
        free(n);
    }
    cb->last = NULL;
    cb->count = 0;
}
static HRESULT STDMETHODCALLTYPE cb_query(IMFSourceReaderCallback *self, REFIID iid, void **out) {
    if (IsEqualIID(iid, &IID_IUnknown) || IsEqualIID(iid, &IID_IMFSourceReaderCallback)) {
        *out = self;
        InterlockedIncrement(&((reader_callback *)self)->refs);
        return S_OK;
    }
    *out = NULL;
    return E_NOINTERFACE;
}
static ULONG STDMETHODCALLTYPE cb_add(IMFSourceReaderCallback *self) {
    return InterlockedIncrement(&((reader_callback *)self)->refs);
}
static ULONG STDMETHODCALLTYPE cb_release(IMFSourceReaderCallback *self) {
    reader_callback *cb = (reader_callback *)self;
    ULONG refs = InterlockedDecrement(&cb->refs);
    if (!refs) {
        clear_samples(cb);
        DeleteCriticalSection(&cb->lock);
        free(cb);
    }
    return refs;
}
static HRESULT STDMETHODCALLTYPE cb_read(IMFSourceReaderCallback *self, HRESULT status,
                                         DWORD stream, DWORD flags, LONGLONG ts,
                                         IMFSample *sample) {
    (void)stream;
    (void)ts;
    reader_callback *cb = (reader_callback *)self;
    EnterCriticalSection(&cb->lock);
    if (!cb->stop) {
        cb->status = status;
        if (flags & (MF_SOURCE_READERF_ERROR | MF_SOURCE_READERF_ENDOFSTREAM |
                     MF_SOURCE_READERF_CURRENTMEDIATYPECHANGED))
            cb->status = MF_E_INVALIDMEDIATYPE;
        if (SUCCEEDED(cb->status) && sample) {
            if (!cb->audio)
                clear_samples(cb);
            if (cb->count >= 32)
                cb->status = HRESULT_FROM_WIN32(ERROR_BUFFER_OVERFLOW);
            else {
                sample_node *n = calloc(1, sizeof(*n));
                if (!n)
                    cb->status = E_OUTOFMEMORY;
                else {
                    IMFSample_AddRef(sample);
                    n->sample = sample;
                    if (cb->last)
                        cb->last->next = n;
                    else
                        cb->first = n;
                    cb->last = n;
                    cb->count++;
                }
            }
        }
        if (SUCCEEDED(cb->status) && cb->reader)
            cb->status = IMFSourceReader_ReadSample(cb->reader,
                                                    cb->audio ? MF_SOURCE_READER_FIRST_AUDIO_STREAM
                                                              : MF_SOURCE_READER_FIRST_VIDEO_STREAM,
                                                    0, NULL, NULL, NULL, NULL);
    }
    LeaveCriticalSection(&cb->lock);
    return S_OK;
}
static HRESULT STDMETHODCALLTYPE cb_flush(IMFSourceReaderCallback *self, DWORD stream) {
    (void)self;
    (void)stream;
    return S_OK;
}
static HRESULT STDMETHODCALLTYPE cb_event(IMFSourceReaderCallback *self, DWORD stream,
                                          IMFMediaEvent *event) {
    (void)self;
    (void)stream;
    (void)event;
    return S_OK;
}
static IMFSourceReaderCallbackVtbl cb_vtable = {cb_query, cb_add,   cb_release,
                                                cb_read,  cb_flush, cb_event};

static HRESULT open_media(bk_win_capture *c, const char *id, int width, int height, double fps,
                          const char *format, int jpeg) {
    HRESULT hr;
    IMFAttributes *attrs = NULL;
    IMFMediaType *type = NULL;
    media_mode selected = {0};
    TRY(MFStartup(MF_VERSION, MFSTARTUP_FULL));
    c->mf_started = 1;
    TRY(activate_media(id, c->audio, &c->source));
    c->callback = calloc(1, sizeof(*c->callback));
    if (!c->callback) {
        hr = E_OUTOFMEMORY;
        goto done;
    }
    c->callback->iface.lpVtbl = &cb_vtable;
    c->callback->refs = 1;
    c->callback->audio = c->audio;
    InitializeCriticalSection(&c->callback->lock);
    TRY(MFCreateAttributes(&attrs, 3));
    TRY(IMFAttributes_SetUnknown(attrs, &MF_SOURCE_READER_ASYNC_CALLBACK,
                                 (IUnknown *)&c->callback->iface));
    if (!c->audio)
        TRY(IMFAttributes_SetUINT32(attrs, &MF_SOURCE_READER_ENABLE_ADVANCED_VIDEO_PROCESSING,
                                    TRUE));
    TRY(MFCreateSourceReaderFromMediaSource(c->source, attrs, &c->reader));
    DWORD stream =
        c->audio ? MF_SOURCE_READER_FIRST_AUDIO_STREAM : MF_SOURCE_READER_FIRST_VIDEO_STREAM;
    TRY(IMFSourceReader_SetStreamSelection(c->reader, MF_SOURCE_READER_ALL_STREAMS, FALSE));
    TRY(IMFSourceReader_SetStreamSelection(c->reader, stream, TRUE));
    if (!c->audio) {
        TRY(select_video(c, width, height, fps, format, &selected));
        snprintf(c->input_format, sizeof(c->input_format), "%s", selected.format);
    }
    TRY(MFCreateMediaType(&type));
    TRY(IMFMediaType_SetGUID(type, &MF_MT_MAJOR_TYPE,
                             c->audio ? &MFMediaType_Audio : &MFMediaType_Video));
    TRY(IMFMediaType_SetGUID(
        type, &MF_MT_SUBTYPE,
        c->audio ? &MFAudioFormat_PCM
                 : (jpeg == 2 && IsEqualGUID(&selected.subtype, &MFVideoFormat_MJPG)
                        ? &MFVideoFormat_MJPG
                    : jpeg == 1 && strcmp(selected.format, "nv12_full") ? &MFVideoFormat_NV12
                                                                        : &MFVideoFormat_RGB32)));
    if (!c->audio) {
        TRY(IMFMediaType_SetUINT64(type, &MF_MT_FRAME_SIZE, selected.size));
        TRY(IMFMediaType_SetUINT64(type, &MF_MT_FRAME_RATE, selected.rate));
    }
    if (c->audio) {
        TRY(IMFMediaType_SetUINT32(type, &MF_MT_AUDIO_NUM_CHANNELS, 2));
        TRY(IMFMediaType_SetUINT32(type, &MF_MT_AUDIO_SAMPLES_PER_SECOND, 48000));
        TRY(IMFMediaType_SetUINT32(type, &MF_MT_AUDIO_BITS_PER_SAMPLE, 16));
        TRY(IMFMediaType_SetUINT32(type, &MF_MT_AUDIO_BLOCK_ALIGNMENT, 4));
        TRY(IMFMediaType_SetUINT32(type, &MF_MT_AUDIO_AVG_BYTES_PER_SECOND, 192000));
    }
    hr = IMFSourceReader_SetCurrentMediaType(c->reader, stream, NULL, type);
    if (FAILED(hr) && !c->audio) {
        TRY(IMFMediaType_SetGUID(type, &MF_MT_SUBTYPE, &MFVideoFormat_RGB32));
        TRY(IMFSourceReader_SetCurrentMediaType(c->reader, stream, NULL, type));
    } else if (FAILED(hr))
        goto done;
    DROP(IMFMediaType, type);
    TRY(IMFSourceReader_GetCurrentMediaType(c->reader, stream, &type));
    if (!c->audio) {
        GUID subtype;
        TRY(IMFMediaType_GetGUID(type, &MF_MT_SUBTYPE, &subtype));
        c->kind = IsEqualGUID(&subtype, &MFVideoFormat_NV12)   ? 1
                  : IsEqualGUID(&subtype, &MFVideoFormat_MJPG) ? 2
                                                               : 0;
        UINT64 size = 0;
        UINT32 stride = 0;
        TRY(IMFMediaType_GetUINT64(type, &MF_MT_FRAME_SIZE, &size));
        c->width = (int)(size >> 32);
        c->height = (int)(UINT32)size;
        if (c->width <= 0 || c->height <= 0 || (size_t)c->width * c->height * 4 > LIMIT) {
            hr = E_INVALIDARG;
            goto done;
        }
        if (c->kind == 2)
            stride = c->width;
        else if (FAILED(IMFMediaType_GetUINT32(type, &MF_MT_DEFAULT_STRIDE, &stride))) {
            LONG calculated = 0;
            TRY(MFGetStrideForBitmapInfoHeader(c->kind == 1 ? MFVideoFormat_NV12.Data1
                                                            : MFVideoFormat_RGB32.Data1,
                                               c->width, &calculated));
            stride = (UINT32)calculated;
        }
        c->stride = (LONG)stride;
        if (c->width != (int)(selected.size >> 32) || c->height != (int)(UINT32)selected.size) {
            hr = MF_E_INVALIDMEDIATYPE;
            goto done;
        }
        TRY(verify_video(c, &selected));
        UINT64 rate = 0;
        TRY(IMFMediaType_GetUINT64(type, &MF_MT_FRAME_RATE, &rate));
        if (fabs(media_fps(rate) - media_fps(selected.rate)) > 0.001 || !(rate >> 32) ||
            (rate >> 32) > INT_MAX || !(UINT32)rate || (UINT32)rate > INT_MAX) {
            hr = MF_E_INVALIDMEDIATYPE;
            goto done;
        }
        c->rate_num = (int)(rate >> 32);
        c->rate_den = (int)(UINT32)rate;
    }
    c->callback->reader = c->reader;
    TRY(IMFSourceReader_ReadSample(c->reader, stream, 0, NULL, NULL, NULL, NULL));
done:
    DROP(IMFAttributes, attrs);
    DROP(IMFMediaType, type);
    return hr;
}

// Match the selected DXGI output to the active Windows display path. Keep
// the exact rational vertical refresh rate rather than rounding 59.94 to 60.
static void desktop_rate(bk_win_capture *c) {
    c->rate_num = 0;
    c->rate_den = 1;
    for (int retry = 0; retry < 3; retry++) {
        UINT32 np = 0, nm = 0;
        LONG status = GetDisplayConfigBufferSizes(QDC_ONLY_ACTIVE_PATHS, &np, &nm);
        if (status != ERROR_SUCCESS)
            break;
        DISPLAYCONFIG_PATH_INFO *paths = calloc(np, sizeof(*paths));
        DISPLAYCONFIG_MODE_INFO *modes = calloc(nm, sizeof(*modes));
        if (!paths || !modes) {
            free(paths);
            free(modes);
            break;
        }
        status = QueryDisplayConfig(QDC_ONLY_ACTIVE_PATHS, &np, paths, &nm, modes, NULL);
        if (status == ERROR_SUCCESS)
            for (UINT32 i = 0; i < np; i++) {
                DISPLAYCONFIG_SOURCE_DEVICE_NAME name = {0};
                name.header.type = DISPLAYCONFIG_DEVICE_INFO_GET_SOURCE_NAME;
                name.header.size = sizeof(name);
                name.header.adapterId = paths[i].sourceInfo.adapterId;
                name.header.id = paths[i].sourceInfo.id;
                if (DisplayConfigGetDeviceInfo(&name.header) != ERROR_SUCCESS ||
                    wcscmp(name.viewGdiDeviceName, c->output_desc.DeviceName))
                    continue;
                DISPLAYCONFIG_RATIONAL rate = paths[i].targetInfo.refreshRate;
                UINT32 index = paths[i].targetInfo.modeInfoIdx;
                if (index < nm && modes[index].infoType == DISPLAYCONFIG_MODE_INFO_TYPE_TARGET) {
                    DISPLAYCONFIG_RATIONAL signal =
                        modes[index].targetMode.targetVideoSignalInfo.vSyncFreq;
                    if (signal.Numerator > signal.Denominator && signal.Denominator)
                        rate = signal;
                }
                if (rate.Denominator && rate.Numerator > rate.Denominator &&
                    rate.Numerator <= INT_MAX && rate.Denominator <= INT_MAX) {
                    c->rate_num = (int)rate.Numerator;
                    c->rate_den = (int)rate.Denominator;
                }
                break;
            }
        free(paths);
        free(modes);
        if (status != ERROR_INSUFFICIENT_BUFFER)
            break;
    }
    if (c->rate_num <= 0 && c->duplication) {
        DXGI_OUTDUPL_DESC desc;
        IDXGIOutputDuplication_GetDesc(c->duplication, &desc);
        if (desc.ModeDesc.RefreshRate.Denominator &&
            desc.ModeDesc.RefreshRate.Numerator > desc.ModeDesc.RefreshRate.Denominator) {
            c->rate_num = desc.ModeDesc.RefreshRate.Numerator;
            c->rate_den = desc.ModeDesc.RefreshRate.Denominator;
        }
    }
    if (c->rate_num <= 0) {
        DEVMODEW mode = {0};
        mode.dmSize = sizeof(mode);
        if (EnumDisplaySettingsW(c->output_desc.DeviceName, ENUM_CURRENT_SETTINGS, &mode) &&
            mode.dmDisplayFrequency > 1) {
            c->rate_num = mode.dmDisplayFrequency;
            c->rate_den = 1;
        }
    }
}

static HRESULT open_desktop(bk_win_capture *c, int index) {
    IDXGIFactory1 *factory = NULL;
    IDXGIAdapter1 *adapter = NULL;
    IDXGIOutput *output = NULL;
    HRESULT hr;
    int current = 0;
    TRY(CreateDXGIFactory1(&IID_IDXGIFactory1, (void **)&factory));
    hr = DXGI_ERROR_NOT_FOUND;
    for (UINT a = 0; IDXGIFactory1_EnumAdapters1(factory, a, &adapter) == S_OK; a++) {
        for (UINT o = 0; IDXGIAdapter1_EnumOutputs(adapter, o, &output) == S_OK; o++) {
            DXGI_OUTPUT_DESC desc;
            if (SUCCEEDED(IDXGIOutput_GetDesc(output, &desc)) && desc.AttachedToDesktop &&
                current++ == index) {
                c->output_desc = desc;
                hr = IDXGIAdapter1_GetDesc1(adapter, &c->adapter_desc);
                if (FAILED(hr)) goto done;
                hr = IDXGIOutput_QueryInterface(output, &IID_IDXGIOutput1, (void **)&c->output);
                if (SUCCEEDED(hr))
                    hr = D3D11CreateDevice(
                        (IDXGIAdapter *)adapter, D3D_DRIVER_TYPE_UNKNOWN, NULL,
                        D3D11_CREATE_DEVICE_BGRA_SUPPORT | D3D11_CREATE_DEVICE_VIDEO_SUPPORT, NULL,
                        0, D3D11_SDK_VERSION, &c->device, NULL, &c->context);
                if (SUCCEEDED(hr))
                    hr = IDXGIOutput1_DuplicateOutput(c->output, (IUnknown *)c->device,
                                                      &c->duplication);
                if (SUCCEEDED(hr))
                    desktop_rate(c);
                goto done;
            }
            DROP(IDXGIOutput, output);
        }
        DROP(IDXGIAdapter1, adapter);
    }
done:
    DROP(IDXGIOutput, output);
    DROP(IDXGIAdapter1, adapter);
    DROP(IDXGIFactory1, factory);
    return hr;
}

int bk_win_adapter(bk_win_capture *c, uint64_t *luid, uint32_t *vendor) {
    if (!c || !c->screen || !c->device) return 0;
    *luid = ((uint64_t)(uint32_t)c->adapter_desc.AdapterLuid.HighPart << 32) |
            c->adapter_desc.AdapterLuid.LowPart;
    *vendor = c->adapter_desc.VendorId;
    return 1;
}

static bk_win_capture *open_capture(const char *id, int width, int height, double fps,
                                    const char *format, int hide, int audio, int jpeg, char *error,
                                    size_t capacity) {
    bk_win_capture *c = calloc(1, sizeof(*c));
    if (!c) {
        error_text(error, capacity, "allocation", E_OUTOFMEMORY);
        return NULL;
    }
    c->audio = audio;
    c->hide = hide;
    c->rate_num = (int)llround(fps);
    c->rate_den = 1;
    c->screen = !audio && (!strcmp(id, "screen") || !strncmp(id, "screen:", 7));
    HRESULT com = CoInitializeEx(NULL, COINIT_MULTITHREADED);
    HRESULT hr;
    if (c->screen) {
        char *end = NULL;
        long index = !strcmp(id, "screen") ? 0 : strtol(id + 7, &end, 10);
        if (index < 0 || index > INT_MAX || (end && (end == id + 7 || *end)))
            hr = E_INVALIDARG;
        else
            hr = open_desktop(c, (int)index);
    } else
        hr = open_media(c, id, width, height, fps, format, jpeg);
    if (SUCCEEDED(com))
        CoUninitialize();
    if (FAILED(hr)) {
        error_text(error, capacity,
                   c->screen ? "DXGI desktop duplication" : "Media Foundation capture", hr);
        bk_win_close(c);
        return NULL;
    }
    return c;
}

bk_win_capture *bk_win_open(const char *id, int width, int height, int fps, int hide, int audio,
                            char *error, size_t capacity) {
    return open_capture(id, width, height, fps, "", hide, audio, 0, error, capacity);
}
bk_win_capture *bk_win_open_video(const char *id, int width, int height, double fps,
                                  const char *format, int hide, int jpeg, char *error,
                                  size_t capacity) {
    return open_capture(id, width, height, fps, format, hide, 0, jpeg, error, capacity);
}
const char *bk_win_input_format(bk_win_capture *c) {
    return c->screen ? "bgra" : c->input_format;
}

bk_win_frame *bk_win_next(bk_win_capture *c, int timeout, char *error, size_t capacity) {
    HRESULT hr = S_OK;
    bk_win_frame *f = calloc(1, sizeof(*f));
    if (!f) {
        error_text(error, capacity, "allocation", E_OUTOFMEMORY);
        return NULL;
    }
    if (!c->screen) {
        reader_callback *cb = c->callback;
        EnterCriticalSection(&cb->lock);
        hr = cb->status;
        if (cb->first) {
            sample_node *n = cb->first;
            cb->first = n->next;
            if (!cb->first)
                cb->last = NULL;
            cb->count--;
            f->sample = n->sample;
            free(n);
        }
        LeaveCriticalSection(&cb->lock);
        if (FAILED(hr) || !f->sample) {
            if (FAILED(hr))
                error_text(error, capacity, "Media Foundation sample", hr);
            bk_win_frame_free(f);
            return NULL;
        }
        f->width = c->width;
        f->height = c->height;
        f->stride = c->stride;
        f->kind = c->kind;
        return f;
    }
    IDXGIResource *resource = NULL;
    ID3D11Texture2D *texture = NULL;
    IDXGIKeyedMutex *mutex = NULL;
    DXGI_OUTDUPL_FRAME_INFO info;
    if (!c->duplication) {
        hr = IDXGIOutput1_DuplicateOutput(c->output, (IUnknown *)c->device, &c->duplication);
        if (FAILED(hr)) {
            error_text(error, capacity, "DXGI duplication recovery", hr);
            free(f);
            return NULL;
        }
    }
    LARGE_INTEGER clock;
    QueryPerformanceCounter(&clock);
    f->timing.acquire_start = clock.QuadPart;
    hr = IDXGIOutputDuplication_AcquireNextFrame(c->duplication, timeout, &info, &resource);
    QueryPerformanceCounter(&clock);
    f->timing.acquired = clock.QuadPart;
    if (hr == DXGI_ERROR_WAIT_TIMEOUT) {
        free(f);
        return NULL;
    }
    if (hr == DXGI_ERROR_ACCESS_LOST) {
        // Reopen through the bridge so display mode and physical refresh rate
        // are queried together, including when FPS was omitted.
        DROP(IDXGIOutputDuplication, c->duplication);
        error_text(error, capacity, "DXGI display mode changed", hr);
        free(f);
        return NULL;
    }
    if (FAILED(hr)) {
        error_text(error, capacity, "DXGI AcquireNextFrame", hr);
        free(f);
        return NULL;
    }
    if (!info.LastPresentTime.QuadPart && c->shared) {
        IDXGIResource_Release(resource);
        IDXGIOutputDuplication_ReleaseFrame(c->duplication);
        free(f);
        return NULL;
    }
    c->last_present = info.LastPresentTime.QuadPart;
    f->timing.present = c->last_present;
    f->timing.accumulated_frames = info.AccumulatedFrames;
    TRY(IDXGIResource_QueryInterface(resource, &IID_ID3D11Texture2D, (void **)&texture));
    D3D11_TEXTURE2D_DESC desc;
    ID3D11Texture2D_GetDesc(texture, &desc);
    DXGI_OUTDUPL_DESC dupdesc;
    IDXGIOutputDuplication_GetDesc(c->duplication, &dupdesc);
    if (c->shared) {
        D3D11_TEXTURE2D_DESC old;
        ID3D11Texture2D_GetDesc(c->shared, &old);
        if (old.Width != desc.Width || old.Height != desc.Height) {
            DROP(ID3D11Texture2D, c->shared);
        }
    }
    if (!c->shared) {
        desc.Usage = D3D11_USAGE_DEFAULT;
        desc.BindFlags = D3D11_BIND_SHADER_RESOURCE | D3D11_BIND_RENDER_TARGET;
        desc.CPUAccessFlags = 0;
        desc.MiscFlags = D3D11_RESOURCE_MISC_SHARED_KEYEDMUTEX;
        TRY(ID3D11Device_CreateTexture2D(c->device, &desc, NULL, &c->shared));
        IDXGIResource *shared = NULL;
        hr = ID3D11Texture2D_QueryInterface(c->shared, &IID_IDXGIResource, (void **)&shared);
        if (SUCCEEDED(hr)) {
            hr = IDXGIResource_GetSharedHandle(shared, &c->handle);
            IDXGIResource_Release(shared);
        }
        if (FAILED(hr))
            goto done;
    }
    TRY(ID3D11Texture2D_QueryInterface(c->shared, &IID_IDXGIKeyedMutex, (void **)&mutex));
    hr = IDXGIKeyedMutex_AcquireSync(mutex, 0, 1000);
    if (hr != S_OK) {
        hr = E_FAIL;
        goto done;
    }
    ID3D11DeviceContext_CopyResource(c->context, (ID3D11Resource *)c->shared,
                                     (ID3D11Resource *)texture);
    // The client draws the hardware cursor.
    hr = IDXGIKeyedMutex_ReleaseSync(mutex, 0);
    QueryPerformanceCounter(&clock);
    f->timing.copy_submitted = clock.QuadPart;
    if (FAILED(hr))
        goto done;
    f->texture = c->shared;
    ID3D11Texture2D_AddRef(f->texture);
    f->device = c->device;
    ID3D11Device_AddRef(f->device);
    f->context = c->context;
    ID3D11DeviceContext_AddRef(f->context);
    f->handle = c->handle;
    f->rotation = dupdesc.Rotation == DXGI_MODE_ROTATION_ROTATE90    ? 90
                  : dupdesc.Rotation == DXGI_MODE_ROTATION_ROTATE180 ? 180
                  : dupdesc.Rotation == DXGI_MODE_ROTATION_ROTATE270 ? 270
                                                                     : 0;
    f->width = (f->rotation == 90 || f->rotation == 270) ? desc.Height : desc.Width;
    f->height = (f->rotation == 90 || f->rotation == 270) ? desc.Width : desc.Height;
    c->width = f->width;
    c->height = f->height;
    c->rotation = f->rotation;
done:
    DROP(IDXGIKeyedMutex, mutex);
    DROP(ID3D11Texture2D, texture);
    DROP(IDXGIResource, resource);
    IDXGIOutputDuplication_ReleaseFrame(c->duplication);
    if (FAILED(hr)) {
        error_text(error, capacity, "DXGI shared texture", hr);
        bk_win_frame_free(f);
        return NULL;
    }
    return f;
}

// A requested IDR must also work on an unchanged desktop. Reuse its latest
// shared texture, without a CPU copy or a new duplication frame.
bk_win_frame *bk_win_repeat(bk_win_capture *c) {
    if (!c->screen || !c->shared)
        return NULL;
    bk_win_frame *f = calloc(1, sizeof(*f));
    if (!f)
        return NULL;
    LARGE_INTEGER clock;
    QueryPerformanceCounter(&clock);
    f->timing.present = c->last_present;
    // No AcquireNextFrame or desktop copy occurred for this recovery picture.
    f->timing.acquired = clock.QuadPart;
    f->timing.repeated = 1;
    f->texture = c->shared;
    ID3D11Texture2D_AddRef(f->texture);
    f->device = c->device;
    ID3D11Device_AddRef(f->device);
    f->context = c->context;
    ID3D11DeviceContext_AddRef(f->context);
    f->handle = c->handle;
    f->width = c->width;
    f->height = c->height;
    f->rotation = c->rotation;
    return f;
}

void bk_win_frame_timing(bk_win_frame *f, bk_win_timing *timing) {
    *timing = f->timing;
    LARGE_INTEGER clock, frequency;
    QueryPerformanceCounter(&clock);
    QueryPerformanceFrequency(&frequency);
    timing->now = clock.QuadPart;
    timing->frequency = frequency.QuadPart;
}

uint64_t bk_win_process_cpu_100ns(void) {
    FILETIME creation, exit, kernel, user;
    if (!GetProcessTimes(GetCurrentProcess(), &creation, &exit, &kernel, &user)) return 0;
    return (((uint64_t)kernel.dwHighDateTime << 32) | kernel.dwLowDateTime) +
           (((uint64_t)user.dwHighDateTime << 32) | user.dwLowDateTime);
}

static void frame_unborrow(bk_win_frame *f) {
    if (f->borrow_lock == 2)
        IMF2DBuffer_Unlock2D(f->borrowed2d);
    if (f->borrow_lock == 1)
        IMFMediaBuffer_Unlock(f->borrowed);
    f->borrow_lock = 0;
    DROP(IMF2DBuffer, f->borrowed2d);
    DROP(IMFMediaBuffer, f->borrowed);
}
int bk_win_frame_data(bk_win_frame *f, uint8_t **data, size_t *size, char *error, size_t capacity) {
    HRESULT hr = S_OK;
    IMFMediaBuffer *buffer = NULL;
    IMF2DBuffer *two = NULL;
    BYTE *pixels = NULL;
    LONG pitch = f->stride;
    ID3D11Texture2D *staging = NULL;
    IDXGIKeyedMutex *mutex = NULL;
    int locked = 0, mapped = 0;
    D3D11_MAPPED_SUBRESOURCE map = {0};
    *data = NULL;
    *size = 0;
    frame_unborrow(f);
    if (f->sample) {
        TRY(IMFSample_ConvertToContiguousBuffer(f->sample, &buffer));
        DWORD length = 0;
        TRY(IMFMediaBuffer_GetCurrentLength(buffer, &length));
        if (f->width == 0 || f->kind == 2) {
            if (length > LIMIT || (!f->kind && length % 4)) {
                hr = E_INVALIDARG;
                goto done;
            }
            TRY(IMFMediaBuffer_Lock(buffer, &pixels, NULL, NULL));
            locked = 1;
            *data = malloc(length);
            if (!*data) {
                hr = E_OUTOFMEMORY;
                goto done;
            }
            memcpy(*data, pixels, length);
            *size = length;
            goto done;
        }
        if (SUCCEEDED(IMFMediaBuffer_QueryInterface(buffer, &IID_IMF2DBuffer, (void **)&two))) {
            TRY(IMF2DBuffer_Lock2D(two, &pixels, &pitch));
            locked = 2;
        } else {
            TRY(IMFMediaBuffer_Lock(buffer, &pixels, NULL, NULL));
            locked = 1;
            size_t row = (size_t)f->width * (f->kind == 1 ? 1 : 4),
                   abs_pitch = (size_t)(pitch < 0 ? -(int64_t)pitch : pitch);
            if (abs_pitch < row ||
                length <
                    abs_pitch * (f->kind == 1 ? f->height + f->height / 2 - 1 : f->height - 1) +
                        row) {
                hr = E_INVALIDARG;
                goto done;
            }
            if (pitch < 0)
                pixels += abs_pitch * (f->height - 1);
        }
    } else {
        D3D11_TEXTURE2D_DESC desc;
        ID3D11Texture2D_GetDesc(f->texture, &desc);
        desc.Usage = D3D11_USAGE_STAGING;
        desc.BindFlags = desc.MiscFlags = 0;
        desc.CPUAccessFlags = D3D11_CPU_ACCESS_READ;
        TRY(ID3D11Device_CreateTexture2D(f->device, &desc, NULL, &staging));
        TRY(ID3D11Texture2D_QueryInterface(f->texture, &IID_IDXGIKeyedMutex, (void **)&mutex));
        hr = IDXGIKeyedMutex_AcquireSync(mutex, 0, 1000);
        if (hr != S_OK) {
            hr = E_FAIL;
            goto done;
        }
        locked = 3;
        ID3D11DeviceContext_CopyResource(f->context, (ID3D11Resource *)staging,
                                         (ID3D11Resource *)f->texture);
        TRY(ID3D11DeviceContext_Map(f->context, (ID3D11Resource *)staging, 0, D3D11_MAP_READ, 0,
                                    &map));
        mapped = 1;
        pixels = map.pData;
        pitch = map.RowPitch;
    }
    if (f->kind == 1) {
        if (pitch < f->width || (f->width | f->height) & 1) {
            hr = E_INVALIDARG;
            goto done;
        }
        *size = (size_t)f->width * f->height * 3 / 2;
        *data = malloc(*size);
        if (!*data) {
            hr = E_OUTOFMEMORY;
            goto done;
        }
        for (int y = 0; y < f->height + f->height / 2; y++)
            memcpy(*data + (size_t)y * f->width, pixels + (size_t)y * pitch, f->width);
        goto done;
    }
    *size = (size_t)f->width * f->height * 4;
    if (*size > LIMIT) {
        hr = E_INVALIDARG;
        goto done;
    }
    *data = malloc(*size);
    if (!*data) {
        hr = E_OUTOFMEMORY;
        goto done;
    }
    for (int y = 0; y < f->height; y++)
        for (int x = 0; x < f->width; x++) {
            int sx = x, sy = y;
            if (f->rotation == 90) {
                sx = y;
                sy = f->width - 1 - x;
            }
            if (f->rotation == 180) {
                sx = f->width - 1 - x;
                sy = f->height - 1 - y;
            }
            if (f->rotation == 270) {
                sx = f->height - 1 - y;
                sy = x;
            }
            uint8_t *dst = *data + ((size_t)y * f->width + x) * 4;
            memcpy(dst, pixels + (ptrdiff_t)sy * pitch + sx * 4, 4);
            dst[3] = 255;
        }
done:
    if (mapped)
        ID3D11DeviceContext_Unmap(f->context, (ID3D11Resource *)staging, 0);
    if (locked == 1)
        IMFMediaBuffer_Unlock(buffer);
    if (locked == 2)
        IMF2DBuffer_Unlock2D(two);
    if (locked == 3)
        IDXGIKeyedMutex_ReleaseSync(mutex, 0);
    DROP(IDXGIKeyedMutex, mutex);
    DROP(ID3D11Texture2D, staging);
    DROP(IMF2DBuffer, two);
    DROP(IMFMediaBuffer, buffer);
    if (FAILED(hr)) {
        free(*data);
        *data = NULL;
        *size = 0;
        error_text(error, capacity, "native frame readback", hr);
        return -1;
    }
    return 0;
}
uintptr_t bk_win_frame_handle(bk_win_frame *f) {
    return (uintptr_t)f->handle;
}
int bk_win_frame_width(bk_win_frame *f) {
    return f->width;
}
int bk_win_frame_height(bk_win_frame *f) {
    return f->height;
}
int bk_win_frame_rotation(bk_win_frame *f) {
    return f->rotation;
}
// A retained reference to c->shared would still observe the next capture.
// Copy into a separate shared texture before returning to the capture loop.
bk_win_frame *bk_win_frame_snapshot(bk_win_frame *f,char *error,size_t capacity) {
    if (!f||!f->texture||!f->device||!f->context||!f->handle||
        f->width<1||f->height<1||(uint64_t)f->width*f->height>64u*1024*1024/4) {
        snprintf(error,capacity,"invalid GPU capture source"); return NULL;
    }
    bk_win_frame *out=calloc(1,sizeof(*out));
    if (!out) {snprintf(error,capacity,"GPU snapshot allocation failed");return NULL;}
    HRESULT hr=S_OK;IDXGIKeyedMutex *source_mutex=NULL,*snapshot_mutex=NULL;IDXGIResource *resource=NULL;
    int source_locked=0,snapshot_locked=0;
    D3D11_TEXTURE2D_DESC desc;ID3D11Texture2D_GetDesc(f->texture,&desc);
    desc.Usage=D3D11_USAGE_DEFAULT;desc.CPUAccessFlags=0;
    desc.BindFlags=D3D11_BIND_SHADER_RESOURCE;desc.MiscFlags=D3D11_RESOURCE_MISC_SHARED_KEYEDMUTEX;
    TRY(ID3D11Device_CreateTexture2D(f->device,&desc,NULL,&out->texture));
    TRY(ID3D11Texture2D_QueryInterface(out->texture,&IID_IDXGIResource,(void **)&resource));
    TRY(IDXGIResource_GetSharedHandle(resource,&out->handle));
    TRY(ID3D11Texture2D_QueryInterface(f->texture,&IID_IDXGIKeyedMutex,(void **)&source_mutex));
    TRY(ID3D11Texture2D_QueryInterface(out->texture,&IID_IDXGIKeyedMutex,(void **)&snapshot_mutex));
    hr=IDXGIKeyedMutex_AcquireSync(source_mutex,0,1000);if(hr!=S_OK){hr=E_FAIL;goto done;}source_locked=1;
    hr=IDXGIKeyedMutex_AcquireSync(snapshot_mutex,0,1000);if(hr!=S_OK){hr=E_FAIL;goto done;}snapshot_locked=1;
    ID3D11DeviceContext_CopyResource(f->context,(ID3D11Resource *)out->texture,(ID3D11Resource *)f->texture);
    out->width=f->width;out->height=f->height;out->rotation=f->rotation;
    out->device=f->device;ID3D11Device_AddRef(out->device);
    out->context=f->context;ID3D11DeviceContext_AddRef(out->context);
done:
    if(snapshot_locked){HRESULT released=IDXGIKeyedMutex_ReleaseSync(snapshot_mutex,0);if(SUCCEEDED(hr))hr=released;}
    if(source_locked){HRESULT released=IDXGIKeyedMutex_ReleaseSync(source_mutex,0);if(SUCCEEDED(hr))hr=released;}
    DROP(IDXGIKeyedMutex,source_mutex);DROP(IDXGIKeyedMutex,snapshot_mutex);DROP(IDXGIResource,resource);
    if(FAILED(hr)){error_text(error,capacity,"immutable GPU snapshot",hr);bk_win_frame_free(out);return NULL;}
    return out;
}

void bk_win_frame_free(bk_win_frame *f) {
    if (!f)
        return;
    if (f->borrow_lock == 2)
        IMF2DBuffer_Unlock2D(f->borrowed2d);
    if (f->borrow_lock == 1)
        IMFMediaBuffer_Unlock(f->borrowed);
    DROP(IMF2DBuffer, f->borrowed2d);
    DROP(IMFMediaBuffer, f->borrowed);
    DROP(IMFSample, f->sample);
    DROP(ID3D11Texture2D, f->texture);
    DROP(ID3D11DeviceContext, f->context);
    DROP(ID3D11Device, f->device);
    free(f);
}
void bk_win_close(bk_win_capture *c) {
    if (!c)
        return;
    HRESULT com = CoInitializeEx(NULL, COINIT_MULTITHREADED);
    if (c->callback) {
        EnterCriticalSection(&c->callback->lock);
        c->callback->stop = 1;
        c->callback->reader = NULL;
        LeaveCriticalSection(&c->callback->lock);
    }
    if (c->source)
        IMFMediaSource_Shutdown(c->source);
    DROP(IMFSourceReader, c->reader);
    DROP(IMFMediaSource, c->source);
    if (c->callback)
        cb_release(&c->callback->iface);
    DROP(ID3D11Texture2D, c->shared);
    DROP(IDXGIOutputDuplication, c->duplication);
    DROP(IDXGIOutput1, c->output);
    DROP(ID3D11DeviceContext, c->context);
    DROP(ID3D11Device, c->device);
    if (c->mf_started)
        MFShutdown();
    if (SUCCEEDED(com))
        CoUninitialize();
    free(c);
}
int bk_win_rate_num(bk_win_capture *c) {
    return c->rate_num;
}
int bk_win_rate_den(bk_win_capture *c) {
    return c->rate_den;
}
char *bk_win_devices(int audio, char *error, size_t capacity) {
    IMFActivate **devices = NULL;
    UINT32 count = 0;
    char *result = NULL;
    size_t used = 0;
    HRESULT com = CoInitializeEx(NULL, COINIT_MULTITHREADED),
            hr = MFStartup(MF_VERSION, MFSTARTUP_FULL);
    int started = SUCCEEDED(hr);
    if (FAILED(hr))
        goto done;
    TRY(enumerate(audio, &devices, &count));
    result = calloc(1, 1);
    if (!result) {
        hr = E_OUTOFMEMORY;
        goto done;
    }
    for (UINT32 i = 0; i < count; i++) {
        wchar_t *name = NULL;
        UINT32 length = 0;
        if (SUCCEEDED(IMFActivate_GetAllocatedString(
                devices[i], &MF_DEVSOURCE_ATTRIBUTE_FRIENDLY_NAME, &name, &length))) {
            char *s = utf8(name);
            CoTaskMemFree(name);
            if (!s) {
                hr = E_OUTOFMEMORY;
                goto done;
            }
            size_t n = strlen(s);
            char *next = realloc(result, used + n + 2);
            if (!next) {
                free(s);
                hr = E_OUTOFMEMORY;
                goto done;
            }
            result = next;
            memcpy(result + used, s, n);
            used += n;
            result[used++] = '\n';
            result[used] = 0;
            free(s);
        }
    }
done:
    if (devices) {
        for (UINT32 i = 0; i < count; i++)
            IMFActivate_Release(devices[i]);
        CoTaskMemFree(devices);
    }
    if (started)
        MFShutdown();
    if (SUCCEEDED(com))
        CoUninitialize();
    if (FAILED(hr)) {
        free(result);
        result = NULL;
        error_text(error, capacity, "Media Foundation enumeration", hr);
    }
    return result;
}

int bk_win_frame_kind(bk_win_frame *f) {
    return f->kind;
}
void *bk_win_frame_sample(bk_win_frame *f) {
    return f->sample;
}
int bk_win_frame_borrow(bk_win_frame *f, uint8_t **data, size_t *size, int *pitch, char *error,
                        size_t capacity) {
    HRESULT hr;
    DWORD length = 0, count = 0;
    *data = NULL;
    *size = 0;
    *pitch = 0;
    frame_unborrow(f);
    if (!f->sample || !f->kind)
        return 0;
    // GetBufferByIndex avoids ConvertToContiguousBuffer's implicit flatten copy.
    TRY(IMFSample_GetBufferCount(f->sample, &count));
    if (count != 1)
        return 0;
    TRY(IMFSample_GetBufferByIndex(f->sample, 0, &f->borrowed));
    TRY(IMFMediaBuffer_GetCurrentLength(f->borrowed, &length));
    if (!length || length > LIMIT) {
        hr = E_INVALIDARG;
        goto done;
    }
    LONG stride = f->stride;
    if (f->kind == 1 && SUCCEEDED(IMFMediaBuffer_QueryInterface(f->borrowed, &IID_IMF2DBuffer,
                                                                (void **)&f->borrowed2d))) {
        TRY(IMF2DBuffer_Lock2D(f->borrowed2d, data, &stride));
        f->borrow_lock = 2;
    } else {
        TRY(IMFMediaBuffer_Lock(f->borrowed, data, NULL, NULL));
        f->borrow_lock = 1;
    }
    if (f->kind == 1 && (stride < f->width || (f->width | f->height) & 1 ||
                         (size_t)stride * f->height * 3 / 2 > length)) {
        hr = E_INVALIDARG;
        goto done;
    }
    *pitch = stride;
    *size = length;
    return 1;
done:
    error_text(error, capacity, "borrow native sample", hr);
    return -1;
}

int bk_win_frame_pitch(bk_win_frame *f) {
    return f->stride;
}
