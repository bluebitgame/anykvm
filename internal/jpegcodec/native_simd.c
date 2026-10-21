//go:build cgo && (amd64 || 386 || arm64) && (linux || windows || (darwin && amd64) || anykvm_jpeg_simd)

#include "simd.h"
#if defined(__aarch64__) || defined(_M_ARM64)
#include <arm_neon.h>
#else
#include <immintrin.h>
#endif
#include <math.h>
#include <stdlib.h>
#include <string.h>

static uint8_t to_byte(float value) {
    return value <= 0 ? 0 : value >= 255 ? 255 : (uint8_t)(value + 0.5f);
}

// The encoder keeps transposed coefficients to avoid its final 8x8 transpose.
static const int32_t scan_positions[64] = {
    0,2,3,9,10,20,21,35,
    1,4,8,11,19,22,34,36,
    5,7,12,18,23,33,37,48,
    6,13,17,24,32,38,47,49,
    14,16,25,31,39,46,50,57,
    15,26,30,40,45,51,56,58,
    27,29,41,44,52,55,59,62,
    28,42,43,53,54,60,61,63,
};

#if defined(__aarch64__) || defined(_M_ARM64)
#define SIMD_TARGET
#define SIMD_NAME(name) name##_neon
#define SIMD_WIDTH 4
#define SIMD_VECTOR float32x4_t
#define SIMD_INTEGER int32x4_t
#define SIMD_ZERO() vdupq_n_f32(0)
#define SIMD_SET vdupq_n_f32
#define SIMD_LOAD vld1q_f32
#define SIMD_STORE vst1q_f32
#define SIMD_ADD vaddq_f32
#define SIMD_SUB vsubq_f32
#define SIMD_MIN vminq_f32
#define SIMD_MAX vmaxq_f32
#define SIMD_MUL vmulq_f32
#include "simd_transform.inc"
#include "simd_pixels.inc"
#else
#define SIMD_TARGET __attribute__((target("sse2")))
#define SIMD_NAME(name) name##_sse2
#define SIMD_WIDTH 4
#define SIMD_VECTOR __m128
#define SIMD_INTEGER __m128i
#define SIMD_ZERO _mm_setzero_ps
#define SIMD_SET _mm_set1_ps
#define SIMD_LOAD _mm_loadu_ps
#define SIMD_STORE _mm_storeu_ps
#define SIMD_ADD _mm_add_ps
#define SIMD_SUB _mm_sub_ps
#define SIMD_MIN _mm_min_ps
#define SIMD_MAX _mm_max_ps
#define SIMD_MUL _mm_mul_ps
#include "simd_transform.inc"
#include "simd_pixels.inc"
#undef SIMD_TARGET
#undef SIMD_NAME
#undef SIMD_WIDTH
#undef SIMD_VECTOR
#undef SIMD_INTEGER
#undef SIMD_ZERO
#undef SIMD_SET
#undef SIMD_LOAD
#undef SIMD_STORE
#undef SIMD_SUB
#undef SIMD_MIN
#undef SIMD_MAX
#undef SIMD_ADD
#undef SIMD_MUL

#define SIMD_TARGET __attribute__((target("avx2")))
#define SIMD_NAME(name) name##_avx2
#define SIMD_WIDTH 8
#define SIMD_VECTOR __m256
#define SIMD_INTEGER __m256i
#define SIMD_ZERO _mm256_setzero_ps
#define SIMD_SET _mm256_set1_ps
#define SIMD_LOAD _mm256_loadu_ps
#define SIMD_STORE _mm256_storeu_ps
#define SIMD_ADD _mm256_add_ps
#define SIMD_SUB _mm256_sub_ps
#define SIMD_MIN _mm256_min_ps
#define SIMD_MAX _mm256_max_ps
#define SIMD_MUL _mm256_mul_ps
#include "simd_transform.inc"
#include "simd_pixels.inc"

#endif

typedef unsigned (*transform_fn)(const float *, int32_t *, const float *);
typedef void (*load_fn)(const uint8_t *, size_t, float [6][64]);
typedef void (*store_fn)(const float [6][64], const uint32_t *, unsigned, unsigned, uint8_t *);
typedef void (*inverse_fn)(const int32_t *, const float *, float *);
static transform_fn forward_transform(int level) {
#if defined(__aarch64__) || defined(_M_ARM64)
    return level == BK_JPEG_NEON ? fdct_neon : NULL;
#else
    return level == BK_JPEG_AVX2 ? fdct_avx2 : level == BK_JPEG_SSE2 ? fdct_sse2 : NULL;
#endif
}

