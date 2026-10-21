//go:build windows && cgo

#define BK_VENDOR_IMPLEMENTATION
#include "vendor_windows.h"
#define GPU_DROP(type, field)                                                                                          \
    do {                                                                                                               \
        if (e->field) {                                                                                                \
            type##_Release(e->field);                                                                                  \
            e->field = NULL;                                                                                           \
        }                                                                                                              \
    } while (0)
#define GPU_TRY(call)                                                                                                  \
    do {                                                                                                               \
        hr = (call);                                                                                                   \
        if (FAILED(hr))                                                                                                \
            goto fallback;                                                                                             \
    } while (0)

void bk_d3d_converter_close(bk_d3d_converter *e) {
    GPU_DROP(ID3D11VideoProcessorInputView, input_view);
    GPU_DROP(ID3D11VideoProcessorOutputView, output_view);
    GPU_DROP(ID3D11VideoProcessor, processor);
    GPU_DROP(ID3D11VideoProcessorEnumerator, enumerator);
    GPU_DROP(ID3D11VideoContext, video_context);
    GPU_DROP(ID3D11VideoDevice, video_device);
    GPU_DROP(IDXGIKeyedMutex, shared_mutex);
    GPU_DROP(ID3D11Texture2D, shared_texture);
    memset(e, 0, sizeof(*e));
}

