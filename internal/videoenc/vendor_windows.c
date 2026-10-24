//go:build windows && cgo

#define BK_VENDOR_IMPLEMENTATION
#include "vendor_windows.h"
#include <d3d10.h>

struct bk_vendor_encoder {
    HANDLE thread, request, done;
    bk_vendor_options options;
    bk_vendor_timing timing;
    const bk_vendor_api *api;
    void *backend;
    const uint8_t *input;
    uintptr_t texture_handle;
    int rotation;
    uint8_t *output;
    size_t output_size;
    int stop, force, result;
    void *volatile phase;
    char error[512];
};

// Retain ownership while a driver call is outstanding. Timing out and freeing
// this encoder would let its worker access released textures and buffers.
static void bk_vendor_wait(bk_vendor_encoder *e, HANDLE event, const char *operation) {
    for (;;) {
        DWORD status = WaitForSingleObject(event, 5000);
        if (status == WAIT_OBJECT_0) return;
        const char *phase = InterlockedCompareExchangePointer(&e->phase, NULL, NULL);
        if (status == WAIT_TIMEOUT)
            fprintf(stderr, "videoenc: native %s still waiting after 5s; stage=%s\n",
                    operation, phase ? phase : "unknown");
        else {
            fprintf(stderr, "videoenc: native %s wait failed (error %lu); stage=%s\n",
                    operation, GetLastError(), phase ? phase : "unknown");
            Sleep(1000);
        }
        fflush(stderr);
    }
}

HMODULE bk_vendor_library(const wchar_t *name) {
    return LoadLibraryExW(name, NULL, LOAD_LIBRARY_SEARCH_SYSTEM32);
}

int bk_vendor_adapter_index(UINT vendor, uint64_t luid) {
    IDXGIFactory1 *factory = NULL;
    IDXGIAdapter1 *adapter = NULL;
    if (FAILED(CreateDXGIFactory1(&IID_IDXGIFactory1, (void **)&factory))) return -1;
    int found = -1;
    for (UINT i=0; IDXGIFactory1_EnumAdapters1(factory, i, &adapter)==S_OK; ++i) {
        DXGI_ADAPTER_DESC1 desc = {0};
        HRESULT hr = IDXGIAdapter1_GetDesc1(adapter, &desc);
        IDXGIAdapter1_Release(adapter);
        uint64_t actual = ((uint64_t)(uint32_t)desc.AdapterLuid.HighPart << 32) | desc.AdapterLuid.LowPart;
        if (SUCCEEDED(hr) && desc.VendorId==vendor && actual==luid && !(desc.Flags & DXGI_ADAPTER_FLAG_SOFTWARE)) {
            found = (int)i; break;
        }
    }
    IDXGIFactory1_Release(factory);
    return found;
}

HRESULT bk_vendor_device(UINT vendor, const bk_vendor_options *options, ID3D11Device **device, ID3D11DeviceContext **context) {
    HRESULT hr = E_FAIL;
    IDXGIFactory1 *factory = NULL;
    IDXGIAdapter1 *adapter = NULL;
    hr = CreateDXGIFactory1(&IID_IDXGIFactory1, (void **)&factory);
    if (FAILED(hr))
        goto done;
    for (UINT i = 0; IDXGIFactory1_EnumAdapters1(factory, i, &adapter) == S_OK; ++i) {
        DXGI_ADAPTER_DESC1 desc = {0};
        hr = IDXGIAdapter1_GetDesc1(adapter, &desc);
        uint64_t luid = ((uint64_t)(uint32_t)desc.AdapterLuid.HighPart << 32) | desc.AdapterLuid.LowPart;
        if (SUCCEEDED(hr) && desc.VendorId == vendor &&
            (!options->adapter_set || luid == options->luid) &&
            !(desc.Flags & DXGI_ADAPTER_FLAG_SOFTWARE)) {
            const D3D_FEATURE_LEVEL levels[] = {D3D_FEATURE_LEVEL_11_0};
            hr = D3D11CreateDevice((IDXGIAdapter *)adapter, D3D_DRIVER_TYPE_UNKNOWN, NULL,
                                   D3D11_CREATE_DEVICE_VIDEO_SUPPORT, levels, 1, D3D11_SDK_VERSION,
                                   device, NULL, context);
            IDXGIAdapter1_Release(adapter);
            adapter = NULL;
            if (SUCCEEDED(hr)) {
                // SDK runtimes can submit work from their own internal threads.
                ID3D10Multithread *multithread = NULL;
                hr = ID3D11Device_QueryInterface(*device, &IID_ID3D10Multithread,
                                                 (void **)&multithread);
                if (SUCCEEDED(hr)) {
                    ID3D10Multithread_SetMultithreadProtected(multithread, TRUE);
                    ID3D10Multithread_Release(multithread);
                    break;
                }
                ID3D11DeviceContext_Release(*context);
                *context = NULL;
                ID3D11Device_Release(*device);
                *device = NULL;
            }
        } else {
            hr = DXGI_ERROR_NOT_FOUND;
            IDXGIAdapter1_Release(adapter);
            adapter = NULL;
        }
    }
done:
    if (adapter)
        IDXGIAdapter1_Release(adapter);
    if (factory)
        IDXGIFactory1_Release(factory);
    return hr;
}

