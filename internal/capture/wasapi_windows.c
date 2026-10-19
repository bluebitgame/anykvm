//go:build windows && cgo

#define COBJMACROS
#define WINVER 0x0a00
#include <windows.h>
#include <initguid.h>
#include <mmdeviceapi.h>
#include <audioclient.h>
#include <functiondiscoverykeys_devpkey.h>
#include <avrt.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include "wasapi_windows.h"
#include "wasapi_queue.h"
#define RELEASE(t, p)                                                                              \
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
// Keep at most 100ms of PCM; a slow receiver must not accumulate latency.
struct bk_wasapi {
    HANDLE thread, stop, ready, event;
    CRITICAL_SECTION lock;
    IMMDeviceEnumerator *enumerator;
    IMMDevice *device;
    IAudioClient *client;
    IAudioCaptureClient *capture;
    wchar_t *selector, *endpoint_id;
    int follow_default, available;
    HRESULT initial_result;
    char error[512];
    bk_pcm_queue queue;
};
static void wasapi_error(char *error, size_t capacity, const char *operation, HRESULT hr) {
    snprintf(error, capacity, "%s (HRESULT 0x%08lx)", operation, (unsigned long)hr);
}
static wchar_t *wide(const char *s) {
    int n = MultiByteToWideChar(CP_UTF8, MB_ERR_INVALID_CHARS, s, -1, NULL, 0);
    wchar_t *value = n > 0 ? calloc(n, sizeof(*value)) : NULL;
    if (value)
        MultiByteToWideChar(CP_UTF8, MB_ERR_INVALID_CHARS, s, -1, value, n);
    return value;
}
static char *utf8(const wchar_t *s) {
    int n = WideCharToMultiByte(CP_UTF8, 0, s, -1, NULL, 0, NULL, NULL);
    char *value = n > 0 ? malloc(n) : NULL;
    if (value)
        WideCharToMultiByte(CP_UTF8, 0, s, -1, value, n, NULL, NULL);
    return value;
}
static HRESULT friendly_name(IMMDevice *device, wchar_t **name) {
    IPropertyStore *store = NULL;
    PROPVARIANT value;
    PropVariantInit(&value);
    HRESULT hr = IMMDevice_OpenPropertyStore(device, STGM_READ, &store);
    if (SUCCEEDED(hr))
        hr = IPropertyStore_GetValue(store, &PKEY_Device_FriendlyName, &value);
    if (SUCCEEDED(hr) && value.vt == VT_LPWSTR && value.pwszVal) {
        size_t size = (wcslen(value.pwszVal) + 1) * sizeof(wchar_t);
        *name = malloc(size);
        if (*name)
            memcpy(*name, value.pwszVal, size);
        else
            hr = E_OUTOFMEMORY;
    } else if (SUCCEEDED(hr))
        hr = E_INVALIDARG;
    PropVariantClear(&value);
    RELEASE(IPropertyStore, store);
    return hr;
}
static HRESULT select_device(bk_wasapi *s, IMMDevice **result) {
    if (s->follow_default)
        return IMMDeviceEnumerator_GetDefaultAudioEndpoint(s->enumerator, eRender, eMultimedia,
                                                           result);
    if (!wcsncmp(s->selector, L"id:", 3))
        return IMMDeviceEnumerator_GetDevice(s->enumerator, s->selector + 3, result);
    IMMDeviceCollection *devices = NULL;
    IMMDevice *device = NULL;
    UINT count = 0;
    HRESULT hr = IMMDeviceEnumerator_EnumAudioEndpoints(s->enumerator, eRender, DEVICE_STATE_ACTIVE,
                                                        &devices);
    if (SUCCEEDED(hr))
        hr = IMMDeviceCollection_GetCount(devices, &count);
    if (FAILED(hr))
        goto done;
    hr = HRESULT_FROM_WIN32(ERROR_NOT_FOUND);
    for (UINT i = 0; i < count; i++) {
        if (FAILED(IMMDeviceCollection_Item(devices, i, &device)))
            continue;
        wchar_t *name = NULL;
        if (SUCCEEDED(friendly_name(device, &name)) && !wcscmp(name, s->selector)) {
            free(name);
            *result = device;
            device = NULL;
            hr = S_OK;
            break;
        }
        free(name);
        RELEASE(IMMDevice, device);
    }
done:
    RELEASE(IMMDevice, device);
    RELEASE(IMMDeviceCollection, devices);
    return hr;
}
static void stop_stream(bk_wasapi *s) {
    EnterCriticalSection(&s->lock);
    s->available = 0;
    bk_pcm_queue_reset(&s->queue);
    LeaveCriticalSection(&s->lock);
    if (s->client)
        IAudioClient_Stop(s->client);
    RELEASE(IAudioCaptureClient, s->capture);
    RELEASE(IAudioClient, s->client);
    RELEASE(IMMDevice, s->device);
    CoTaskMemFree(s->endpoint_id);
    s->endpoint_id = NULL;
}
static HRESULT start_stream(bk_wasapi *s) {
    HRESULT hr;
    TRY(select_device(s, &s->device));
    TRY(IMMDevice_GetId(s->device, &s->endpoint_id));
    TRY(IMMDevice_Activate(s->device, &IID_IAudioClient, CLSCTX_ALL, NULL, (void **)&s->client));
    WAVEFORMATEX pcm = {0};
    pcm.wFormatTag = WAVE_FORMAT_PCM;
    pcm.nChannels = 2;
    pcm.nSamplesPerSec = 48000;
    pcm.wBitsPerSample = 16;
    pcm.nBlockAlign = 4;
    pcm.nAvgBytesPerSec = 192000;
    // Shared-mode engine conversion handles float/integer mix formats, channel
    // matrixing and resampling without FFmpeg or a third-party audio DLL.
    DWORD flags = AUDCLNT_STREAMFLAGS_LOOPBACK | AUDCLNT_STREAMFLAGS_EVENTCALLBACK |
                  AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM | AUDCLNT_STREAMFLAGS_SRC_DEFAULT_QUALITY;
    TRY(IAudioClient_Initialize(s->client, AUDCLNT_SHAREMODE_SHARED, flags, 0, 0, &pcm, NULL));
    TRY(IAudioClient_SetEventHandle(s->client, s->event));
    TRY(IAudioClient_GetService(s->client, &IID_IAudioCaptureClient, (void **)&s->capture));
    TRY(IAudioClient_Start(s->client));
    EnterCriticalSection(&s->lock);
    bk_pcm_queue_reset(&s->queue);
    s->available = 1;
    LeaveCriticalSection(&s->lock);
done:
    if (FAILED(hr)) {
        wasapi_error(s->error, sizeof(s->error), "WASAPI loopback initialization failed", hr);
        stop_stream(s);
    }
    return hr;
}
static HRESULT queue_packet(bk_wasapi *s, const BYTE *data, UINT32 frames, DWORD flags) {
    EnterCriticalSection(&s->lock);
    int result = bk_pcm_queue_push(&s->queue, data, (size_t)frames * 4,
                                   (flags & AUDCLNT_BUFFERFLAGS_SILENT) != 0,
                                   (flags & AUDCLNT_BUFFERFLAGS_DATA_DISCONTINUITY) != 0);
    LeaveCriticalSection(&s->lock);
    return result < 0 ? E_INVALIDARG : S_OK;
}
static int default_changed(bk_wasapi *s) {
    if (!s->follow_default)
        return 0;
    IMMDevice *device = NULL;
    wchar_t *id = NULL;
    HRESULT hr =
        IMMDeviceEnumerator_GetDefaultAudioEndpoint(s->enumerator, eRender, eMultimedia, &device);
    if (SUCCEEDED(hr))
        hr = IMMDevice_GetId(device, &id);
    int changed = FAILED(hr) || !s->endpoint_id || wcscmp(id, s->endpoint_id);
    CoTaskMemFree(id);
    RELEASE(IMMDevice, device);
    return changed;
}
static DWORD WINAPI capture_worker(void *opaque) {
    bk_wasapi *s = opaque;
    HRESULT com = CoInitializeEx(NULL, COINIT_MULTITHREADED), hr = com;
    HANDLE mmcss = NULL;
    DWORD task_index = 0;
    if (SUCCEEDED(hr))
        hr = CoCreateInstance(&CLSID_MMDeviceEnumerator, NULL, CLSCTX_ALL, &IID_IMMDeviceEnumerator,
                              (void **)&s->enumerator);
    if (SUCCEEDED(hr))
        hr = start_stream(s);
    else
        wasapi_error(s->error, sizeof(s->error), "WASAPI COM initialization failed", hr);
    s->initial_result = hr;
    SetEvent(s->ready);
    if (FAILED(hr))
        goto done;
    mmcss = AvSetMmThreadCharacteristicsW(L"Audio", &task_index);
    HANDLE events[2] = {s->stop, s->event};
    ULONGLONG check = GetTickCount64();
    for (;;) {
        DWORD event = WaitForMultipleObjects(2, events, FALSE, 100);
        if (event == WAIT_OBJECT_0 || event == WAIT_FAILED)
            break;
        if (GetTickCount64() - check >= 1000) {
            check = GetTickCount64();
            if (default_changed(s))
                hr = AUDCLNT_E_DEVICE_INVALIDATED;
        }
        UINT32 pending = 0;
        if (SUCCEEDED(hr))
            hr = IAudioCaptureClient_GetNextPacketSize(s->capture, &pending);
        while (SUCCEEDED(hr) && pending) {
            BYTE *data = NULL;
            UINT32 frames = 0;
            DWORD flags = 0;
            hr = IAudioCaptureClient_GetBuffer(s->capture, &data, &frames, &flags, NULL, NULL);
            if (FAILED(hr))
                break;
            HRESULT queued = frames ? queue_packet(s, data, frames, flags) : S_OK;
            hr = IAudioCaptureClient_ReleaseBuffer(s->capture, frames);
            if (SUCCEEDED(hr))
                hr = queued;
            if (SUCCEEDED(hr))
                hr = IAudioCaptureClient_GetNextPacketSize(s->capture, &pending);
            if (WaitForSingleObject(s->stop, 0) == WAIT_OBJECT_0)
                goto done;
        }
        if (FAILED(hr)) {
            stop_stream(s);
            EnterCriticalSection(&s->lock);
            bk_pcm_queue_reset(&s->queue);
            LeaveCriticalSection(&s->lock);
            do {
                if (WaitForSingleObject(s->stop, 250) == WAIT_OBJECT_0)
                    goto done;
                hr = start_stream(s);
            } while (FAILED(hr));
            check = GetTickCount64();
        }
    }
done:
    stop_stream(s);
    RELEASE(IMMDeviceEnumerator, s->enumerator);
    if (mmcss)
        AvRevertMmThreadCharacteristics(mmcss);
    if (SUCCEEDED(com))
        CoUninitialize();
    return 0;
}

