//go:build windows && cgo && !ffmpeg

#define BK_VENDOR_IMPLEMENTATION
#include "third_party/amf/components/VideoEncoderAV1.h"
#include "third_party/amf/components/VideoEncoderVCE.h"
#include "third_party/amf/core/Factory.h"
#include "third_party/amf/core/Version.h"
#include "vendor_windows.h"

typedef struct {
    bk_vendor_options options;
    HMODULE module;
    AMFContext *context;
    AMFContext1 *context1;
    AMFComponent *encoder;
    ID3D11Device *device;
    ID3D11DeviceContext *device_context;
    amf_pts clock, remainder;
    bk_d3d_converter converter;
} bk_amf;

static AMFVariantStruct bk_amf_int(amf_int64 value) {
    AMFVariantStruct result = {0};
    result.type = AMF_VARIANT_INT64;
    result.int64Value = value;
    return result;
}
static AMFVariantStruct bk_amf_bool(int value) {
    AMFVariantStruct result = {0};
    result.type = AMF_VARIANT_BOOL;
    result.boolValue = value != 0;
    return result;
}
static int bk_amf_status(AMF_RESULT status, const char *operation, char *error, size_t capacity) {
    if (status == AMF_OK)
        return 0;
    snprintf(error, capacity, "%s failed (AMF status %d)", operation, status);
    return -1;
}
#define AMF_TRY(call)                                                                                                  \
    do {                                                                                                               \
        if (bk_amf_status((call), #call, error, capacity))                                                             \
            goto failed;                                                                                               \
    } while (0)
#define AMF_PROP(key, value) AMF_TRY(e->encoder->pVtbl->SetProperty(e->encoder, (key), (value)))

static void bk_amf_close(void *opaque) {
    bk_amf *e = opaque;
    if (!e)
        return;
    if (e->encoder) {
        e->encoder->pVtbl->Terminate(e->encoder);
        e->encoder->pVtbl->Release(e->encoder);
    }
    if (e->context1)
        e->context1->pVtbl->Release(e->context1);
    if (e->context) {
        e->context->pVtbl->Terminate(e->context);
        e->context->pVtbl->Release(e->context);
    }
    bk_d3d_converter_close(&e->converter);
    if (e->device_context)
        ID3D11DeviceContext_Release(e->device_context);
    if (e->device)
        ID3D11Device_Release(e->device);
    if (e->module)
        FreeLibrary(e->module);
    free(e);
}

static void *bk_amf_open(const bk_vendor_options *o, char *error, size_t capacity) {
    bk_amf *e = calloc(1, sizeof(*e));
    if (!e) {
        snprintf(error, capacity, "out of memory");
        return NULL;
    }
    e->options = *o;
    e->module = bk_vendor_library(L"amfrt64.dll");
    AMFInit_Fn init = e->module ? (void *)GetProcAddress(e->module, AMF_INIT_FUNCTION_NAME) : NULL;
    if (!init) {
        snprintf(error, capacity, "AMD driver does not provide amfrt64.dll");
        goto failed;
    }
    AMFFactory *factory = NULL;
    // Only the stable base interfaces are used. Do not require the header's newest runtime.
    AMF_TRY(init(AMF_MAKE_FULL_VERSION(1, 4, 28, 0), &factory));
    if (!factory) {
        snprintf(error, capacity, "AMD runtime returned no factory");
        goto failed;
    }
    HRESULT hr = bk_vendor_device(0x1002, o, &e->device, &e->device_context);
    if (FAILED(hr)) {
        snprintf(error, capacity, "AMD D3D11 device failed: 0x%08lx", (unsigned long)hr);
        goto failed;
    }
    AMF_TRY(factory->pVtbl->CreateContext(factory, &e->context));
    AMF_TRY(e->context->pVtbl->InitDX11(e->context, e->device, AMF_DX11_0));
    const AMFGuid context1_iid = IID_AMFContext1();
    e->context->pVtbl->QueryInterface(e->context, &context1_iid, (void **)&e->context1);
    AMF_TRY(factory->pVtbl->CreateComponent(factory, e->context,
                                            o->codec == 2 ? AMFVideoEncoder_AV1 : AMFVideoEncoderVCE_AVC, &e->encoder));
    int av1 = o->codec == 2;
    AMF_PROP(
        av1 ? AMF_VIDEO_ENCODER_AV1_USAGE : AMF_VIDEO_ENCODER_USAGE,
        bk_amf_int(av1 ? AMF_VIDEO_ENCODER_AV1_USAGE_ULTRA_LOW_LATENCY : AMF_VIDEO_ENCODER_USAGE_ULTRA_LOW_LATENCY));
    AMF_PROP(av1 ? AMF_VIDEO_ENCODER_AV1_QUALITY_PRESET : AMF_VIDEO_ENCODER_QUALITY_PRESET,
             bk_amf_int(av1 ? AMF_VIDEO_ENCODER_AV1_QUALITY_PRESET_SPEED : AMF_VIDEO_ENCODER_QUALITY_PRESET_SPEED));
    AMFVariantStruct dimensions = {0};
    dimensions.type = AMF_VARIANT_SIZE;
    dimensions.sizeValue.width = o->width;
    dimensions.sizeValue.height = o->height;
    AMF_PROP(av1 ? AMF_VIDEO_ENCODER_AV1_FRAMESIZE : AMF_VIDEO_ENCODER_FRAMESIZE, dimensions);
    AMFVariantStruct rate = {0};
    rate.type = AMF_VARIANT_RATE;
    rate.rateValue.num = o->num;
    rate.rateValue.den = o->den;
    AMF_PROP(av1 ? AMF_VIDEO_ENCODER_AV1_FRAMERATE : AMF_VIDEO_ENCODER_FRAMERATE, rate);
    AMF_PROP(
        av1 ? AMF_VIDEO_ENCODER_AV1_RATE_CONTROL_METHOD : AMF_VIDEO_ENCODER_RATE_CONTROL_METHOD,
        bk_amf_int(av1 ? AMF_VIDEO_ENCODER_AV1_RATE_CONTROL_METHOD_CBR : AMF_VIDEO_ENCODER_RATE_CONTROL_METHOD_CBR));
    AMF_PROP(av1 ? AMF_VIDEO_ENCODER_AV1_TARGET_BITRATE : AMF_VIDEO_ENCODER_TARGET_BITRATE,
             bk_amf_int((amf_int64)o->bitrate * 1000));
    AMF_PROP(av1 ? AMF_VIDEO_ENCODER_AV1_VBV_BUFFER_SIZE : AMF_VIDEO_ENCODER_VBV_BUFFER_SIZE,
             bk_amf_int(((amf_int64)o->bitrate * 1000 * o->den + o->num - 1) / o->num));
    AMF_PROP(av1 ? AMF_VIDEO_ENCODER_AV1_GOP_SIZE : AMF_VIDEO_ENCODER_IDR_PERIOD,
             bk_amf_int(((amf_int64)o->num * 2 + o->den - 1) / o->den));
    if (av1) {
        AMF_PROP(AMF_VIDEO_ENCODER_AV1_HEADER_INSERTION_MODE,
                 bk_amf_int(AMF_VIDEO_ENCODER_AV1_HEADER_INSERTION_MODE_KEY_FRAME_ALIGNED));
        // Older AV1 runtimes have no B-frame property and always use zero B frames.
        if (e->encoder->pVtbl->HasProperty(e->encoder, AMF_VIDEO_ENCODER_AV1_B_PIC_PATTERN)) {
            AMF_PROP(AMF_VIDEO_ENCODER_AV1_B_PIC_PATTERN, bk_amf_int(0));
        }
    } else {
        AMF_PROP(AMF_VIDEO_ENCODER_PROFILE, bk_amf_int(AMF_VIDEO_ENCODER_PROFILE_BASELINE));
        AMF_PROP(AMF_VIDEO_ENCODER_B_PIC_PATTERN, bk_amf_int(0));
    }
    AMF_TRY(e->encoder->pVtbl->Init(e->encoder, AMF_SURFACE_NV12, o->width, o->height));
    return e;
failed:
    bk_amf_close(e);
    return NULL;
}

static int bk_amf_submit(bk_amf *e, AMFSurface *surface, int force, uint8_t **output, size_t *size, char *error,
                         size_t capacity) {
    AMFData *data = NULL;
    AMFBuffer *buffer = NULL;
    int result = -1;
    amf_pts pts = e->clock;
    e->remainder += (amf_pts)10000000 * e->options.den;
    amf_pts duration = e->remainder / e->options.num;
    e->remainder %= e->options.num;
    e->clock += duration;
    surface->pVtbl->SetPts(surface, pts);
    surface->pVtbl->SetDuration(surface, duration);
    if (force) {
        if (e->options.codec == 2) {
            AMF_TRY(surface->pVtbl->SetProperty(surface, AMF_VIDEO_ENCODER_AV1_FORCE_FRAME_TYPE,
                                                bk_amf_int(AMF_VIDEO_ENCODER_AV1_FORCE_FRAME_TYPE_KEY)));
            AMF_TRY(surface->pVtbl->SetProperty(surface, AMF_VIDEO_ENCODER_AV1_FORCE_INSERT_SEQUENCE_HEADER,
                                                bk_amf_bool(1)));
        } else {
            AMF_TRY(surface->pVtbl->SetProperty(surface, AMF_VIDEO_ENCODER_FORCE_PICTURE_TYPE,
                                                bk_amf_int(AMF_VIDEO_ENCODER_PICTURE_TYPE_IDR)));
            AMF_TRY(surface->pVtbl->SetProperty(surface, AMF_VIDEO_ENCODER_INSERT_SPS, bk_amf_bool(1)));
            AMF_TRY(surface->pVtbl->SetProperty(surface, AMF_VIDEO_ENCODER_INSERT_PPS, bk_amf_bool(1)));
        }
    }
    BK_MARK(&e->options, encode_submit);
    AMF_TRY(e->encoder->pVtbl->SubmitInput(e->encoder, (AMFData *)surface));
    ULONGLONG deadline = GetTickCount64() + 5000;
    for (;;) {
        AMF_RESULT status = e->encoder->pVtbl->QueryOutput(e->encoder, &data);
        if (status != AMF_OK && status != AMF_REPEAT) {
            bk_amf_status(status, "AMF QueryOutput", error, capacity);
            goto failed;
        }
        if (data)
            break;
        if (GetTickCount64() >= deadline) {
            snprintf(error, capacity, "AMF output timed out; buffered encoding is unsupported");
            goto failed;
        }
        Sleep(1);
    }
    BK_MARK(&e->options, encode_complete);
    if (data->pVtbl->GetPts(data) != pts) {
        snprintf(error, capacity, "AMF returned a reordered picture");
        goto failed;
    }
    const AMFGuid iid = IID_AMFBuffer();
    AMF_TRY(data->pVtbl->QueryInterface(data, &iid, (void **)&buffer));
    result =
        bk_vendor_copy(buffer->pVtbl->GetNative(buffer), buffer->pVtbl->GetSize(buffer), output, size, error, capacity);
failed:
    if (buffer)
        buffer->pVtbl->Release(buffer);
    if (data)
        data->pVtbl->Release(data);
    return result;
}

static int bk_amf_encode(void *opaque, const uint8_t *input, int force, uint8_t **output, size_t *size, char *error,
                         size_t capacity) {
    bk_amf *e = opaque;
    AMFSurface *surface = NULL;
    int result = -1;
    AMF_TRY(e->context->pVtbl->AllocSurface(e->context, AMF_MEMORY_HOST, AMF_SURFACE_NV12, e->options.width,
                                            e->options.height, &surface));
    if (surface->pVtbl->GetPlanesCount(surface) != 2) {
        snprintf(error, capacity, "AMD returned invalid NV12 planes");
        goto failed;
    }
    for (int i = 0; i < 2; ++i) {
        AMFPlane *plane = surface->pVtbl->GetPlaneAt(surface, i);
        uint8_t *destination = plane->pVtbl->GetNative(plane);
        int pitch = plane->pVtbl->GetHPitch(plane), rows = e->options.height / (i + 1);
        if (!destination || pitch < e->options.width || plane->pVtbl->GetHeight(plane) < rows) {
            snprintf(error, capacity, "AMD returned invalid NV12 stride");
            goto failed;
        }
        const uint8_t *source = input + (i ? (size_t)e->options.width * e->options.height : 0);
        for (int row = 0; row < rows; ++row)
            memcpy(destination + (size_t)row * pitch, source + (size_t)row * e->options.width, e->options.width);
    }
    AMF_TRY(surface->pVtbl->Convert(surface, AMF_MEMORY_DX11));
    result = bk_amf_submit(e, surface, force, output, size, error, capacity);
failed:
    if (surface)
        surface->pVtbl->Release(surface);
    return result;
}

static int bk_amf_encode_texture(void *opaque, uintptr_t handle, int rotation, int force, uint8_t **output,
                                 size_t *size, char *error, size_t capacity) {
    bk_amf *e = opaque;
    AMFSurface *surface = NULL;
    // Request a video-processor render target explicitly. Older runtimes can
    // still use their default DX11 surfaces if they support processor output.
    AMF_RESULT status = e->context1
        ? e->context1->pVtbl->AllocSurfaceEx(e->context1, AMF_MEMORY_DX11, AMF_SURFACE_NV12,
                                            e->options.width, e->options.height,
                                            AMF_SURFACE_USAGE_RENDER_TARGET | AMF_SURFACE_USAGE_SHADER_RESOURCE,
                                            AMF_MEMORY_CPU_NONE, &surface)
        : e->context->pVtbl->AllocSurface(e->context, AMF_MEMORY_DX11, AMF_SURFACE_NV12,
                                         e->options.width, e->options.height, &surface);
    if (status != AMF_OK || !surface) {
        snprintf(error, capacity, "AMF DX11 surface allocation failed (status %d)", status);
        return 1;
    }
    AMFPlane *plane = surface->pVtbl->GetPlaneAt(surface, 0);
    ID3D11Texture2D *texture = plane ? (ID3D11Texture2D *)plane->pVtbl->GetNative(plane) : NULL;
    int result = 1;
    if (!texture)
        snprintf(error, capacity, "AMF DX11 surface has no native texture");
    else {
        result = bk_d3d_convert(&e->converter, e->device, e->device_context, texture, &e->options, handle, rotation,
                                error, capacity);
        if (!result)
            result = bk_amf_submit(e, surface, force, output, size, error, capacity);
    }
    // AMF's surface pool tracks runtime references, so no texture is overwritten
    // while the driver still owns it. The converter retains only its output view.
    surface->pVtbl->Release(surface);
    return result;
}
const bk_vendor_api bk_amf_api = {bk_amf_open, bk_amf_encode, bk_amf_close, bk_amf_encode_texture};
