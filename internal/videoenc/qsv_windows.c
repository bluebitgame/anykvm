//go:build windows && cgo

#define BK_VENDOR_IMPLEMENTATION
#include "third_party/vpl/mfxvideo.h"
#include "vendor_windows.h"
#include <cfgmgr32.h>
#include <malloc.h>

typedef struct {
    ID3D11Texture2D *texture;
    ID3D11Buffer *bitstream;
    int mapped;
} bk_qsv_mid;
typedef struct bk_qsv_pool {
    struct bk_qsv_pool *next;
    mfxMemId *mids;
    mfxU16 count;
} bk_qsv_pool;

typedef struct {
    bk_vendor_options options;
    HMODULE module;
    mfxSession session;
    ID3D11Device *device;
    ID3D11DeviceContext *context;
    mfxStatus(MFX_CDECL *close_session)(mfxSession);
    mfxStatus(MFX_CDECL *close_encoder)(mfxSession);
    mfxStatus(MFX_CDECL *init_encoder)(mfxSession, mfxVideoParam *);
    mfxStatus(MFX_CDECL *get_params)(mfxSession, mfxVideoParam *);
    mfxStatus(MFX_CDECL *set_handle)(mfxSession, mfxHandleType, mfxHDL);
    mfxStatus(MFX_CDECL *encode)(mfxSession, mfxEncodeCtrl *, mfxFrameSurface1 *, mfxBitstream *, mfxSyncPoint *);
    mfxStatus(MFX_CDECL *sync)(mfxSession, mfxSyncPoint, mfxU32);
    mfxStatus(MFX_CDECL *set_allocator)(mfxSession, mfxFrameAllocator *);
    mfxFrameAllocator allocator;
    bk_qsv_pool *pools;
    bk_qsv_mid input;
    bk_d3d_converter converter;
    mfxFrameSurface1 surface;
    mfxBitstream bitstream;
    uint8_t *pixels;
    uint64_t frame, clock, remainder;
} bk_qsv;

