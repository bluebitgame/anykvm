//go:build windows && cgo

#define COBJMACROS
#include "progressive_windows.h"
#include "progressive_layout.h"
#include <windows.h>
#include <d3d11.h>
#include <d3dcompiler.h>
#include <dxgi1_2.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define DROP(type,x) do { if (x) { type##_Release(x); (x)=NULL; } } while (0)
#define TRY(call) do { hr=(call); if (FAILED(hr)) goto done; } while (0)
typedef struct {
    ID3D11Buffer *buffer;
    ID3D11ShaderResourceView *srv;
    ID3D11UnorderedAccessView *uav;
    UINT bytes;
} progressive_buffer;
struct bk_d3d_progressive {
    ID3D11Device *device;
    ID3D11DeviceContext *context;
    ID3D11ComputeShader *stages[4];
    progressive_buffer buffers[6];
    ID3D11Buffer *status,*readback;
    UINT readback_bytes;
    ID3D11ComputeShader *surface_shader;
    progressive_buffer current,previous,dirty,surface_parameters;
    ID3D11Buffer *dirty_readback;
    UINT dirty_readback_bytes,surface_width,surface_height;
    int prepared,previous_valid;
};
static void progressive_buffer_free(progressive_buffer *b) {
    DROP(ID3D11ShaderResourceView,b->srv);DROP(ID3D11UnorderedAccessView,b->uav);DROP(ID3D11Buffer,b->buffer);b->bytes=0;
}
void bk_d3d_progressive_free(bk_d3d_progressive *p) {
    if (!p) return;
    if (p->context) ID3D11DeviceContext_ClearState(p->context);
    for (int i=0;i<6;i++) {
        DROP(ID3D11ShaderResourceView,p->buffers[i].srv);
        DROP(ID3D11UnorderedAccessView,p->buffers[i].uav);
        DROP(ID3D11Buffer,p->buffers[i].buffer);
    }
    for (int i=0;i<4;i++) DROP(ID3D11ComputeShader,p->stages[i]);
    DROP(ID3D11Buffer,p->status); DROP(ID3D11Buffer,p->readback);
    progressive_buffer_free(&p->current);progressive_buffer_free(&p->previous);
    progressive_buffer_free(&p->dirty);progressive_buffer_free(&p->surface_parameters);
    DROP(ID3D11Buffer,p->dirty_readback);DROP(ID3D11ComputeShader,p->surface_shader);
    DROP(ID3D11DeviceContext,p->context); DROP(ID3D11Device,p->device); free(p);
}
typedef HRESULT (WINAPI *progressive_compile)(LPCVOID,SIZE_T,LPCSTR,const D3D_SHADER_MACRO *,
    ID3DInclude *,LPCSTR,LPCSTR,UINT,UINT,ID3DBlob **,ID3DBlob **);
