//go:build windows && cgo

#define BK_VENDOR_IMPLEMENTATION
#include "third_party/nvenc/nvEncodeAPI.h"
#include "vendor_windows.h"

typedef struct {
    bk_vendor_options options;
    HMODULE module;
    NV_ENCODE_API_FUNCTION_LIST api;
    void *session;
    ID3D11Device *device;
    ID3D11DeviceContext *context;
    ID3D11Texture2D *texture;
    NV_ENC_REGISTERED_PTR resource;
    NV_ENC_INPUT_PTR mapped;
    NV_ENC_OUTPUT_PTR bitstream;
    uint64_t frame;
    bk_d3d_converter converter;
} bk_nvenc;

static void bk_nvenc_close(void *opaque) {
    bk_nvenc *e = opaque;
    if (!e)
        return;
    if (e->session) {
        if (e->mapped)
            e->api.nvEncUnmapInputResource(e->session, e->mapped);
        if (e->bitstream)
            e->api.nvEncDestroyBitstreamBuffer(e->session, e->bitstream);
        if (e->resource)
            e->api.nvEncUnregisterResource(e->session, e->resource);
        e->api.nvEncDestroyEncoder(e->session);
    }
    bk_d3d_converter_close(&e->converter);
    if (e->texture)
        ID3D11Texture2D_Release(e->texture);
    if (e->context)
        ID3D11DeviceContext_Release(e->context);
    if (e->device)
        ID3D11Device_Release(e->device);
    if (e->module)
        FreeLibrary(e->module);
    free(e);
}