// External NV12 allocation never creates staging resources or maps desktop
// pixels. Optional P8 buffers hold only compressed encoder output. The SDK gets a texture/subresource pair through
// GetHDL.
static void bk_qsv_pool_close(bk_qsv_pool *pool) {
    for (int i = 0; i < pool->count; i++) {
        bk_qsv_mid *mid = pool->mids[i];
        if (mid) {
            if (mid->texture)
                ID3D11Texture2D_Release(mid->texture);
            if (mid->bitstream)
                ID3D11Buffer_Release(mid->bitstream);
            free(mid);
        }
    }
    free(pool->mids);
    free(pool);
}
static mfxStatus MFX_CDECL bk_qsv_alloc(mfxHDL opaque, mfxFrameAllocRequest *request, mfxFrameAllocResponse *response) {
    bk_qsv *e = opaque;
    if (!request || !response)
        return MFX_ERR_NULL_PTR;
    int bitstream = request->Info.FourCC == MFX_FOURCC_P8;
    if ((!bitstream && request->Info.FourCC != MFX_FOURCC_NV12) || !request->Info.Width || !request->Info.Height ||
        (!bitstream && (request->Info.Width > 8192 || request->Info.Height > 8192)) ||
        (bitstream && (size_t)request->Info.Width * request->Info.Height > BK_VENDOR_LIMIT) ||
        !request->NumFrameSuggested || request->NumFrameSuggested > 128 ||
        !(request->Type & (MFX_MEMTYPE_VIDEO_MEMORY_DECODER_TARGET | MFX_MEMTYPE_VIDEO_MEMORY_PROCESSOR_TARGET |
                           MFX_MEMTYPE_VIDEO_MEMORY_ENCODER_TARGET)))
        return MFX_ERR_UNSUPPORTED;
    bk_qsv_pool *pool = calloc(1, sizeof(*pool));
    if (!pool)
        return MFX_ERR_MEMORY_ALLOC;
    pool->count = request->NumFrameSuggested;
    pool->mids = calloc(pool->count, sizeof(*pool->mids));
    if (!pool->mids) {
        free(pool);
        return MFX_ERR_MEMORY_ALLOC;
    }
    D3D11_TEXTURE2D_DESC desc = {0};
    desc.Width = request->Info.Width;
    desc.Height = request->Info.Height;
    desc.MipLevels = desc.ArraySize = desc.SampleDesc.Count = 1;
    desc.Format = DXGI_FORMAT_NV12;
    desc.Usage = D3D11_USAGE_DEFAULT;
    desc.BindFlags =
        (request->Type & MFX_MEMTYPE_VIDEO_MEMORY_PROCESSOR_TARGET) ? D3D11_BIND_RENDER_TARGET : D3D11_BIND_DECODER;
    if (request->Type & MFX_MEMTYPE_VIDEO_MEMORY_ENCODER_TARGET)
        desc.BindFlags |= D3D11_BIND_VIDEO_ENCODER;
    for (int i = 0; i < pool->count; i++) {
        bk_qsv_mid *mid = calloc(1, sizeof(*mid));
        pool->mids[i] = mid;
        HRESULT hr = E_OUTOFMEMORY;
        if (mid) {
            if (bitstream) {
                D3D11_BUFFER_DESC coded = {0};
                coded.ByteWidth = (UINT)request->Info.Width * request->Info.Height;
                coded.Usage = D3D11_USAGE_STAGING;
                coded.CPUAccessFlags = D3D11_CPU_ACCESS_READ;
                hr = ID3D11Device_CreateBuffer(e->device, &coded, NULL, &mid->bitstream);
            } else
                hr = ID3D11Device_CreateTexture2D(e->device, &desc, NULL, &mid->texture);
        }
        if (FAILED(hr)) {
            bk_qsv_pool_close(pool);
            return MFX_ERR_MEMORY_ALLOC;
        }
    }
    response->AllocId = request->AllocId;
    response->mids = pool->mids;
    response->NumFrameActual = pool->count;
    pool->next = e->pools;
    e->pools = pool;
    return MFX_ERR_NONE;
}
static mfxStatus MFX_CDECL bk_qsv_free(mfxHDL opaque, mfxFrameAllocResponse *response) {
    bk_qsv *e = opaque;
    if (!response)
        return MFX_ERR_NULL_PTR;
    bk_qsv_pool **link = &e->pools;
    while (*link && (*link)->mids != response->mids)
        link = &(*link)->next;
    if (*link) {
        bk_qsv_pool *pool = *link;
        *link = pool->next;
        bk_qsv_pool_close(pool);
    }
    response->mids = NULL;
    response->NumFrameActual = 0;
    return MFX_ERR_NONE;
}
static mfxStatus MFX_CDECL bk_qsv_lock(mfxHDL opaque, mfxMemId memory, mfxFrameData *data) {
    bk_qsv *e = opaque;
    bk_qsv_mid *mid = memory;
    // Legacy runtimes may request a P8 compressed-bitstream buffer. Only that
    // buffer can be mapped; raw NV12 surfaces never have CPU-readable storage.
    if (!mid || !mid->bitstream || !data || mid->mapped)
        return MFX_ERR_LOCK_MEMORY;
    D3D11_MAPPED_SUBRESOURCE mapped = {0};
    if (FAILED(ID3D11DeviceContext_Map(e->context, (ID3D11Resource *)mid->bitstream, 0, D3D11_MAP_READ, 0, &mapped)))
        return MFX_ERR_LOCK_MEMORY;
    mid->mapped = 1;
    data->Y = mapped.pData;
    data->Pitch = (mfxU16)mapped.RowPitch;
    return MFX_ERR_NONE;
}
static mfxStatus MFX_CDECL bk_qsv_unlock(mfxHDL opaque, mfxMemId memory, mfxFrameData *data) {
    bk_qsv *e = opaque;
    bk_qsv_mid *mid = memory;
    if (mid && mid->bitstream && mid->mapped) {
        ID3D11DeviceContext_Unmap(e->context, (ID3D11Resource *)mid->bitstream, 0);
        mid->mapped = 0;
    }
    if (data) {
        data->Y = NULL;
        data->UV = NULL;
        data->Pitch = 0;
    }
    return MFX_ERR_NONE;
}
static mfxStatus MFX_CDECL bk_qsv_handle(mfxHDL opaque, mfxMemId memory, mfxHDL *handle) {
    (void)opaque;
    bk_qsv_mid *mid = memory;
    if (!mid || !handle || (!mid->texture && !mid->bitstream))
        return MFX_ERR_INVALID_HANDLE;
    // D3D11's external-allocator ABI returns the borrowed pair, as in Intel's
    // sample D3D11 allocator. The pool owns the resource until the SDK calls Free.
    mfxHDLPair *pair = (mfxHDLPair *)handle;
    pair->first = mid->bitstream ? (mfxHDL)mid->bitstream : (mfxHDL)mid->texture;
    pair->second = (mfxHDL)(uintptr_t)0;
    return MFX_ERR_NONE;
}