static inverse_fn inverse_transform(int level) {
#if defined(__aarch64__) || defined(_M_ARM64)
    return level == BK_JPEG_NEON ? idct_neon : NULL;
#else
    return level == BK_JPEG_AVX2 ? idct_avx2 : level == BK_JPEG_SSE2 ? idct_sse2 : NULL;
#endif
}

static load_fn load_pixels(int level) {
#if defined(__aarch64__) || defined(_M_ARM64)
    return level == BK_JPEG_NEON ? load_mcu_neon : NULL;
#else
    return level == BK_JPEG_AVX2 ? load_mcu_avx2 : level == BK_JPEG_SSE2 ? load_mcu_sse2 : NULL;
#endif
}

static store_fn store_pixels(int level) {
#if defined(__aarch64__) || defined(_M_ARM64)
    return level == BK_JPEG_NEON ? store_mcu_neon : NULL;
#else
    return level == BK_JPEG_AVX2 ? store_mcu_avx2 : level == BK_JPEG_SSE2 ? store_mcu_sse2 : NULL;
#endif
}

static const unsigned encode_zigzag[64] = {
    0,8,1,2,9,16,24,17,
    10,3,4,11,18,25,32,40,
    33,26,19,12,5,6,13,20,
    27,34,41,48,56,49,42,35,
    28,21,14,7,15,22,29,36,
    43,50,57,58,51,44,37,30,
    23,31,38,45,52,59,60,53,
    46,39,47,54,61,62,55,63,
};

static const unsigned zigzag[64] = {
    0,1,8,16,9,2,3,10,17,24,32,25,18,11,4,5,
    12,19,26,33,40,48,41,34,27,20,13,6,7,14,21,28,
    35,42,49,56,57,50,43,36,29,22,15,23,30,37,44,51,
    58,59,52,45,38,31,39,46,53,60,61,54,47,55,62,63
};

// JPEG bits are emitted most-significant first; FF data bytes are stuffed.
typedef struct {
    uint8_t *data;
    size_t size, capacity;
    uint64_t pending;
    unsigned bits;
    int failed;
    const uint32_t *huffman;
} bit_writer;

static void put_byte(bit_writer *w, uint8_t value) {
    if (w->size == w->capacity) { w->failed = 1; return; }
    w->data[w->size++] = value;
}
static void put_stuffed_word(bit_writer *w, uint64_t word) {
    for (int shift = 56; shift >= 0; shift -= 8) {
        uint8_t byte = (uint8_t)(word >> shift);
        put_byte(w, byte);
        if (byte == 0xff) put_byte(w, 0);
    }
}
static inline __attribute__((always_inline)) void put_bits(bit_writer *w, uint32_t value, unsigned count) {
    if (!count) return;
    unsigned total = w->bits + count;
    if (total < 64) {
        w->pending = (w->pending << count) | value;
        w->bits = total;
        return;
    }
    unsigned remaining = total - 64;
    uint64_t word = (w->pending << (64 - w->bits)) | ((uint64_t)value >> remaining);
    w->pending = value;
    w->bits = remaining;
    uint64_t inverted = ~word;
    int has_ff = ((inverted - UINT64_C(0x0101010101010101)) & word & UINT64_C(0x8080808080808080)) != 0;
    if (!has_ff && w->capacity - w->size >= 8) {
        uint64_t native_word = __builtin_bswap64(word);
        memcpy(w->data + w->size, &native_word, sizeof(native_word));
        w->size += 8;
    } else {
        put_stuffed_word(w, word);
    }
}
static void finish_interval(bit_writer *w) {
    unsigned padding = (8 - (w->bits & 7)) & 7;
    put_bits(w, (1u << padding) - 1, padding);
    while (w->bits >= 8) {
        w->bits -= 8;
        uint8_t byte = (uint8_t)(w->pending >> w->bits);
        put_byte(w, byte);
        if (byte == 0xff) put_byte(w, 0);
    }
    w->pending = 0;
}
static unsigned magnitude_bits(int value) {
    unsigned magnitude = (unsigned)abs(value);
    return magnitude ? 32u - (unsigned)__builtin_clz(magnitude) : 0;
}
static void put_symbol(bit_writer *w, unsigned table, unsigned symbol) {
    uint32_t code = w->huffman[table * 256 + symbol];
    if (!code) { w->failed = 1; return; }
    put_bits(w, code & 0xffff, code >> 16);
}
static void put_coefficient(bit_writer *w, unsigned table, unsigned symbol, int value, unsigned count) {
    uint32_t code = w->huffman[table * 256 + symbol];
    if (!code) { w->failed = 1; return; }
    uint32_t magnitude = (uint32_t)(value < 0 ? value - 1 : value) & ((1u << count) - 1);
    put_bits(w, ((code & 0xffff) << count) | magnitude, (code >> 16) + count);
}
static void encode_block(bit_writer *w, const int32_t *block, unsigned last, unsigned table, int *previous) {
    int difference = block[0] - *previous;
    *previous = block[0];
    unsigned bits = magnitude_bits(difference);
    put_coefficient(w, table, bits, difference, bits);
    unsigned zeros = 0;
    for (unsigned i = 1; i <= last; i++) {
        int value = block[encode_zigzag[i]];
        if (!value) { zeros++; continue; }
        while (zeros >= 16) { put_symbol(w, table + 1, 0xf0); zeros -= 16; }
        if ((unsigned)(value + 31) < 63) {
            uint32_t code = w->huffman[1024 + table * 512 + zeros * 64 + ((unsigned)value & 63)];
            put_bits(w, code & 0x07ffffffu, code >> 27);
        } else {
            bits = magnitude_bits(value);
            put_coefficient(w, table + 1, zeros * 16 + bits, value, bits);
        }
        zeros = 0;
    }
    if (last < 63) put_symbol(w, table + 1, 0);
}
// Keep bounds checks out of the SIMD loop; only incomplete edge MCUs need
// a padded copy. Replication matches JPEG's block extension at the image edge.
static void load_edge_mcu(load_fn load, const uint8_t *pixels, int width, int height,
    int mx, int my, float samples[6][64]) {
    uint8_t padded[16 * 16 * 4];
    int columns = width - mx;
    if (columns > 16) columns = 16;
    for (int y = 0; y < 16; y++) {
        int row = my + y < height ? my + y : height - 1;
        const uint8_t *source = pixels + ((size_t)row * width + mx) * 4;
        uint8_t *destination = padded + y * 16 * 4;
        memcpy(destination, source, columns * 4);
        for (int x = columns; x < 16; x++) memcpy(destination + x * 4, source + (columns - 1) * 4, 4);
    }
    load(padded, 16 * 4, samples);
}

