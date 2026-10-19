#import "../annexb_darwin.h"
#include <assert.h>

static CMBlockBufferRef block(const uint8_t *bytes, size_t size, size_t chunk) {
 CMBlockBufferRef result = NULL;
 assert(CMBlockBufferCreateEmpty(kCFAllocatorDefault, 0, 0, &result) == noErr);
 for (size_t offset = 0; offset < size;) {
  size_t n = size - offset < chunk ? size - offset : chunk;
  assert(CMBlockBufferAppendMemoryBlock(result, NULL, n, kCFAllocatorDefault, NULL, 0, n, 0) == noErr);
  assert(CMBlockBufferReplaceDataBytes(bytes + offset, result, offset, n) == noErr);
  offset += n;
 }
 return result;
}

static void valid(int length, size_t chunk, size_t parameter_count) {
 const uint8_t nal1[] = {0x65, 1, 2, 3, 4}, nal2[] = {0x41, 5, 6};
 uint8_t avcc[16]; size_t size = 0;
 for (int n = 0; n < 2; n++) {
  const uint8_t *nal = n == 0 ? nal1 : nal2;
  size_t count = n == 0 ? sizeof(nal1) : sizeof(nal2);
  for (int i = length - 1; i >= 0; i--) avcc[size++] = (uint8_t)(count >> (i * 8));
  memcpy(avcc + size, nal, count); size += count;
 }
 const uint8_t vps[] = {0x40, 8}, sps[] = {0x67, 9}, pps[] = {0x68, 10};
 const bk_parameter_set parameters[] = {{vps, 2}, {sps, 2}, {pps, 2}};
 NSMutableData *expected = [NSMutableData data];
 const uint8_t start[] = {0, 0, 0, 1};
 for (size_t i = 0; i < parameter_count; i++) {
  [expected appendBytes:start length:4]; [expected appendBytes:parameters[i].bytes length:parameters[i].size];
 }
 [expected appendBytes:start length:4]; [expected appendBytes:nal1 length:sizeof(nal1)];
 [expected appendBytes:start length:4]; [expected appendBytes:nal2 length:sizeof(nal2)];
 CMBlockBufferRef source = block(avcc, size, chunk);
 if (chunk < size) assert(!CMBlockBufferIsRangeContiguous(source, 0, size));
 OSStatus status = -99;
 NSData *output = [bk_annexb_block(source, length, parameters, parameter_count, &status) retain];
 assert(status == noErr && [output isEqualToData:expected]);
 uint8_t original[16];
 assert(CMBlockBufferCopyDataBytes(source, 0, size, original) == noErr);
 assert(memcmp(original, avcc, size) == 0); // Never mutate borrowed VT storage.
 CFRelease(source);
 assert([output isEqualToData:expected]); // Output survives the native sample.
 [output release];
}

int main(void) { @autoreleasepool {
 for (int length = 1; length <= 4; length++) {
  for (size_t count = 0; count <= 3; count++) {
   valid(length, 100, count);
   valid(length, 2, count); // Split length prefixes and payloads across segments.
  }
 }
 const uint8_t malformed[][5] = {{0,0,0,0,0}, {0,0,0,9,0x65}, {0,0,0,1,0x65}};
 for (int n = 0; n < 3; n++) {
  CMBlockBufferRef source = block(malformed[n], n == 2 ? 3 : 5, 2);
  OSStatus status = noErr;
  assert(bk_annexb_block(source, 4, NULL, 0, &status) == nil && status != noErr);
  CFRelease(source);
 }
 CMBlockBufferRef empty = block(NULL, 0, 2);
 OSStatus status = noErr;
 assert(bk_annexb_block(empty, 4, NULL, 0, &status) == nil && status != noErr);
 CFRelease(empty);
 return 0;
} }
