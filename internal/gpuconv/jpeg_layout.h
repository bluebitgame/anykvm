#ifndef BK_GPU_JPEG_LAYOUT_H
#define BK_GPU_JPEG_LAYOUT_H
#include <stddef.h>
#include <stdint.h>

// Shared host ABI for shaders/jpeg420.hlsl.
#define BK_JPEG_COEFFICIENTS_PER_MCU (6u * 64u)
#define BK_JPEG_SEGMENT_CAPACITY 3072u
#define BK_JPEG_STORAGE_LIMIT (128u * 1024u * 1024u)
typedef struct {
    uint32_t source_width, source_height;
    uint32_t output_width, output_height;
    uint32_t rotation_degrees, mcu_columns;
    uint32_t header_bytes, mcu_count;
    uint32_t crop_origin[2], crop_size[2];
    float quantization_reciprocals[128];
    uint32_t huffman[3072];
} bk_jpeg_parameters;

static inline void bk_jpeg_prepare_quantization(bk_jpeg_parameters *parameters, const uint32_t *quantization) {
    for (unsigned i = 0; i < 128; i++) parameters->quantization_reciprocals[i] = 1.0f / quantization[i];
}

_Static_assert(offsetof(bk_jpeg_parameters, quantization_reciprocals) == 48, "JPEG quantization offset");
_Static_assert(offsetof(bk_jpeg_parameters, huffman) == 560, "JPEG Huffman offset");
_Static_assert(sizeof(bk_jpeg_parameters) == 12848, "JPEG constant buffer size");
#endif
