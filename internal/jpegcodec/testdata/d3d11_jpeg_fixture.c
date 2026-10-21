// Synthetic GPU fixture: no display capture, vendor SDK or FFmpeg required.
// stdin: geometry, quantizers, Huffman tables, header length/header, then BGRA
// pixels (or int32 coefficients when the optional third argument is present).
// stdout: a complete JPEG produced and packed on the GPU.
#include "../d3d11_windows.c"
#include <io.h>
#include <fcntl.h>

// Hardware-only validation of the compare stage and cache invalidation.
// expected=-1: every tile dirty; -2: every tile clean; otherwise one dirty tile.
static HRESULT check_dirty_flags(bk_jpeg_gpu *e,int expected) {
    HRESULT hr;
    ID3D11Buffer *readback=NULL;
    D3D11_BUFFER_DESC desc={0};
    desc.ByteWidth=e->dirty_tiles.bytes;desc.Usage=D3D11_USAGE_STAGING;desc.CPUAccessFlags=D3D11_CPU_ACCESS_READ;
    TRY(ID3D11Device_CreateBuffer(e->device,&desc,NULL,&readback));
    ID3D11DeviceContext_CopyResource(e->context,(ID3D11Resource *)readback,(ID3D11Resource *)e->dirty_tiles.buffer);
    D3D11_MAPPED_SUBRESOURCE map={0};
    TRY(ID3D11DeviceContext_Map(e->context,(ID3D11Resource *)readback,0,D3D11_MAP_READ,0,&map));
    hr=S_OK;
    UINT tiles=((e->cached_parameters.output_width+63)/64)*((e->cached_parameters.output_height+63)/64);
    for(UINT i=0;i<tiles;i++) {
        UINT want=expected==-1||expected==(int)i;
        if(((UINT *)map.pData)[i]!=want) hr=E_FAIL;
    }
    ID3D11DeviceContext_Unmap(e->context,(ID3D11Resource *)readback,0);
done:
    if(readback)ID3D11Buffer_Release(readback);
    return hr;
}