// Intel's PnP software key identifies the active driver package. This avoids
// searching stale DriverStore packages or requiring the VPL dispatcher DLL.
static HMODULE bk_qsv_driver(void) {
    const wchar_t *display = L"{4d36e968-e325-11ce-bfc1-08002be10318}";
    ULONG length = 0;
    ULONG flags = CM_GETIDLIST_FILTER_CLASS | CM_GETIDLIST_FILTER_PRESENT;
    if (CM_Get_Device_ID_List_SizeW(&length, display, flags) != CR_SUCCESS || !length || length > 1048576)
        return NULL;
    wchar_t *devices = calloc(length, sizeof(*devices));
    if (!devices)
        return NULL;
    HMODULE module = NULL;
    if (CM_Get_Device_ID_ListW(display, devices, length, flags) != CR_SUCCESS)
        goto done;
    for (wchar_t *id = devices; *id; id += wcslen(id) + 1) {
        if (!wcsstr(id, L"VEN_8086") && !wcsstr(id, L"ven_8086"))
            continue;
        DEVINST device;
        HKEY key;
        if (CM_Locate_DevNodeW(&device, id, CM_LOCATE_DEVNODE_NORMAL) != CR_SUCCESS ||
            CM_Open_DevNode_Key(device, KEY_QUERY_VALUE, 0, RegDisposition_OpenExisting, &key, CM_REGISTRY_SOFTWARE) !=
                CR_SUCCESS)
            continue;
        const wchar_t *values[] = {L"DriverStorePathForVPL", L"DriverStorePathForMediaSDK"};
        const wchar_t *names[] = {L"libmfx64-gen.dll", L"libmfxhw64.dll"};
        for (int i = 0; i < 2 && !module; ++i) {
            wchar_t path[32768] = {0};
            DWORD bytes = sizeof(path) - 2, type = 0;
            if (RegQueryValueExW(key, values[i], NULL, &type, (BYTE *)path, &bytes) != ERROR_SUCCESS || type != REG_SZ)
                continue;
            size_t n = wcslen(path), dll = wcslen(names[i]);
            if (n < 3 || path[1] != L':' || n + dll + 2 >= sizeof(path) / sizeof(*path))
                continue;
            if (path[n - 1] != L'\\' && path[n - 1] != L'/')
                path[n++] = L'\\';
            memcpy(path + n, names[i], (dll + 1) * sizeof(*path));
            module = LoadLibraryExW(path, NULL, LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR | LOAD_LIBRARY_SEARCH_SYSTEM32);
        }
        RegCloseKey(key);
        if (module)
            break;
    }
done:
    free(devices);
    if (!module)
        module = bk_vendor_library(L"libmfx64-gen.dll");
    if (!module)
        module = bk_vendor_library(L"libmfxhw64.dll");
    return module;
}

