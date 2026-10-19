#import <Foundation/Foundation.h>
#import "../pixeltransfer_darwin.h"
#include <assert.h>

static CVPixelBufferRef pixel(int width, int height) {
 CVPixelBufferRef result = NULL;
 NSDictionary *attrs = @{(id)kCVPixelBufferIOSurfacePropertiesKey:@{}};
 assert(CVPixelBufferCreate(NULL, width, height, kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange, (CFDictionaryRef)attrs, &result) == noErr);
 return result;
}
static void fill(CVPixelBufferRef pixel, int y, int u, int v) {
 assert(CVPixelBufferLockBaseAddress(pixel, 0) == noErr);
 for (size_t p = 0; p < 2; p++) {
  size_t stride = CVPixelBufferGetBytesPerRowOfPlane(pixel, p), height = CVPixelBufferGetHeightOfPlane(pixel, p);
  uint8_t *bytes = CVPixelBufferGetBaseAddressOfPlane(pixel, p);
  for (size_t row = 0; row < height; row++) for (size_t x = 0; x < stride; x++) bytes[row*stride+x] = p ? (x%2 ? v : u) : y;
 }
 CVPixelBufferUnlockBaseAddress(pixel, 0);
}
int main(void) { @autoreleasepool {
 VTPixelTransferSessionRef transfer = NULL;
 assert(VTPixelTransferSessionCreate(NULL, &transfer) == noErr);
 assert(VTSessionSetProperty(transfer, kVTPixelTransferPropertyKey_ScalingMode, kVTScalingMode_Letterbox) == noErr);
 for (int portrait = 0; portrait < 2; portrait++) {
  CVPixelBufferRef source = pixel(portrait ? 1206 : 2622, portrait ? 2622 : 1206);
  fill(source, 128, 128, 128);
  for (int width = 1280; width <= 2560; width += 640) {
   int height = width * 9 / 16;
   CVPixelBufferRef destination = pixel(width, height);
   for (int reuse = 0; reuse < 2; reuse++) {
    // Dirty chroma makes a missing boundary write deterministic. Repeat after
    // transfer to represent pooled storage left by a different source frame.
    fill(destination, 47, reuse ? 209 : 83, reuse ? 37 : 179);
    assert(bk_transfer_nv12(transfer, source, destination) == noErr);
    assert(CVPixelBufferLockBaseAddress(destination, kCVPixelBufferLock_ReadOnly) == noErr);
    uint8_t *uv = CVPixelBufferGetBaseAddressOfPlane(destination, 1);
    size_t pitch = CVPixelBufferGetBytesPerRowOfPlane(destination, 1);
    for (int y = 0; y < height / 2; y++) for (int x = 0; x < width; x++) assert(uv[y*pitch+x] == 128);
    uint8_t *luma = CVPixelBufferGetBaseAddressOfPlane(destination, 0);
    size_t yp = CVPixelBufferGetBytesPerRowOfPlane(destination, 0);
    assert(luma[height/2*yp+width/2] == 128); // Content still transfers.
    assert(luma[0] == 16); // Letterbox margin remains video-range black.
    CVPixelBufferUnlockBaseAddress(destination, kCVPixelBufferLock_ReadOnly);
   }
   CVPixelBufferRelease(destination);
  }
  // The capture-owned source remains unchanged.
  assert(CVPixelBufferLockBaseAddress(source, kCVPixelBufferLock_ReadOnly) == noErr);
  for (size_t p = 0; p < 2; p++) {
   uint8_t *data = CVPixelBufferGetBaseAddressOfPlane(source, p);
   size_t size = CVPixelBufferGetBytesPerRowOfPlane(source, p) * CVPixelBufferGetHeightOfPlane(source, p);
   for (size_t x = 0; x < size; x++) assert(data[x] == 128);
  }
  CVPixelBufferUnlockBaseAddress(source, kCVPixelBufferLock_ReadOnly);
  CVPixelBufferRelease(source);
 }
 CFRelease(transfer); return 0;
} }
