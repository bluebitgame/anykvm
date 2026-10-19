#ifndef ANYKVM_H264_REFERENCE_DARWIN_H
#define ANYKVM_H264_REFERENCE_DARWIN_H
#import <VideoToolbox/VideoToolbox.h>
#include <sys/sysctl.h>

static inline int bk_h264_single_reference(void) {
#if defined(__x86_64__)
 if (@available(macOS 13.0, *)) {
  // An amd64 executable under Rosetta still uses Apple Silicon's encoder.
  // Leave its reference-buffer policy unchanged, as for a native arm64 build.
  int translated = 0; size_t size = sizeof(translated);
  if (sysctlbyname("sysctl.proc_translated", &translated, &size, NULL, 0) == 0 && translated) return 0;
  return 1;
 }
#endif
 return 0;
}

static inline OSStatus bk_h264_reference_buffers(VTCompressionSessionRef session) {
 if (bk_h264_single_reference()) {
  if (@available(macOS 13.0, *)) return VTSessionSetProperty(session, kVTCompressionPropertyKey_ReferenceBufferCount, (CFNumberRef)@1);
 }
 return noErr;
}
#endif
