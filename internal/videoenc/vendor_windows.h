#ifndef BK_VENDOR_WINDOWS_H
#define BK_VENDOR_WINDOWS_H
#include <stdint.h>
#include <stddef.h>

typedef struct bk_vendor_encoder bk_vendor_encoder;
typedef struct {
    int64_t conversion_start, conversion_end, encode_submit, encode_complete;
} bk_vendor_timing;
void bk_vendor_last_timing(bk_vendor_encoder *, bk_vendor_timing *);
int bk_vendor_supports(int vendor, int codec);
bk_vendor_encoder *bk_vendor_open(int vendor, int width, int height, int num, int den,
                                 int bitrate, int codec, int adapter_set, uint64_t luid, int timing, char *error, size_t capacity);
int bk_vendor_encode(bk_vendor_encoder *, const uint8_t *, size_t, int,
                     uint8_t **, size_t *, char *, size_t);
int bk_vendor_encode_texture(bk_vendor_encoder *, uintptr_t, int, int,
                             uint8_t **, size_t *, char *, size_t);
const uint8_t *bk_vendor_probe(bk_vendor_encoder *, size_t *);
void bk_vendor_close(bk_vendor_encoder *);

#ifdef BK_VENDOR_IMPLEMENTATION
#define COBJMACROS
#include <windows.h>
#include <d3d11.h>
#include <dxgi.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define BK_VENDOR_LIMIT (32u * 1024u * 1024u)
typedef struct {
    int width, height, num, den, bitrate, codec;
    int adapter_set;
    uint64_t luid;
    bk_vendor_timing *timing;
    void *volatile *phase; // Static stage name, published by the worker for stalled waits.
} bk_vendor_options;
static inline void bk_vendor_phase(const bk_vendor_options *o, const char *phase) {
    if (o->phase) InterlockedExchangePointer(o->phase, (PVOID)phase);
}
static inline void bk_vendor_mark(int64_t *field) {
    LARGE_INTEGER clock;
    QueryPerformanceCounter(&clock);
    *field = clock.QuadPart;
}
#define BK_MARK(o, field) do { if ((o)->timing) bk_vendor_mark(&(o)->timing->field); } while (0)
#include "d3d11_video_windows.h"

typedef struct {
    void *(*open)(const bk_vendor_options *, char *, size_t);
    int (*encode)(void *, const uint8_t *, int, uint8_t **, size_t *, char *, size_t);
    void (*close)(void *);
    int (*encode_texture)(void *, uintptr_t, int, int, uint8_t **, size_t *, char *, size_t);
} bk_vendor_api;
extern const bk_vendor_api bk_nvenc_api, bk_amf_api, bk_qsv_api;
int bk_nvenc_supports(int);
int bk_amf_supports(int);
int bk_qsv_supports(int);

// Load driver libraries only from the system directory. No PATH/CWD search.
HMODULE bk_vendor_library(const wchar_t *name);
HRESULT bk_vendor_device(UINT vendor, const bk_vendor_options *, ID3D11Device **, ID3D11DeviceContext **);
int bk_vendor_adapter_index(UINT vendor, uint64_t luid);
int bk_vendor_copy(const void *, size_t, uint8_t **, size_t *, char *, size_t);
#endif
#endif
