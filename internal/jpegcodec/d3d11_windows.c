//go:build windows && cgo

#define COBJMACROS
#include "d3d11_windows.h"
#include "../gpuconv/jpeg_layout.h"
#include <windows.h>
#include <d3d11.h>
#include <d3dcompiler.h>
#include <dxgi1_2.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

typedef struct {
    ID3D11Buffer *buffer;
    ID3D11UnorderedAccessView *view;
    UINT bytes;
} jpeg_buffer;

struct bk_jpeg_gpu {
    ID3D11Device *device;
    ID3D11DeviceContext *context;
    ID3D11ComputeShader *transform, *entropy, *index, *pack, *compare, *work;
    ID3D11ComputeShader *scan_groups, *add_offsets, *select_entropy, *prepare_entropy;
    ID3D11Buffer *parameters, *header, *length_readback, *jpeg_readback;
    ID3D11ShaderResourceView *header_view, *source_view;
    ID3D11Texture2D *texture;
    IDXGIKeyedMutex *mutex;
    jpeg_buffer coefficients, segments, segment_info, output, arguments, previous_pixels, dirty_tiles;
    bk_jpeg_parameters cached_parameters;
    int cache_valid;
    uintptr_t handle;
    UINT header_capacity, readback_capacity;
};

#define DROP(type, value) do { if (value) { type##_Release(value); (value) = NULL; } } while (0)
#define TRY(call) do { hr = (call); if (FAILED(hr)) goto done; } while (0)

static void buffer_free(jpeg_buffer *buffer) {
    DROP(ID3D11UnorderedAccessView, buffer->view);
    DROP(ID3D11Buffer, buffer->buffer);
    buffer->bytes = 0;
}

void bk_jpeg_gpu_close(bk_jpeg_gpu *e) {
    if (!e) return;
    if (e->context) ID3D11DeviceContext_ClearState(e->context);
    buffer_free(&e->coefficients);
    buffer_free(&e->segments);
    buffer_free(&e->segment_info);
    buffer_free(&e->output);
    buffer_free(&e->arguments);
    buffer_free(&e->previous_pixels);
    buffer_free(&e->dirty_tiles);
    DROP(ID3D11ShaderResourceView, e->header_view);
    DROP(ID3D11ShaderResourceView, e->source_view);
    DROP(IDXGIKeyedMutex, e->mutex);
    DROP(ID3D11Texture2D, e->texture);
    DROP(ID3D11Buffer, e->parameters);
    DROP(ID3D11Buffer, e->header);
    DROP(ID3D11Buffer, e->length_readback);
    DROP(ID3D11Buffer, e->jpeg_readback);
    DROP(ID3D11ComputeShader, e->transform);
    DROP(ID3D11ComputeShader, e->compare);
    DROP(ID3D11ComputeShader, e->work);
    DROP(ID3D11ComputeShader, e->select_entropy);
    DROP(ID3D11ComputeShader, e->prepare_entropy);
    DROP(ID3D11ComputeShader, e->entropy);
    DROP(ID3D11ComputeShader, e->index);
    DROP(ID3D11ComputeShader, e->scan_groups);
    DROP(ID3D11ComputeShader, e->add_offsets);
    DROP(ID3D11ComputeShader, e->pack);
    DROP(ID3D11DeviceContext, e->context);
    DROP(ID3D11Device, e->device);
    free(e);
}

typedef HRESULT (WINAPI *compile_fn)(LPCVOID, SIZE_T, LPCSTR, const D3D_SHADER_MACRO *,
    ID3DInclude *, LPCSTR, LPCSTR, UINT, UINT, ID3DBlob **, ID3DBlob **);

