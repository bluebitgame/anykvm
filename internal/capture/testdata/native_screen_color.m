#include "../screen_darwin.m"
#include <assert.h>
#include <string.h>

static void policy_tests(void) {
 BKScreen *s = [BKScreen new];
 s->codec = 1;
 assert(bk_screen_capture_format(s) == kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange);
 s->rdpFullRange = YES;
 assert(bk_screen_capture_format(s) == kCVPixelFormatType_420YpCbCr8BiPlanarFullRange);
 assert(bk_screen_encoder_format(s) == kCVPixelFormatType_420YpCbCr8BiPlanarFullRange);
 s->rdpFullRange = NO; s->codec = 2; // GameStream HEVC keeps video range.
 assert(bk_screen_capture_format(s) == kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange);
 s->codec = 3; // RDP Planar keeps its existing NV12 conversion contract.
 assert(bk_screen_capture_format(s) == kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange);
 s->codec = 0;
 assert(bk_screen_capture_format(s) == kCVPixelFormatType_32BGRA);
 [s shutdown]; [s release];
 NSMutableDictionary *extensions = [@{
  (id)kCMFormatDescriptionExtension_FullRangeVideo:@YES,
  (id)kCMFormatDescriptionExtension_YCbCrMatrix:(id)kCVImageBufferYCbCrMatrix_ITU_R_709_2,
  (id)kCMFormatDescriptionExtension_ColorPrimaries:(id)kCVImageBufferColorPrimaries_ITU_R_709_2,
  (id)kCMFormatDescriptionExtension_TransferFunction:(id)kCVImageBufferTransferFunction_sRGB
 } mutableCopy];
 for (int test = 0; test < 5; test++) {
  if (test == 1) extensions[(id)kCMFormatDescriptionExtension_FullRangeVideo] = @NO;
  if (test == 2) [extensions removeObjectForKey:(id)kCMFormatDescriptionExtension_FullRangeVideo];
  if (test == 3) {
   extensions[(id)kCMFormatDescriptionExtension_FullRangeVideo] = @YES;
   extensions[(id)kCMFormatDescriptionExtension_YCbCrMatrix] = (id)kCVImageBufferYCbCrMatrix_ITU_R_601_4;
  }
  if (test == 4) {
   [extensions removeObjectForKey:(id)kCMFormatDescriptionExtension_YCbCrMatrix];
   [extensions removeObjectForKey:(id)kCMFormatDescriptionExtension_ColorPrimaries];
  }
  CMVideoFormatDescriptionRef format = NULL;
  assert(CMVideoFormatDescriptionCreate(NULL, kCMVideoCodecType_H264, 1280, 720, (CFDictionaryRef)extensions, &format) == noErr);
  assert(bk_screen_full_range_output(format) == (test == 0 || test == 4));
  if (test == 1) {
   // A mismatched compressed frame is rejected before entering the frame queue.
   CMSampleBufferRef sample = NULL;
   assert(CMSampleBufferCreateReady(NULL, NULL, format, 0, 0, NULL, 0, NULL, &sample) == noErr);
   BKScreen *failed = [BKScreen new]; failed->rdpFullRange = YES; failed->inFlight = 1;
   bk_encode_output(failed, NULL, noErr, 0, sample);
   assert(failed->rangeRejected && failed->frames.count == 0);
   void *lease, *data, *pixel; int size, w, h, sw, sh; long long age; char error[512];
   assert(bk_screen_take_geometry(failed, &lease, &data, &pixel, &size, &w, &h, &sw, &sh, NULL, &age, error, sizeof(error)) == -2);
   [failed shutdown]; [failed release]; CFRelease(sample);
  }
  CFRelease(format);
 }
 [extensions release];
 BKScreen *submission = [BKScreen new]; submission->rdpFullRange = YES; submission->inFlight = 1;
 bk_encode_output(submission, NULL, kVTParameterErr, 0, NULL);
 assert(submission->rangeRejected && submission->frames.count == 0);
 [submission shutdown]; [submission release];
}

static CVPixelBufferRef decodedPixel;
static OSStatus decodeStatus;
static void decoded(void *ref, void *frame, OSStatus status, VTDecodeInfoFlags flags, CVImageBufferRef pixel, CMTime pts, CMTime duration) {
 decodeStatus = status;
 if (pixel) decodedPixel = CVPixelBufferRetain(pixel);
}