int main(int argc, char **argv) {
    if (argc != 2 && argc != 3) return 2;
    int incremental = argc == 3 && strcmp(argv[2],"incremental")==0;
    int coefficients_only = argc == 3 && !incremental;
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
    uint32_t geometry[5], quantization[128], huffman[3072], header_bytes;
    if (fread(geometry, sizeof(uint32_t), 5, stdin) != 5 ||
        fread(quantization, sizeof(uint32_t), 128, stdin) != 128 ||
        fread(huffman, sizeof(uint32_t), 3072, stdin) != 3072 ||
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
        bk_jpeg_prepare_quantization(&parameters, quantization);
        memcpy(parameters.huffman,huffman,sizeof(huffman));
        TRY(prepare_frame(encoder,&parameters,header));
        // Match the transform output: packed coefficients, then AC masks.
        uint32_t *packed = calloc(count / 64 * BK_JPEG_BLOCK_WORDS, sizeof(uint32_t));
        if (!packed) goto cleanup;
        for (size_t i = 0; i < count; i += 2) packed[i/2] = (uint16_t)coefficients[i] | ((uint32_t)(uint16_t)coefficients[i+1] << 16);
        static const uint8_t zigzag[64] = {
            0,1,8,16,9,2,3,10,17,24,32,25,18,11,4,5,
            12,19,26,33,40,48,41,34,27,20,13,6,7,14,21,28,
            35,42,49,56,57,50,43,36,29,22,15,23,30,37,44,51,
            58,59,52,45,38,31,39,46,53,60,61,54,47,55,62,63
        };
        for (size_t block = 0; block < count / 64; block++) {
            for (unsigned scan = 1; scan < 64; scan++) {
                if (coefficients[block * 64 + zigzag[scan]] != 0)
                    packed[count / 2 + block * 2 + scan / 32] |= 1u << (scan & 31);
            }
        }
        ID3D11DeviceContext_UpdateSubresource(encoder->context,(ID3D11Resource *)encoder->coefficients.buffer,0,NULL,packed,0,0);
        free(packed);
        bind_frame(encoder);
        encode_scan(encoder,parameters.mcu_count);
        TRY(read_jpeg(encoder,&jpeg,&jpeg_bytes));
    } else if (!bk_jpeg_gpu_encode(encoder,(uintptr_t)handle,geometry[2],w,h,quantization,huffman,header,header_bytes,
                                  &jpeg,&jpeg_bytes,message,sizeof(message))) {
        fprintf(stderr,"%s\n",message);
        goto cleanup;
    }
    if(incremental) {
        TRY(check_dirty_flags(encoder,-1));
        for(int phase=0;phase<4;phase++) {
            if(phase==0) { // Alpha-only changes are not dirty.
                for(size_t i=3;i<(size_t)sw*sh*4;i+=4)pixels[i]^=255;
            }
            if(phase==1) pixels[((size_t)sh*sw-1)*4]^=31;
            if(phase==2) encoder->cache_valid=0; // Recovery must force initialization.
            if(phase==3) { // Quantizer changes invalidate otherwise static pixels.
                quantization[0]=quantization[0]==255?254:quantization[0]+1;
                for(uint32_t i=0;i+5<header_bytes;i++)if(header[i]==255&&header[i+1]==0xdb) {header[i+5]=(uint8_t)quantization[0];break;}
            }
            hr=IDXGIKeyedMutex_AcquireSync(mutex,0,1000);
            if(hr!=S_OK){hr=E_FAIL;goto cleanup;}
            ID3D11DeviceContext_UpdateSubresource(context,(ID3D11Resource *)texture,0,NULL,pixels,sw*4,0);
            TRY(IDXGIKeyedMutex_ReleaseSync(mutex,0));
            uint8_t *next=NULL;size_t next_bytes=0;
            if(!bk_jpeg_gpu_encode(encoder,(uintptr_t)handle,geometry[2],w,h,quantization,huffman,header,header_bytes,&next,&next_bytes,message,sizeof(message))) {
                free(next);goto cleanup;
            }
            if(phase==0&&(next_bytes!=jpeg_bytes||memcmp(next,jpeg,jpeg_bytes)!=0)) {free(next);goto cleanup;}
            free(next);
            int expected=phase==0?-2:phase==1?(int)(((h-1)/64)*((w+63)/64)+(w-1)/64):-1;
            TRY(check_dirty_flags(encoder,expected));
        }
    }

    if(incremental) {
        for(int phase=0;phase<2;phase++) {
            uint8_t *packet=NULL;size_t packet_bytes=0;
            if(!bk_jpeg_gpu_encode_mode(encoder,(uintptr_t)handle,geometry[2],w,h,quantization,huffman,header,header_bytes,1,&packet,&packet_bytes,message,sizeof(message))) {
                free(packet);goto cleanup;
            }
            uint32_t packet_count=0;
            if(packet_bytes>=4)memcpy(&packet_count,packet,4);
            uint32_t expected_count=((w+15)/16)*((h+15)/16);
            if(packet_count!=expected_count || (phase==1 && packet_bytes!=4)) {free(packet);goto cleanup;}
            size_t pos=4;uint32_t records=0;uint8_t seen[65536]={0};
            while(pos<packet_bytes) {
                uint32_t index=0,length=0;
                if(packet_bytes-pos<8) {free(packet);goto cleanup;}
                memcpy(&index,packet+pos,4);memcpy(&length,packet+pos+4,4);
                if(index>=expected_count || seen[index] || length<1 || length>BK_JPEG_SEGMENT_CAPACITY) {free(packet);goto cleanup;}
                seen[index]=1;pos+=8+((length+3)&~3u);records++;
            }
            if(pos!=packet_bytes || (phase==0 && records!=expected_count)) {free(packet);goto cleanup;}
            free(packet);
            TRY(check_dirty_flags(encoder,phase==0?-1:-2));
        }
    }

    if(incremental) {
        uint32_t tile_columns=(w+63)/64,tile_rows=(h+63)/64;
        uint32_t last_tile=tile_columns*tile_rows-1;
        bk_jpeg_damage damage={.tiles=&last_tile,.count=1,.source_width=sw,.source_height=sh};
        // Two source changes, but only the hinted edge tile may be compared.
        pixels[0]^=31;pixels[((size_t)sw*sh-1)*4]^=31;
        hr=IDXGIKeyedMutex_AcquireSync(mutex,0,1000);if(hr!=S_OK){hr=E_FAIL;goto cleanup;}
        ID3D11DeviceContext_UpdateSubresource(context,(ID3D11Resource *)texture,0,NULL,pixels,sw*4,0);
        TRY(IDXGIKeyedMutex_ReleaseSync(mutex,0));
        for(int phase=0;phase<4;phase++) {
            if(phase==1) damage.count=0; // Valid empty hint skips comparison.
            if(phase==2) {last_tile=tile_columns*tile_rows;damage.count=1;} // Invalid hint falls back.
            if(phase==3) {damage.count=0;encoder->cache_valid=0;} // Initialization overrides hints.
            uint8_t *packet=NULL;size_t packet_bytes=0;
            if(!bk_jpeg_gpu_encode_damage(encoder,(uintptr_t)handle,geometry[2],w,h,quantization,huffman,header,header_bytes,1,&damage,&packet,&packet_bytes,message,sizeof(message))) {free(packet);goto cleanup;}
            if(((encoder->cached_parameters.tile_state[3]&2)!=0)!=(phase<2)) {free(packet);goto cleanup;}
            if(phase==1 && packet_bytes!=4) {free(packet);goto cleanup;}
            size_t pos=4;uint32_t records=0,columns=(w+15)/16;
            while(pos<packet_bytes) {
                uint32_t index=0,length=0;
                if(packet_bytes-pos<8) {free(packet);goto cleanup;}
                memcpy(&index,packet+pos,4);memcpy(&length,packet+pos+4,4);
                uint32_t tile=(index/columns/4)*tile_columns+(index%columns/4);
                if(phase==0 && tile!=tile_columns*tile_rows-1) {free(packet);goto cleanup;}
                if(phase==2 && tile!=0) {free(packet);goto cleanup;}
                pos+=8+((length+3)&~3u);records++;
            }
            if(pos!=packet_bytes || ((phase==0||phase==2) && !records) || (phase==3 && records!=((w+15)/16)*((h+15)/16))) {free(packet);goto cleanup;}
            free(packet);
        }
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
