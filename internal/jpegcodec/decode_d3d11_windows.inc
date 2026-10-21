// Shared device/compiler helpers are defined in d3d11_windows.c.
typedef struct {
    ID3D11Buffer *buffer;
    ID3D11ShaderResourceView *srv;
    ID3D11UnorderedAccessView *uav;
    UINT bytes;
} decode_buffer;
struct bk_jpeg_decoder {
    ID3D11Device *device;
    ID3D11DeviceContext *context;
    ID3D11ComputeShader *entropy, *idct, *pack;
    decode_buffer buffers[6];
    ID3D11Buffer *readback, *status;
    UINT readback_bytes;
};
void bk_jpeg_decoder_close(bk_jpeg_decoder *d) {
    if (!d) return;
    if (d->context) ID3D11DeviceContext_ClearState(d->context);
    for (int i=0;i<6;i++) {
        DROP(ID3D11ShaderResourceView,d->buffers[i].srv);
        DROP(ID3D11UnorderedAccessView,d->buffers[i].uav);
        DROP(ID3D11Buffer,d->buffers[i].buffer);
    }
    DROP(ID3D11Buffer,d->readback); DROP(ID3D11Buffer,d->status);
    DROP(ID3D11ComputeShader,d->entropy); DROP(ID3D11ComputeShader,d->idct); DROP(ID3D11ComputeShader,d->pack);
    DROP(ID3D11DeviceContext,d->context); DROP(ID3D11Device,d->device); free(d);
}
bk_jpeg_decoder *bk_jpeg_decoder_open(const char *source,size_t size,char *error,size_t capacity) {
    bk_jpeg_decoder *d=calloc(1,sizeof(*d));
    if (!d) return NULL;
    HRESULT hr=E_FAIL; HMODULE compiler=NULL; IDXGIFactory1 *factory=NULL; IDXGIAdapter1 *adapter=NULL;
    TRY(CreateDXGIFactory1(&IID_IDXGIFactory1,(void **)&factory));
    for (UINT i=0;;i++) {
        TRY(IDXGIFactory1_EnumAdapters1(factory,i,&adapter));
        DXGI_ADAPTER_DESC1 desc; TRY(IDXGIAdapter1_GetDesc1(adapter,&desc));
        if (!(desc.Flags&DXGI_ADAPTER_FLAG_SOFTWARE)) {
            D3D_FEATURE_LEVEL level=D3D_FEATURE_LEVEL_11_0;
            hr=D3D11CreateDevice((IDXGIAdapter *)adapter,D3D_DRIVER_TYPE_UNKNOWN,NULL,0,&level,1,D3D11_SDK_VERSION,&d->device,NULL,&d->context);
            if (SUCCEEDED(hr)) break;
        }
        DROP(IDXGIAdapter1,adapter);
    }
    compiler=LoadLibraryExW(L"d3dcompiler_47.dll",NULL,LOAD_LIBRARY_SEARCH_SYSTEM32);
    if (!compiler) { hr=HRESULT_FROM_WIN32(GetLastError()); goto done; }
    compile_fn compile=(compile_fn)(void *)GetProcAddress(compiler,"D3DCompile");
    if (!compile) { hr=E_NOINTERFACE; goto done; }
    bk_jpeg_gpu owner={.device=d->device};
    TRY(compile_stage(&owner,compile,source,size,"main",&d->entropy,error,capacity));
    TRY(compile_stage(&owner,compile,source,size,"inverseDCT",&d->idct,error,capacity));
    TRY(compile_stage(&owner,compile,source,size,"packNV12",&d->pack,error,capacity));
    D3D11_BUFFER_DESC desc={.ByteWidth=4,.Usage=D3D11_USAGE_STAGING,.CPUAccessFlags=D3D11_CPU_ACCESS_READ};
    TRY(ID3D11Device_CreateBuffer(d->device,&desc,NULL,&d->status));
done:
    if (compiler) FreeLibrary(compiler);
    DROP(IDXGIAdapter1,adapter); DROP(IDXGIFactory1,factory);
    if (FAILED(hr)) {
        if (!error[0]) snprintf(error,capacity,"D3D11 JPEG decoder initialization: 0x%08lx",(unsigned long)hr);
        bk_jpeg_decoder_close(d); return NULL;
    }
    return d;
}
static HRESULT decode_buffer_ensure(bk_jpeg_decoder *d,int slot,UINT bytes) {
    decode_buffer *b=&d->buffers[slot];
    if (b->bytes>=bytes) return S_OK;
    DROP(ID3D11ShaderResourceView,b->srv); DROP(ID3D11UnorderedAccessView,b->uav); DROP(ID3D11Buffer,b->buffer); b->bytes=0;
    int structured=slot==2||slot==3;
    D3D11_BUFFER_DESC desc={.ByteWidth=bytes,.Usage=D3D11_USAGE_DEFAULT,
        .BindFlags=slot<2?D3D11_BIND_SHADER_RESOURCE:D3D11_BIND_UNORDERED_ACCESS,
        .MiscFlags=structured?D3D11_RESOURCE_MISC_BUFFER_STRUCTURED:D3D11_RESOURCE_MISC_BUFFER_ALLOW_RAW_VIEWS,
        .StructureByteStride=structured?4:0};
    HRESULT hr=ID3D11Device_CreateBuffer(d->device,&desc,NULL,&b->buffer);
    if (FAILED(hr)) return hr;
    if (slot<2) {
        D3D11_SHADER_RESOURCE_VIEW_DESC view={.Format=DXGI_FORMAT_R32_TYPELESS,.ViewDimension=D3D11_SRV_DIMENSION_BUFFEREX};
        view.BufferEx.NumElements=bytes/4; view.BufferEx.Flags=D3D11_BUFFEREX_SRV_FLAG_RAW;
        hr=ID3D11Device_CreateShaderResourceView(d->device,(ID3D11Resource *)b->buffer,&view,&b->srv);
    } else {
        D3D11_UNORDERED_ACCESS_VIEW_DESC view={.Format=structured?DXGI_FORMAT_UNKNOWN:DXGI_FORMAT_R32_TYPELESS,.ViewDimension=D3D11_UAV_DIMENSION_BUFFER};
        view.Buffer.NumElements=bytes/4; view.Buffer.Flags=structured?0:D3D11_BUFFER_UAV_FLAG_RAW;
        hr=ID3D11Device_CreateUnorderedAccessView(d->device,(ID3D11Resource *)b->buffer,&view,&b->uav);
    }
    if (SUCCEEDED(hr)) b->bytes=bytes;
    return hr;
}
int bk_jpeg_decoder_decode(bk_jpeg_decoder *d,const uint8_t *input,size_t input_bytes,const uint32_t *meta,size_t meta_bytes,
                           uint8_t *output,size_t output_bytes,char *error,size_t capacity) {
    if (!d||!input||!meta||!output||meta_bytes<2056*4||input_bytes<4||input_bytes>128u*1024*1024||
        meta_bytes>128u*1024*1024||meta[2]<2||meta[3]<2||meta[2]>8192||meta[3]>8192||
        output_bytes!=(size_t)meta[2]*meta[3]*3/2) return -1;
    uint64_t workspace=(uint64_t)meta[6]*meta[7]*meta[8]*64*4;
    if (!workspace||workspace>128u*1024*1024) return -1;
    HRESULT hr=S_OK; D3D11_MAPPED_SUBRESOURCE mapped;
    UINT sizes[6]={(UINT)input_bytes,(UINT)meta_bytes,(UINT)workspace,(UINT)workspace,(UINT)((output_bytes+3)&~3u),4};
    for (int i=0;i<6;i++) { TRY(decode_buffer_ensure(d,i,sizes[i])); }
    // Sized boxes are essential when a persistent allocation exceeds this frame.
    D3D11_BOX box={.left=0,.top=0,.front=0,.right=sizes[0],.bottom=1,.back=1};
    ID3D11DeviceContext_UpdateSubresource(d->context,(ID3D11Resource *)d->buffers[0].buffer,0,&box,input,0,0);
    box.right=sizes[1]; ID3D11DeviceContext_UpdateSubresource(d->context,(ID3D11Resource *)d->buffers[1].buffer,0,&box,meta,0,0);
    UINT zero[4]={0}; ID3D11DeviceContext_ClearUnorderedAccessViewUint(d->context,d->buffers[5].uav,zero);
    ID3D11ShaderResourceView *srvs[]={d->buffers[0].srv,d->buffers[1].srv};
    ID3D11UnorderedAccessView *uavs[]={d->buffers[2].uav,d->buffers[3].uav,d->buffers[4].uav,d->buffers[5].uav};
    ID3D11DeviceContext_CSSetShaderResources(d->context,0,2,srvs);
    ID3D11DeviceContext_CSSetUnorderedAccessViews(d->context,0,4,uavs,NULL);
    ID3D11DeviceContext_CSSetShader(d->context,d->entropy,NULL,0);
    ID3D11DeviceContext_Dispatch(d->context,(meta[9]+63)/64,1,1);
    ID3D11DeviceContext_ClearState(d->context);
    ID3D11DeviceContext_CopyResource(d->context,(ID3D11Resource *)d->status,(ID3D11Resource *)d->buffers[5].buffer);
    TRY(ID3D11DeviceContext_Map(d->context,(ID3D11Resource *)d->status,0,D3D11_MAP_READ,0,&mapped));
    UINT invalid=*(UINT *)mapped.pData; ID3D11DeviceContext_Unmap(d->context,(ID3D11Resource *)d->status,0);
    if (invalid) { snprintf(error,capacity,"invalid JPEG entropy"); hr=E_INVALIDARG; goto done; }
    ID3D11DeviceContext_CSSetShaderResources(d->context,0,2,srvs);
    ID3D11DeviceContext_CSSetUnorderedAccessViews(d->context,0,4,uavs,NULL);
    ID3D11DeviceContext_CSSetShader(d->context,d->idct,NULL,0);
    ID3D11DeviceContext_Dispatch(d->context,meta[6]*meta[8],meta[7],1);
    // Rebind between dependent dispatches to make UAV writes visible.
    ID3D11DeviceContext_CSSetUnorderedAccessViews(d->context,0,4,(ID3D11UnorderedAccessView *[4]){0},NULL);
    ID3D11DeviceContext_CSSetUnorderedAccessViews(d->context,0,4,uavs,NULL);
    ID3D11DeviceContext_CSSetShader(d->context,d->pack,NULL,0);
    ID3D11DeviceContext_Dispatch(d->context,min(256u,(sizes[4]/4+63)/64),(sizes[4]/4+16383)/16384,1);
    ID3D11DeviceContext_ClearState(d->context);
    if (d->readback_bytes!=d->buffers[4].bytes) {
        DROP(ID3D11Buffer,d->readback); d->readback_bytes=0;
        D3D11_BUFFER_DESC desc={.ByteWidth=d->buffers[4].bytes,.Usage=D3D11_USAGE_STAGING,.CPUAccessFlags=D3D11_CPU_ACCESS_READ};
        TRY(ID3D11Device_CreateBuffer(d->device,&desc,NULL,&d->readback)); d->readback_bytes=desc.ByteWidth;
    }
    ID3D11DeviceContext_CopyResource(d->context,(ID3D11Resource *)d->readback,(ID3D11Resource *)d->buffers[4].buffer);
    TRY(ID3D11DeviceContext_Map(d->context,(ID3D11Resource *)d->readback,0,D3D11_MAP_READ,0,&mapped));
    memcpy(output,mapped.pData,output_bytes); ID3D11DeviceContext_Unmap(d->context,(ID3D11Resource *)d->readback,0);
done:
    ID3D11DeviceContext_ClearState(d->context);
    if (FAILED(hr)&&!error[0]) snprintf(error,capacity,"D3D11 JPEG decode: 0x%08lx",(unsigned long)hr);
    return FAILED(hr)?-1:0;
}