static HRESULT compile_stage(bk_jpeg_gpu *e, compile_fn compile, const char *source, size_t size,
                              const char *entry, ID3D11ComputeShader **shader, char *error, size_t capacity) {
    ID3DBlob *code = NULL, *messages = NULL;
    HRESULT hr = compile(source, size, "anykvm-jpeg", NULL, NULL, entry, "cs_5_0",
        D3DCOMPILE_OPTIMIZATION_LEVEL3 | D3DCOMPILE_ENABLE_STRICTNESS, 0, &code, &messages);
    if (SUCCEEDED(hr))
        hr = ID3D11Device_CreateComputeShader(e->device, ID3D10Blob_GetBufferPointer(code),
                                              ID3D10Blob_GetBufferSize(code), NULL, shader);
    if (FAILED(hr) && messages)
        snprintf(error, capacity, "JPEG %s: %.*s", entry, (int)ID3D10Blob_GetBufferSize(messages),
                 (char *)ID3D10Blob_GetBufferPointer(messages));
    DROP(ID3D10Blob, code);
    DROP(ID3D10Blob, messages);
    return hr;
}

bk_jpeg_gpu *bk_jpeg_gpu_open(uint64_t luid, const char *source, size_t size, char *error, size_t capacity) {
    bk_jpeg_gpu *e = calloc(1, sizeof(*e));
    if (!e) { snprintf(error, capacity, "GPU JPEG allocation failed"); return NULL; }
    HRESULT hr = E_FAIL;
    IDXGIFactory1 *factory = NULL;
    IDXGIAdapter1 *adapter = NULL;
    HMODULE compiler = NULL;
    TRY(CreateDXGIFactory1(&IID_IDXGIFactory1, (void **)&factory));
    for (UINT i = 0;; i++) {
        TRY(IDXGIFactory1_EnumAdapters1(factory, i, &adapter));
        DXGI_ADAPTER_DESC1 desc;
        TRY(IDXGIAdapter1_GetDesc1(adapter, &desc));
        uint64_t id = (uint64_t)(uint32_t)desc.AdapterLuid.LowPart |
                      ((uint64_t)(uint32_t)desc.AdapterLuid.HighPart << 32);
        if (id == luid && !(desc.Flags & DXGI_ADAPTER_FLAG_SOFTWARE)) break;
        DROP(IDXGIAdapter1, adapter);
    }
    D3D_FEATURE_LEVEL level = D3D_FEATURE_LEVEL_11_0;
    TRY(D3D11CreateDevice((IDXGIAdapter *)adapter, D3D_DRIVER_TYPE_UNKNOWN, NULL,
        D3D11_CREATE_DEVICE_BGRA_SUPPORT, &level, 1, D3D11_SDK_VERSION, &e->device, NULL, &e->context));
    compiler = LoadLibraryExW(L"d3dcompiler_47.dll", NULL, LOAD_LIBRARY_SEARCH_SYSTEM32);
    if (!compiler) { hr = HRESULT_FROM_WIN32(GetLastError()); goto done; }
    compile_fn compile = (compile_fn)(void *)GetProcAddress(compiler, "D3DCompile");
    if (!compile) { hr = E_NOINTERFACE; goto done; }
    TRY(compile_stage(e, compile, source, size, "selectEntropy", &e->select_entropy, error, capacity));
    TRY(compile_stage(e, compile, source, size, "prepareEntropy", &e->prepare_entropy, error, capacity));
    TRY(compile_stage(e, compile, source, size, "prepareWork", &e->work, error, capacity));
    TRY(compile_stage(e, compile, source, size, "compareTiles", &e->compare, error, capacity));
    TRY(compile_stage(e, compile, source, size, "main", &e->transform, error, capacity));
    TRY(compile_stage(e, compile, source, size, "encodeMCUs", &e->entropy, error, capacity));
    TRY(compile_stage(e, compile, source, size, "indexSegments", &e->index, error, capacity));
    TRY(compile_stage(e, compile, source, size, "scanSegmentGroups", &e->scan_groups, error, capacity));
    TRY(compile_stage(e, compile, source, size, "addSegmentOffsets", &e->add_offsets, error, capacity));
    TRY(compile_stage(e, compile, source, size, "packJPEG", &e->pack, error, capacity));
    D3D11_BUFFER_DESC desc = {0};
    desc.ByteWidth = sizeof(bk_jpeg_parameters);
    desc.Usage = D3D11_USAGE_DEFAULT;
    desc.BindFlags = D3D11_BIND_CONSTANT_BUFFER;
    TRY(ID3D11Device_CreateBuffer(e->device, &desc, NULL, &e->parameters));
    desc.ByteWidth = 4;
    desc.Usage = D3D11_USAGE_STAGING;
    desc.BindFlags = 0;
    desc.CPUAccessFlags = D3D11_CPU_ACCESS_READ;
    TRY(ID3D11Device_CreateBuffer(e->device, &desc, NULL, &e->length_readback));
done:
    if (FAILED(hr) && !error[0]) snprintf(error, capacity, "D3D11 JPEG initialization failed (0x%08lx)", (unsigned long)hr);
    if (compiler) FreeLibrary(compiler);
    DROP(IDXGIAdapter1, adapter);
    DROP(IDXGIFactory1, factory);
    if (FAILED(hr)) { bk_jpeg_gpu_close(e); return NULL; }
    return e;
}

