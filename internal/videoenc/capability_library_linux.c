// go:build linux && cgo && ffmpeg

#include "third_party/nvenc/nvEncodeAPI.h"
#include <dlfcn.h>
#include <fcntl.h>
#include <string.h>
#include <unistd.h>

// Query the driver on each CUDA device, without initializing an encoder or
// allocating/submitting video pictures. Keep the caller's CUDA context intact.
int bk_linux_nvenc_supports(int codec) {
    void *cuda = dlopen("libcuda.so.1", RTLD_NOW | RTLD_LOCAL);
    void *nvenc = dlopen("libnvidia-encode.so.1", RTLD_NOW | RTLD_LOCAL);
    int supported = 0;
    if (!cuda || !nvenc)
        goto done;
    int (*init)(unsigned) = dlsym(cuda, "cuInit");
    int (*get_count)(int *) = dlsym(cuda, "cuDeviceGetCount");
    int (*get_device)(int *, int) = dlsym(cuda, "cuDeviceGet");
    int (*create_context)(void **, unsigned, int) = dlsym(cuda, "cuCtxCreate_v2");
    int (*destroy_context)(void *) = dlsym(cuda, "cuCtxDestroy_v2");
    int (*get_context)(void **) = dlsym(cuda, "cuCtxGetCurrent");
    int (*set_context)(void *) = dlsym(cuda, "cuCtxSetCurrent");
    NVENCSTATUS (*create_api)(NV_ENCODE_API_FUNCTION_LIST *) = dlsym(nvenc, "NvEncodeAPICreateInstance");
    if (!init || !get_count || !get_device || !create_context || !destroy_context || !get_context || !set_context ||
        !create_api)
        goto done;
    int count = 0;
    if (init(0) || get_count(&count) || count < 1 || count > 64)
        goto done;
    NV_ENCODE_API_FUNCTION_LIST api = {.version = NV_ENCODE_API_FUNCTION_LIST_VER};
    if (create_api(&api) != NV_ENC_SUCCESS)
        goto done;
    GUID wanted = codec == 1 ? NV_ENC_CODEC_HEVC_GUID : NV_ENC_CODEC_AV1_GUID;
    for (int i = 0; i < count && !supported; i++) {
        int device = 0;
        void *context = NULL, *previous = NULL, *session = NULL;
        if (get_device(&device, i) || get_context(&previous) || create_context(&context, 0, device))
            continue;
        NV_ENC_OPEN_ENCODE_SESSION_EX_PARAMS open = {.version = NV_ENC_OPEN_ENCODE_SESSION_EX_PARAMS_VER,
                                                     .deviceType = NV_ENC_DEVICE_TYPE_CUDA,
                                                     .device = context,
                                                     .apiVersion = NVENCAPI_VERSION};
        if (api.nvEncOpenEncodeSessionEx(&open, &session) == NV_ENC_SUCCESS) {
            GUID guids[32];
            uint32_t n = 0;
            if (api.nvEncGetEncodeGUIDs(session, guids, 32, &n) == NV_ENC_SUCCESS && n <= 32)
                for (unsigned j = 0; j < n; j++) {
                    if (memcmp(&wanted, &guids[j], sizeof(wanted)))
                        continue;
                    NV_ENC_BUFFER_FORMAT formats[64];
                    uint32_t f = 0;
                    if (api.nvEncGetInputFormats(session, wanted, formats, 64, &f) == NV_ENC_SUCCESS && f <= 64)
                        for (unsigned k = 0; k < f; k++)
                            if (formats[k] == NV_ENC_BUFFER_FORMAT_NV12)
                                supported = 1;
                }
            api.nvEncDestroyEncoder(session);
        }
        destroy_context(context);
        set_context(previous);
    }
done:
    if (nvenc)
        dlclose(nvenc);
    if (cuda)
        dlclose(cuda);
    return supported;
}

// The libva 2 ABI uses an opaque display and integer profile/entrypoint enums.
// Values below are the public VAProfileHEVCMain/VAProfileAV1Profile0 and
// VAEntrypointEncSlice/VAEntrypointEncSliceLP constants. No libva headers or
// link-time dependency are needed; only declarations are queried.
int bk_linux_vaapi_supports(const char *device, int codec) {
    void *va = dlopen("libva.so.2", RTLD_NOW | RTLD_LOCAL);
    void *drm = dlopen("libva-drm.so.2", RTLD_NOW | RTLD_LOCAL);
    int supported = 0, fd = -1, started = 0;
    void *display = NULL;
    if (!va || !drm)
        goto done;
    void *(*get_display)(int) = dlsym(drm, "vaGetDisplayDRM");
    int (*initialize)(void *, int *, int *) = dlsym(va, "vaInitialize");
    int (*terminate)(void *) = dlsym(va, "vaTerminate");
    int (*max_entries)(void *) = dlsym(va, "vaMaxNumEntrypoints");
    int (*query_entries)(void *, int, int *, int *) = dlsym(va, "vaQueryConfigEntrypoints");
    if (!get_display || !initialize || !terminate || !max_entries || !query_entries)
        goto done;
    fd = open(device, O_RDWR | O_CLOEXEC);
    if (fd < 0 || !(display = get_display(fd)))
        goto done;
    int major, minor;
    if (initialize(display, &major, &minor))
        goto done;
    started = 1;
    int count = 0, entries[64];
    int maximum = max_entries(display);
    if (maximum < 1 || maximum > 64)
        goto done;
    if (!query_entries(display, codec == 1 ? 17 : 32, entries, &count) && count >= 0 && count <= 64)
        for (int i = 0; i < count; i++)
            if (entries[i] == 6 || entries[i] == 8)
                supported = 1;
done:
    if (started) {
        int (*terminate)(void *) = dlsym(va, "vaTerminate");
        terminate(display);
    }
    if (fd >= 0)
        close(fd);
    if (drm)
        dlclose(drm);
    if (va)
        dlclose(va);
    return supported;
}
