//go:build windows && cgo

#define COBJMACROS
#ifndef WINVER
#define WINVER 0x0a00
#endif
#include <windows.h>
#include <mfapi.h>
#include <mferror.h>
#include <mfidl.h>
#include <mftransform.h>
#include <codecapi.h>
// MinGW import libraries do not consistently define IID_ICodecAPI.
#include <initguid.h>
#include <icodecapi.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include "mediafoundation_windows.h"

// Every COM operation, including release, stays on one native MTA thread.
// The Go wrapper serializes requests and keeps borrowed input alive until done.
struct bk_mf_encoder {
    HANDLE thread, request, done;
    HMODULE module;
    HRESULT(WINAPI *startup)(ULONG, DWORD);
    HRESULT(WINAPI *shutdown)(void);
    HRESULT(WINAPI *enumerate)(GUID, UINT32, const MFT_REGISTER_TYPE_INFO *,
                               const MFT_REGISTER_TYPE_INFO *, IMFActivate ***, UINT32 *);
    HRESULT(WINAPI *create_type)(IMFMediaType **);
    HRESULT(WINAPI *create_sample)(IMFSample **);
    HRESULT(WINAPI *create_buffer)(DWORD, DWORD, IMFMediaBuffer **);
    int width, height, rate_num, rate_den, bitrate, codec, hardware, stop;
    const uint8_t *input;
    IMFSample *borrowed_sample;
    int borrowed_rejected;
    size_t input_size;
    int force_idr;
    uint8_t *output, *header;
    size_t output_size, header_size;
    HRESULT result;
    char operation[96], name[256];
    IMFActivate *activation;
    IMFTransform *transform;
    ICodecAPI *control;
    IMFMediaEventGenerator *events;
    DWORD input_id, output_id;
    int asynchronous, need_input;
    LONGLONG clock, remainder;
    unsigned frames;
};

#define BK_LIMIT (32 * 1024 * 1024)
#define RELEASE(type, object)                                                                      \
    do {                                                                                           \
        if (object) {                                                                              \
            type##_Release(object);                                                                \
            object = NULL;                                                                         \
        }                                                                                          \
    } while (0)
#define TRY(expression)                                                                            \
    do {                                                                                           \
        hr = (expression);                                                                         \
        if (FAILED(hr))                                                                            \
            goto done;                                                                             \
    } while (0)

// MinGW's named codec GUID macros use C++ __uuidof even in C translation units.
#define BK_GUID_VALUE(a, b, c, d, e, f, g, h, i, j, k)                                             \
    {                                                                                              \
        a, b, c, {                                                                                 \
            d, e, f, g, h, i, j, k                                                                 \
        }                                                                                          \
    }
#define BK_GUID_EXPAND(...) BK_GUID_VALUE(__VA_ARGS__)
#define BK_CODEC_GUID(name) static const GUID bk_##name = BK_GUID_EXPAND(STATIC_CODECAPI_##name)
BK_CODEC_GUID(AVLowLatencyMode);
BK_CODEC_GUID(AVEncCommonRateControlMode);
BK_CODEC_GUID(AVEncCommonMeanBitRate);
BK_CODEC_GUID(AVEncMPVDefaultBPictureCount);
BK_CODEC_GUID(AVEncMPVGOPSize);
BK_CODEC_GUID(AVEncVideoForceKeyFrame);

static void bk_mf_error(bk_mf_encoder *e, char *error, size_t capacity) {
    snprintf(error, capacity, "%s failed (HRESULT 0x%08lx)", e->operation,
             (unsigned long)e->result);
}

static const GUID *bk_subtype(int codec) {
    return codec == 1 ? &MFVideoFormat_HEVC : codec == 2 ? &MFVideoFormat_AV1 : &MFVideoFormat_H264;
}

static HRESULT bk_property(bk_mf_encoder *e, const GUID *key, ULONG value, int boolean) {
    if (!e->control)
        return E_NOINTERFACE;
    VARIANT setting;
    VariantInit(&setting);
    setting.vt = boolean ? VT_BOOL : VT_UI4;
    if (boolean)
        setting.boolVal = value ? VARIANT_TRUE : VARIANT_FALSE;
    else
        setting.ulVal = value;
    return ICodecAPI_SetValue(e->control, key, &setting);
}