static HRESULT open_shared_texture(bk_jpeg_gpu *e, uintptr_t handle) {
    if (e->handle == handle && e->texture) return S_OK;
    HRESULT hr;
    DROP(ID3D11ShaderResourceView, e->source_view);
    DROP(IDXGIKeyedMutex, e->mutex);
    DROP(ID3D11Texture2D, e->texture);
    e->handle = 0;
    TRY(ID3D11Device_OpenSharedResource(e->device, (HANDLE)handle, &IID_ID3D11Texture2D, (void **)&e->texture));
    TRY(ID3D11Texture2D_QueryInterface(e->texture, &IID_IDXGIKeyedMutex, (void **)&e->mutex));
    D3D11_TEXTURE2D_DESC desc;
    ID3D11Texture2D_GetDesc(e->texture, &desc);
    if ((desc.Format != DXGI_FORMAT_B8G8R8A8_UNORM && desc.Format != DXGI_FORMAT_R8G8B8A8_UNORM) ||
        desc.ArraySize != 1 || desc.SampleDesc.Count != 1) { hr = E_INVALIDARG; goto done; }
    TRY(ID3D11Device_CreateShaderResourceView(e->device, (ID3D11Resource *)e->texture, NULL, &e->source_view));
    e->handle = handle;
done:
    return hr;
}

static HRESULT gpu_buffer_new(bk_jpeg_gpu *e, jpeg_buffer *buffer, UINT bytes, int raw, int indirect) {
    if (buffer->bytes == bytes) return S_OK;
    buffer_free(buffer);
    HRESULT hr;
    D3D11_BUFFER_DESC desc = {0};
    desc.ByteWidth = bytes;
    desc.Usage = D3D11_USAGE_DEFAULT;
    desc.BindFlags = D3D11_BIND_UNORDERED_ACCESS;
    desc.MiscFlags = raw ? D3D11_RESOURCE_MISC_BUFFER_ALLOW_RAW_VIEWS : D3D11_RESOURCE_MISC_BUFFER_STRUCTURED;
    if (indirect) desc.MiscFlags |= D3D11_RESOURCE_MISC_DRAWINDIRECT_ARGS;
    desc.StructureByteStride = raw ? 0 : 4;
    TRY(ID3D11Device_CreateBuffer(e->device, &desc, NULL, &buffer->buffer));
    D3D11_UNORDERED_ACCESS_VIEW_DESC view = {0};
    view.Format = raw ? DXGI_FORMAT_R32_TYPELESS : DXGI_FORMAT_UNKNOWN;
    view.ViewDimension = D3D11_UAV_DIMENSION_BUFFER;
    view.Buffer.NumElements = bytes / 4;
    view.Buffer.Flags = raw ? D3D11_BUFFER_UAV_FLAG_RAW : 0;
    TRY(ID3D11Device_CreateUnorderedAccessView(e->device, (ID3D11Resource *)buffer->buffer, &view, &buffer->view));
    buffer->bytes = bytes;
done:
    return hr;
}

