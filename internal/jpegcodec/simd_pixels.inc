// Convert one row fragment at a time. Chroma is either full width or doubled
// horizontally; both layouts stay within the current 8-pixel block.
static SIMD_TARGET SIMD_VECTOR SIMD_NAME(load_chroma)(const float *row, unsigned horizontal_shift) {
    if (!horizontal_shift) return SIMD_LOAD(row);
#if defined(__aarch64__) || defined(_M_ARM64)
    float32x2x2_t pairs = vzip_f32(vld1_f32(row), vld1_f32(row));
    return vcombine_f32(pairs.val[0], pairs.val[1]);
#elif SIMD_WIDTH == 8
    __m128 values = _mm_loadu_ps(row);
    return _mm256_insertf128_ps(_mm256_castps128_ps256(_mm_unpacklo_ps(values, values)),
        _mm_unpackhi_ps(values, values), 1);
#else
    __m128 values = _mm_castsi128_ps(_mm_loadl_epi64((const __m128i *)row));
    return _mm_unpacklo_ps(values, values);
#endif
}

static SIMD_TARGET void SIMD_NAME(store_bgra)(uint8_t *pixels, SIMD_VECTOR blue, SIMD_VECTOR green, SIMD_VECTOR red) {
    blue = SIMD_ADD(SIMD_MIN(SIMD_MAX(blue, SIMD_ZERO()), SIMD_SET(255)), SIMD_SET(0.5f));
    green = SIMD_ADD(SIMD_MIN(SIMD_MAX(green, SIMD_ZERO()), SIMD_SET(255)), SIMD_SET(0.5f));
    red = SIMD_ADD(SIMD_MIN(SIMD_MAX(red, SIMD_ZERO()), SIMD_SET(255)), SIMD_SET(0.5f));
#if defined(__aarch64__) || defined(_M_ARM64)
    uint32x4_t packed = vorrq_u32(vcvtq_u32_f32(blue), vshlq_n_u32(vcvtq_u32_f32(green), 8));
    packed = vorrq_u32(packed, vshlq_n_u32(vcvtq_u32_f32(red), 16));
    packed = vorrq_u32(packed, vdupq_n_u32(0xff000000u));
    vst1q_u8(pixels, vreinterpretq_u8_u32(packed));
#elif SIMD_WIDTH == 8
    __m256i packed = _mm256_or_si256(_mm256_cvttps_epi32(blue), _mm256_slli_epi32(_mm256_cvttps_epi32(green), 8));
    packed = _mm256_or_si256(packed, _mm256_slli_epi32(_mm256_cvttps_epi32(red), 16));
    packed = _mm256_or_si256(packed, _mm256_set1_epi32((int)0xff000000u));
    _mm256_storeu_si256((__m256i *)pixels, packed);
#else
    __m128i packed = _mm_or_si128(_mm_cvttps_epi32(blue), _mm_slli_epi32(_mm_cvttps_epi32(green), 8));
    packed = _mm_or_si128(packed, _mm_slli_epi32(_mm_cvttps_epi32(red), 16));
    packed = _mm_or_si128(packed, _mm_set1_epi32((int)0xff000000u));
    _mm_storeu_si128((__m128i *)pixels, packed);
#endif
}

static SIMD_TARGET void SIMD_NAME(store_mcu)(const float samples[6][64], const uint32_t *meta,
    unsigned mx, unsigned my, uint8_t *pixels) {
    unsigned width = meta[0], height = meta[1];
    unsigned columns = width - mx, rows = height - my;
    if (columns > meta[4] * 8) columns = meta[4] * 8;
    if (rows > meta[5] * 8) rows = meta[5] * 8;
    unsigned horizontal_shift = meta[4] == 2, vertical_shift = meta[5] == 2;
    int colored = meta[14] == 3;
    for (unsigned y = 0; y < rows; y++) {
        unsigned luma_block = (y >> 3) * meta[4], luma_row = (y & 7) * 8;
        const float *cb = samples[meta[16 + 8 + 2]] + (y >> vertical_shift) * 8;
        const float *cr = samples[meta[16 + 16 + 2]] + (y >> vertical_shift) * 8;
        uint8_t *p = pixels + ((size_t)(my + y) * width + mx) * 4;
        unsigned x = 0;
        for (; x + SIMD_WIDTH <= columns; x += SIMD_WIDTH, p += SIMD_WIDTH * 4) {
            SIMD_VECTOR luma = SIMD_LOAD(samples[luma_block + (x >> 3)] + luma_row + (x & 7));
            SIMD_VECTOR blue = SIMD_ZERO(), red = SIMD_ZERO();
            if (colored) {
                blue = SIMD_SUB(SIMD_NAME(load_chroma)(cb + (x >> horizontal_shift), horizontal_shift), SIMD_SET(128));
                red = SIMD_SUB(SIMD_NAME(load_chroma)(cr + (x >> horizontal_shift), horizontal_shift), SIMD_SET(128));
            }
            SIMD_VECTOR green = SIMD_SUB(SIMD_SUB(luma, SIMD_MUL(blue, SIMD_SET(0.344136f))), SIMD_MUL(red, SIMD_SET(0.714136f)));
            SIMD_NAME(store_bgra)(p, SIMD_ADD(luma, SIMD_MUL(blue, SIMD_SET(1.772f))), green,
                SIMD_ADD(luma, SIMD_MUL(red, SIMD_SET(1.402f))));
        }
        for (; x < columns; x++, p += 4) {
            float luma = samples[luma_block + (x >> 3)][luma_row + (x & 7)];
            float blue = colored ? cb[x >> horizontal_shift] - 128 : 0;
            float red = colored ? cr[x >> horizontal_shift] - 128 : 0;
            p[0] = to_byte(luma + 1.772f * blue);
            p[1] = to_byte(luma - 0.344136f * blue - 0.714136f * red);
            p[2] = to_byte(luma + 1.402f * red);
            p[3] = 255;
        }
    }
}