struct bk_jpeg_encoder {
    uint8_t *data;
    size_t capacity;
};
bk_jpeg_encoder *bk_jpeg_encoder_new(void) { return calloc(1, sizeof(bk_jpeg_encoder)); }
void bk_jpeg_encoder_free(bk_jpeg_encoder *encoder) {
    if (!encoder) return;
    free(encoder->data);
    free(encoder);
}

int bk_jpeg_encoder_encode(bk_jpeg_encoder *encoder, const uint8_t *pixels, int width, int height, int level,
    const uint32_t *quantization, const uint32_t *huffman, const uint8_t *header, size_t header_bytes,
    uint8_t **jpeg, size_t *jpeg_bytes) {
    *jpeg = NULL; *jpeg_bytes = 0;
    transform_fn transform = forward_transform(level);
    load_fn load = load_pixels(level);
    if (!transform || !load) return 0;
    size_t columns = ((size_t)width + 15) / 16, rows = ((size_t)height + 15) / 16;
    size_t count = columns * rows;
    if (!pixels || !quantization || !huffman || !header || width < 1 || height < 1 ||
        width > 8192 || height > 8192 || count > (128u << 20) / 3072 || header_bytes > 1024) return 0;
    size_t capacity = header_bytes + count * 3072 + 2;
    if (encoder->capacity < capacity) {
        uint8_t *data = malloc(capacity);
        if (!data) return 0;
        free(encoder->data);
        encoder->data = data;
        encoder->capacity = capacity;
    }
    bit_writer writer = {.data = encoder->data, .capacity = encoder->capacity, .huffman = huffman};
    memcpy(writer.data, header, header_bytes);
    writer.size = header_bytes;
    float reciprocal_quantization[128];
    for (int i = 0; i < 128; i++) {
        float row_scale = (i % 64) / 8 == 0 ? 0.3535533906f : 0.5f;
        float column_scale = i % 8 == 0 ? 0.3535533906f : 0.5f;
        unsigned natural = (unsigned)(i / 64) * 64 + (unsigned)(i % 8) * 8 + (unsigned)(i % 64) / 8;
        reciprocal_quantization[i] = row_scale * column_scale / quantization[natural];
    }
    for (size_t mcu = 0; mcu < count; mcu++) {
        int previous[3] = {0};
        float samples[6][64];
        int mx = (int)(mcu % columns) * 16, my = (int)(mcu / columns) * 16;
        if (mx + 16 <= width && my + 16 <= height) {
            load(pixels + ((size_t)my * width + mx) * 4, (size_t)width * 4, samples);
        } else {
            load_edge_mcu(load, pixels, width, height, mx, my, samples);
        }
        for (int b = 0; b < 6; b++) {
            int32_t coefficients[64];
            unsigned last = transform(samples[b], coefficients, reciprocal_quantization + (b < 4 ? 0 : 64));
            int component = b < 4 ? 0 : b - 3;
            encode_block(&writer, coefficients, last, b < 4 ? 0 : 2, &previous[component]);
        }
        finish_interval(&writer);
        if (mcu + 1 < count) { put_byte(&writer, 0xff); put_byte(&writer, (uint8_t)(0xd0 + mcu % 8)); }
    }
    put_byte(&writer, 0xff); put_byte(&writer, 0xd9);
    if (writer.failed) return 0;
    *jpeg = writer.data; *jpeg_bytes = writer.size;
    return 1;
}