static int bk_nvenc_status(NVENCSTATUS status, const char *operation, char *error, size_t capacity) {
    if (status == NV_ENC_SUCCESS)
        return 0;
    snprintf(error, capacity, "%s failed (NVENC status %d)", operation, status);
    return -1;
}
#define NV_TRY(call)                                                                                                   \
    do {                                                                                                               \
        if (bk_nvenc_status((call), #call, error, capacity))                                                           \
            goto failed;                                                                                               \
    } while (0)

static int bk_nvenc_session(bk_nvenc *e, const bk_vendor_options *o, char *error, size_t capacity) {
    e->module = bk_vendor_library(L"nvEncodeAPI64.dll");
    NVENCSTATUS(NVENCAPI * create)(NV_ENCODE_API_FUNCTION_LIST *) =
        e->module ? (void *)GetProcAddress(e->module, "NvEncodeAPICreateInstance") : NULL;
    NVENCSTATUS(NVENCAPI * version)(uint32_t *) =
        e->module ? (void *)GetProcAddress(e->module, "NvEncodeAPIGetMaxSupportedVersion") : NULL;
    if (!create || !version) {
        snprintf(error, capacity, "NVIDIA driver does not provide nvEncodeAPI64.dll");
        goto failed;
    }
    uint32_t supported = 0;
    NV_TRY(version(&supported));
    if (supported < ((NVENCAPI_MAJOR_VERSION << 4) | NVENCAPI_MINOR_VERSION)) {
        snprintf(error, capacity, "NVIDIA driver requires NVENC API %d.%d or newer", NVENCAPI_MAJOR_VERSION,
                 NVENCAPI_MINOR_VERSION);
        goto failed;
    }
    e->api.version = NV_ENCODE_API_FUNCTION_LIST_VER;
    NV_TRY(create(&e->api));
    HRESULT hr = bk_vendor_device(0x10de, o, &e->device, &e->context);
    if (FAILED(hr)) {
        snprintf(error, capacity, "NVIDIA D3D11 device failed: 0x%08lx", (unsigned long)hr);
        goto failed;
    }
    NV_ENC_OPEN_ENCODE_SESSION_EX_PARAMS session = {0};
    session.version = NV_ENC_OPEN_ENCODE_SESSION_EX_PARAMS_VER;
    session.deviceType = NV_ENC_DEVICE_TYPE_DIRECTX;
    session.device = e->device;
    session.apiVersion = NVENCAPI_VERSION;
    NV_TRY(e->api.nvEncOpenEncodeSessionEx(&session, &e->session));
    return 0;
failed:
    return -1;
}

int bk_nvenc_supports(int codec) {
    bk_vendor_options options = {0};
    bk_nvenc *e = calloc(1, sizeof(*e));
    if (!e)
        return 0;
    char error[512];
    int supported = 0;
    if (bk_nvenc_session(e, &options, error, sizeof(error)) < 0)
        goto done;
    GUID wanted = codec == 1 ? NV_ENC_CODEC_HEVC_GUID : NV_ENC_CODEC_AV1_GUID;
    GUID guids[32];
    uint32_t count = 0;
    if (e->api.nvEncGetEncodeGUIDs(e->session, guids, 32, &count) != NV_ENC_SUCCESS || count > 32)
        goto done;
    for (uint32_t i = 0; i < count; i++) {
        if (memcmp(&guids[i], &wanted, sizeof(wanted)) != 0)
            continue;
        NV_ENC_BUFFER_FORMAT formats[64];
        uint32_t n = 0;
        if (e->api.nvEncGetInputFormats(e->session, wanted, formats, 64, &n) != NV_ENC_SUCCESS || n > 64)
            break;
        for (uint32_t j = 0; j < n; j++)
            if (formats[j] == NV_ENC_BUFFER_FORMAT_NV12)
                supported = 1;
        break;
    }
done:
    bk_nvenc_close(e);
    return supported;
}

static void *bk_nvenc_open(const bk_vendor_options *o, char *error, size_t capacity) {
    bk_nvenc *e = calloc(1, sizeof(*e));
    if (!e) {
        snprintf(error, capacity, "out of memory");
        return NULL;
    }
    e->options = *o;
    HRESULT hr;
    if (bk_nvenc_session(e, o, error, capacity) < 0) goto failed;
    GUID codec = o->codec == 2 ? NV_ENC_CODEC_AV1_GUID : o->codec == 1 ? NV_ENC_CODEC_HEVC_GUID : NV_ENC_CODEC_H264_GUID;
    NV_ENC_PRESET_CONFIG preset = {0};
    preset.version = NV_ENC_PRESET_CONFIG_VER;
    preset.presetCfg.version = NV_ENC_CONFIG_VER;
    NV_TRY(e->api.nvEncGetEncodePresetConfigEx(e->session, codec, NV_ENC_PRESET_P1_GUID,
                                               NV_ENC_TUNING_INFO_ULTRA_LOW_LATENCY, &preset));
    NV_ENC_CONFIG config = preset.presetCfg;
    config.version = NV_ENC_CONFIG_VER;
    config.gopLength = (uint32_t)(((uint64_t)o->num * 2 + o->den - 1) / o->den);
    config.frameIntervalP = 1;
    config.rcParams.rateControlMode = NV_ENC_PARAMS_RC_CBR;
    config.rcParams.averageBitRate = config.rcParams.maxBitRate = (uint32_t)o->bitrate * 1000;
    config.rcParams.vbvBufferSize = (uint32_t)(((uint64_t)o->bitrate * 1000 * o->den + o->num - 1) / o->num);
    config.rcParams.vbvInitialDelay = config.rcParams.vbvBufferSize;
    config.rcParams.enableLookahead = 0;
    config.rcParams.lookaheadDepth = 0;
    config.rcParams.zeroReorderDelay = 1;
    config.rcParams.enableAQ = 0;
    config.rcParams.enableTemporalAQ = 0;
    if (o->codec == 2) {
        config.profileGUID = NV_ENC_AV1_PROFILE_MAIN_GUID;
        config.encodeCodecConfig.av1Config.repeatSeqHdr = 1;
        config.encodeCodecConfig.av1Config.outputAnnexBFormat = 0;
        config.encodeCodecConfig.av1Config.idrPeriod = config.gopLength;
        config.encodeCodecConfig.av1Config.chromaFormatIDC = 1;
    } else if (o->codec == 1) {
        config.profileGUID = NV_ENC_HEVC_PROFILE_MAIN_GUID;
        config.encodeCodecConfig.hevcConfig.repeatSPSPPS = 1;
        config.encodeCodecConfig.hevcConfig.idrPeriod = config.gopLength;
        config.encodeCodecConfig.hevcConfig.chromaFormatIDC = 1;
    } else {
        config.profileGUID = NV_ENC_H264_PROFILE_BASELINE_GUID;
        config.encodeCodecConfig.h264Config.repeatSPSPPS = 1;
        config.encodeCodecConfig.h264Config.idrPeriod = config.gopLength;
        config.encodeCodecConfig.h264Config.chromaFormatIDC = 1;
    }
    NV_ENC_INITIALIZE_PARAMS init = {0};
    init.version = NV_ENC_INITIALIZE_PARAMS_VER;
    init.encodeGUID = codec;
    init.presetGUID = NV_ENC_PRESET_P1_GUID;
    init.encodeWidth = init.darWidth = o->width;
    init.encodeHeight = init.darHeight = o->height;
    init.frameRateNum = o->num;
    init.frameRateDen = o->den;
    init.enablePTD = 1;
    init.enableEncodeAsync = 0;
    init.tuningInfo = NV_ENC_TUNING_INFO_ULTRA_LOW_LATENCY;
    init.encodeConfig = &config;
    NV_TRY(e->api.nvEncInitializeEncoder(e->session, &init));
    D3D11_TEXTURE2D_DESC desc = {0};
    desc.Width = o->width;
    desc.Height = o->height;
    desc.MipLevels = desc.ArraySize = 1;
    desc.Format = DXGI_FORMAT_NV12;
    desc.SampleDesc.Count = 1;
    desc.Usage = D3D11_USAGE_DEFAULT;
    desc.BindFlags = D3D11_BIND_RENDER_TARGET;
    hr = ID3D11Device_CreateTexture2D(e->device, &desc, NULL, &e->texture);
    if (FAILED(hr)) {
        snprintf(error, capacity, "NVIDIA NV12 texture failed: 0x%08lx", (unsigned long)hr);
        goto failed;
    }
    NV_ENC_REGISTER_RESOURCE resource = {0};
    resource.version = NV_ENC_REGISTER_RESOURCE_VER;
    resource.resourceType = NV_ENC_INPUT_RESOURCE_TYPE_DIRECTX;
    resource.resourceToRegister = e->texture;
    resource.width = o->width;
    resource.height = o->height;
    resource.bufferFormat = NV_ENC_BUFFER_FORMAT_NV12;
    resource.bufferUsage = NV_ENC_INPUT_IMAGE;
    NV_TRY(e->api.nvEncRegisterResource(e->session, &resource));
    e->resource = resource.registeredResource;
    NV_ENC_CREATE_BITSTREAM_BUFFER buffer = {0};
    buffer.version = NV_ENC_CREATE_BITSTREAM_BUFFER_VER;
    NV_TRY(e->api.nvEncCreateBitstreamBuffer(e->session, &buffer));
    e->bitstream = buffer.bitstreamBuffer;
    return e;
failed:
    bk_nvenc_close(e);
    return NULL;
}

static int bk_nvenc_encode_current(void *opaque, int force, uint8_t **output, size_t *size, char *error,
                                   size_t capacity) {
    bk_nvenc *e = opaque;
    NV_ENC_MAP_INPUT_RESOURCE mapped = {0};
    mapped.version = NV_ENC_MAP_INPUT_RESOURCE_VER;
    mapped.registeredResource = e->resource;
    NV_TRY(e->api.nvEncMapInputResource(e->session, &mapped));
    e->mapped = mapped.mappedResource;
    NV_ENC_PIC_PARAMS picture = {0};
    picture.version = NV_ENC_PIC_PARAMS_VER;
    picture.inputBuffer = e->mapped;
    picture.bufferFmt = mapped.mappedBufferFmt;
    picture.inputWidth = e->options.width;
    picture.inputHeight = e->options.height;
    picture.outputBitstream = e->bitstream;
    picture.pictureStruct = NV_ENC_PIC_STRUCT_FRAME;
    picture.inputTimeStamp = e->frame++;
    picture.frameIdx = (uint32_t)picture.inputTimeStamp;
    if (force)
        picture.encodePicFlags = NV_ENC_PIC_FLAG_FORCEIDR | NV_ENC_PIC_FLAG_OUTPUT_SPSPPS;
    BK_MARK(&e->options, encode_submit);
    NV_TRY(e->api.nvEncEncodePicture(e->session, &picture));
    NV_ENC_LOCK_BITSTREAM locked = {0};
    locked.version = NV_ENC_LOCK_BITSTREAM_VER;
    locked.outputBitstream = e->bitstream;
    NV_TRY(e->api.nvEncLockBitstream(e->session, &locked));
    BK_MARK(&e->options, encode_complete);
    int result = bk_vendor_copy(locked.bitstreamBufferPtr, locked.bitstreamSizeInBytes, output, size, error, capacity);
    NVENCSTATUS unlocked = e->api.nvEncUnlockBitstream(e->session, e->bitstream);
    NVENCSTATUS unmapped = e->api.nvEncUnmapInputResource(e->session, e->mapped);
    e->mapped = NULL;
    if (result || bk_nvenc_status(unlocked, "NvEncUnlockBitstream", error, capacity) ||
        bk_nvenc_status(unmapped, "NvEncUnmapInputResource", error, capacity)) {
        free(*output);
        *output = NULL;
        *size = 0;
        return -1;
    }
    return 0;
failed:
    if (e->mapped) {
        e->api.nvEncUnmapInputResource(e->session, e->mapped);
        e->mapped = NULL;
    }
    return -1;
}

static int bk_nvenc_encode(void *opaque, const uint8_t *input, int force, uint8_t **output, size_t *size, char *error,
                           size_t capacity) {
    bk_nvenc *e = opaque;
    ID3D11DeviceContext_UpdateSubresource(e->context, (ID3D11Resource *)e->texture, 0, NULL, input, e->options.width,
                                          0);
    return bk_nvenc_encode_current(e, force, output, size, error, capacity);
}

static int bk_nvenc_encode_texture(void *opaque, uintptr_t handle, int rotation, int force, uint8_t **output,
                                   size_t *size, char *error, size_t capacity) {
    bk_nvenc *e = opaque;
    int status = bk_d3d_convert(&e->converter, e->device, e->context, e->texture, &e->options, handle, rotation, error,
                                capacity);
    if (status)
        return status;
    return bk_nvenc_encode_current(e, force, output, size, error, capacity);
}
const bk_vendor_api bk_nvenc_api = {bk_nvenc_open, bk_nvenc_encode, bk_nvenc_close, bk_nvenc_encode_texture};
