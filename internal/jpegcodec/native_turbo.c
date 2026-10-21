//go:build cgo && !ffmpeg && (linux || windows || darwin)

#include "turbo.h"
#include <limits.h>
#include <stdio.h>
#include <string.h>
#include <stdlib.h>
#ifdef _WIN32
#include <windows.h>
#else
#include <dlfcn.h>
#endif

// The stable TurboJPEG 2 API (libjpeg-turbo >= 2.0). No development headers
// or link-time dependency are needed. Each call owns a separate codec handle.
typedef void *tjhandle;
typedef tjhandle (*init_fn)(void);
typedef int (*destroy_fn)(tjhandle);
typedef int (*compress_fn)(tjhandle, const unsigned char *, int, int, int, int,
                           unsigned char **, unsigned long *, int, int, int);
typedef int (*header_fn)(tjhandle, const unsigned char *, unsigned long, int *, int *, int *, int *);
typedef int (*decompress_fn)(tjhandle, const unsigned char *, unsigned long, unsigned char *, int, int, int, int, int);
typedef void (*free_fn)(unsigned char *);
typedef char *(*error_fn)(tjhandle);
static init_fn init_compress, init_decompress;
static destroy_fn destroy;
static compress_fn compress;
static header_fn header;
static decompress_fn decompress;
static free_fn release;
static error_fn error_string;

#ifdef _WIN32
typedef HMODULE library;
static library open_library(const char *path) {
    int length = MultiByteToWideChar(CP_UTF8, MB_ERR_INVALID_CHARS, path, -1, NULL, 0);
    if (!length) return NULL;
    wchar_t *wide = malloc(length * sizeof(wchar_t));
    if (!wide) return NULL;
    MultiByteToWideChar(CP_UTF8, MB_ERR_INVALID_CHARS, path, -1, wide, length);
    DWORD flags = LOAD_LIBRARY_SEARCH_DEFAULT_DIRS;
    if (strchr(path, '/') || strchr(path, '\\')) flags |= LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR;
    library lib = LoadLibraryExW(wide, NULL, flags);
    free(wide);
    return lib;
}
#define symbol(lib, name) GetProcAddress(lib, name)
#define close_library(lib) FreeLibrary(lib)
#else
typedef void *library;
static library open_library(const char *path) { return dlopen(path, RTLD_NOW | RTLD_LOCAL); }
#define symbol(lib, name) dlsym(lib, name)
#define close_library(lib) dlclose(lib)
#endif

int bk_tj_load(const char *path) {
    library lib = open_library(path);
    if (!lib) return 0;
    // Resolve into local variables so a failed candidate cannot publish a
    // partially populated function table. Go's sync.Once serializes loading.
    init_fn ic = (init_fn)symbol(lib, "tjInitCompress");
    init_fn id = (init_fn)symbol(lib, "tjInitDecompress");
    destroy_fn d = (destroy_fn)symbol(lib, "tjDestroy");
    compress_fn c = (compress_fn)symbol(lib, "tjCompress2");
    header_fn h = (header_fn)symbol(lib, "tjDecompressHeader3");
    decompress_fn dc = (decompress_fn)symbol(lib, "tjDecompress2");
    free_fn f = (free_fn)symbol(lib, "tjFree");
    error_fn e = (error_fn)symbol(lib, "tjGetErrorStr2");
    if (!ic || !id || !d || !c || !h || !dc || !f || !e) {
        close_library(lib);
        return 0;
    }
    init_compress = ic; init_decompress = id; destroy = d;
    compress = c; header = h; decompress = dc; release = f; error_string = e;
    // Retain the module for the process lifetime, like the FFmpeg loader.
    return 1;
}

static void codec_error(tjhandle handle, char *error, size_t size) {
    const char *detail = error_string(handle);
    snprintf(error, size, "%s", detail ? detail : "codec initialization failed");
}

enum { BGRA = 8, SUBSAMPLE_420 = 2 }; // TJPF_BGRA, TJSAMP_420 from turbojpeg.h.

int bk_tj_encode(const unsigned char *pixels, int width, int height, int quality,
                 unsigned char **data, unsigned long *size, char *error, size_t error_size) {
    tjhandle handle = init_compress();
    if (!handle) { codec_error(NULL, error, error_size); return 0; }
    // Consume BGRA directly; no full-frame RGBA scratch buffer. Use accurate
    // DCT and 4:2:0, matching Go JPEG's chroma subsampling and quality scale.
    int ok = compress(handle, pixels, width, width * 4, height, BGRA,
                       data, size, SUBSAMPLE_420, quality, 0) == 0;
    if (!ok) codec_error(handle, error, error_size);
    destroy(handle);
    return ok;
}

int bk_tj_decode(const unsigned char *data, size_t size, unsigned char *pixels,
                 int width, int height, char *error, size_t error_size) {
    if (size > ULONG_MAX) {
        snprintf(error, error_size, "JPEG input is too large");
        return 0;
    }
    tjhandle handle = init_decompress();
    if (!handle) { codec_error(NULL, error, error_size); return 0; }
    int w = 0, h = 0, subsampling = 0, colorspace = 0;
    int ok = header(handle, data, (unsigned long)size, &w, &h, &subsampling, &colorspace) == 0;
    if (!ok) codec_error(handle, error, error_size);
    else if (w != width || h != height) {
        snprintf(error, error_size, "JPEG dimensions changed");
        ok = 0;
    } else {
        ok = decompress(handle, data, (unsigned long)size, pixels, width, width * 4, height, BGRA, 0) == 0;
        if (!ok) codec_error(handle, error, error_size);
    }
    destroy(handle);
    return ok;
}

void bk_tj_free(unsigned char *data) { release(data); }