static int bk_qsv_status(mfxStatus status, const char *operation, char *error, size_t capacity) {
    // Partial acceleration would violate the hardware-first selection policy.
    if (status >= 0 && status != MFX_WRN_PARTIAL_ACCELERATION)
        return 0;
    snprintf(error, capacity, "%s failed (Intel VPL status %d)", operation, status);
    return -1;
}
#define QSV_TRY(call)                                                                                                  \
    do {                                                                                                               \
        if (bk_qsv_status((call), #call, error, capacity))                                                             \
            goto failed;                                                                                               \
    } while (0)

static void bk_qsv_close(void *opaque) {
    bk_qsv *e = opaque;
    if (!e)
        return;
    if (e->session) {
        if (e->close_encoder)
            e->close_encoder(e->session);
        e->close_session(e->session);
    }
    bk_d3d_converter_close(&e->converter);
    while (e->pools) {
        bk_qsv_pool *pool = e->pools;
        e->pools = pool->next;
        bk_qsv_pool_close(pool);
    }
    if (e->input.texture)
        ID3D11Texture2D_Release(e->input.texture);
    _aligned_free(e->pixels);
    _aligned_free(e->bitstream.Data);
    if (e->context)
        ID3D11DeviceContext_Release(e->context);
    if (e->device)
        ID3D11Device_Release(e->device);
    if (e->module)
        FreeLibrary(e->module);
    free(e);
}

// Intel's Windows VendorImplID identifies the DXGI adapter index (the same
// identity used by VPL's DXGIAdapterIndex filter). Resolve the index by LUID,
// then confirm the loaded runtime actually advertises that implementation.
static int bk_qsv_implementation(HMODULE module, uint64_t luid, mfxU32 *implementation) {
    int index = bk_vendor_adapter_index(0x8086, luid);
    if (index < 0) return 0;
    mfxHDL *(MFX_CDECL *query)(mfxImplCapsDeliveryFormat, mfxU32 *) =
        (void *)GetProcAddress(module, "MFXQueryImplsDescription");
    mfxStatus(MFX_CDECL *release)(mfxHDL) =
        (void *)GetProcAddress(module, "MFXReleaseImplDescription");
    if (!query || !release) return 0;
    mfxU32 count = 0;
    mfxHDL *descriptions = query(MFX_IMPLCAPS_IMPLDESCSTRUCTURE, &count);
    int found = 0;
    if (descriptions) {
        for (mfxU32 i = 0; i < count; ++i) {
            mfxImplDescription *description = descriptions[i];
            if (description && description->VendorID == 0x8086 &&
                description->Impl == MFX_IMPL_TYPE_HARDWARE &&
                description->VendorImplID == (mfxU32)index) {
                *implementation = description->VendorImplID;
                found = 1;
            }
        }
        // The runtime releases the handle array when its last handle is freed.
        for (mfxU32 i = 0; i < count; ++i) release(descriptions[i]);
    }
    return found;
}

// VPL publishes codec/profile/input-format support without creating an encoder.
int bk_qsv_supports(int codec) {
    HMODULE module = bk_qsv_driver();
    if (!module)
        return 0;
    mfxHDL *(MFX_CDECL * query)(mfxImplCapsDeliveryFormat, mfxU32 *) =
        (void *)GetProcAddress(module, "MFXQueryImplsDescription");
    mfxStatus(MFX_CDECL * release)(mfxHDL) = (void *)GetProcAddress(module, "MFXReleaseImplDescription");
    mfxU32 id = codec == 1 ? MFX_CODEC_HEVC : MFX_CODEC_AV1;
    mfxU32 profile = codec == 1 ? MFX_PROFILE_HEVC_MAIN : MFX_PROFILE_AV1_MAIN;
    int supported = 0;
    if (query && release) {
        mfxU32 count = 0;
        mfxHDL *descriptions = query(MFX_IMPLCAPS_IMPLDESCSTRUCTURE, &count);
        for (mfxU32 i = 0; descriptions && i < count; i++) {
            mfxImplDescription *d = descriptions[i];
            if (!d || d->Impl != MFX_IMPL_TYPE_HARDWARE)
                continue;
            for (mfxU16 c = 0; c < d->Enc.NumCodecs; c++) {
                if (d->Enc.Codecs[c].CodecID != id)
                    continue;
                for (mfxU16 p = 0; p < d->Enc.Codecs[c].NumProfiles; p++) {
                    struct encprofile *pr = &d->Enc.Codecs[c].Profiles[p];
                    if (pr->Profile != profile)
                        continue;
                    for (mfxU16 m = 0; m < pr->NumMemTypes; m++)
                        for (mfxU16 f = 0; f < pr->MemDesc[m].NumColorFormats; f++)
                            if (pr->MemDesc[m].ColorFormats[f] == MFX_FOURCC_NV12)
                                supported = 1;
                }
            }
        }
        for (mfxU32 i = 0; descriptions && i < count; i++)
            release(descriptions[i]);
    } else if (codec == 1) {
        // Older Media SDK drivers expose the capability query on a session.
        mfxStatus(MFX_CDECL * init)(mfxInitParam, mfxSession *) = (void *)GetProcAddress(module, "MFXInitEx");
        mfxStatus(MFX_CDECL * close)(mfxSession) = (void *)GetProcAddress(module, "MFXClose");
        mfxStatus(MFX_CDECL * encode_query)(mfxSession, mfxVideoParam *, mfxVideoParam *) =
            (void *)GetProcAddress(module, "MFXVideoENCODE_Query");
        mfxInitParam params = {0};
        mfxSession session = NULL;
        params.Implementation = MFX_IMPL_HARDWARE_ANY | MFX_IMPL_VIA_D3D11;
        params.Version.Major = 1;
        params.Version.Minor = 19;
        mfxStatus status = init && close && encode_query ? init(params, &session) : MFX_ERR_UNSUPPORTED;
        if (status >= 0 && session) {
            mfxVideoParam output = {0};
            output.mfx.CodecId = id;
            mfxStatus result = encode_query(session, NULL, &output);
            supported = status != MFX_WRN_PARTIAL_ACCELERATION && result >= 0 &&
                        result != MFX_WRN_PARTIAL_ACCELERATION && output.mfx.CodecId == id;
            close(session);
        }
    }
    FreeLibrary(module);
    return supported;
}

static void *bk_qsv_open(const bk_vendor_options *o, char *error, size_t capacity) {
    bk_qsv *e = calloc(1, sizeof(*e));
    if (!e) {
        snprintf(error, capacity, "out of memory");
        return NULL;
    }
    e->options = *o;
    e->module = bk_qsv_driver();
    if (!e->module) {
        snprintf(error, capacity, "Intel driver does not provide a native VPL/Media SDK runtime");
        goto failed;
    }
#define QSV_LOAD(field, symbol)                                                                                        \
    do {                                                                                                               \
        e->field = (void *)GetProcAddress(e->module, (symbol));                                                        \
        if (!e->field) {                                                                                               \
            snprintf(error, capacity, "Intel runtime missing %s", (symbol));                                           \
            goto failed;                                                                                               \
        }                                                                                                              \
    } while (0)
    QSV_LOAD(close_session, "MFXClose");
    QSV_LOAD(close_encoder, "MFXVideoENCODE_Close");
    QSV_LOAD(init_encoder, "MFXVideoENCODE_Init");
    QSV_LOAD(get_params, "MFXVideoENCODE_GetVideoParam");
    QSV_LOAD(set_handle, "MFXVideoCORE_SetHandle");
    QSV_LOAD(encode, "MFXVideoENCODE_EncodeFrameAsync");
    QSV_LOAD(sync, "MFXVideoCORE_SyncOperation");
    QSV_LOAD(set_allocator, "MFXVideoCORE_SetFrameAllocator");
#undef QSV_LOAD
    mfxStatus(MFX_CDECL * initialize)(mfxInitializationParam, mfxSession *) =
        (void *)GetProcAddress(e->module, "MFXInitialize");
    mfxStatus(MFX_CDECL * legacy_init)(mfxInitParam, mfxSession *) = (void *)GetProcAddress(e->module, "MFXInitEx");
    if (initialize) {
        mfxInitializationParam init = {0};
        init.AccelerationMode = MFX_ACCEL_MODE_VIA_D3D11;
        if (o->adapter_set && !bk_qsv_implementation(e->module, o->luid, &init.VendorImplID)) {
            snprintf(error, capacity, "Intel runtime cannot identify requested adapter LUID %08x:%08x",
                     (unsigned)(o->luid >> 32), (unsigned)o->luid);
            goto failed;
        }
        QSV_TRY(initialize(init, &e->session));
    } else if (legacy_init && o->codec != 2) {
        mfxInitParam init = {0};
        init.Implementation = MFX_IMPL_HARDWARE_ANY | MFX_IMPL_VIA_D3D11;
        if (o->adapter_set) {
            int index = bk_vendor_adapter_index(0x8086, o->luid);
            const mfxIMPL implementations[] = {MFX_IMPL_HARDWARE, MFX_IMPL_HARDWARE2, MFX_IMPL_HARDWARE3, MFX_IMPL_HARDWARE4};
            if (index < 0 || index >= 4) {
                snprintf(error, capacity, "Intel legacy runtime cannot select requested DXGI adapter");
                goto failed;
            }
            init.Implementation = implementations[index] | MFX_IMPL_VIA_D3D11;
        }
        init.Version.Major = 1;
        init.Version.Minor = 19;
        QSV_TRY(legacy_init(init, &e->session));
    } else {
        snprintf(error, capacity, "Intel runtime has no compatible native initialization API");
        goto failed;
    }
    if (!e->session) {
        snprintf(error, capacity, "Intel runtime returned no session");
        goto failed;
    }
    HRESULT hr = bk_vendor_device(0x8086, o, &e->device, &e->context);
    if (FAILED(hr)) {
        snprintf(error, capacity, "Intel D3D11 device failed: 0x%08lx", (unsigned long)hr);
        goto failed;
    }
    QSV_TRY(e->set_handle(e->session, MFX_HANDLE_D3D11_DEVICE, e->device));
    e->allocator.pthis = e;
    e->allocator.Alloc = bk_qsv_alloc;
    e->allocator.Free = bk_qsv_free;
    e->allocator.Lock = bk_qsv_lock;
    e->allocator.Unlock = bk_qsv_unlock;
    e->allocator.GetHDL = bk_qsv_handle;
    QSV_TRY(e->set_allocator(e->session, &e->allocator));
    mfxVideoParam params = {0};
    params.AsyncDepth = 1;
    params.IOPattern = MFX_IOPATTERN_IN_VIDEO_MEMORY;
    params.mfx.CodecId = o->codec == 2 ? MFX_CODEC_AV1 : o->codec == 1 ? MFX_CODEC_HEVC : MFX_CODEC_AVC;
    params.mfx.CodecProfile = o->codec == 2 ? MFX_PROFILE_AV1_MAIN : o->codec == 1 ? MFX_PROFILE_HEVC_MAIN : MFX_PROFILE_AVC_BASELINE;
    params.mfx.TargetUsage = MFX_TARGETUSAGE_BEST_SPEED;
    params.mfx.GopPicSize = (mfxU16)(((uint64_t)o->num * 2 + o->den - 1) / o->den);
    params.mfx.GopRefDist = 1;
    params.mfx.GopOptFlag = MFX_GOP_CLOSED;
    params.mfx.RateControlMethod = MFX_RATECONTROL_CBR;
    mfxU16 multiplier = (mfxU16)((o->bitrate + 65534) / 65535);
    params.mfx.BRCParamMultiplier = multiplier;
    params.mfx.TargetKbps = params.mfx.MaxKbps = (mfxU16)((o->bitrate + multiplier - 1) / multiplier);
    mfxFrameInfo *info = &params.mfx.FrameInfo;
    info->FourCC = MFX_FOURCC_NV12;
    info->ChromaFormat = MFX_CHROMAFORMAT_YUV420;
    info->PicStruct = MFX_PICSTRUCT_PROGRESSIVE;
    int alignment = o->codec == 2 ? 64 : 16;
    info->Width = (mfxU16)((o->width + alignment - 1) & ~(alignment - 1));
    info->Height = (mfxU16)((o->height + alignment - 1) & ~(alignment - 1));
    info->CropW = o->width;
    info->CropH = o->height;
    info->FrameRateExtN = o->num;
    info->FrameRateExtD = o->den;
    mfxExtCodingOption coding = {0};
    mfxExtCodingOption2 coding2 = {0};
    mfxExtCodingOption3 coding3 = {0};
    mfxExtAV1BitstreamParam av1_bitstream = {0};
    mfxExtBuffer *extensions[] = {(mfxExtBuffer *)&coding, (mfxExtBuffer *)&coding2, (mfxExtBuffer *)&coding3,
                                  (mfxExtBuffer *)&av1_bitstream};
    coding.Header.BufferId = MFX_EXTBUFF_CODING_OPTION;
    coding.Header.BufferSz = sizeof(coding);
    coding2.Header.BufferId = MFX_EXTBUFF_CODING_OPTION2;
    coding2.Header.BufferSz = sizeof(coding2);
    coding3.Header.BufferId = MFX_EXTBUFF_CODING_OPTION3;
    coding3.Header.BufferSz = sizeof(coding3);
    coding3.LowDelayBRC = MFX_CODINGOPTION_ON;
    av1_bitstream.Header.BufferId = MFX_EXTBUFF_AV1_BITSTREAM_PARAM;
    av1_bitstream.Header.BufferSz = sizeof(av1_bitstream);
    av1_bitstream.WriteIVFHeaders = MFX_CODINGOPTION_OFF;
    if (o->codec == 0) {
        coding.CAVLC = MFX_CODINGOPTION_ON;
        coding.MaxDecFrameBuffering = 1;
        coding2.RepeatPPS = MFX_CODINGOPTION_ON;
        coding2.AdaptiveI = coding2.AdaptiveB = MFX_CODINGOPTION_OFF;
        params.ExtParam = extensions;
        params.NumExtParam = 3;
    } else if (o->codec == 1) {
        coding2.RepeatPPS = MFX_CODINGOPTION_ON;
        coding2.AdaptiveI = coding2.AdaptiveB = MFX_CODINGOPTION_OFF;
        params.ExtParam = &extensions[1];
        params.NumExtParam = 2;
    } else {
        params.ExtParam = &extensions[2];
        params.NumExtParam = 2;
    }
    QSV_TRY(e->init_encoder(e->session, &params));
    QSV_TRY(e->get_params(e->session, &params));
    if (params.IOPattern != MFX_IOPATTERN_IN_VIDEO_MEMORY || params.AsyncDepth != 1 || params.mfx.GopRefDist != 1 ||
        params.mfx.RateControlMethod != MFX_RATECONTROL_CBR || info->Width < o->width || info->Height < o->height ||
        info->Width > 8192 || info->Height > 8192 || info->FourCC != MFX_FOURCC_NV12 || info->CropW != o->width ||
        info->CropH != o->height) {
        snprintf(error, capacity, "Intel runtime changed the low-delay NV12 configuration");
        goto failed;
    }
    e->surface.Info = *info;
    size_t pixels = (size_t)info->Width * info->Height;
    e->pixels = _aligned_malloc(pixels * 3 / 2, 32);
    e->bitstream.Data = _aligned_malloc(BK_VENDOR_LIMIT, 32);
    e->bitstream.MaxLength = BK_VENDOR_LIMIT;
    if (!e->pixels || !e->bitstream.Data) {
        snprintf(error, capacity, "out of memory");
        goto failed;
    }
    memset(e->pixels, 16, pixels);
    memset(e->pixels + pixels, 128, pixels / 2);
    D3D11_TEXTURE2D_DESC texture = {0};
    texture.Width = info->Width;
    texture.Height = info->Height;
    texture.MipLevels = texture.ArraySize = texture.SampleDesc.Count = 1;
    texture.Format = DXGI_FORMAT_NV12;
    texture.Usage = D3D11_USAGE_DEFAULT;
    texture.BindFlags = D3D11_BIND_RENDER_TARGET;
    hr = ID3D11Device_CreateTexture2D(e->device, &texture, NULL, &e->input.texture);
    if (FAILED(hr)) {
        snprintf(error, capacity, "Intel input texture allocation failed: 0x%08lx", (unsigned long)hr);
        goto failed;
    }
    e->surface.Data.MemId = &e->input;
    return e;
failed:
    bk_qsv_close(e);
    return NULL;
}

static int bk_qsv_submit(bk_qsv *e, int force, uint8_t **output, size_t *size, char *error, size_t capacity) {
    e->surface.Data.FrameOrder = (mfxU32)e->frame++;
    e->surface.Data.TimeStamp = e->clock;
    e->remainder += (uint64_t)90000 * e->options.den;
    e->clock += e->remainder / e->options.num;
    e->remainder %= e->options.num;
    e->bitstream.DataLength = e->bitstream.DataOffset = 0;
    mfxEncodeCtrl control = {0};
    if (force)
        control.FrameType = MFX_FRAMETYPE_I | MFX_FRAMETYPE_IDR | MFX_FRAMETYPE_REF;
    mfxSyncPoint point = NULL;
    ULONGLONG deadline = GetTickCount64() + 5000;
    mfxStatus status;
    BK_MARK(&e->options, encode_submit);
    for (;;) {
        status = e->encode(e->session, &control, &e->surface, &e->bitstream, &point);
        if (status != MFX_WRN_DEVICE_BUSY)
            break;
        if (GetTickCount64() >= deadline) {
            snprintf(error, capacity, "Intel encode submission timed out");
            goto failed;
        }
        Sleep(1);
    }
    QSV_TRY(status);
    if (!point) {
        snprintf(error, capacity, "Intel returned no immediate output; buffered encoding is unsupported");
        goto failed;
    }
    do {
        status = e->sync(e->session, point, 100);
    } while (status == MFX_WRN_IN_EXECUTION && GetTickCount64() < deadline);
    if (status != MFX_ERR_NONE) {
        snprintf(error, capacity, "Intel encode completion failed or timed out (status %d)", status);
        goto failed;
    }
    BK_MARK(&e->options, encode_complete);
    if (e->bitstream.DataOffset > e->bitstream.MaxLength ||
        e->bitstream.DataLength > e->bitstream.MaxLength - e->bitstream.DataOffset ||
        (e->bitstream.TimeStamp != (mfxU64)MFX_TIMESTAMP_UNKNOWN &&
         e->bitstream.TimeStamp != e->surface.Data.TimeStamp)) {
        snprintf(error, capacity, "Intel returned an invalid or reordered bitstream");
        goto failed;
    }
    return bk_vendor_copy(e->bitstream.Data + e->bitstream.DataOffset, e->bitstream.DataLength, output, size, error,
                          capacity);
failed:
    return -1;
}

static int bk_qsv_encode(void *opaque, const uint8_t *input, int force, uint8_t **output, size_t *size, char *error,
                         size_t capacity) {
    bk_qsv *e = opaque;
    if (e->surface.Data.Locked) {
        snprintf(error, capacity, "Intel retained the previous input surface");
        return -1;
    }
    UINT pitch = e->surface.Info.Width;
    for (int i = 0; i < 2; i++) {
        uint8_t *destination = e->pixels + (i ? (size_t)pitch * e->surface.Info.Height : 0);
        const uint8_t *source = input + (i ? (size_t)e->options.width * e->options.height : 0);
        for (int row = 0; row < e->options.height / (i + 1); row++)
            memcpy(destination + (size_t)row * pitch, source + (size_t)row * e->options.width, e->options.width);
    }
    ID3D11DeviceContext_UpdateSubresource(e->context, (ID3D11Resource *)e->input.texture, 0, NULL, e->pixels, pitch, 0);
    ID3D11DeviceContext_Flush(e->context);
    return bk_qsv_submit(e, force, output, size, error, capacity);
}
static int bk_qsv_encode_texture(void *opaque, uintptr_t handle, int rotation, int force, uint8_t **output,
                                 size_t *size, char *error, size_t capacity) {
    bk_qsv *e = opaque;
    if (e->surface.Data.Locked) {
        snprintf(error, capacity, "Intel retained the previous input surface");
        return -1;
    }
    int result = bk_d3d_convert(&e->converter, e->device, e->context, e->input.texture, &e->options, handle, rotation,
                                error, capacity);
    if (result)
        return result;
    return bk_qsv_submit(e, force, output, size, error, capacity);
}
const bk_vendor_api bk_qsv_api = {bk_qsv_open, bk_qsv_encode, bk_qsv_close, bk_qsv_encode_texture};