static HRESULT bk_type(bk_mf_encoder *e, int output, IMFMediaType **type) {
    IMFMediaType *t = NULL;
    HRESULT hr;
    TRY(e->create_type(&t));
    TRY(IMFMediaType_SetGUID(t, &MF_MT_MAJOR_TYPE, &MFMediaType_Video));
    TRY(IMFMediaType_SetGUID(t, &MF_MT_SUBTYPE,
                             output ? bk_subtype(e->codec) : &MFVideoFormat_NV12));
    TRY(IMFMediaType_SetUINT64(t, &MF_MT_FRAME_SIZE, ((UINT64)e->width << 32) | (UINT32)e->height));
    TRY(IMFMediaType_SetUINT64(t, &MF_MT_FRAME_RATE,
                               ((UINT64)e->rate_num << 32) | (UINT32)e->rate_den));
    TRY(IMFMediaType_SetUINT64(t, &MF_MT_PIXEL_ASPECT_RATIO, ((UINT64)1 << 32) | 1));
    TRY(IMFMediaType_SetUINT32(t, &MF_MT_INTERLACE_MODE, MFVideoInterlace_Progressive));
    if (output) {
        TRY(IMFMediaType_SetUINT32(t, &MF_MT_AVG_BITRATE, e->bitrate * 1000));
        if (e->codec == 0) {
            TRY(IMFMediaType_SetUINT32(t, &MF_MT_MPEG2_PROFILE, eAVEncH264VProfile_Base));
        }
    } else {
        TRY(IMFMediaType_SetUINT32(t, &MF_MT_DEFAULT_STRIDE, e->width));
        TRY(IMFMediaType_SetUINT32(t, &MF_MT_SAMPLE_SIZE, e->width * e->height * 3 / 2));
        TRY(IMFMediaType_SetUINT32(t, &MF_MT_FIXED_SIZE_SAMPLES, TRUE));
        TRY(IMFMediaType_SetUINT32(t, &MF_MT_ALL_SAMPLES_INDEPENDENT, TRUE));
    }
    *type = t;
    return S_OK;
done:
    RELEASE(IMFMediaType, t);
    return hr;
}

static void bk_read_header(bk_mf_encoder *e) {
    IMFMediaType *type = NULL;
    UINT32 size = 0;
    if (SUCCEEDED(IMFTransform_GetOutputCurrentType(e->transform, e->output_id, &type)) &&
        SUCCEEDED(IMFMediaType_GetBlobSize(type, &MF_MT_MPEG_SEQUENCE_HEADER, &size)) && size &&
        size <= 65536) {
        uint8_t *header = malloc(size);
        if (header && SUCCEEDED(IMFMediaType_GetBlob(type, &MF_MT_MPEG_SEQUENCE_HEADER, header,
                                                     size, &size))) {
            free(e->header);
            e->header = header;
            e->header_size = size;
        } else
            free(header);
    }
    RELEASE(IMFMediaType, type);
}

static void bk_reset(bk_mf_encoder *e) {
    if (e->transform)
        IMFTransform_ProcessMessage(e->transform, MFT_MESSAGE_COMMAND_FLUSH, 0);
    RELEASE(IMFMediaEventGenerator, e->events);
    RELEASE(ICodecAPI, e->control);
    RELEASE(IMFTransform, e->transform);
    if (e->activation)
        IMFActivate_ShutdownObject(e->activation);
    RELEASE(IMFActivate, e->activation);
    free(e->header);
    e->header = NULL;
    e->header_size = 0;
    free(e->output);
    e->output = NULL;
    e->output_size = 0;
    e->asynchronous = e->need_input = 0;
    e->clock = e->remainder = e->frames = 0;
}

