#ifndef ANYKVM_ANNEXB_DARWIN_H
#define ANYKVM_ANNEXB_DARWIN_H
#import <Foundation/Foundation.h>
#import <CoreMedia/CoreMedia.h>
#include <stdlib.h>
#include <string.h>

typedef struct {
    const uint8_t *bytes;
    size_t size;
} bk_parameter_set;

static OSStatus bk_block_read(CMBlockBufferRef block, const uint8_t *raw, size_t offset, void *dst,
                              size_t size) {
    if (raw) {
        memcpy(dst, raw + offset, size);
        return noErr;
    }
    return CMBlockBufferCopyDataBytes(block, offset, size, dst);
}

// Measure first, then allocate exactly once. Segmented CMBlockBuffers are read
// directly into final Annex-B storage instead of flattening the entire AVCC AU.
static NSData *bk_annexb_block(CMBlockBufferRef block, int nal_length,
                               const bk_parameter_set *parameters, size_t parameter_count,
                               OSStatus *status) {
    *status = noErr;
    size_t total = block ? CMBlockBufferGetDataLength(block) : 0;
    const size_t limit = 32 * 1024 * 1024;
    if (!total || total > limit || nal_length < 1 || nal_length > 4) {
        *status = -3;
        return nil;
    }
    size_t contiguous = 0;
    char *pointer = NULL;
    OSStatus result = CMBlockBufferGetDataPointer(block, 0, &contiguous, NULL, &pointer);
    const uint8_t *raw = !result && contiguous == total ? (const uint8_t *)pointer : NULL;
    size_t output_size = 0;
    for (size_t i = 0; i < parameter_count; i++) {
        if (!parameters[i].bytes || !parameters[i].size || output_size > limit - 4 ||
            parameters[i].size > limit - output_size - 4) {
            *status = -3;
            return nil;
        }
        output_size += 4 + parameters[i].size;
    }
    size_t offset = 0;
    while (offset < total) {
        if (total - offset < (size_t)nal_length) {
            *status = -3;
            return nil;
        }
        uint8_t length[4];
        *status = bk_block_read(block, raw, offset, length, nal_length);
        if (*status)
            return nil;
        uint32_t n = 0;
        for (int i = 0; i < nal_length; i++)
            n = (n << 8) | length[i];
        offset += nal_length;
        if (!n || n > total - offset || output_size > limit - 4 || n > limit - output_size - 4) {
            *status = -3;
            return nil;
        }
        output_size += 4 + n;
        offset += n;
    }
    uint8_t *output = malloc(output_size);
    if (!output) {
        *status = -4;
        return nil;
    }
    const uint8_t start[] = {0, 0, 0, 1};
    size_t written = 0;
    for (size_t i = 0; i < parameter_count; i++) {
        memcpy(output + written, start, 4);
        written += 4;
        memcpy(output + written, parameters[i].bytes, parameters[i].size);
        written += parameters[i].size;
    }
    offset = 0;
    while (offset < total) {
        uint8_t length[4];
        *status = bk_block_read(block, raw, offset, length, nal_length);
        if (*status) {
            free(output);
            return nil;
        }
        uint32_t n = 0;
        for (int i = 0; i < nal_length; i++)
            n = (n << 8) | length[i];
        offset += nal_length;
        memcpy(output + written, start, 4);
        written += 4;
        *status = bk_block_read(block, raw, offset, output + written, n);
        if (*status) {
            free(output);
            return nil;
        }
        written += n;
        offset += n;
    }
    return [NSData dataWithBytesNoCopy:output length:output_size freeWhenDone:YES];
}
#endif
