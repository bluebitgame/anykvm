#ifndef BK_D3D11_VIDEO_WINDOWS_H
#define BK_D3D11_VIDEO_WINDOWS_H
// Included after the D3D11 interfaces and bk_vendor_options definition.
typedef struct {
    HANDLE shared_handle;
    ID3D11Texture2D *shared_texture;
    IDXGIKeyedMutex *shared_mutex;
    ID3D11VideoDevice *video_device;
    ID3D11VideoContext *video_context;
    ID3D11VideoProcessorEnumerator *enumerator;
    ID3D11VideoProcessor *processor;
    ID3D11VideoProcessorInputView *input_view;
    ID3D11VideoProcessorOutputView *output_view;
    UINT input_width, input_height;
    ID3D11Texture2D *destination; // borrowed; output_view retains the resource
    UINT output_width, output_height;
} bk_d3d_converter;
void bk_d3d_converter_close(bk_d3d_converter *);
// 0 means GPU conversion succeeded; 1 means no input was encoded and CPU
// fallback is safe. No staging resources or CPU mappings are used here.
int bk_d3d_convert(bk_d3d_converter *, ID3D11Device *, ID3D11DeviceContext *, ID3D11Texture2D *,
                   const bk_vendor_options *, uintptr_t, int, char *, size_t);
#endif
