// Synthetic GPU fixture: no display capture, vendor SDK or FFmpeg required.
// stdin: geometry, quantizers, Huffman tables, header length/header, then BGRA
// pixels (or int32 coefficients when the optional third argument is present).
// stdout: a complete JPEG produced and packed on the GPU.
#include "../d3d11_windows.c"
#include <io.h>
#include <fcntl.h>

int main(int argc, char **argv) {
    if (argc != 2 && argc != 3) return 2;
    int coefficients_only = argc == 3;
    int result = 1;
    FILE *file = fopen(argv[1], "rb");
    if (!file) return 2;
    fseek(file, 0, SEEK_END);
    long length = ftell(file);
    rewind(file);
    if (length <= 0 || length > 1024*1024) { fclose(file); return 2; }
    char *shader = malloc(length);
    if (!shader || fread(shader, 1, length, file) != (size_t)length) return 2;
    fclose(file);
    // Windows text-mode streams would corrupt binary pixel/coefficient data.
    _setmode(_fileno(stdin), _O_BINARY);
    _setmode(_fileno(stdout), _O_BINARY);
    uint32_t geometry[5], quantization[128], huffman[1024], header_bytes;
    if (fread(geometry, sizeof(uint32_t), 5, stdin) != 5 ||
        fread(quantization, sizeof(uint32_t), 128, stdin) != 128 ||
        fread(huffman, sizeof(uint32_t), 1024, stdin) != 1024 ||
        fread(&header_bytes, sizeof(uint32_t), 1, stdin) != 1 || header_bytes > 1024 || header_bytes < 2) return 2;
    uint8_t header[1024];
    if (fread(header, 1, header_bytes, stdin) != header_bytes) return 2;
    UINT sw = geometry[0], sh = geometry[1], w = geometry[3], h = geometry[4];
    if (!sw || !sh || !w || !h || sw > 8192 || sh > 8192 || w > 8192 || h > 8192) return 2;
    size_t count = (size_t)((w+15)/16)*((h+15)/16)*384;
    uint8_t *pixels = calloc((size_t)sw*sh, 4);
    int32_t *coefficients = malloc(count*sizeof(int32_t));
    if (!pixels || !coefficients) return 2;
    if (coefficients_only) {
        if (fread(coefficients, sizeof(int32_t), count, stdin) != count) return 2;
    } else if (fread(pixels, 4, (size_t)sw*sh, stdin) != (size_t)sw*sh) return 2;
    uint8_t *jpeg = NULL;
    size_t jpeg_bytes = 0;
    ID3D11Device *device = NULL;
    ID3D11DeviceContext *context = NULL;
    ID3D11Texture2D *texture = NULL;
    IDXGIDevice *dxgi = NULL;
    IDXGIAdapter *adapter = NULL;
    IDXGIResource *resource = NULL;
    IDXGIKeyedMutex *mutex = NULL;
    bk_jpeg_gpu *encoder = NULL;
    HRESULT hr;
    D3D_FEATURE_LEVEL level = D3D_FEATURE_LEVEL_11_0;
#undef TRY
#define TRY(call) do { hr = (call); if (FAILED(hr)) goto cleanup; } while (0)
    TRY(D3D11CreateDevice(NULL, D3D_DRIVER_TYPE_HARDWARE, NULL, D3D11_CREATE_DEVICE_BGRA_SUPPORT,
        &level, 1, D3D11_SDK_VERSION, &device, NULL, &context));
    TRY(ID3D11Device_QueryInterface(device, &IID_IDXGIDevice, (void **)&dxgi));
    TRY(IDXGIDevice_GetAdapter(dxgi, &adapter));
    DXGI_ADAPTER_DESC ad;
    TRY(IDXGIAdapter_GetDesc(adapter, &ad));
    uint64_t luid = (uint64_t)(uint32_t)ad.AdapterLuid.LowPart | (uint64_t)(uint32_t)ad.AdapterLuid.HighPart<<32;
    D3D11_TEXTURE2D_DESC td = {0};
    td.Width=sw; td.Height=sh; td.ArraySize=td.MipLevels=td.SampleDesc.Count=1;
    td.Format=DXGI_FORMAT_B8G8R8A8_UNORM;
    td.Usage=D3D11_USAGE_DEFAULT;
    td.BindFlags=D3D11_BIND_SHADER_RESOURCE | D3D11_BIND_RENDER_TARGET;
    td.MiscFlags=D3D11_RESOURCE_MISC_SHARED_KEYEDMUTEX;
    TRY(ID3D11Device_CreateTexture2D(device, &td, NULL, &texture));
    TRY(ID3D11Texture2D_QueryInterface(texture, &IID_IDXGIResource, (void **)&resource));
    HANDLE handle;
    TRY(IDXGIResource_GetSharedHandle(resource, &handle));
    TRY(ID3D11Texture2D_QueryInterface(texture, &IID_IDXGIKeyedMutex, (void **)&mutex));
    hr=IDXGIKeyedMutex_AcquireSync(mutex,0,1000);
    if (hr!=S_OK) { hr=E_FAIL; goto cleanup; }
    ID3D11DeviceContext_UpdateSubresource(context,(ID3D11Resource *)texture,0,NULL,pixels,sw*4,0);
    TRY(IDXGIKeyedMutex_ReleaseSync(mutex,0));
    char message[2048] = {0};
    encoder=bk_jpeg_gpu_open(luid,shader,length,message,sizeof(message));
    if (!encoder) { fprintf(stderr,"%s\n",message); goto cleanup; }
    if (coefficients_only) {
        bk_jpeg_parameters parameters = {
            .source_width=sw,.source_height=sh,.output_width=w,.output_height=h,
            .mcu_columns=(w+15)/16,.mcu_count=(UINT)(count/384),.header_bytes=header_bytes,
            .crop_size={sw,sh}
        };
        memcpy(parameters.quantization,quantization,sizeof(quantization));
        memcpy(parameters.huffman,huffman,sizeof(huffman));
        TRY(prepare_frame(encoder,&parameters,header));
        ID3D11DeviceContext_UpdateSubresource(encoder->context,(ID3D11Resource *)encoder->coefficients.buffer,0,NULL,coefficients,0,0);
        bind_frame(encoder);
        encode_scan(encoder,parameters.mcu_count);
        TRY(read_jpeg(encoder,&jpeg,&jpeg_bytes));
    } else if (!bk_jpeg_gpu_encode(encoder,(uintptr_t)handle,geometry[2],w,h,quantization,huffman,header,header_bytes,
                                  &jpeg,&jpeg_bytes,message,sizeof(message))) {
        fprintf(stderr,"%s\n",message);
        goto cleanup;
    }
    if (fwrite(jpeg,1,jpeg_bytes,stdout)!=jpeg_bytes) goto cleanup;
    result=0;
cleanup:
    if (FAILED(hr)) fprintf(stderr,"fixture HRESULT 0x%08lx\n",(unsigned long)hr);
    bk_jpeg_gpu_close(encoder);
    if (mutex) IDXGIKeyedMutex_Release(mutex);
    if (resource) IDXGIResource_Release(resource);
    if (texture) ID3D11Texture2D_Release(texture);
    if (adapter) IDXGIAdapter_Release(adapter);
    if (dxgi) IDXGIDevice_Release(dxgi);
    if (context) ID3D11DeviceContext_Release(context);
    if (device) ID3D11Device_Release(device);
    free(pixels);free(coefficients);free(shader);free(jpeg);
    return result;
}