int bk_vendor_copy(const void *data, size_t size, uint8_t **output, size_t *length, char *error,
                   size_t capacity) {
    if (!data || !size || size > BK_VENDOR_LIMIT) {
        snprintf(error, capacity, "invalid native bitstream size %zu", size);
        return -1;
    }
    uint8_t *copy = malloc(size);
    if (!copy) {
        snprintf(error, capacity, "out of memory");
        return -1;
    }
    memcpy(copy, data, size);
    *output = copy;
    *length = size;
    return 0;
}

// Capability queries never initialize an encoder or submit pictures.
int bk_vendor_supports(int vendor, int codec) {
    if (vendor < 0 || vendor > 2 || (codec != 1 && codec != 2))
        return 0;
    HRESULT com = CoInitializeEx(NULL, COINIT_MULTITHREADED);
    if (FAILED(com) && com != RPC_E_CHANGED_MODE)
        return 0;
    int (*queries[])(int) = {bk_nvenc_supports, bk_amf_supports, bk_qsv_supports};
    int supported = queries[vendor](codec);
    if (SUCCEEDED(com))
        CoUninitialize();
    return supported;
}

// Encoding, D3D11 and COM releases stay on the owning MTA thread.
static DWORD WINAPI bk_vendor_worker(void *opaque) {
    bk_vendor_encoder *e = opaque;
    HRESULT hr = CoInitializeEx(NULL, COINIT_MULTITHREADED);
    e->result = -1;
    if (SUCCEEDED(hr)) {
        e->backend = e->api->open(&e->options, e->error, sizeof(e->error));
        if (e->backend) {
            e->result = 0;
            if (e->options.codec == 0) {
                e->result = -1;
                size_t size = (size_t)e->options.width * e->options.height * 3 / 2;
                uint8_t *frame = malloc(size);
                if (frame) {
                    memset(frame, 16, size * 2 / 3);
                    memset(frame + size * 2 / 3, 128, size / 3);
                    e->result =
                        e->api->encode(e->backend, frame, 1, &e->output, &e->output_size, e->error, sizeof(e->error));
                    free(frame);
                } else
                    snprintf(e->error, sizeof(e->error), "out of memory");
            }
        }
    } else
        snprintf(e->error, sizeof(e->error), "COM initialization failed: 0x%08lx", (unsigned long)hr);
    SetEvent(e->done);
    if (e->result == 0) {
        for (;;) {
            WaitForSingleObject(e->request, INFINITE);
            if (e->stop)
                break;
            if (e->texture_handle) {
                e->result = e->api->encode_texture
                                ? e->api->encode_texture(e->backend, e->texture_handle, e->rotation, e->force,
                                                         &e->output, &e->output_size, e->error, sizeof(e->error))
                                : 1;
            } else
                e->result = e->api->encode(e->backend, e->input, e->force, &e->output, &e->output_size, e->error,
                                           sizeof(e->error));
            SetEvent(e->done);
        }
    }
    if (e->backend) {
        bk_vendor_phase(&e->options, "closing encoder");
        e->api->close(e->backend);
    }
    if (SUCCEEDED(hr))
        CoUninitialize();
    return 0;
}

