#include <atomic>
#include <cassert>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <mutex>
#include <string>
#include <thread>
#include <vector>

using HRESULT = int32_t;
using UINT = unsigned int;
using ULONGLONG = unsigned long long;
using LPCVOID = const void *;
using SIZE_T = size_t;
using LPCSTR = const char *;
struct D3D_SHADER_MACRO { const char *Name; const char *Definition; };
struct ID3DInclude {};
constexpr HRESULT S_OK = 0, E_FAIL = -1;
constexpr UINT D3DCOMPILE_OPTIMIZATION_LEVEL1 = 0, D3DCOMPILE_ENABLE_STRICTNESS = 0x800;
constexpr UINT D3DCOMPILE_OPTIMIZATION_LEVEL3 = 0x8000;
#define WINAPI
#define SUCCEEDED(hr) ((hr) >= 0)
#define FAILED(hr) ((hr) < 0)
#define DROP(type, value) do { if (value) { type##_Release(value); (value) = NULL; } } while (0)
#define TRY(call) do { hr = (call); if (FAILED(hr)) goto done; } while (0)
using SRWLOCK = std::mutex;
#define SRWLOCK_INIT {}
void AcquireSRWLockExclusive(SRWLOCK *lock) { lock->lock(); }
void ReleaseSRWLockExclusive(SRWLOCK *lock) { lock->unlock(); }
ULONGLONG GetTickCount64() { return 1; }

struct ID3DBlob { std::string bytes; };
std::atomic<int> liveBlobs{0}, compilations{0};
void *ID3D10Blob_GetBufferPointer(ID3DBlob *blob) { return blob->bytes.data(); }
size_t ID3D10Blob_GetBufferSize(ID3DBlob *blob) { return blob->bytes.size(); }
void ID3D10Blob_Release(ID3DBlob *blob) {
    memset(blob->bytes.data(), 0xcc, blob->bytes.size());
    delete blob;
    liveBlobs--;
}
struct ID3D11ComputeShader {};
struct ID3D11Device {
    std::string expected;
    int creations = 0;
    bool rejectNext = false;
};
struct bk_jpeg_gpu {
    ID3D11Device *device;
    int debug;
    UINT compile_stage_index;
    ID3D11ComputeShader *select_entropy, *prepare_entropy, *work, *compare, *transform;
    ID3D11ComputeShader *entropy, *index, *scan_groups, *add_offsets, *pack;
};
ID3D11ComputeShader shaderObject;
HRESULT ID3D11Device_CreateComputeShader(ID3D11Device *device, const void *bytes, size_t size,
                                        void *, ID3D11ComputeShader **shader) {
    device->creations++;
    assert(std::string(static_cast<const char *>(bytes), size) == device->expected);
    if (device->rejectNext) { device->rejectNext = false; return E_FAIL; }
    *shader = &shaderObject;
    return S_OK;
}

#include "shader_cache.hpp"

bool failCompile = false;
HRESULT compileMock(LPCVOID source, SIZE_T size, LPCSTR, const D3D_SHADER_MACRO *macros, ID3DInclude *,
                    LPCSTR entry, LPCSTR target, UINT flags, UINT, ID3DBlob **code, ID3DBlob **messages) {
    compilations++;
    assert(std::string(target) == "cs_5_0");
#ifdef BK_JPEG_PRECOMPILED
    assert(flags == (D3DCOMPILE_OPTIMIZATION_LEVEL3 | D3DCOMPILE_ENABLE_STRICTNESS));
    assert(!macros);
#else
    assert(flags == (D3DCOMPILE_OPTIMIZATION_LEVEL1 | D3DCOMPILE_ENABLE_STRICTNESS));
    assert(macros && !strcmp(macros[0].Name, "ANYKVM_RUNTIME_COMPILE") && !strcmp(macros[0].Definition, "1"));
    assert(!macros[1].Name && !macros[1].Definition);
#endif
    if (failCompile) {
        *messages = new ID3DBlob{"warning before error\nanykvm: error X9999: fixture error"};
        liveBlobs++;
        return E_FAIL;
    }
    *code = new ID3DBlob{"DXBC:" + std::string(static_cast<const char *>(source), size) + ":" + entry};
    liveBlobs++;
    return S_OK;
}

