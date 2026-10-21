#ifndef BK_GPU_JPEG_LAYOUT_H
#define BK_GPU_JPEG_LAYOUT_H
#include <stddef.h>
#include <stdint.h>

// Shared host ABI for shaders/jpeg420.hlsl.
// Packed coefficient blocks, followed by two AC nonzero masks per block.
#define BK_JPEG_BLOCK_WORDS (64u / 2u + 2u)
#define BK_JPEG_MCU_WORDS (6u * BK_JPEG_BLOCK_WORDS)
#define BK_JPEG_SEGMENT_CAPACITY 3072u
#define BK_JPEG_STORAGE_LIMIT (128u * 1024u * 1024u)
#define BK_JPEG_SCAN_THREADS 256u
#define BK_JPEG_ARGUMENT_BYTES 64u
#define BK_JPEG_REGION_HEADER_BYTES 12u
#define BK_JPEG_SOLID_FLAG (1u << 24)
#define BK_JPEG_SOLID_MCU_TAG (1u << 31)
#define BK_JPEG_SOLID_DC_HIGH (1u << 30)
// Dirty flags, three count/MCU queues, then optional candidate tile IDs.
static inline uint32_t bk_jpeg_work_words(uint32_t tiles, uint32_t count) {
    return tiles * 2 + 3 + count * 3;
}
static inline uint32_t bk_jpeg_candidate_offset(uint32_t tiles, uint32_t count) {
    return tiles + 3 + count * 3;
}
static inline uint32_t bk_jpeg_scan_groups(uint32_t count) {
    return (count + BK_JPEG_SCAN_THREADS - 1) / BK_JPEG_SCAN_THREADS;
}
static inline uint32_t bk_jpeg_segment_info_words(uint32_t count) {
    // Tagged length/offset pairs, total bytes, scan groups, then one DC word/MCU.
    return count * 3 + 1 + bk_jpeg_scan_groups(count) * 2;
}
_Static_assert(BK_JPEG_STORAGE_LIMIT / BK_JPEG_SEGMENT_CAPACITY <= BK_JPEG_SCAN_THREADS * BK_JPEG_SCAN_THREADS,
               "JPEG group totals must fit one scan group");
typedef struct {
    uint32_t source_width, source_height;
    uint32_t output_width, output_height;
    uint32_t rotation_degrees, mcu_columns;
    uint32_t header_bytes, mcu_count;
    uint32_t crop_origin[2], crop_size[2];
    float quantization_reciprocals[128];
    uint32_t huffman[3072];
    uint32_t tile_state[4];
} bk_jpeg_parameters;

static inline void bk_jpeg_prepare_quantization(bk_jpeg_parameters *parameters, const uint32_t *quantization) {
    // AAN outputs are scaled by sqrt(8)*scale[k] in each dimension.
    // scale[0]=1; scale[k]=sqrt(2)*cos(k*pi/16) for k>0.
    static const double scale[8] = {
        1.0, 1.3870398453221475, 1.3065629648763766, 1.1758756024193588,
        1.0, 0.7856949583871022, 0.5411961001461970, 0.2758993792829431
    };
    for (unsigned i = 0; i < 128; i++) {
        unsigned row = (i % 64) / 8, column = i % 8;
        parameters->quantization_reciprocals[i] = (float)(1.0 / (quantization[i] * 8.0 * scale[row] * scale[column]));
    }
}

_Static_assert(offsetof(bk_jpeg_parameters, quantization_reciprocals) == 48, "JPEG quantization offset");
_Static_assert(offsetof(bk_jpeg_parameters, huffman) == 560, "JPEG Huffman offset");
_Static_assert(offsetof(bk_jpeg_parameters, tile_state) == 12848, "JPEG tile state offset");
_Static_assert(sizeof(bk_jpeg_parameters) == 12864, "JPEG constant buffer size");
#endif