static int hardware_test(void) {
 BKScreen *s = [BKScreen new]; s->codec = 1; s->fps = 30;
 s->width = 1280; s->height = 720; s->rdpFullRange = YES;
 NSString *error = nil;
 bk_prepare_encoder(s, 12000, &error);
 if (error) { fprintf(stderr, "encoder: %s\n", error.UTF8String); [s shutdown]; [s release]; return 1; }
 CVPixelBufferRef pixel = NULL;
 assert(CVPixelBufferCreate(NULL, s->width, s->height, kCVPixelFormatType_420YpCbCr8BiPlanarFullRange, NULL, &pixel) == noErr);
 CVBufferSetAttachment(pixel, kCVImageBufferYCbCrMatrixKey, kCVImageBufferYCbCrMatrix_ITU_R_709_2, kCVAttachmentMode_ShouldPropagate);
 CVBufferSetAttachment(pixel, kCVImageBufferColorPrimariesKey, kCVImageBufferColorPrimaries_ITU_R_709_2, kCVAttachmentMode_ShouldPropagate);
 CVBufferSetAttachment(pixel, kCVImageBufferTransferFunctionKey, kCVImageBufferTransferFunction_sRGB, kCVAttachmentMode_ShouldPropagate);
 const uint8_t levels[] = {0, 16, 128, 235, 255};
 assert(CVPixelBufferLockBaseAddress(pixel, 0) == noErr);
 uint8_t *y = CVPixelBufferGetBaseAddressOfPlane(pixel, 0);
 size_t pitch = CVPixelBufferGetBytesPerRowOfPlane(pixel, 0);
 for (int row = 0; row < s->height; row++) for (int band = 0; band < 5; band++) memset(y+row*pitch+band*256, levels[band], 256);
 memset(CVPixelBufferGetBaseAddressOfPlane(pixel, 1), 128, CVPixelBufferGetBytesPerRowOfPlane(pixel, 1)*s->height/2);
 CVPixelBufferUnlockBaseAddress(pixel, 0);
 dispatch_sync(s->queue, ^{ [s encodePixel:pixel atTime:kCMTimeZero repeat:NO]; });
 assert(VTCompressionSessionCompleteFrames(s->encoder, kCMTimeInvalid) == noErr);
 if (s->failure) { fprintf(stderr, "output: %s\n", s->failure.UTF8String); CVPixelBufferRelease(pixel); [s shutdown]; [s release]; return 1; }
 assert(s->rangeVerified && s->frames.count == 1);
 NSData *annex = s->frames[0][@"data"];
 const uint8_t *bytes = annex.bytes; size_t length = annex.length;
 const uint8_t *parameters[2] = {NULL, NULL}; size_t sizes[2] = {0, 0};
 NSMutableData *avcc = [NSMutableData data];
 for (size_t pos = 0; pos+4 < length;) {
  assert(!memcmp(bytes+pos, "\0\0\0\1", 4));
  size_t begin = pos+4, end = begin;
  while (end+4 <= length && memcmp(bytes+end, "\0\0\0\1", 4)) end++;
  if (end+4 > length) end = length;
  size_t n = end-begin; int type = bytes[begin]&31;
  if (type == 7 || type == 8) { int i = type-7; parameters[i] = bytes+begin; sizes[i] = n; }
  uint32_t network = CFSwapInt32HostToBig((uint32_t)n);
  [avcc appendBytes:&network length:4]; [avcc appendBytes:bytes+begin length:n]; pos = end;
 }
 assert(parameters[0] && parameters[1]);
 CMVideoFormatDescriptionRef format = NULL;
 assert(CMVideoFormatDescriptionCreateFromH264ParameterSets(NULL, 2, parameters, sizes, 4, &format) == noErr);
 // Rebuild from actual SPS/PPS: validate the bitstream, not just VT's side data.
 NSLog(@"SPS color information: %@", CMFormatDescriptionGetExtensions(format));
 assert(bk_screen_full_range_output(format));
 NSDictionary *output = @{(id)kCVPixelBufferPixelFormatTypeKey:@(kCVPixelFormatType_32BGRA)};
 VTDecompressionOutputCallbackRecord callback = {decoded, NULL};
 VTDecompressionSessionRef decoder = NULL;
 assert(VTDecompressionSessionCreate(NULL, format, NULL, (CFDictionaryRef)output, &callback, &decoder) == noErr);
 CMBlockBufferRef block = NULL; CMSampleBufferRef sample = NULL;
 assert(CMBlockBufferCreateWithMemoryBlock(NULL, (void *)avcc.bytes, avcc.length, kCFAllocatorNull, NULL, 0, avcc.length, 0, &block) == noErr);
 size_t sampleSize = avcc.length;
 assert(CMSampleBufferCreateReady(NULL, block, format, 1, 0, NULL, 1, &sampleSize, &sample) == noErr);
 assert(VTDecompressionSessionDecodeFrame(decoder, sample, 0, NULL, NULL) == noErr);
 assert(VTDecompressionSessionWaitForAsynchronousFrames(decoder) == noErr);
 assert(decodeStatus == noErr && decodedPixel);
 assert(CVPixelBufferLockBaseAddress(decodedPixel, kCVPixelBufferLock_ReadOnly) == noErr);
 uint8_t *rgb = CVPixelBufferGetBaseAddress(decodedPixel);
 size_t stride = CVPixelBufferGetBytesPerRow(decodedPixel);
 for (int band = 0; band < 5; band++) {
  uint8_t *p = rgb+360*stride+(band*256+128)*4;
  printf("gray input=%d decoded=%d,%d,%d\n", levels[band], p[0], p[1], p[2]);
  for (int channel = 0; channel < 3; channel++) assert(abs((int)p[channel]-levels[band]) <= 3);
 }
 CVPixelBufferUnlockBaseAddress(decodedPixel, kCVPixelBufferLock_ReadOnly);
 CVPixelBufferRelease(decodedPixel); CFRelease(sample); CFRelease(block);
 VTDecompressionSessionInvalidate(decoder); CFRelease(decoder); CFRelease(format);
 CVPixelBufferRelease(pixel); [s shutdown]; [s release];
 return 0;
}

int main(int argc, char **argv) {
 @autoreleasepool {
  policy_tests();
  if (argc == 2 && !strcmp(argv[1], "--hardware")) return hardware_test();
 }
 return 0;
}