// Standalone callers receive a malloc-owned output; Go's owned-frame path
// retains an encoder allocation until the frame's last consumer releases it.
int bk_jpeg_simd_encode(const uint8_t *pixels, int width, int height, int level,
    const uint32_t *quantization, const uint32_t *huffman, const uint8_t *header, size_t header_bytes,
    uint8_t **jpeg, size_t *jpeg_bytes) {
    bk_jpeg_encoder encoder = {0};
    int ok = bk_jpeg_encoder_encode(&encoder, pixels, width, height, level, quantization, huffman, header, header_bytes, jpeg, jpeg_bytes);
    if (!ok) free(encoder.data);
    return ok;
}

// Parser metadata and unstuffed entropy use the same layout as jpeg_decode.hlsl.
typedef struct {
    const uint8_t *data;
    size_t position, end, next_byte;
    uint32_t pending;
    unsigned bits;
    int valid;
} bit_reader;

// The parser has already removed byte stuffing and separated restart intervals.
static void refill_bits(bit_reader *r, unsigned count) {
    while (r->bits < count && r->next_byte < r->end / 8) {
        r->pending = (r->pending << 8) | r->data[r->next_byte++];
        r->bits += 8;
    }
}
static unsigned read_bits(bit_reader *r, unsigned count) {
    if (!r->valid || count > 16 || count > r->end - r->position) {
        r->valid = 0; return 0;
    }
    if (!count) return 0;
    refill_bits(r, count);
    r->bits -= count;
    r->position += count;
    return (r->pending >> r->bits) & ((1u << count) - 1);
}

enum { HUFFMAN_LOOKUP_BITS = 9, HUFFMAN_LOOKUP_SIZE = 1 << HUFFMAN_LOOKUP_BITS };
typedef struct {
    const uint32_t *canonical;
    uint32_t lookup[HUFFMAN_LOOKUP_SIZE];
} decode_huffman;

static void prepare_huffman(decode_huffman *h, const uint32_t *table) {
    h->canonical = table;
    memset(h->lookup, 0, sizeof(h->lookup));
    for (unsigned length = 1; length <= HUFFMAN_LOOKUP_BITS; length++) {
        unsigned i = length - 1, shift = HUFFMAN_LOOKUP_BITS - length;
        for (unsigned code = table[i]; code < table[16 + i]; code++) {
            unsigned symbol = table[48 + table[32 + i] + code - table[i]];
            for (unsigned suffix = 0; suffix < (1u << shift); suffix++) {
                unsigned prefix = (code << shift) | suffix, size = symbol & 15;
                unsigned total = length + size;
                uint32_t entry = (length << 8) | symbol;
                if (total <= HUFFMAN_LOOKUP_BITS) {
                    unsigned raw = (prefix >> (HUFFMAN_LOOKUP_BITS - total)) & ((1u << size) - 1);
                    int value = size && raw < (1u << (size - 1)) ? (int)raw - (int)((1u << size) - 1) : (int)raw;
                    entry |= (total << 12) | ((uint32_t)(uint16_t)value << 16);
                }
                h->lookup[prefix] = entry;
            }
        }
    }
}
typedef struct { unsigned symbol; int value; } huffman_value;

static int read_amplitude(bit_reader *r, unsigned count) {
    unsigned value = read_bits(r, count);
    return count && value < (1u << (count - 1)) ? (int)value - (int)((1u << count) - 1) : (int)value;
}