static HRESULT bk_configure(bk_mf_encoder *e, IMFActivate *activation) {
    IMFAttributes *attributes = NULL;
    IMFMediaType *input = NULL, *output = NULL;
    UINT32 asynchronous = 0;
    HRESULT hr;
    e->activation = activation;
    IMFActivate_AddRef(activation);
    TRY(IMFActivate_ActivateObject(activation, &IID_IMFTransform, (void **)&e->transform));
    hr = IMFTransform_GetAttributes(e->transform, &attributes);
    if (SUCCEEDED(hr)) {
        IMFAttributes_GetUINT32(attributes, &MF_TRANSFORM_ASYNC, &asynchronous);
        if (asynchronous)
            TRY(IMFAttributes_SetUINT32(attributes, &MF_TRANSFORM_ASYNC_UNLOCK, TRUE));
        IMFAttributes_SetUINT32(attributes, &bk_AVLowLatencyMode, TRUE);
    }
    e->asynchronous = asynchronous != 0;
    if (e->asynchronous)
        TRY(IMFTransform_QueryInterface(e->transform, &IID_IMFMediaEventGenerator,
                                        (void **)&e->events));
    e->input_id = e->output_id = 0;
    hr = IMFTransform_GetStreamIDs(e->transform, 1, &e->input_id, 1, &e->output_id);
    if (hr != E_NOTIMPL && FAILED(hr))
        goto done;
    IMFTransform_QueryInterface(e->transform, &IID_ICodecAPI, (void **)&e->control);
    // Optional driver controls must not disqualify an otherwise usable MFT.
    bk_property(e, &bk_AVLowLatencyMode, 1, 1);
    bk_property(e, &bk_AVEncCommonRateControlMode, eAVEncCommonRateControlMode_CBR, 0);
    bk_property(e, &bk_AVEncCommonMeanBitRate, e->bitrate * 1000, 0);
    bk_property(e, &bk_AVEncMPVDefaultBPictureCount, 0, 0);
    bk_property(e, &bk_AVEncMPVGOPSize, ((INT64)e->rate_num * 2 + e->rate_den - 1) / e->rate_den,
                0);
    TRY(bk_type(e, 1, &output));
    TRY(IMFTransform_SetOutputType(e->transform, e->output_id, output, 0));
    TRY(bk_type(e, 0, &input));
    TRY(IMFTransform_SetInputType(e->transform, e->input_id, input, 0));
    TRY(IMFTransform_ProcessMessage(e->transform, MFT_MESSAGE_NOTIFY_BEGIN_STREAMING, 0));
    TRY(IMFTransform_ProcessMessage(e->transform, MFT_MESSAGE_NOTIFY_START_OF_STREAM, 0));
done:
    RELEASE(IMFAttributes, attributes);
    RELEASE(IMFMediaType, input);
    RELEASE(IMFMediaType, output);
    return hr;
}

// Poll rather than block in GetEvent so driver stalls have a bounded timeout.
// NeedInput tokens remain available for the next Encode call.
static HRESULT bk_wait(bk_mf_encoder *e, MediaEventType wanted, ULONGLONG deadline) {
    if (wanted == METransformNeedInput && e->need_input)
        return S_OK;
    while (GetTickCount64() < deadline) {
        IMFMediaEvent *event = NULL;
        HRESULT hr = IMFMediaEventGenerator_GetEvent(e->events, MF_EVENT_FLAG_NO_WAIT, &event);
        if (hr == MF_E_NO_EVENTS_AVAILABLE) {
            Sleep(1);
            continue;
        }
        if (FAILED(hr))
            return hr;
        MediaEventType type = MEUnknown;
        HRESULT status = S_OK;
        hr = IMFMediaEvent_GetType(event, &type);
        if (SUCCEEDED(hr))
            hr = IMFMediaEvent_GetStatus(event, &status);
        IMFMediaEvent_Release(event);
        if (FAILED(hr))
            return hr;
        if (FAILED(status))
            return status;
        if (type == METransformNeedInput)
            e->need_input++;
        if (type == wanted)
            return S_OK;
        // One submitted picture must produce output before another is admitted.
        if (type == METransformHaveOutput)
            return E_UNEXPECTED;
    }
    return HRESULT_FROM_WIN32(WAIT_TIMEOUT);
}