static HRESULT prepare_frame(bk_jpeg_gpu *e, const bk_jpeg_parameters *parameters, const uint8_t *header) {
    HRESULT hr;
    UINT count = parameters->mcu_count;
    TRY(gpu_buffer_new(e, &e->previous_pixels, parameters->output_width * parameters->output_height * 4, 1, 0));
    TRY(gpu_buffer_new(e, &e->dirty_tiles, bk_jpeg_work_words(((parameters->output_width+63)/64)*((parameters->output_height+63)/64),count)*4, 0, 0));
    UINT output_capacity = (parameters->header_bytes + count * (BK_JPEG_SEGMENT_CAPACITY + (parameters->tile_state[2] ? BK_JPEG_REGION_HEADER_BYTES : 0)) + 2 + 3) & ~3u;
    TRY(gpu_buffer_new(e, &e->coefficients, count * BK_JPEG_MCU_WORDS * sizeof(uint32_t), 0, 0));
    TRY(gpu_buffer_new(e, &e->segments, count * BK_JPEG_SEGMENT_CAPACITY, 1, 0));
    TRY(gpu_buffer_new(e, &e->segment_info, bk_jpeg_segment_info_words(count) * 4, 0, 0));
    TRY(gpu_buffer_new(e, &e->output, output_capacity, 1, 0));
    TRY(gpu_buffer_new(e, &e->arguments, BK_JPEG_ARGUMENT_BYTES, 1, 1));
    UINT header_capacity = (parameters->header_bytes + 3) & ~3u;
    if (e->header_capacity != header_capacity) {
        DROP(ID3D11ShaderResourceView, e->header_view);
        DROP(ID3D11Buffer, e->header);
        e->header_capacity = 0;
        D3D11_BUFFER_DESC desc = {0};
        desc.ByteWidth = header_capacity;
        desc.Usage = D3D11_USAGE_DEFAULT;
        desc.BindFlags = D3D11_BIND_SHADER_RESOURCE;
        desc.MiscFlags = D3D11_RESOURCE_MISC_BUFFER_ALLOW_RAW_VIEWS;
        TRY(ID3D11Device_CreateBuffer(e->device, &desc, NULL, &e->header));
        D3D11_SHADER_RESOURCE_VIEW_DESC view = {0};
        view.Format = DXGI_FORMAT_R32_TYPELESS;
        view.ViewDimension = D3D11_SRV_DIMENSION_BUFFEREX;
        view.BufferEx.NumElements = header_capacity / 4;
        view.BufferEx.Flags = D3D11_BUFFEREX_SRV_FLAG_RAW;
        TRY(ID3D11Device_CreateShaderResourceView(e->device, (ID3D11Resource *)e->header, &view, &e->header_view));
        e->header_capacity = header_capacity;
    }
    uint8_t padded_header[1024] = {0};
    memcpy(padded_header, header, parameters->header_bytes);
    ID3D11DeviceContext_UpdateSubresource(e->context, (ID3D11Resource *)e->header, 0, NULL, padded_header, 0, 0);
    ID3D11DeviceContext_UpdateSubresource(e->context, (ID3D11Resource *)e->parameters, 0, NULL, parameters, 0, 0);
done:
    return hr;
}

static void bind_frame(bk_jpeg_gpu *e) {
    ID3D11ShaderResourceView *sources[] = {e->source_view, e->header_view};
    ID3D11UnorderedAccessView *outputs[] = {e->coefficients.view, e->segments.view, e->segment_info.view,
                                           e->output.view, e->arguments.view, e->previous_pixels.view, e->dirty_tiles.view};
    ID3D11DeviceContext_CSSetConstantBuffers(e->context, 0, 1, &e->parameters);
    ID3D11DeviceContext_CSSetShaderResources(e->context, 0, 2, sources);
    ID3D11DeviceContext_CSSetUnorderedAccessViews(e->context, 0, 7, outputs, NULL);
}

