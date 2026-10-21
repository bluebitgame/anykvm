"""Execute native Windows startup logic with counters instead of GPU drivers."""
import pathlib
import shutil
import subprocess
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parent.parent


def function_source(filename, signature):
    source = (ROOT / "internal/videoenc" / filename).read_text()
    start = source.index(signature)
    end = source.index("\n}", start) + 2
    return source[start:end]


class WindowsCodecStartupTests(unittest.TestCase):
    def compile_and_run(self, source):
        compiler = shutil.which("cc") or shutil.which("clang")
        if not compiler:
            self.skipTest("C compiler unavailable")
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory)
            (path / "test.c").write_text(source)
            subprocess.run([compiler, "-std=c11", str(path / "test.c"), "-o", str(path / "test")], check=True)
            subprocess.run([str(path / "test")], check=True)

    def test_vendor_startup_never_encodes_hevc_or_av1(self):
        source = r'''
#include <assert.h>
#include <stdint.h>
#include <stdlib.h>
#include <stdio.h>
#include <string.h>
typedef unsigned long DWORD;
typedef int HRESULT;
#define WINAPI
#define COINIT_MULTITHREADED 0
#define SUCCEEDED(hr) ((hr)>=0)
#define INFINITE 0xffffffff
static int encoded, opened, closed, signaled;
typedef struct { int width,height,codec; } bk_vendor_options;
typedef struct {
    void *(*open)(const bk_vendor_options *,char *,size_t);
    int (*encode)(void *,const uint8_t *,int,uint8_t **,size_t *,char *,size_t);
    void (*close)(void *);
    int (*encode_texture)(void *,uintptr_t,int,int,uint8_t **,size_t *,char *,size_t);
} bk_vendor_api;
typedef struct {
    bk_vendor_options options; const bk_vendor_api *api;
    void *backend,*done,*request;
    uint8_t *output;size_t output_size;
    const uint8_t *input; uintptr_t texture_handle;
    int result,stop,rotation,force;char error[512];
} bk_vendor_encoder;
static HRESULT CoInitializeEx(void *unused,int flags) {(void)unused;(void)flags;return 0;}
static void CoUninitialize(void) {}
static void SetEvent(void *event) {(void)event;signaled++;}
static void WaitForSingleObject(void *event,unsigned long timeout) {(void)event;(void)timeout;}
static void *open_backend(const bk_vendor_options *o,char *error,size_t n) {
    (void)error;(void)n;opened++;return (void *)o;
}
static int encode(void *e,const uint8_t *input,int force,uint8_t **output,size_t *size,char *error,size_t n) {
    (void)e;(void)force;(void)output;(void)size;(void)error;(void)n;
    assert(input);encoded++;return 0;
}
static void close_backend(void *e) {assert(e);closed++;}
'''
        source += function_source("vendor_windows.c", "static DWORD WINAPI bk_vendor_worker")
        source += r'''
int main(void) {
    bk_vendor_api api={.open=open_backend,.encode=encode,.close=close_backend};
    for(int codec=0;codec<=2;codec++) {
        encoded=opened=closed=signaled=0;
        // Stop after initialization so any encode call can only be a trial.
        bk_vendor_encoder e={.options={.width=1280,.height=768,.codec=codec},.api=&api,.stop=1};
        bk_vendor_worker(&e);
        assert(e.result==0 && opened==1 && closed==1 && signaled==1);
        assert(encoded==(codec==0?1:0));
    }
    return 0;
}
'''
        self.compile_and_run(source)

    def test_media_foundation_startup_never_encodes_hevc_or_av1(self):
        source = r'''
#include <assert.h>
#include <stdint.h>
#include <stdlib.h>
#include <stdio.h>
#include <string.h>
#include <wchar.h>
typedef int HRESULT;
typedef unsigned int UINT32;
typedef wchar_t WCHAR;
typedef struct {int unused;} IMFActivate;
typedef struct {int major,subtype;} MFT_REGISTER_TYPE_INFO;
#define MFMediaType_Video 1
#define MFVideoFormat_NV12 2
#define MFT_CATEGORY_VIDEO_ENCODER 3
#define MFT_ENUM_FLAG_HARDWARE 4
#define MFT_ENUM_FLAG_SYNCMFT 8
#define MFT_ENUM_FLAG_SORTANDFILTER 16
#define MF_E_TOPO_CODEC_NOT_FOUND -1
#define E_OUTOFMEMORY -2
static const int MFT_FRIENDLY_NAME_Attribute = 0;
#define CP_UTF8 65001
#define FAILED(hr) ((hr)<0)
#define SUCCEEDED(hr) ((hr)>=0)
static int encoded, configured;
typedef struct {
    int hardware,codec,width,height,force_idr;
    const uint8_t *input;size_t input_size;
    char operation[512],name[256];
    HRESULT (*enumerate)(int,UINT32,const MFT_REGISTER_TYPE_INFO *,const MFT_REGISTER_TYPE_INFO *,IMFActivate ***,UINT32 *);
} bk_mf_encoder;
static int *bk_subtype(int codec) {static int subtypes[3]={0,1,2};return &subtypes[codec];}
static HRESULT bk_configure(bk_mf_encoder *e,IMFActivate *a) {(void)e;(void)a;configured++;return 0;}
static HRESULT bk_encode_frame(bk_mf_encoder *e) {assert(e->input && e->input_size);encoded++;return 0;}
static void bk_reset(bk_mf_encoder *e) {(void)e;}
static HRESULT IMFActivate_GetAllocatedString(IMFActivate *a,const int *key,WCHAR **friendly,UINT32 *n) {
    (void)a;(void)key;(void)friendly;(void)n;return -1;
}
static int WideCharToMultiByte(int page,int flags,const WCHAR *src,int len,char *dst,int capacity,void *def,void *used) {
    (void)page;(void)flags;(void)src;(void)len;(void)dst;(void)capacity;(void)def;(void)used;return 0;
}
static void CoTaskMemFree(void *p) {free(p);}
static void IMFActivate_Release(IMFActivate *a) {(void)a;}
static HRESULT enumerate(int category,UINT32 flags,const MFT_REGISTER_TYPE_INFO *input,const MFT_REGISTER_TYPE_INFO *output,IMFActivate ***items,UINT32 *count) {
    (void)category;(void)flags;(void)input;(void)output;
    static IMFActivate activation;
    *items=malloc(sizeof(**items));assert(*items);(*items)[0]=&activation;*count=1;return 0;
}
'''
        source += function_source("mediafoundation_windows.c", "static HRESULT bk_initialize")
        source += r'''
int main(void) {
    for(int codec=0;codec<=2;codec++) {
        encoded=configured=0;
        bk_mf_encoder e={.hardware=1,.codec=codec,.width=1280,.height=768,.enumerate=enumerate};
        assert(bk_initialize(&e)==0 && configured==1);
        assert(encoded==(codec==0?1:0));
        assert(!e.input && e.input_size==0);
    }
    return 0;
}
'''
        self.compile_and_run(source)


if __name__ == "__main__":
    unittest.main()