static HRESULT bk_output(bk_mf_encoder *e, unsigned changes) {
    MFT_OUTPUT_STREAM_INFO info;
    MFT_OUTPUT_DATA_BUFFER out = {0};
    IMFMediaBuffer *buffer = NULL, *contiguous = NULL;
    IMFMediaType *type = NULL;
    BYTE *bytes = NULL;
    DWORD length = 0, status = 0;
    int locked = 0;
    HRESULT hr;
    TRY(IMFTransform_GetOutputStreamInfo(e->transform, e->output_id, &info));
    out.dwStreamID = e->output_id;
    if (!(info.dwFlags & MFT_OUTPUT_STREAM_PROVIDES_SAMPLES)) {
        DWORD size = info.cbSize > 1024 * 1024 ? info.cbSize : 1024 * 1024;
        if (size > BK_LIMIT) {
            hr = E_OUTOFMEMORY;
            goto done;
        }
        TRY(e->create_sample(&out.pSample));
        TRY(e->create_buffer(size, info.cbAlignment ? info.cbAlignment - 1 : 15, &buffer));
        TRY(IMFSample_AddBuffer(out.pSample, buffer));
    }
    hr = IMFTransform_ProcessOutput(e->transform, 0, 1, &out, &status);
    if (hr == MF_E_TRANSFORM_STREAM_CHANGE) {
        if (changes >= 4) {
            hr = E_UNEXPECTED;
            goto done;
        }
        // A driver may finalize its compressed output type after the first input.
        RELEASE(IMFSample, out.pSample);
        RELEASE(IMFCollection, out.pEvents);
        RELEASE(IMFMediaBuffer, buffer);
        TRY(IMFTransform_GetOutputAvailableType(e->transform, e->output_id, 0, &type));
        TRY(IMFTransform_SetOutputType(e->transform, e->output_id, type, 0));
        RELEASE(IMFMediaType, type);
        return bk_output(e, changes + 1);
    }
    if (FAILED(hr))
        goto done;
    if (!out.pSample || (out.dwStatus & MFT_OUTPUT_DATA_BUFFER_INCOMPLETE)) {
        hr = E_UNEXPECTED;
        goto done;
    }
    TRY(IMFSample_ConvertToContiguousBuffer(out.pSample, &contiguous));
    TRY(IMFMediaBuffer_Lock(contiguous, &bytes, NULL, &length));
    locked = 1;
    if (!length || length > BK_LIMIT) {
        hr = E_UNEXPECTED;
        goto done;
    }
    e->output = malloc(length);
    if (!e->output) {
        hr = E_OUTOFMEMORY;
        goto done;
    }
    memcpy(e->output, bytes, length);
    e->output_size = length;
    bk_read_header(e);
done:
    if (locked)
        IMFMediaBuffer_Unlock(contiguous);
    RELEASE(IMFMediaType, type);
    RELEASE(IMFMediaBuffer, contiguous);
    RELEASE(IMFMediaBuffer, buffer);
    RELEASE(IMFSample, out.pSample);
    RELEASE(IMFCollection, out.pEvents);
    return hr;
}