static void encode_scan_work(bk_jpeg_gpu *e, UINT count, int compact) {
    if (compact) {
        ID3D11DeviceContext_CSSetShader(e->context, e->select_entropy, NULL, 0);
        ID3D11DeviceContext_DispatchIndirect(e->context, e->arguments.buffer, 32);
        ID3D11UnorderedAccessView *arguments = e->arguments.view;
        ID3D11DeviceContext_CSSetUnorderedAccessViews(e->context, 4, 1, &arguments, NULL);
        ID3D11DeviceContext_CSSetShader(e->context, e->prepare_entropy, NULL, 0);
        ID3D11DeviceContext_Dispatch(e->context, 1, 1, 1);
        ID3D11UnorderedAccessView *empty = NULL;
        ID3D11DeviceContext_CSSetUnorderedAccessViews(e->context, 4, 1, &empty, NULL);
    }
    ID3D11DeviceContext_CSSetShader(e->context, e->entropy, NULL, 0);
    if (compact) ID3D11DeviceContext_DispatchIndirect(e->context, e->arguments.buffer, 32);
    else ID3D11DeviceContext_Dispatch(e->context, (count + 63) / 64, 1, 1);
    ID3D11DeviceContext_CSSetShader(e->context, e->index, NULL, 0);
    if (compact) ID3D11DeviceContext_DispatchIndirect(e->context, e->arguments.buffer, 44);
    else ID3D11DeviceContext_Dispatch(e->context, bk_jpeg_scan_groups(count), 1, 1);
    ID3D11UnorderedAccessView *arguments = e->arguments.view;
    ID3D11DeviceContext_CSSetUnorderedAccessViews(e->context, 4, 1, &arguments, NULL);
    ID3D11DeviceContext_CSSetShader(e->context, e->scan_groups, NULL, 0);
    ID3D11DeviceContext_Dispatch(e->context, 1, 1, 1);
    // Indirect arguments cannot remain bound for UAV access in D3D11.
    ID3D11UnorderedAccessView *empty = NULL;
    ID3D11DeviceContext_CSSetUnorderedAccessViews(e->context, 4, 1, &empty, NULL);
    ID3D11DeviceContext_CSSetShader(e->context, e->add_offsets, NULL, 0);
    if (compact) ID3D11DeviceContext_DispatchIndirect(e->context, e->arguments.buffer, 44);
    else ID3D11DeviceContext_Dispatch(e->context, bk_jpeg_scan_groups(count), 1, 1);
    ID3D11DeviceContext_CSSetShader(e->context, e->pack, NULL, 0);
    ID3D11DeviceContext_DispatchIndirect(e->context, e->arguments.buffer, 0);
    ID3D11DeviceContext_ClearState(e->context);
}

static void encode_scan(bk_jpeg_gpu *e, UINT count) { encode_scan_work(e,count,0); }

// Read four bytes of length metadata, then only the compressed output range.
static HRESULT read_payload(bk_jpeg_gpu *e, UINT region_mcus, uint8_t **jpeg, size_t *jpeg_bytes) {
    HRESULT hr;
    D3D11_BOX box = {.left = 12, .right = 16, .bottom = 1, .back = 1};
    ID3D11DeviceContext_CopySubresourceRegion(e->context, (ID3D11Resource *)e->length_readback, 0, 0, 0, 0,
                                              (ID3D11Resource *)e->arguments.buffer, 0, &box);
    D3D11_MAPPED_SUBRESOURCE map = {0};
    TRY(ID3D11DeviceContext_Map(e->context, (ID3D11Resource *)e->length_readback, 0, D3D11_MAP_READ, 0, &map));
    UINT bytes;
    memcpy(&bytes, map.pData, 4);
    ID3D11DeviceContext_Unmap(e->context, (ID3D11Resource *)e->length_readback, 0);
    if (bytes < (region_mcus ? 4u : 2u) || bytes > e->output.bytes) { hr = E_FAIL; goto done; }
    // An empty compact packet contains only the known MCU count. Synthesize it
    // locally after reading length metadata, avoiding another GPU copy/map.
    if (region_mcus && bytes == 4) {
        *jpeg = malloc(4);
        if (!*jpeg) { hr = E_OUTOFMEMORY; goto done; }
        memcpy(*jpeg, &region_mcus, 4);
        *jpeg_bytes = 4;
        goto done;
    }
    UINT aligned_bytes = (bytes + 3) & ~3u;
    if (e->readback_capacity < aligned_bytes) {
        DROP(ID3D11Buffer, e->jpeg_readback);
        e->readback_capacity = 0;
        D3D11_BUFFER_DESC desc = {0};
        desc.ByteWidth = aligned_bytes;
        desc.Usage = D3D11_USAGE_STAGING;
        desc.CPUAccessFlags = D3D11_CPU_ACCESS_READ;
        TRY(ID3D11Device_CreateBuffer(e->device, &desc, NULL, &e->jpeg_readback));
        e->readback_capacity = aligned_bytes;
    }
    box.left = 0;
    box.right = aligned_bytes;
    ID3D11DeviceContext_CopySubresourceRegion(e->context, (ID3D11Resource *)e->jpeg_readback, 0, 0, 0, 0,
                                              (ID3D11Resource *)e->output.buffer, 0, &box);
    TRY(ID3D11DeviceContext_Map(e->context, (ID3D11Resource *)e->jpeg_readback, 0, D3D11_MAP_READ, 0, &map));
    *jpeg = malloc(bytes);
    if (*jpeg) memcpy(*jpeg, map.pData, bytes);
    ID3D11DeviceContext_Unmap(e->context, (ID3D11Resource *)e->jpeg_readback, 0);
    if (!*jpeg) { hr = E_OUTOFMEMORY; goto done; }
    *jpeg_bytes = bytes;
done:
    return hr;
}

