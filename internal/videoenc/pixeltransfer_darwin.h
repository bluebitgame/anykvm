#ifndef ANYKVM_PIXELTRANSFER_DARWIN_H
#define ANYKVM_PIXELTRANSFER_DARWIN_H
#import <VideoToolbox/VideoToolbox.h>
#include <string.h>

static OSStatus bk_transfer_nv12(VTPixelTransferSessionRef transfer, CVPixelBufferRef source, CVPixelBufferRef destination) {
 // Letterbox can leave a chroma pair at a fractional content boundary untouched
 // (e.g. 1206x2622 into 1920x1080). A reused pool buffer would expose stale UV
 // as a colored vertical line. Initialize valid black, including row padding.
 if (!destination || CVPixelBufferGetPixelFormatType(destination) != kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange ||
     CVPixelBufferGetPlaneCount(destination) != 2) return kCVReturnInvalidPixelFormat;
 CVReturn status = CVPixelBufferLockBaseAddress(destination, 0);
 if (status) return status;
 for (size_t plane = 0; plane < 2; plane++) {
  memset(CVPixelBufferGetBaseAddressOfPlane(destination, plane), plane ? 128 : 16,
   CVPixelBufferGetBytesPerRowOfPlane(destination, plane) * CVPixelBufferGetHeightOfPlane(destination, plane));
 }
 CVPixelBufferUnlockBaseAddress(destination, 0);
 return VTPixelTransferSessionTransferImage(transfer, source, destination);
}
#endif