bk_d3d_progressive *bk_d3d_progressive_new(const char *source,size_t bytes,uint64_t luid,int exact,char *error,size_t capacity) {
    bk_d3d_progressive *p=calloc(1,sizeof(*p));
    if (!p) { snprintf(error,capacity,"Progressive allocation failed"); return NULL; }
    HRESULT hr=E_FAIL; IDXGIFactory1 *factory=NULL; IDXGIAdapter1 *adapter=NULL; HMODULE library=NULL;
    TRY(CreateDXGIFactory1(&IID_IDXGIFactory1,(void **)&factory));
    for (UINT i=0;;i++) {
        TRY(IDXGIFactory1_EnumAdapters1(factory,i,&adapter));
        DXGI_ADAPTER_DESC1 desc; TRY(IDXGIAdapter1_GetDesc1(adapter,&desc));
        uint64_t adapter_luid=(uint64_t)(uint32_t)desc.AdapterLuid.LowPart|((uint64_t)(uint32_t)desc.AdapterLuid.HighPart<<32);
        if (!(desc.Flags&DXGI_ADAPTER_FLAG_SOFTWARE)&&(!exact||adapter_luid==luid)) {
            D3D_FEATURE_LEVEL level=D3D_FEATURE_LEVEL_11_0;
            hr=D3D11CreateDevice((IDXGIAdapter *)adapter,D3D_DRIVER_TYPE_UNKNOWN,NULL,D3D11_CREATE_DEVICE_BGRA_SUPPORT,&level,1,D3D11_SDK_VERSION,&p->device,NULL,&p->context);
            if (SUCCEEDED(hr)) break;
        }
        DROP(IDXGIAdapter1,adapter);
    }
    library=LoadLibraryExW(L"d3dcompiler_47.dll",NULL,LOAD_LIBRARY_SEARCH_SYSTEM32);
    if (!library) { hr=HRESULT_FROM_WIN32(GetLastError()); goto done; }
    progressive_compile compile=(progressive_compile)(void *)GetProcAddress(library,"D3DCompile");
    if (!compile) { hr=E_NOINTERFACE; goto done; }
    const char *entries[]={"main","encodeTiles","indexSegments","packSegments"};
    for (int i=0;i<4;i++) {
        ID3DBlob *code=NULL,*messages=NULL;
        hr=compile(source,bytes,"anykvm-progressive",NULL,NULL,entries[i],"cs_5_0",
            D3DCOMPILE_OPTIMIZATION_LEVEL3|D3DCOMPILE_ENABLE_STRICTNESS,0,&code,&messages);
        if (SUCCEEDED(hr)) hr=ID3D11Device_CreateComputeShader(p->device,ID3D10Blob_GetBufferPointer(code),ID3D10Blob_GetBufferSize(code),NULL,&p->stages[i]);
        if (FAILED(hr)&&messages) snprintf(error,capacity,"Progressive %s: %.*s",entries[i],
            (int)ID3D10Blob_GetBufferSize(messages),(char *)ID3D10Blob_GetBufferPointer(messages));
        DROP(ID3D10Blob,code); DROP(ID3D10Blob,messages);
        if (FAILED(hr)) goto done;
    }
    D3D11_BUFFER_DESC desc={.ByteWidth=BK_PROGRESSIVE_INFO_BYTES,.Usage=D3D11_USAGE_STAGING,.CPUAccessFlags=D3D11_CPU_ACCESS_READ};
    TRY(ID3D11Device_CreateBuffer(p->device,&desc,NULL,&p->status));
done:
    if (library) FreeLibrary(library);
    DROP(IDXGIAdapter1,adapter); DROP(IDXGIFactory1,factory);
    if (FAILED(hr)) {
        if (!error[0]) snprintf(error,capacity,"Progressive D3D11 initialization: 0x%08lx",(unsigned long)hr);
        bk_d3d_progressive_free(p); return NULL;
    }
    return p;
}
static HRESULT progressive_buffer_ensure(bk_d3d_progressive *p,int slot,UINT bytes) {
    progressive_buffer *b=&p->buffers[slot];
    if (b->bytes>=bytes) return S_OK;
    DROP(ID3D11ShaderResourceView,b->srv); DROP(ID3D11UnorderedAccessView,b->uav); DROP(ID3D11Buffer,b->buffer); b->bytes=0;
    int structured=slot==2||slot==4;
    D3D11_BUFFER_DESC desc={.ByteWidth=bytes,.Usage=D3D11_USAGE_DEFAULT,
        .BindFlags=slot<2?D3D11_BIND_SHADER_RESOURCE:D3D11_BIND_UNORDERED_ACCESS,
        .MiscFlags=structured?D3D11_RESOURCE_MISC_BUFFER_STRUCTURED:D3D11_RESOURCE_MISC_BUFFER_ALLOW_RAW_VIEWS,
        .StructureByteStride=structured?4:0};
    HRESULT hr=ID3D11Device_CreateBuffer(p->device,&desc,NULL,&b->buffer);
    if (FAILED(hr)) return hr;
    if (slot<2) {
        D3D11_SHADER_RESOURCE_VIEW_DESC view={.Format=DXGI_FORMAT_R32_TYPELESS,.ViewDimension=D3D11_SRV_DIMENSION_BUFFEREX};
        view.BufferEx.NumElements=bytes/4; view.BufferEx.Flags=D3D11_BUFFEREX_SRV_FLAG_RAW;
        hr=ID3D11Device_CreateShaderResourceView(p->device,(ID3D11Resource *)b->buffer,&view,&b->srv);
    } else {
        D3D11_UNORDERED_ACCESS_VIEW_DESC view={.Format=structured?DXGI_FORMAT_UNKNOWN:DXGI_FORMAT_R32_TYPELESS,.ViewDimension=D3D11_UAV_DIMENSION_BUFFER};
        view.Buffer.NumElements=bytes/4; view.Buffer.Flags=structured?0:D3D11_BUFFER_UAV_FLAG_RAW;
        hr=ID3D11Device_CreateUnorderedAccessView(p->device,(ID3D11Resource *)b->buffer,&view,&b->uav);
    }
    if (SUCCEEDED(hr)) b->bytes=bytes;
    return hr;
}
static void progressive_bind(bk_d3d_progressive *p,int resident) {
    ID3D11ShaderResourceView *srvs[]={resident?p->current.srv:p->buffers[0].srv,p->buffers[1].srv};
    ID3D11UnorderedAccessView *uavs[]={p->buffers[2].uav,p->buffers[3].uav,p->buffers[4].uav,p->buffers[5].uav};
    ID3D11DeviceContext_CSSetShaderResources(p->context,0,2,srvs);
    ID3D11DeviceContext_CSSetUnorderedAccessViews(p->context,0,4,uavs,NULL);
}
int bk_d3d_progressive_run(bk_d3d_progressive *p,const uint8_t *input,size_t input_bytes,const uint32_t *meta,
    uint8_t **output,size_t *output_bytes,uint32_t *lengths,char *error,size_t capacity) {
    if (!p||!input||!output||!output_bytes||!lengths||!bk_progressive_validate(meta,input_bytes)) {
        snprintf(error,capacity,"invalid Progressive compute input"); return -1;
    }
    *output=NULL; *output_bytes=0;
    if(meta[6]&&(!p->prepared||meta[6]!=p->surface_width*4||meta[7]!=p->surface_height)) {
        snprintf(error,capacity,"Progressive GPU surface not prepared");return -1;
    }
    HRESULT hr=S_OK; D3D11_MAPPED_SUBRESOURCE mapped;
    UINT sizes[6]={(UINT)input_bytes,64,meta[5]*12288*4,
        BK_PROGRESSIVE_SEGMENTS*BK_PROGRESSIVE_SEGMENT_BYTES,BK_PROGRESSIVE_INFO_BYTES,
        BK_PROGRESSIVE_SEGMENTS*BK_PROGRESSIVE_SEGMENT_BYTES};
    for (int i=0;i<6;i++) TRY(progressive_buffer_ensure(p,i,sizes[i]));
    D3D11_BOX box={.right=sizes[0],.bottom=1,.back=1};
    ID3D11DeviceContext_UpdateSubresource(p->context,(ID3D11Resource *)p->buffers[0].buffer,0,&box,input,0,0);
    box.right=64; ID3D11DeviceContext_UpdateSubresource(p->context,(ID3D11Resource *)p->buffers[1].buffer,0,&box,meta,0,0);
    progressive_bind(p,meta[6]!=0);
    for (int stage=meta[4]==2?1:0;stage<4;stage++) {
        ID3D11DeviceContext_CSSetShader(p->context,p->stages[stage],NULL,0);
        ID3D11DeviceContext_Dispatch(p->context,stage==2?1:stage==3?meta[0]*6:meta[0],stage<2?3:1,1);
        // Unbind/rebind dependent UAVs between dispatches.
        ID3D11DeviceContext_CSSetUnorderedAccessViews(p->context,0,4,(ID3D11UnorderedAccessView *[4]){0},NULL);
        progressive_bind(p,meta[6]!=0);
    }
    ID3D11DeviceContext_ClearState(p->context);
    ID3D11DeviceContext_CopyResource(p->context,(ID3D11Resource *)p->status,(ID3D11Resource *)p->buffers[4].buffer);
    TRY(ID3D11DeviceContext_Map(p->context,(ID3D11Resource *)p->status,0,D3D11_MAP_READ,0,&mapped));
    uint32_t *status=mapped.pData,bytes=status[48],invalid=status[49];
    memcpy(lengths,status,BK_PROGRESSIVE_SEGMENTS*4);
    ID3D11DeviceContext_Unmap(p->context,(ID3D11Resource *)p->status,0);
    if (invalid||!bytes||bytes>sizes[5]||bytes%4) { snprintf(error,capacity,"Progressive segment overflow or invalid size"); hr=E_FAIL; goto done; }
    if (p->readback_bytes<bytes) {
        DROP(ID3D11Buffer,p->readback); p->readback_bytes=0;
        D3D11_BUFFER_DESC desc={.ByteWidth=bytes,.Usage=D3D11_USAGE_STAGING,.CPUAccessFlags=D3D11_CPU_ACCESS_READ};
        TRY(ID3D11Device_CreateBuffer(p->device,&desc,NULL,&p->readback)); p->readback_bytes=bytes;
    }
    box.right=bytes;
    ID3D11DeviceContext_CopySubresourceRegion(p->context,(ID3D11Resource *)p->readback,0,0,0,0,(ID3D11Resource *)p->buffers[5].buffer,0,&box);
    TRY(ID3D11DeviceContext_Map(p->context,(ID3D11Resource *)p->readback,0,D3D11_MAP_READ,0,&mapped));
    uint8_t *result=malloc(bytes);
    if (result) memcpy(result,mapped.pData,bytes);
    ID3D11DeviceContext_Unmap(p->context,(ID3D11Resource *)p->readback,0);
    if (!result) { hr=E_OUTOFMEMORY; goto done; }
    *output=result; *output_bytes=bytes;
done:
    ID3D11DeviceContext_ClearState(p->context);
    if (FAILED(hr)&&!error[0]) snprintf(error,capacity,"Progressive D3D11 encode: 0x%08lx",(unsigned long)hr);
    return FAILED(hr)?-1:0;
}