bk_vendor_encoder *bk_vendor_open(int vendor, int width, int height, int num, int den, int bitrate,
                                  int codec, int adapter_set, uint64_t luid, int timing, char *error, size_t capacity) {
    if (vendor < 0 || vendor > 2 || width < 2 || height < 2 || width > 8192 || height > 8192 ||
        width % 2 || height % 2 || num <= 0 || den <= 0 || bitrate <= 0 || bitrate > 1000000 ||
        (codec != 0 && codec != 1 && codec != 2)) {
        snprintf(error, capacity, "invalid native encoder options");
        return NULL;
    }
    bk_vendor_encoder *e = calloc(1, sizeof(*e));
    if (!e) {
        snprintf(error, capacity, "out of memory");
        return NULL;
    }
    const bk_vendor_api *apis[] = {&bk_nvenc_api, &bk_amf_api, &bk_qsv_api};
    e->api = apis[vendor];
    e->options = (bk_vendor_options){.width=width, .height=height, .num=num, .den=den,
        .bitrate=bitrate, .codec=codec, .adapter_set=adapter_set, .luid=luid};
    e->options.phase = &e->phase;
    bk_vendor_phase(&e->options, "opening encoder");
    if (timing) e->options.timing = &e->timing;
    e->request = CreateEventW(NULL, FALSE, FALSE, NULL);
    e->done = CreateEventW(NULL, FALSE, FALSE, NULL);
    if (e->request && e->done)
        e->thread = CreateThread(NULL, 0, bk_vendor_worker, e, 0, NULL);
    if (!e->thread) {
        snprintf(error, capacity, "cannot create native encoder worker (error %lu)",
                 GetLastError());
        bk_vendor_close(e);
        return NULL;
    }
    bk_vendor_wait(e, e->done, "open/encode");
    if (e->result != 0) {
        snprintf(error, capacity, "%s", e->error);
        bk_vendor_close(e);
        return NULL;
    }
    return e;
}

int bk_vendor_encode(bk_vendor_encoder *e, const uint8_t *input, size_t size, int force,
                     uint8_t **output, size_t *length, char *error, size_t capacity) {
    if (!input || size != (size_t)e->options.width * e->options.height * 3 / 2) {
        snprintf(error, capacity, "invalid native NV12 input size");
        return -1;
    }
    // Discard H.264 startup output before the first real picture.
    free(e->output);
    e->output = NULL;
    e->output_size = 0;
    e->input = input;
    memset(&e->timing, 0, sizeof(e->timing));
    e->force = force;
    SetEvent(e->request);
    bk_vendor_wait(e, e->done, "open/encode");
    e->input = NULL;
    if (e->result != 0) {
        snprintf(error, capacity, "%s", e->error);
        return -1;
    }
    *output = e->output;
    *length = e->output_size;
    e->output = NULL;
    e->output_size = 0;
    return 0;
}

int bk_vendor_encode_texture(bk_vendor_encoder *e, uintptr_t handle, int rotation, int force,
                             uint8_t **output, size_t *length, char *error, size_t capacity) {
    if (!e->api->encode_texture) {
        snprintf(error, capacity, "encoder does not accept shared D3D11 textures");
        return 1;
    }
    free(e->output);
    e->output = NULL;
    e->output_size = 0;
    e->texture_handle = handle;
    memset(&e->timing, 0, sizeof(e->timing));
    e->rotation = rotation;
    e->force = force;
    SetEvent(e->request);
    bk_vendor_wait(e, e->done, "open/encode");
    e->texture_handle = 0;
    if (e->result != 0) {
        snprintf(error, capacity, "%s", e->error);
        return e->result;
    }
    *output = e->output;
    *length = e->output_size;
    e->output = NULL;
    e->output_size = 0;
    return 0;
}

const uint8_t *bk_vendor_probe(bk_vendor_encoder *e, size_t *size) {
    *size = e->output_size;
    return e->output;
}

void bk_vendor_last_timing(bk_vendor_encoder *e, bk_vendor_timing *timing) {
    *timing = e->timing; // caller has waited for done and serialized requests
}
void bk_vendor_close(bk_vendor_encoder *e) {
    if (!e)
        return;
    if (e->thread) {
        e->stop = 1;
        SetEvent(e->request);
        bk_vendor_wait(e, e->thread, "close");
        CloseHandle(e->thread);
    }
    if (e->request)
        CloseHandle(e->request);
    if (e->done)
        CloseHandle(e->done);
    free(e->output);
    free(e);
}