static SIMD_TARGET SIMD_VECTOR SIMD_NAME(pair_average)(SIMD_VECTOR sum) {
#if defined(__aarch64__) || defined(_M_ARM64)
    return vmulq_n_f32(vpaddq_f32(sum, sum), 0.25f);
#elif SIMD_WIDTH == 8
    __m256 pairs = _mm256_hadd_ps(sum, sum);
    return _mm256_mul_ps(_mm256_castpd_ps(_mm256_permute4x64_pd(_mm256_castps_pd(pairs), 0xd8)), SIMD_SET(0.25f));
#else
    __m128 pairs = _mm_add_ps(sum, _mm_shuffle_ps(sum, sum, 0xb1));
    return _mm_mul_ps(_mm_shuffle_ps(pairs, pairs, 0x88), SIMD_SET(0.25f));
#endif
}

static SIMD_TARGET void SIMD_NAME(store_chroma)(float *output, SIMD_VECTOR value) {
#if defined(__aarch64__) || defined(_M_ARM64)
    vst1_f32(output, vget_low_f32(value));
#elif SIMD_WIDTH == 8
    _mm_storeu_ps(output, _mm256_castps256_ps128(value));
#else
    _mm_storel_epi64((__m128i *)output, _mm_castps_si128(value));
#endif
}

static SIMD_TARGET void SIMD_NAME(load_mcu)(const uint8_t *pixels, size_t stride, float samples[6][64]) {
    float previous_red[16], previous_green[16], previous_blue[16];
    for (unsigned y = 0; y < 16; y++) {
        for (unsigned x = 0; x < 16; x += SIMD_WIDTH) {
            const uint8_t *p = pixels + y * stride + x * 4;
            SIMD_VECTOR red, green, blue;
#if defined(__aarch64__) || defined(_M_ARM64)
            uint32x4_t packed = vld1q_u32((const uint32_t *)p), mask = vdupq_n_u32(255);
            red = vcvtq_f32_u32(vandq_u32(vshrq_n_u32(packed, 16), mask));
            green = vcvtq_f32_u32(vandq_u32(vshrq_n_u32(packed, 8), mask));
            blue = vcvtq_f32_u32(vandq_u32(packed, mask));
#elif SIMD_WIDTH == 8
            __m256i packed = _mm256_loadu_si256((const __m256i *)p), mask = _mm256_set1_epi32(255);
            red = _mm256_cvtepi32_ps(_mm256_and_si256(_mm256_srli_epi32(packed, 16), mask));
            green = _mm256_cvtepi32_ps(_mm256_and_si256(_mm256_srli_epi32(packed, 8), mask));
            blue = _mm256_cvtepi32_ps(_mm256_and_si256(packed, mask));
#else
            __m128i packed = _mm_loadu_si128((const __m128i *)p), mask = _mm_set1_epi32(255);
            red = _mm_cvtepi32_ps(_mm_and_si128(_mm_srli_epi32(packed, 16), mask));
            green = _mm_cvtepi32_ps(_mm_and_si128(_mm_srli_epi32(packed, 8), mask));
            blue = _mm_cvtepi32_ps(_mm_and_si128(packed, mask));
#endif
            SIMD_VECTOR luma = SIMD_ADD(SIMD_ADD(SIMD_MUL(red, SIMD_SET(0.299f)),
                SIMD_MUL(green, SIMD_SET(0.587f))), SIMD_MUL(blue, SIMD_SET(0.114f)));
            SIMD_STORE(samples[(y >> 3) * 2 + (x >> 3)] + (y & 7) * 8 + (x & 7), SIMD_SUB(luma, SIMD_SET(128)));
            if (!(y & 1)) {
                SIMD_STORE(previous_red + x, red);
                SIMD_STORE(previous_green + x, green);
                SIMD_STORE(previous_blue + x, blue);
                continue;
            }
            red = SIMD_NAME(pair_average)(SIMD_ADD(red, SIMD_LOAD(previous_red + x)));
            green = SIMD_NAME(pair_average)(SIMD_ADD(green, SIMD_LOAD(previous_green + x)));
            blue = SIMD_NAME(pair_average)(SIMD_ADD(blue, SIMD_LOAD(previous_blue + x)));
            SIMD_VECTOR cb = SIMD_ADD(SIMD_ADD(SIMD_MUL(red, SIMD_SET(-0.168736f)),
                SIMD_MUL(green, SIMD_SET(-0.331264f))), SIMD_MUL(blue, SIMD_SET(0.5f)));
            SIMD_VECTOR cr = SIMD_ADD(SIMD_ADD(SIMD_MUL(red, SIMD_SET(0.5f)),
                SIMD_MUL(green, SIMD_SET(-0.418688f))), SIMD_MUL(blue, SIMD_SET(-0.081312f)));
            SIMD_NAME(store_chroma)(samples[4] + (y >> 1) * 8 + (x >> 1), cb);
            SIMD_NAME(store_chroma)(samples[5] + (y >> 1) * 8 + (x >> 1), cr);
        }
    }
}