int bk_d3d_progressive_surface_init(bk_d3d_progressive *p,const char *source,size_t bytes,char *error,size_t capacity) {
    if(!p||p->surface_shader||!source||!bytes)return -1;
    HMODULE library=LoadLibraryExW(L"d3dcompiler_47.dll",NULL,LOAD_LIBRARY_SEARCH_SYSTEM32);
    if(!library){snprintf(error,capacity,"D3DCompiler unavailable");return -1;}
    progressive_compile compile=(progressive_compile)(void *)GetProcAddress(library,"D3DCompile");
    ID3DBlob *code=NULL,*messages=NULL;HRESULT hr=E_NOINTERFACE;
    if(compile)hr=compile(source,bytes,"anykvm-progressive-surface",NULL,NULL,"main","cs_5_0",
        D3DCOMPILE_OPTIMIZATION_LEVEL3|D3DCOMPILE_ENABLE_STRICTNESS,0,&code,&messages);
    if(SUCCEEDED(hr))hr=ID3D11Device_CreateComputeShader(p->device,ID3D10Blob_GetBufferPointer(code),ID3D10Blob_GetBufferSize(code),NULL,&p->surface_shader);
    if(FAILED(hr))snprintf(error,capacity,"surface shader: %.*s (0x%08lx)",messages?(int)ID3D10Blob_GetBufferSize(messages):0,
        messages?(char *)ID3D10Blob_GetBufferPointer(messages):"",(unsigned long)hr);
    DROP(ID3D10Blob,code);DROP(ID3D10Blob,messages);FreeLibrary(library);return FAILED(hr)?-1:0;
}
static HRESULT surface_buffer_ensure(bk_d3d_progressive *p,progressive_buffer *b,UINT bytes,int read,int write,int structured) {
    if(b->bytes>=bytes)return S_OK;
    progressive_buffer_free(b);
    D3D11_BUFFER_DESC desc={.ByteWidth=bytes,.Usage=D3D11_USAGE_DEFAULT,
        .BindFlags=(read?D3D11_BIND_SHADER_RESOURCE:0)|(write?D3D11_BIND_UNORDERED_ACCESS:0),
        .MiscFlags=structured?D3D11_RESOURCE_MISC_BUFFER_STRUCTURED:D3D11_RESOURCE_MISC_BUFFER_ALLOW_RAW_VIEWS,
        .StructureByteStride=structured?4:0};
    HRESULT hr=ID3D11Device_CreateBuffer(p->device,&desc,NULL,&b->buffer);if(FAILED(hr))return hr;
    if(read) {
        D3D11_SHADER_RESOURCE_VIEW_DESC view={.Format=DXGI_FORMAT_R32_TYPELESS,.ViewDimension=D3D11_SRV_DIMENSION_BUFFEREX};
        view.BufferEx.NumElements=bytes/4;view.BufferEx.Flags=D3D11_BUFFEREX_SRV_FLAG_RAW;
        hr=ID3D11Device_CreateShaderResourceView(p->device,(ID3D11Resource *)b->buffer,&view,&b->srv);if(FAILED(hr))return hr;
    }
    if(write) {
        D3D11_UNORDERED_ACCESS_VIEW_DESC view={.Format=structured?DXGI_FORMAT_UNKNOWN:DXGI_FORMAT_R32_TYPELESS,.ViewDimension=D3D11_UAV_DIMENSION_BUFFER};
        view.Buffer.NumElements=bytes/4;view.Buffer.Flags=structured?0:D3D11_BUFFER_UAV_FLAG_RAW;
        hr=ID3D11Device_CreateUnorderedAccessView(p->device,(ID3D11Resource *)b->buffer,&view,&b->uav);if(FAILED(hr))return hr;
    }
    b->bytes=bytes;return S_OK;
}
int bk_d3d_progressive_surface_prepare(bk_d3d_progressive *p,uintptr_t handle,const uint32_t *parameters,uint32_t *flags,char *error,size_t capacity) {
    if(!p||!p->surface_shader||!handle||!flags||!bk_progressive_surface_validate(parameters)){
        snprintf(error,capacity,"invalid immutable GPU surface");return -1;
    }
    HRESULT hr=S_OK;ID3D11Texture2D *texture=NULL;ID3D11ShaderResourceView *view=NULL;IDXGIKeyedMutex *mutex=NULL;
    int locked=0;D3D11_MAPPED_SUBRESOURCE mapped;
    uint32_t values[12];memcpy(values,parameters,48);UINT w=values[4],h=values[5],tiles=((w+63)/64)*((h+63)/64);
    p->prepared=0;
    TRY(ID3D11Device_OpenSharedResource(p->device,(HANDLE)handle,&IID_ID3D11Texture2D,(void **)&texture));
    D3D11_TEXTURE2D_DESC desc;ID3D11Texture2D_GetDesc(texture,&desc);
    if(desc.Width!=values[0]||desc.Height!=values[1]||desc.ArraySize!=1||desc.SampleDesc.Count!=1||
       (desc.Format!=DXGI_FORMAT_B8G8R8A8_UNORM&&desc.Format!=DXGI_FORMAT_R8G8B8A8_UNORM)){
        snprintf(error,capacity,"GPU snapshot geometry or format mismatch");hr=E_INVALIDARG;goto done;
    }
    TRY(ID3D11Texture2D_QueryInterface(texture,&IID_IDXGIKeyedMutex,(void **)&mutex));
    TRY(ID3D11Device_CreateShaderResourceView(p->device,(ID3D11Resource *)texture,NULL,&view));
    if(p->surface_width!=w||p->surface_height!=h) {
        progressive_buffer_free(&p->current);progressive_buffer_free(&p->previous);
        p->surface_width=w;p->surface_height=h;p->previous_valid=0;
    }
    if(!p->previous_valid)values[10]=1;
    TRY(surface_buffer_ensure(p,&p->current,w*h*4,1,1,0));
    TRY(surface_buffer_ensure(p,&p->previous,w*h*4,0,1,0));
    TRY(surface_buffer_ensure(p,&p->dirty,tiles*4,0,1,1));
    TRY(surface_buffer_ensure(p,&p->surface_parameters,48,1,0,0));
    if(p->dirty_readback_bytes<tiles*4) {
        DROP(ID3D11Buffer,p->dirty_readback);p->dirty_readback_bytes=0;
        D3D11_BUFFER_DESC buffer={.ByteWidth=tiles*4,.Usage=D3D11_USAGE_STAGING,.CPUAccessFlags=D3D11_CPU_ACCESS_READ};
        TRY(ID3D11Device_CreateBuffer(p->device,&buffer,NULL,&p->dirty_readback));p->dirty_readback_bytes=tiles*4;
    }
    ID3D11DeviceContext_UpdateSubresource(p->context,(ID3D11Resource *)p->surface_parameters.buffer,0,NULL,values,0,0);
    hr=IDXGIKeyedMutex_AcquireSync(mutex,0,1000);if(hr!=S_OK){hr=E_FAIL;goto done;}locked=1;
    ID3D11ShaderResourceView *inputs[]={view,p->surface_parameters.srv};
    ID3D11UnorderedAccessView *outputs[]={p->current.uav,p->previous.uav,p->dirty.uav};
    ID3D11DeviceContext_CSSetShaderResources(p->context,0,2,inputs);
    ID3D11DeviceContext_CSSetUnorderedAccessViews(p->context,0,3,outputs,NULL);
    ID3D11DeviceContext_CSSetShader(p->context,p->surface_shader,NULL,0);
    ID3D11DeviceContext_Dispatch(p->context,(w+63)/64,(h+63)/64,1);
    ID3D11DeviceContext_ClearState(p->context);
    D3D11_BOX box={.right=tiles*4,.bottom=1,.back=1};
    ID3D11DeviceContext_CopySubresourceRegion(p->context,(ID3D11Resource *)p->dirty_readback,0,0,0,0,(ID3D11Resource *)p->dirty.buffer,0,&box);
    TRY(ID3D11DeviceContext_Map(p->context,(ID3D11Resource *)p->dirty_readback,0,D3D11_MAP_READ,0,&mapped));
    memcpy(flags,mapped.pData,tiles*4);ID3D11DeviceContext_Unmap(p->context,(ID3D11Resource *)p->dirty_readback,0);
    p->prepared=1;
done:
    ID3D11DeviceContext_ClearState(p->context);
    if(locked){HRESULT released=IDXGIKeyedMutex_ReleaseSync(mutex,0);if(SUCCEEDED(hr))hr=released;}
    DROP(IDXGIKeyedMutex,mutex);DROP(ID3D11ShaderResourceView,view);DROP(ID3D11Texture2D,texture);
    if(FAILED(hr)&&!error[0])snprintf(error,capacity,"D3D11 surface prepare: 0x%08lx",(unsigned long)hr);
    return FAILED(hr)?-1:0;
}
int bk_d3d_progressive_surface_commit(bk_d3d_progressive *p,char *error,size_t capacity) {
    if(!p||!p->prepared){snprintf(error,capacity,"no prepared GPU surface");return -1;}
    ID3D11DeviceContext_ClearState(p->context);
    ID3D11DeviceContext_CopyResource(p->context,(ID3D11Resource *)p->previous.buffer,(ID3D11Resource *)p->current.buffer);
    HRESULT hr=ID3D11Device_GetDeviceRemovedReason(p->device);
    if(FAILED(hr)){snprintf(error,capacity,"GPU surface commit: 0x%08lx",(unsigned long)hr);return -1;}
    p->previous_valid=1;return 0;
}