static huffman_value read_coefficient(bit_reader *r, const decode_huffman *h) {
    const uint32_t *table = h->canonical;
    unsigned code = 0, length = 0;
    if (r->valid && r->end - r->position >= HUFFMAN_LOOKUP_BITS) {
        refill_bits(r, HUFFMAN_LOOKUP_BITS);
        code = (r->pending >> (r->bits - HUFFMAN_LOOKUP_BITS)) & (HUFFMAN_LOOKUP_SIZE - 1);
        unsigned entry = h->lookup[code];
        if (entry) {
            // A short code and its amplitude fit in one prefix lookup.
            unsigned total = (entry >> 12) & 15, symbol = entry & 255;
            length = total ? total : (entry >> 8) & 15;
            r->bits -= length;
            r->position += length;
            int value = total ? (int16_t)(entry >> 16) : read_amplitude(r, symbol & 15);
            return (huffman_value){symbol, value};
        }
        r->bits -= HUFFMAN_LOOKUP_BITS;
        r->position += HUFFMAN_LOOKUP_BITS;
        length = HUFFMAN_LOOKUP_BITS;
    }
    while (length < 16) {
        code = code * 2 + read_bits(r, 1);
        if (!r->valid) return (huffman_value){0, 0};
        unsigned i = length++;
        if (code >= table[i] && code < table[16 + i]) {
            unsigned symbol = table[48 + table[32 + i] + code - table[i]];
            return (huffman_value){symbol, read_amplitude(r, symbol & 15)};
        }
    }
    r->valid = 0;
    return (huffman_value){0, 0};
}
static int decode_block(bit_reader *r, const decode_huffman *dc, const decode_huffman *ac, int *previous, int32_t *block) {
    memset(block, 0, 64 * sizeof(*block));
    huffman_value difference = read_coefficient(r, dc);
    if (difference.symbol > 11) return 0;
    *previous += difference.value;
    if (*previous < -2048 || *previous > 2047) return 0;
    block[0] = *previous;
    for (unsigned i = 1; i < 64 && r->valid;) {
        huffman_value coefficient = read_coefficient(r, ac);
        unsigned symbol = coefficient.symbol, zeros = symbol >> 4, count = symbol & 15;
        if (!symbol) break;
        if (!count) {
            if (zeros != 15 || i + 16 > 64) return 0;
            i += 16; continue;
        }
        i += zeros;
        if (i >= 64 || count > 10) return 0;
        block[zigzag[i++]] = coefficient.value;
    }
    return r->valid;
}
int bk_jpeg_simd_decode(const uint32_t *meta, const uint8_t *entropy, int level, uint8_t *pixels) {
    inverse_fn inverse = inverse_transform(level);
    store_fn store = store_pixels(level);
    if (!inverse || !store) return 0;
    unsigned mcu_width = meta[4] * 8, mcu_height = meta[5] * 8;
    float quantization[3][64];
    for (unsigned c = 0; c < meta[14]; c++) {
        for (unsigned i = 0; i < 64; i++) {
            float row_scale = i / 8 == 0 ? 0.3535533906f : 0.5f;
            float column_scale = i % 8 == 0 ? 0.3535533906f : 0.5f;
            quantization[c][i] = meta[40 + c * 64 + i] * row_scale * column_scale;
        }
    }
    decode_huffman huffman[6];
    for (unsigned c = 0; c < meta[14] * 2; c++) prepare_huffman(&huffman[c], meta + 232 + c * 304);
    for (unsigned segment = 0; segment < meta[9]; segment++) {
        const uint32_t *s = meta + 2056 + segment * 4;
        bit_reader reader = {.data = entropy + s[0], .end = (size_t)s[1] * 8, .valid = 1};
        int previous[3] = {0};
        for (unsigned mcu = s[2]; mcu < s[2] + s[3]; mcu++) {
            float samples[6][64];
            for (unsigned c = 0; c < meta[14]; c++) {
                const uint32_t *component = meta + 16 + c * 8;
                for (unsigned b = 0; b < component[0] * component[1]; b++) {
                    int32_t block[64];
                    if (!decode_block(&reader, &huffman[c * 2], &huffman[c * 2 + 1], &previous[c], block)) return 0;
                    inverse(block, quantization[c], samples[component[2] + b]);
                }
            }
            unsigned mx = mcu % meta[6] * mcu_width, my = mcu / meta[6] * mcu_height;
            store(samples, meta, mx, my, pixels);
        }
        size_t remaining = reader.end - reader.position;
        if (!reader.valid || remaining > 7 || read_bits(&reader, (unsigned)remaining) != (1u << remaining) - 1) return 0;
    }
    return 1;
}