// Each call represents a new device/encoder after closing the previous session.
void reopen(const std::string &source, const char *entry, bool debug = false, bool rejectCached = false) {
    ID3D11Device device{"DXBC:" + source + ":" + entry};
    device.rejectNext = rejectCached;
    bk_jpeg_gpu encoder{&device, debug ? 1 : 0, 0};
    ID3D11ComputeShader *shader = nullptr;
    char error[1024] = {};
    HRESULT hr = compile_stage(&encoder, compileMock, source.data(), source.size(), entry, &shader, error, sizeof(error));
    assert(SUCCEEDED(hr) && shader == &shaderObject && !error[0]);
    assert(device.creations == (rejectCached ? 2 : 1));
}

int main() {
    std::string original = "first source";
    reopen(original, "main", true);
    assert(compilations == 1);
    original[0] = 'X'; // Cache must own the original source as well as DXBC.
    reopen("first source", "main", true);
    assert(compilations == 1);
    reopen("first source", "encodeMCUs");
    assert(compilations == 2); // Entry points cannot share bytecode.
    reopen("other source", "main");
    assert(compilations == 3); // Changed source invalidates the entry.
    reopen("other source", "main", false, true);
    assert(compilations == 4); // Rejected bytecode gets one fresh compilation.
    reopen("other source", "main");
    assert(compilations == 4 && liveBlobs == 0);

    // Failed compilations never become hits, and release the global lock.
    failCompile = true;
    ID3D11Device device;
    bk_jpeg_gpu encoder{&device, 0, 0};
    ID3D11ComputeShader *shader = nullptr;
    char error[1024] = {};
    assert(FAILED(compile_stage(&encoder, compileMock, "retry", 5, "retry", &shader, error, sizeof(error))));
    assert(strstr(error, "anykvm: error X9999") && !strstr(error, "warning before error"));
    assert(liveBlobs == 0);
    failCompile = false;
    reopen("retry", "retry");
    assert(compilations == 6);

    // Concurrent reopen attempts must compile a missing entry only once.
    std::vector<std::thread> workers;
    for (int i = 0; i < 8; i++) workers.emplace_back([] { reopen("parallel", "parallel"); });
    for (auto &worker : workers) worker.join();
    assert(compilations == 7 && liveBlobs == 0);
    for (auto &slot : jpeg_shader_cache) { free(slot.source); free(slot.bytes); slot = {}; }

    // Startup compiles all stages without a device. The first connection and
    // every reconnect then create shaders entirely from the warmed bytecode.
    compilations = 0;
    bk_jpeg_gpu startup{};
#ifdef BK_JPEG_PRECOMPILED
    // Offline builds never call FXC, including on the first connection and
    // after a device rejects bytecode. Decoder shaders retain their own path.
    startup.debug = 1;
    assert(SUCCEEDED(compile_shaders(&startup, nullptr, "warmup", 6, error, sizeof(error))));
    assert(compilations == 0 && startup.compile_stage_index == 10 && startup.entropy == nullptr);
    for (int pass = 0; pass < 2; pass++) {
        ID3D11Device offlineDevice{"DXBC offline"};
        bk_jpeg_gpu offlineEncoder{&offlineDevice, 1, 0};
        assert(SUCCEEDED(compile_shaders(&offlineEncoder, nullptr, "warmup", 6, error, sizeof(error))));
        assert(offlineDevice.creations == 10 && compilations == 0 && liveBlobs == 0);
    }
    ID3D11Device rejected{"DXBC offline"};
    rejected.rejectNext = true;
    bk_jpeg_gpu rejectedEncoder{&rejected, 0, 0};
    assert(FAILED(compile_shaders(&rejectedEncoder, nullptr, "warmup", 6, error, sizeof(error))));
    assert(strstr(error, "CreateComputeShader failed") && compilations == 0);
    error[0] = 0;
    reopen("decoder source", "main");
    reopen("decoder source", "inverseDCT");
    assert(compilations == 2); // Decoder's main must not load the encoding shader.
#else
    assert(SUCCEEDED(compile_shaders(&startup, compileMock, "warmup", 6, error, sizeof(error))));
    assert(compilations == 10 && liveBlobs == 0);
    assert(startup.compile_stage_index == 10 && startup.entropy == nullptr && startup.transform == nullptr);
    for (int pass = 0; pass < 2; pass++) {
        for (auto &slot : jpeg_shader_cache) reopen("warmup", slot.entry);
        assert(compilations == 10 && liveBlobs == 0);
    }
#endif
    for (auto &slot : jpeg_shader_cache) { free(slot.source); free(slot.bytes); }
}