// The keyed mutex orders producer/consumer GPU work across D3D11 devices.
// Only the compressed encoder output is ever copied into CPU memory.
int bk_d3d_convert(bk_d3d_converter *converter, ID3D11Device *device, ID3D11DeviceContext *context,
                   ID3D11Texture2D *destination, const bk_vendor_options *o, uintptr_t handle, int rotation,
                   char *error, size_t capacity) {
    BK_MARK(o, conversion_start);
    HRESULT hr = S_OK;
    bk_d3d_converter *e = converter;
    if (e->shared_handle != (HANDLE)handle || !e->shared_texture) {
        GPU_DROP(ID3D11VideoProcessorInputView, input_view);
        GPU_DROP(IDXGIKeyedMutex, shared_mutex);
        GPU_DROP(ID3D11Texture2D, shared_texture);
        e->shared_handle = NULL;
        GPU_TRY(
            ID3D11Device_OpenSharedResource(device, (HANDLE)handle, &IID_ID3D11Texture2D, (void **)&e->shared_texture));
        GPU_TRY(ID3D11Texture2D_QueryInterface(e->shared_texture, &IID_IDXGIKeyedMutex, (void **)&e->shared_mutex));
        e->shared_handle = (HANDLE)handle;
    }
    D3D11_TEXTURE2D_DESC desc;
    ID3D11Texture2D_GetDesc(e->shared_texture, &desc);
    D3D11_TEXTURE2D_DESC target;
    ID3D11Texture2D_GetDesc(destination, &target);
    if (target.Format != DXGI_FORMAT_NV12 || target.Width < (UINT)o->width || target.Height < (UINT)o->height ||
        target.ArraySize != 1) {
        hr = E_INVALIDARG;
        goto fallback;
    }
    if (e->destination != destination) {
        GPU_DROP(ID3D11VideoProcessorOutputView, output_view);
        e->destination = destination;
    }
    if (!e->processor || e->input_width != desc.Width || e->input_height != desc.Height ||
        e->output_width != target.Width || e->output_height != target.Height) {
        GPU_DROP(ID3D11VideoProcessorInputView, input_view);
        GPU_DROP(ID3D11VideoProcessorOutputView, output_view);
        GPU_DROP(ID3D11VideoProcessor, processor);
        GPU_DROP(ID3D11VideoProcessorEnumerator, enumerator);
        if (!e->video_device)
            GPU_TRY(ID3D11Device_QueryInterface(device, &IID_ID3D11VideoDevice, (void **)&e->video_device));
        if (!e->video_context)
            GPU_TRY(ID3D11DeviceContext_QueryInterface(context, &IID_ID3D11VideoContext, (void **)&e->video_context));
        D3D11_VIDEO_PROCESSOR_CONTENT_DESC content = {0};
        content.InputFrameFormat = D3D11_VIDEO_FRAME_FORMAT_PROGRESSIVE;
        content.InputWidth = desc.Width;
        content.InputHeight = desc.Height;
        content.OutputWidth = target.Width;
        content.OutputHeight = target.Height;
        content.InputFrameRate.Numerator = content.OutputFrameRate.Numerator = o->num;
        content.InputFrameRate.Denominator = content.OutputFrameRate.Denominator = o->den;
        content.Usage = D3D11_VIDEO_USAGE_OPTIMAL_SPEED;
        GPU_TRY(ID3D11VideoDevice_CreateVideoProcessorEnumerator(e->video_device, &content, &e->enumerator));
        UINT support = 0;
        GPU_TRY(ID3D11VideoProcessorEnumerator_CheckVideoProcessorFormat(e->enumerator, desc.Format, &support));
        if (!(support & D3D11_VIDEO_PROCESSOR_FORMAT_SUPPORT_INPUT)) {
            hr = E_NOTIMPL;
            goto fallback;
        }
        GPU_TRY(ID3D11VideoProcessorEnumerator_CheckVideoProcessorFormat(e->enumerator, DXGI_FORMAT_NV12, &support));
        if (!(support & D3D11_VIDEO_PROCESSOR_FORMAT_SUPPORT_OUTPUT)) {
            hr = E_NOTIMPL;
            goto fallback;
        }
        GPU_TRY(ID3D11VideoDevice_CreateVideoProcessor(e->video_device, e->enumerator, 0, &e->processor));
        e->input_width = desc.Width;
        e->input_height = desc.Height;
        e->output_width = target.Width;
        e->output_height = target.Height;
    }
    if (!e->output_view) {
        D3D11_VIDEO_PROCESSOR_OUTPUT_VIEW_DESC out = {0};
        out.ViewDimension = D3D11_VPOV_DIMENSION_TEXTURE2D;
        GPU_TRY(ID3D11VideoDevice_CreateVideoProcessorOutputView(e->video_device, (ID3D11Resource *)destination,
                                                                 e->enumerator, &out, &e->output_view));
    }
    if (!e->input_view) {
        D3D11_VIDEO_PROCESSOR_INPUT_VIEW_DESC in = {0};
        in.ViewDimension = D3D11_VPIV_DIMENSION_TEXTURE2D;
        GPU_TRY(ID3D11VideoDevice_CreateVideoProcessorInputView(e->video_device, (ID3D11Resource *)e->shared_texture,
                                                                e->enumerator, &in, &e->input_view));
    }
    // Disable automatic enhancements and temporal filtering to avoid extra latency.
    ID3D11VideoContext_VideoProcessorSetStreamAutoProcessingMode(e->video_context, e->processor, 0, FALSE);
    ID3D11VideoContext_VideoProcessorSetStreamFrameFormat(e->video_context, e->processor, 0,
                                                          D3D11_VIDEO_FRAME_FORMAT_PROGRESSIVE);
    D3D11_VIDEO_PROCESSOR_COLOR_SPACE rgb = {0}, yuv = {0};
    rgb.RGB_Range = 0;
    yuv.YCbCr_Matrix = 1;
    yuv.Nominal_Range = 1;
    ID3D11VideoContext_VideoProcessorSetStreamColorSpace(e->video_context, e->processor, 0, &rgb);
    ID3D11VideoContext_VideoProcessorSetOutputColorSpace(e->video_context, e->processor, &yuv);
    RECT source = {0, 0, (LONG)desc.Width, (LONG)desc.Height}, dest = {0, 0, o->width, o->height};
    int sw = (rotation == 90 || rotation == 270) ? desc.Height : desc.Width,
        sh = (rotation == 90 || rotation == 270) ? desc.Width : desc.Height;
    // Fit the desktop without distorting its aspect ratio; the processor fills black bars.
    if ((int64_t)sw * o->height > (int64_t)sh * o->width) {
        int h = (int)((int64_t)o->width * sh / sw) & ~1;
        dest.top = (o->height - h) / 2;
        dest.bottom = dest.top + h;
    } else {
        int w = (int)((int64_t)o->height * sw / sh) & ~1;
        dest.left = (o->width - w) / 2;
        dest.right = dest.left + w;
    }
    ID3D11VideoContext_VideoProcessorSetStreamSourceRect(e->video_context, e->processor, 0, TRUE, &source);
    ID3D11VideoContext_VideoProcessorSetStreamDestRect(e->video_context, e->processor, 0, TRUE, &dest);
    ID3D11VideoContext_VideoProcessorSetStreamRotation(e->video_context, e->processor, 0, rotation != 0,
                                                       rotation == 90    ? D3D11_VIDEO_PROCESSOR_ROTATION_90
                                                       : rotation == 180 ? D3D11_VIDEO_PROCESSOR_ROTATION_180
                                                       : rotation == 270 ? D3D11_VIDEO_PROCESSOR_ROTATION_270
                                                                         : D3D11_VIDEO_PROCESSOR_ROTATION_IDENTITY);
    D3D11_VIDEO_COLOR black = {0};
    black.RGBA.A = 1;
    ID3D11VideoContext_VideoProcessorSetOutputBackgroundColor(e->video_context, e->processor, FALSE, &black);
    D3D11_VIDEO_PROCESSOR_STREAM stream = {0};
    stream.Enable = TRUE;
    stream.pInputSurface = e->input_view;
    hr = IDXGIKeyedMutex_AcquireSync(e->shared_mutex, 0, 1000);
    if (hr != S_OK) {
        hr = E_FAIL;
        goto fallback;
    }
    hr = ID3D11VideoContext_VideoProcessorBlt(e->video_context, e->processor, e->output_view, 0, 1, &stream);
    HRESULT unlocked = IDXGIKeyedMutex_ReleaseSync(e->shared_mutex, 0);
    if (FAILED(hr))
        goto fallback;
    if (FAILED(unlocked)) {
        hr = unlocked;
        goto fallback;
    }
    ID3D11DeviceContext_Flush(context);
    BK_MARK(o, conversion_end);
    return 0;
fallback:
    snprintf(error, capacity,
             "D3D11 shared texture conversion failed: 0x%08lx (display and encoder must use the same adapter)",
             (unsigned long)hr);
    return 1;
}
