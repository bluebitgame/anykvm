#include <cassert>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <string>

using HRESULT = int32_t;
constexpr HRESULT S_OK = 0, E_FAIL = -1;
#define FAILED(hr) ((hr) < 0)
struct ID3D11ComputeShader {};
struct ID3D11Device {
    std::string expected;
    int creations = 0;
    bool reject = false;
};
ID3D11ComputeShader shaderObject;
HRESULT ID3D11Device_CreateComputeShader(ID3D11Device *device, const void *bytes, size_t size,
                                        void *, ID3D11ComputeShader **shader) {
    assert(std::string(static_cast<const char *>(bytes), size) == device->expected);
    device->creations++;
    if (device->reject) return E_FAIL;
    *shader = &shaderObject;
    return S_OK;
}

#include "progressive_precompiled.hpp"

int main() {
    ID3D11Device device;
    char error[1024] = {};
    for (int pass = 0; pass < 2; pass++) {
        for (const char *entry : {"main", "encodeTiles", "indexSegments", "packSegments"}) {
            device.expected = "DXBC:encoder:" + std::string(entry);
            ID3D11ComputeShader *shader = nullptr;
            assert(progressive_load_precompiled(&device, 0, entry, &shader, error, sizeof(error)) == S_OK);
            assert(shader == &shaderObject && !error[0]);
        }
        device.expected = "DXBC:surface:main";
        ID3D11ComputeShader *shader = nullptr;
        assert(progressive_load_precompiled(&device, 1, "main", &shader, error, sizeof(error)) == S_OK);
        assert(shader == &shaderObject && !error[0]);
    }
    assert(device.creations == 10);
    ID3D11ComputeShader *shader = nullptr;
    assert(FAILED(progressive_load_precompiled(&device, 1, "encodeTiles", &shader, error, sizeof(error))));
    assert(strstr(error, "surface/encodeTiles precompiled DXBC missing") && device.creations == 10);
    device.reject = true;
    assert(FAILED(progressive_load_precompiled(&device, 1, "main", &shader, error, sizeof(error))));
    assert(shader == nullptr && device.creations == 11 && strstr(error, "surface/main CreateComputeShader failed"));
}
