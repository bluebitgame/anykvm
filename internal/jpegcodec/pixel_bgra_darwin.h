#include <CoreVideo/CoreVideo.h>

// The returned BGRA buffer is retained and locked until unlock; input is borrowed.
CVPixelBufferRef bk_jpeg_pixel_lock_bgra(CVPixelBufferRef input);
void bk_jpeg_pixel_unlock_bgra(CVPixelBufferRef pixel);
