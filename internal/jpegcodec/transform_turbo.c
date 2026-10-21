//go:build cgo && !ffmpeg && (linux || windows || darwin)

#include "transform_turbo.h"
#include <limits.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#ifdef _WIN32
#include <windows.h>
#else
#include <dlfcn.h>
#endif

// Stable TurboJPEG 2 ABI for lossless transforms in non-FFmpeg builds.
// No development headers needed.
typedef void *jt_handle;
typedef struct { int x,y,w,h; } jt_region;
typedef struct jt_transform {
    jt_region r;
    int op,options;
    void *data;
    int (*filter)(short *,jt_region,jt_region,int,int,struct jt_transform *);
} jt_transform;
static jt_handle (*jt_init)(void);
static int (*jt_destroy)(jt_handle);
static int (*jt_header)(jt_handle,const unsigned char *,unsigned long,int *,int *,int *,int *);
static int (*jt_transform_images)(jt_handle,const unsigned char *,unsigned long,int,unsigned char **,unsigned long *,jt_transform *,int);
static void (*jt_free)(unsigned char *);
static char *(*jt_error)(jt_handle);

int bk_jt_load(const char *path) {
#ifdef _WIN32
    int n=MultiByteToWideChar(CP_UTF8,MB_ERR_INVALID_CHARS,path,-1,NULL,0);
    if(!n) return 0;
    wchar_t *wide=malloc((size_t)n*sizeof(wchar_t));
    if(!wide) return 0;
    MultiByteToWideChar(CP_UTF8,MB_ERR_INVALID_CHARS,path,-1,wide,n);
    DWORD flags=LOAD_LIBRARY_SEARCH_DEFAULT_DIRS;
    if(strchr(path,'/')||strchr(path,'\\')) flags|=LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR;
    HMODULE lib=LoadLibraryExW(wide,NULL,flags); free(wide);
#define JT_SYMBOL(name) GetProcAddress(lib,name)
#define JT_CLOSE() FreeLibrary(lib)
#else
    void *lib=dlopen(path,RTLD_NOW|RTLD_LOCAL);
#define JT_SYMBOL(name) dlsym(lib,name)
#define JT_CLOSE() dlclose(lib)
#endif
    if(!lib) return 0;
    // Publishing is serialized by sync.Once; failed candidates publish nothing.
    void *a=(void *)JT_SYMBOL("tjInitTransform"),*b=(void *)JT_SYMBOL("tjDestroy"),
         *c=(void *)JT_SYMBOL("tjDecompressHeader3"),*d=(void *)JT_SYMBOL("tjTransform"),
         *e=(void *)JT_SYMBOL("tjFree"),*f=(void *)JT_SYMBOL("tjGetErrorStr2");
    if(!a||!b||!c||!d||!e||!f) { JT_CLOSE(); return 0; }
    jt_init=(jt_handle (*)(void))a; jt_destroy=(int (*)(jt_handle))b;
    jt_header=(int (*)(jt_handle,const unsigned char *,unsigned long,int *,int *,int *,int *))c;
    jt_transform_images=(int (*)(jt_handle,const unsigned char *,unsigned long,int,unsigned char **,unsigned long *,jt_transform *,int))d;
    jt_free=(void (*)(unsigned char *))e; jt_error=(char *(*)(jt_handle))f;
    return 1;
}

void bk_jt_tiles_free(bk_jpeg_tile *tiles,int count) {
    if(tiles) { for(int i=0;i<count;i++) if(tiles[i].data) jt_free(tiles[i].data); free(tiles); }
}

int bk_jt_tiles(const unsigned char *data,size_t size,int tile_size,
                bk_jpeg_tile **tiles,int *count,int *width,int *height,char *error,size_t error_size) {
    *tiles=NULL; *count=0;
    if(!data||size>ULONG_MAX||tile_size<32||tile_size>2048||tile_size%32) return 0;
    jt_handle handle=jt_init();
    if(!handle) { snprintf(error,error_size,"TurboJPEG transform initialization failed"); return 0; }
    int subsampling=0,colorspace=0,ok=0,n=0;
    unsigned char **outputs=NULL;
    unsigned long *sizes=NULL;
    jt_transform *transforms=NULL;
    bk_jpeg_tile *result=NULL;
    if(jt_header(handle,data,(unsigned long)size,width,height,&subsampling,&colorspace)<0) goto failed;
    // All supported iMCU axes divide 32. Restrict color interpretation to
    // YCbCr/grayscale. Preserve source markers, including color profiles.
    if(*width<1||*height<1||*width>8192||*height>8192||subsampling<0||subsampling>6||
       (colorspace!=1&&colorspace!=2)) { snprintf(error,error_size,"Unsupported JPEG crop layout"); goto done; }
    int cols=(*width+tile_size-1)/tile_size,rows=(*height+tile_size-1)/tile_size;
    n=cols*rows;
    outputs=calloc((size_t)n,sizeof(*outputs)); sizes=calloc((size_t)n,sizeof(*sizes));
    transforms=calloc((size_t)n,sizeof(*transforms)); result=calloc((size_t)n,sizeof(*result));
    if(!outputs||!sizes||!transforms||!result) { snprintf(error,error_size,"JPEG crop allocation failed"); goto done; }
    for(int i=0;i<n;i++) {
        int x=(i%cols)*tile_size,y=(i/cols)*tile_size;
        int w=*width-x,h=*height-y; if(w>tile_size) w=tile_size; if(h>tile_size) h=tile_size;
        transforms[i].r=(jt_region){x,y,w,h};
        transforms[i].options=4; // TJXOPT_CROP; preserve source metadata/color profiles.
        result[i].x=x; result[i].y=y; result[i].w=w; result[i].h=h;
    }
    // One call reads/entropy-decodes the input coefficients once for all crops.
    // No inverse DCT, forward DCT, pixel conversion or quantization is performed.
    if(jt_transform_images(handle,data,(unsigned long)size,n,outputs,sizes,transforms,0)<0) goto failed;
    for(int i=0;i<n;i++) { result[i].data=outputs[i]; result[i].size=sizes[i]; outputs[i]=NULL; }
    *tiles=result; *count=n; result=NULL; ok=1;
    goto done;
failed:
    snprintf(error,error_size,"%s",jt_error(handle));
done:
    if(outputs) for(int i=0;i<n;i++) if(outputs[i]) jt_free(outputs[i]);
    free(outputs); free(sizes); free(transforms); bk_jt_tiles_free(result,n);
    jt_destroy(handle);
    return ok;
}