static HRESULT bk_encode_frame(bk_mf_encoder *e) {
    IMFSample *sample = NULL;
    IMFMediaBuffer *buffer = NULL;
    MFT_INPUT_STREAM_INFO info;
    BYTE *bytes = NULL;
    int locked = 0;
    HRESULT hr;
    ULONGLONG deadline = GetTickCount64() + 5000;
    snprintf(e->operation, sizeof(e->operation), "Media Foundation input");
    free(e->output);
    e->output = NULL;
    e->output_size = 0;
    if (e->asynchronous)
        TRY(bk_wait(e, METransformNeedInput, deadline));
    TRY(IMFTransform_GetInputStreamInfo(e->transform, e->input_id, &info));
    TRY(e->create_sample(&sample));
    if (e->borrowed_sample) {
        DWORD count = 0;
        TRY(IMFSample_GetBufferCount(e->borrowed_sample, &count));
        for (DWORD i = 0; i < count; i++) {
            TRY(IMFSample_GetBufferByIndex(e->borrowed_sample, i, &buffer));
            TRY(IMFSample_AddBuffer(sample, buffer));
            RELEASE(IMFMediaBuffer, buffer);
        }
    } else {
        TRY(e->create_buffer((DWORD)e->input_size, info.cbAlignment ? info.cbAlignment - 1 : 15,
                             &buffer));
        TRY(IMFMediaBuffer_Lock(buffer, &bytes, NULL, NULL));
        locked = 1;
        memcpy(bytes, e->input, e->input_size);
        TRY(IMFMediaBuffer_Unlock(buffer));
        locked = 0;
        TRY(IMFMediaBuffer_SetCurrentLength(buffer, (DWORD)e->input_size));
        TRY(IMFSample_AddBuffer(sample, buffer));
    }
    LONGLONG ticks = 10000000LL * e->rate_den + e->remainder;
    LONGLONG duration = ticks / e->rate_num;
    TRY(IMFSample_SetSampleTime(sample, e->clock));
    TRY(IMFSample_SetSampleDuration(sample, duration));
    if (e->force_idr || e->frames <= 1) {
        // Unlike optional tuning, a requested recovery frame is mandatory.
        TRY(bk_property(e, &bk_AVEncVideoForceKeyFrame, 1, 0));
    }
    hr = IMFTransform_ProcessInput(e->transform, e->input_id, sample, 0);
    if (FAILED(hr)) {
        if (e->borrowed_sample)
            e->borrowed_rejected = 1;
        goto done;
    }
    e->clock += duration;
    e->remainder = ticks % e->rate_num;
    e->frames++;
    if (e->asynchronous)
        e->need_input--;
    snprintf(e->operation, sizeof(e->operation), "Media Foundation output");
    if (e->asynchronous)
        TRY(bk_wait(e, METransformHaveOutput, deadline));
    TRY(bk_output(e, 0));
done:
    if (locked)
        IMFMediaBuffer_Unlock(buffer);
    RELEASE(IMFSample, sample);
    RELEASE(IMFMediaBuffer, buffer);
    return hr;
}

static HRESULT bk_initialize(bk_mf_encoder *e) {
    IMFActivate **activations = NULL;
    UINT32 count = 0;
    MFT_REGISTER_TYPE_INFO input = {MFMediaType_Video, MFVideoFormat_NV12};
    MFT_REGISTER_TYPE_INFO output = {MFMediaType_Video, *bk_subtype(e->codec)};
    UINT32 flags = (e->hardware ? MFT_ENUM_FLAG_HARDWARE : MFT_ENUM_FLAG_SYNCMFT) |
                   MFT_ENUM_FLAG_SORTANDFILTER;
    snprintf(e->operation, sizeof(e->operation), "Media Foundation encoder enumeration");
    HRESULT hr =
        e->enumerate(MFT_CATEGORY_VIDEO_ENCODER, flags, &input, &output, &activations, &count);
    if (FAILED(hr))
        return hr;
    hr = MF_E_TOPO_CODEC_NOT_FOUND;
    size_t size = (size_t)e->width * e->height * 3 / 2;
    uint8_t *probe = e->codec == 0 ? malloc(size) : NULL;
    if (e->codec == 0 && !probe) {
        hr = E_OUTOFMEMORY;
        goto done;
    }
    if (probe) {
        memset(probe, 16, (size_t)e->width * e->height);
        memset(probe + (size_t)e->width * e->height, 128, size - (size_t)e->width * e->height);
    }
    for (UINT32 i = 0; i < count; i++) {
        snprintf(e->operation, sizeof(e->operation), "Media Foundation encoder configuration");
        hr = bk_configure(e, activations[i]);
        if (SUCCEEDED(hr) && probe) {
            // Reject drivers that buffer indefinitely in this low-delay contract.
            e->input = probe;
            e->input_size = size;
            e->force_idr = 1;
            hr = bk_encode_frame(e);
        }
        if (SUCCEEDED(hr)) {
            WCHAR *friendly = NULL;
            UINT32 length = 0;
            char utf8[192] = {0};
            if (SUCCEEDED(IMFActivate_GetAllocatedString(
                    activations[i], &MFT_FRIENDLY_NAME_Attribute, &friendly, &length))) {
                WideCharToMultiByte(CP_UTF8, 0, friendly, -1, utf8, sizeof(utf8), NULL, NULL);
                CoTaskMemFree(friendly);
            }
            snprintf(e->name, sizeof(e->name), "mediafoundation-%s: %s",
                     e->hardware ? "hardware" : "software", utf8);
            break;
        }
        bk_reset(e);
    }
    free(probe);
    e->input = NULL;
    e->input_size = 0;
done:
    for (UINT32 i = 0; i < count; i++)
        IMFActivate_Release(activations[i]);
    CoTaskMemFree(activations);
    return hr;
}

