// Compile the production entropy entry with the Windows FXC optimizer, no GPU.
#define COBJMACROS
#include <windows.h>
#include <d3dcompiler.h>
#include <fcntl.h>
#include <io.h>
#include <stdio.h>
#include <stdlib.h>

typedef HRESULT (WINAPI *compile_fn)(LPCVOID, SIZE_T, LPCSTR, const D3D_SHADER_MACRO *,
    ID3DInclude *, LPCSTR, LPCSTR, UINT, UINT, ID3DBlob **, ID3DBlob **);

int main(void) {
    const size_t capacity = 1024 * 1024;
    char *source = malloc(capacity);
    if (!source) return 2;
    _setmode(_fileno(stdin), _O_BINARY);
    size_t size = fread(source, 1, capacity, stdin);
    if (!size || !feof(stdin) || ferror(stdin)) {
        fprintf(stderr, "invalid HLSL input\n");
        free(source);
        return 2;
    }
    HMODULE compiler = LoadLibraryExW(L"d3dcompiler_47.dll", NULL, LOAD_LIBRARY_SEARCH_SYSTEM32);
    compile_fn compile = compiler ? (compile_fn)(void *)GetProcAddress(compiler, "D3DCompile") : NULL;
    if (!compile) {
        fprintf(stderr, "D3DCompile unavailable (%lu)\n", (unsigned long)GetLastError());
        if (compiler) FreeLibrary(compiler);
        free(source);
        return 2;
    }
    ID3DBlob *code = NULL, *messages = NULL;
    ULONGLONG started = GetTickCount64();
    fprintf(stderr, "encodeMCUs: FXC O3 compile started\n");
    fflush(stderr);
    HRESULT hr = compile(source, size, "anykvm-jpeg", NULL, NULL, "encodeMCUs", "cs_5_0",
        D3DCOMPILE_OPTIMIZATION_LEVEL3 | D3DCOMPILE_ENABLE_STRICTNESS, 0, &code, &messages);
    fprintf(stderr, "encodeMCUs: FXC O3 compile finished in %llums (0x%08lx), DXBC=%zu bytes\n",
        (unsigned long long)(GetTickCount64() - started), (unsigned long)hr,
        code ? ID3D10Blob_GetBufferSize(code) : 0);
    if (messages) {
        fwrite(ID3D10Blob_GetBufferPointer(messages), 1, ID3D10Blob_GetBufferSize(messages), stderr);
        ID3D10Blob_Release(messages);
    }
    int ok = SUCCEEDED(hr) && code && ID3D10Blob_GetBufferSize(code) > 0;
    if (code) ID3D10Blob_Release(code);
    FreeLibrary(compiler);
    free(source);
    return ok ? 0 : 1;
}