static HRESULT read_jpeg(bk_jpeg_gpu *e, uint8_t **jpeg, size_t *bytes) {
    return read_payload(e, 0, jpeg, bytes);
}

int bk_jpeg_gpu_encode_damage(bk_jpeg_gpu *e, uintptr_t handle, int rotation, int width, int height,
                       const uint32_t *quantization, const uint32_t *huffman, const uint8_t *header,
                       size_t header_bytes, int regions, const bk_jpeg_damage *damage, uint8_t **jpeg, size_t *jpeg_bytes, char *error, size_t capacity) {
    HRESULT hr = E_INVALIDARG;
    int locked = 0;
    if (jpeg) *jpeg = NULL;
    if (jpeg_bytes) *jpeg_bytes = 0;
    if (!e || !handle || !quantization || !huffman || !header || !jpeg || !jpeg_bytes || header_bytes > 1024 ||
        header_bytes < 2 || width < 1 || height < 1 || width > 8192 || height > 8192 ||
        (rotation != 0 && rotation != 90 && rotation != 180 && rotation != 270)) goto done;
    UINT columns = (width + 15) / 16, rows = (height + 15) / 16;
    if ((uint64_t)columns * rows > BK_JPEG_STORAGE_LIMIT / BK_JPEG_SEGMENT_CAPACITY) goto done;
    TRY(open_shared_texture(e, handle));
    D3D11_TEXTURE2D_DESC desc;
    ID3D11Texture2D_GetDesc(e->texture, &desc);
    bk_jpeg_parameters parameters = {
        .source_width = desc.Width, .source_height = desc.Height,
        .output_width = width, .output_height = height, .rotation_degrees = rotation,
        .mcu_columns = columns, .mcu_count = columns * rows, .header_bytes = (UINT)header_bytes,
        .crop_size = {desc.Width, desc.Height}
    };
    bk_jpeg_prepare_quantization(&parameters, quantization);
    memcpy(parameters.huffman, huffman, sizeof(parameters.huffman));
    parameters.tile_state[0] = !e->cache_valid || (regions != 0) != e->cached_parameters.tile_state[2] || memcmp(&parameters, &e->cached_parameters, offsetof(bk_jpeg_parameters, tile_state)) != 0;
    parameters.tile_state[1] = 1;
    parameters.tile_state[2] = regions != 0;
    parameters.tile_state[3] = regions != 0;
    UINT tile_count=((width+63)/64)*((height+63)/64);
    int filtered=regions && !parameters.tile_state[0] && damage && damage->count<=tile_count &&
        damage->source_width==desc.Width && damage->source_height==desc.Height &&
        (!damage->count || damage->tiles);
    if(filtered) for(size_t i=0;i<damage->count;i++) {
        if(damage->tiles[i]>=tile_count || (i && damage->tiles[i]<=damage->tiles[i-1])) {filtered=0;break;}
    }
    if(filtered) parameters.tile_state[3]|=2;
    // Comparison advances resident pixels. Any later failure invalidates the
    // encoding cache so the next attempt initializes every tile.
    e->cache_valid = 0;
    TRY(prepare_frame(e, &parameters, header));
    hr = IDXGIKeyedMutex_AcquireSync(e->mutex, 0, 1000);
    if (hr != S_OK) { hr = E_FAIL; goto done; }
    locked = 1;
    const UINT zero[4] = {0};
    if (regions) ID3D11DeviceContext_ClearUnorderedAccessViewUint(e->context, e->arguments.view, zero);
    if(filtered && damage->count) {
        UINT offset=bk_jpeg_candidate_offset(tile_count,parameters.mcu_count)*4;
        D3D11_BOX box={.left=offset,.right=offset+(UINT)damage->count*4,.bottom=1,.back=1};
        ID3D11DeviceContext_UpdateSubresource(e->context,(ID3D11Resource *)e->dirty_tiles.buffer,0,&box,damage->tiles,0,0);
    }
    bind_frame(e);
    ID3D11DeviceContext_CSSetShader(e->context, e->compare, NULL, 0);
    if(filtered) ID3D11DeviceContext_Dispatch(e->context,(UINT)damage->count,1,1);
    else ID3D11DeviceContext_Dispatch(e->context, (width+63)/64, (height+63)/64, 1);
    if (regions) {
        ID3D11DeviceContext_CSSetShader(e->context, e->work, NULL, 0);
        ID3D11DeviceContext_Dispatch(e->context, 1, 1, 1);
        ID3D11UnorderedAccessView *empty = NULL;
        ID3D11DeviceContext_CSSetUnorderedAccessViews(e->context, 4, 1, &empty, NULL);
    }
    ID3D11DeviceContext_CSSetShader(e->context, e->transform, NULL, 0);
    if (regions) {
        ID3D11DeviceContext_DispatchIndirect(e->context, e->arguments.buffer, 20);
        encode_scan_work(e, parameters.mcu_count, 1);
    } else {
        ID3D11DeviceContext_Dispatch(e->context, columns, rows, 5);
        encode_scan(e, parameters.mcu_count);
    }
    TRY(regions ? read_payload(e, parameters.mcu_count, jpeg, jpeg_bytes) : read_jpeg(e, jpeg, jpeg_bytes));
    e->cached_parameters = parameters;
    e->cache_valid = 1;
done:
    if (locked) {
        HRESULT release = IDXGIKeyedMutex_ReleaseSync(e->mutex, 0);
        if (SUCCEEDED(hr)) hr = release;
    }
    if (FAILED(hr)) {
        if (e) e->cache_valid = 0;
        if (jpeg) { free(*jpeg); *jpeg = NULL; }
        if (jpeg_bytes) *jpeg_bytes = 0;
        snprintf(error, capacity, "D3D11 JPEG encoding failed (0x%08lx)", (unsigned long)hr);
    }
    return SUCCEEDED(hr);
}

int bk_jpeg_gpu_encode_mode(bk_jpeg_gpu *e, uintptr_t handle, int rotation, int width, int height,
 const uint32_t *q, const uint32_t *h, const uint8_t *header, size_t size, int regions,
 uint8_t **out, size_t *bytes, char *error, size_t capacity) {
 return bk_jpeg_gpu_encode_damage(e,handle,rotation,width,height,q,h,header,size,regions,NULL,out,bytes,error,capacity);
}

int bk_jpeg_gpu_encode(bk_jpeg_gpu *e, uintptr_t handle, int rotation, int width, int height,
 const uint32_t *q, const uint32_t *h, const uint8_t *header, size_t size,
 uint8_t **out, size_t *bytes, char *error, size_t capacity) {
 return bk_jpeg_gpu_encode_mode(e,handle,rotation,width,height,q,h,header,size,0,out,bytes,error,capacity);
}
#include "decode_d3d11_windows.inc"