// Loading the platform DLL lazily preserves the FFmpeg path on Windows N or
// installations where Media Foundation has not been enabled.
static HRESULT bk_load(bk_mf_encoder *e) {
    e->module = LoadLibraryExW(L"mfplat.dll", NULL, LOAD_LIBRARY_SEARCH_SYSTEM32);
    if (!e->module)
        return HRESULT_FROM_WIN32(GetLastError());
#define LOAD(member, name, signature)                                                              \
    do {                                                                                           \
        e->member = (signature)GetProcAddress(e->module, name);                                    \
        if (!e->member)                                                                            \
            return HRESULT_FROM_WIN32(ERROR_PROC_NOT_FOUND);                                       \
    } while (0)
    LOAD(startup, "MFStartup", HRESULT(WINAPI *)(ULONG, DWORD));
    LOAD(shutdown, "MFShutdown", HRESULT(WINAPI *)(void));
    LOAD(enumerate, "MFTEnumEx",
         HRESULT(WINAPI *)(GUID, UINT32, const MFT_REGISTER_TYPE_INFO *,
                           const MFT_REGISTER_TYPE_INFO *, IMFActivate ***, UINT32 *));
    LOAD(create_type, "MFCreateMediaType", HRESULT(WINAPI *)(IMFMediaType **));
    LOAD(create_sample, "MFCreateSample", HRESULT(WINAPI *)(IMFSample **));
    LOAD(create_buffer, "MFCreateAlignedMemoryBuffer",
         HRESULT(WINAPI *)(DWORD, DWORD, IMFMediaBuffer **));
#undef LOAD
    return S_OK;
}

int bk_mf_supports(int codec) {
    if (codec != 1 && codec != 2)
        return 0;
    HRESULT com = CoInitializeEx(NULL, COINIT_MULTITHREADED);
    if (FAILED(com) && com != RPC_E_CHANGED_MODE)
        return 0;
    bk_mf_encoder e = {0};
    IMFActivate **activations = NULL;
    UINT32 count = 0;
    int supported = 0, started = 0;
    if (FAILED(bk_load(&e)) || FAILED(e.startup(MF_VERSION, MFSTARTUP_FULL)))
        goto done;
    started = 1;
    MFT_REGISTER_TYPE_INFO input = {MFMediaType_Video, MFVideoFormat_NV12};
    MFT_REGISTER_TYPE_INFO output = {MFMediaType_Video, *bk_subtype(codec)};
    if (SUCCEEDED(e.enumerate(MFT_CATEGORY_VIDEO_ENCODER, MFT_ENUM_FLAG_HARDWARE | MFT_ENUM_FLAG_SORTANDFILTER, &input,
                              &output, &activations, &count)))
        supported = count > 0;
done:
    for (UINT32 i = 0; activations && i < count; i++)
        IMFActivate_Release(activations[i]);
    CoTaskMemFree(activations);
    if (started)
        e.shutdown();
    if (e.module)
        FreeLibrary(e.module);
    if (SUCCEEDED(com))
        CoUninitialize();
    return supported;
}