bk_wasapi *bk_wasapi_open(const char *selector, char *error, size_t capacity) {
    bk_wasapi *s = calloc(1, sizeof(*s));
    if (!s) {
        wasapi_error(error, capacity, "allocation failed", E_OUTOFMEMORY);
        return NULL;
    }
    InitializeCriticalSection(&s->lock);
    s->follow_default = !*selector;
    s->selector = wide(selector);
    s->stop = CreateEventW(NULL, TRUE, FALSE, NULL);
    s->ready = CreateEventW(NULL, FALSE, FALSE, NULL);
    s->event = CreateEventW(NULL, FALSE, FALSE, NULL);
    if (s->selector && s->stop && s->ready && s->event)
        s->thread = CreateThread(NULL, 0, capture_worker, s, 0, NULL);
    if (!s->thread) {
        wasapi_error(error, capacity, "WASAPI worker creation failed",
                     HRESULT_FROM_WIN32(GetLastError()));
        bk_wasapi_close(s);
        return NULL;
    }
    WaitForSingleObject(s->ready, INFINITE);
    if (FAILED(s->initial_result)) {
        snprintf(error, capacity, "%s", s->error);
        bk_wasapi_close(s);
        return NULL;
    }
    return s;
}
size_t bk_wasapi_read(bk_wasapi *s, uint8_t *data, size_t capacity, uint64_t *epoch) {
    EnterCriticalSection(&s->lock);
    size_t size = bk_pcm_queue_read(&s->queue, data, capacity, epoch);
    LeaveCriticalSection(&s->lock);
    return size;
}
int bk_wasapi_available(bk_wasapi *s) {
    EnterCriticalSection(&s->lock);
    int available = s->available;
    LeaveCriticalSection(&s->lock);
    return available;
}
void bk_wasapi_close(bk_wasapi *s) {
    if (!s)
        return;
    if (s->thread) {
        SetEvent(s->stop);
        WaitForSingleObject(s->thread, INFINITE);
        CloseHandle(s->thread);
    }
    if (s->event)
        CloseHandle(s->event);
    if (s->ready)
        CloseHandle(s->ready);
    if (s->stop)
        CloseHandle(s->stop);
    free(s->selector);
    DeleteCriticalSection(&s->lock);
    free(s);
}
char *bk_wasapi_devices(char *error, size_t capacity) {
    IMMDeviceEnumerator *enumerator = NULL;
    IMMDeviceCollection *devices = NULL;
    IMMDevice *device = NULL;
    UINT count = 0;
    char *result = NULL;
    size_t used = 0;
    HRESULT com = CoInitializeEx(NULL, COINIT_MULTITHREADED), hr = com;
    if (hr == RPC_E_CHANGED_MODE)
        hr = S_OK;
    TRY(hr);
    TRY(CoCreateInstance(&CLSID_MMDeviceEnumerator, NULL, CLSCTX_ALL, &IID_IMMDeviceEnumerator,
                         (void **)&enumerator));
    TRY(IMMDeviceEnumerator_EnumAudioEndpoints(enumerator, eRender, DEVICE_STATE_ACTIVE, &devices));
    TRY(IMMDeviceCollection_GetCount(devices, &count));
    result = calloc(1, 1);
    if (!result) {
        hr = E_OUTOFMEMORY;
        goto done;
    }
    for (UINT i = 0; i < count; i++) {
        wchar_t *name = NULL;
        TRY(IMMDeviceCollection_Item(devices, i, &device));
        hr = friendly_name(device, &name);
        RELEASE(IMMDevice, device);
        if (FAILED(hr))
            goto done;
        char *value = utf8(name);
        free(name);
        if (!value) {
            hr = E_OUTOFMEMORY;
            goto done;
        }
        size_t length = strlen(value);
        char *next = realloc(result, used + length + 9);
        if (!next) {
            free(value);
            hr = E_OUTOFMEMORY;
            goto done;
        }
        result = next;
        memcpy(result + used, "wasapi:", 7);
        used += 7;
        memcpy(result + used, value, length);
        used += length;
        result[used++] = '\n';
        result[used] = 0;
        free(value);
    }
done:
    RELEASE(IMMDevice, device);
    RELEASE(IMMDeviceCollection, devices);
    RELEASE(IMMDeviceEnumerator, enumerator);
    if (SUCCEEDED(com))
        CoUninitialize();
    if (FAILED(hr)) {
        free(result);
        result = NULL;
        wasapi_error(error, capacity, "WASAPI playback enumeration failed", hr);
    }
    return result;
}
