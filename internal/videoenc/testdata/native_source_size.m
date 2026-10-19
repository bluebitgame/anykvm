#import "../native_darwin.m"
#include <assert.h>

static void dimensions(const uint8_t *bytes, size_t count, int w, int h) {
 const uint8_t *sets[2] = {NULL, NULL}; size_t lengths[2] = {0, 0};
 for (size_t at = 0; at + 4 < count;) {
  assert(!memcmp(bytes + at, "\0\0\0\1", 4));
  size_t start = at + 4, end = start;
  while (end + 4 <= count && memcmp(bytes + end, "\0\0\0\1", 4)) end++;
  if (end + 4 > count) end = count;
  int kind = bytes[start] & 31;
  if (kind == 7 || kind == 8) { sets[kind - 7] = bytes + start; lengths[kind - 7] = end - start; }
  at = end;
 }
 assert(sets[0] && sets[1]);
 CMVideoFormatDescriptionRef format = NULL;
 assert(CMVideoFormatDescriptionCreateFromH264ParameterSets(NULL, 2, sets, lengths, 4, &format) == noErr);
 CMVideoDimensions size = CMVideoFormatDescriptionGetDimensions(format);
 assert(size.width == w && size.height == h);
 CFRelease(format);
}

static void encode(int w, int h) {
 char error[512] = {0};
 bk_encoder *e = bk_encoder_open("videotoolbox", "", w, h, 30, 1, 24000, 0, 0, 0, error, sizeof(error));
 if (!e) { fprintf(stderr, "%s\n", error); abort(); }
 assert(e->transfer == NULL);
 CVPixelBufferRef source = NULL;
 NSDictionary *attrs = @{(id)kCVPixelBufferIOSurfacePropertiesKey:@{}};
 assert(CVPixelBufferCreate(NULL, w, h, kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange, (CFDictionaryRef)attrs, &source) == noErr);
 CVBufferSetAttachment(source, kCVImageBufferPixelAspectRatioKey,
  (CFDictionaryRef)@{(id)kCVImageBufferPixelAspectRatioHorizontalSpacingKey:@1,(id)kCVImageBufferPixelAspectRatioVerticalSpacingKey:@1}, kCVAttachmentMode_ShouldPropagate);
 CFDictionaryRef attachments = CVBufferCopyAttachments(source, kCVAttachmentMode_ShouldPropagate);
 for (uint64_t id = 1; id <= 4; id++) {
  assert(CVPixelBufferLockBaseAddress(source, 0) == noErr);
  for (size_t plane = 0; plane < 2; plane++)
   memset(CVPixelBufferGetBaseAddressOfPlane(source, plane), plane ? 128 : 64 + id,
    CVPixelBufferGetBytesPerRowOfPlane(source, plane) * CVPixelBufferGetHeightOfPlane(source, plane));
  CVPixelBufferUnlockBaseAddress(source, 0);
  assert(bk_encoder_submit_pixel(e, source, id, id == 1, error, sizeof(error)) == 1);
  assert(bk_encoder_flush(e, error, sizeof(error)) == 0);
  assert(e->transfer == NULL); // No conversion session, destination allocation, or letterbox.
  CFDictionaryRef after = CVBufferCopyAttachments(source, kCVAttachmentMode_ShouldPropagate);
  assert(CFEqual(attachments, after)); CFRelease(after);
  void *lease = NULL; const uint8_t *bytes = NULL; size_t count = 0; uint64_t outputID = 0; int dropped = 0;
  assert(bk_encoder_receive(e, &lease, &bytes, &count, &outputID, &dropped, error, sizeof(error)) == 1);
  assert(outputID == id && !dropped && count > 4);
  if (id == 1) dimensions(bytes, count, w, h);
  bk_encoder_packet_release(lease);
 }
 CFRelease(attachments); CVPixelBufferRelease(source); bk_encoder_close(e);
 printf("H.264 %dx%d: native NV12, 4 frames, no pixel transfer, source attachments unchanged\n", w, h);
}

int main(void) { @autoreleasepool {
 encode(1206, 2622);
 encode(2622, 1206);
 return 0;
} }