static DWORD WINAPI bk_worker(void *context) {
    bk_mf_encoder *e = context;
    int com = 0, media = 0;
    snprintf(e->operation, sizeof(e->operation), "COM initialization");
    e->result = CoInitializeEx(NULL, COINIT_MULTITHREADED);
    if (SUCCEEDED(e->result)) {
        com = 1;
        snprintf(e->operation, sizeof(e->operation), "Media Foundation library loading");
        e->result = bk_load(e);
        if (SUCCEEDED(e->result)) {
            snprintf(e->operation, sizeof(e->operation), "Media Foundation startup");
            e->result = e->startup(MF_VERSION, MFSTARTUP_FULL);
        }
        if (SUCCEEDED(e->result)) {
            media = 1;
            e->result = bk_initialize(e);
        }
    }
    SetEvent(e->done);
    if (SUCCEEDED(e->result)) {
        for (;;) {
            WaitForSingleObject(e->request, INFINITE);
            if (e->stop)
                break;
            e->result = bk_encode_frame(e);
            SetEvent(e->done);
        }
    }
    bk_reset(e);
    if (media)
        e->shutdown();
    if (e->module)
        FreeLibrary(e->module);
    if (com)
        CoUninitialize();
    return 0;
}

bk_mf_encoder *bk_mf_open(int width, int height, int num, int den, int bitrate, int codec,
                          int hardware, char *error, size_t capacity) {
    bk_mf_encoder *e = calloc(1, sizeof(*e));
    if (!e) {
        snprintf(error, capacity, "out of memory");
        return NULL;
    }
    e->width = width;
    e->height = height;
    e->rate_num = num;
    e->rate_den = den;
    e->bitrate = bitrate;
    e->codec = codec;
    e->hardware = hardware;
    e->request = CreateEventW(NULL, FALSE, FALSE, NULL);
    e->done = CreateEventW(NULL, FALSE, FALSE, NULL);
    if (e->request && e->done)
        e->thread = CreateThread(NULL, 0, bk_worker, e, 0, NULL);
    if (!e->thread) {
        snprintf(error, capacity, "cannot create Media Foundation worker (error %lu)",
                 GetLastError());
        bk_mf_close(e);
        return NULL;
    }
    WaitForSingleObject(e->done, INFINITE);
    if (FAILED(e->result)) {
        bk_mf_error(e, error, capacity);
        bk_mf_close(e);
        return NULL;
    }
    return e;
}

int bk_mf_encode(bk_mf_encoder *e, const uint8_t *input, size_t size, int force, uint8_t **output,
                 size_t *output_size, char *error, size_t capacity) {
    e->input = input;
    e->input_size = size;
    e->force_idr = force;
    SetEvent(e->request);
    WaitForSingleObject(e->done, INFINITE);
    e->input = NULL;
    if (FAILED(e->result)) {
        bk_mf_error(e, error, capacity);
        return -1;
    }
    *output = e->output;
    *output_size = e->output_size;
    e->output = NULL;
    e->output_size = 0;
    return 0;
}

const uint8_t *bk_mf_header(bk_mf_encoder *e, size_t *size) {
    *size = e->header_size;
    return e->header;
}
const uint8_t *bk_mf_probe(bk_mf_encoder *e, size_t *size) {
    *size = e->output_size;
    return e->output;
}
const char *bk_mf_name(bk_mf_encoder *e) {
    return e->name;
}
void bk_mf_close(bk_mf_encoder *e) {
    if (!e)
        return;
    if (e->thread) {
        e->stop = 1;
        SetEvent(e->request);
        WaitForSingleObject(e->thread, INFINITE);
        CloseHandle(e->thread);
    }
    if (e->request)
        CloseHandle(e->request);
    if (e->done)
        CloseHandle(e->done);
    free(e);
}

int bk_mf_encode_sample(bk_mf_encoder *e, void *sample, int force, uint8_t **output, size_t *size,
                        char *error, size_t capacity) {
    e->borrowed_sample = (IMFSample *)sample;
    e->force_idr = force;
    e->borrowed_rejected = 0;
    SetEvent(e->request);
    WaitForSingleObject(e->done, INFINITE);
    e->borrowed_sample = NULL;
    if (FAILED(e->result)) {
        bk_mf_error(e, error, capacity);
        return e->borrowed_rejected ? 1 : -1;
    }
    *output = e->output;
    *size = e->output_size;
    e->output = NULL;
    e->output_size = 0;
    return 0;
}
