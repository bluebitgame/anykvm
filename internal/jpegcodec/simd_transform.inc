// Lee's factorization of the 8-point DCT. Each lane transforms an
// independent column. Encoding keeps the second pass transposed; decoding
// restores pixel order. Normalization is folded into the quantizers.
static SIMD_TARGET void SIMD_NAME(dct4)(SIMD_VECTOR *r) {
    SIMD_VECTOR even0 = SIMD_ADD(r[0], r[3]);
    SIMD_VECTOR even1 = SIMD_ADD(r[1], r[2]);
    SIMD_VECTOR odd0 = SIMD_MUL(SIMD_SUB(r[0], r[3]), SIMD_SET(0.5411961001f));
    SIMD_VECTOR odd1 = SIMD_MUL(SIMD_SUB(r[1], r[2]), SIMD_SET(1.3065629649f));
    r[0] = SIMD_ADD(even0, even1);
    r[2] = SIMD_MUL(SIMD_SUB(even0, even1), SIMD_SET(0.7071067812f));
    r[3] = SIMD_MUL(SIMD_SUB(odd0, odd1), SIMD_SET(0.7071067812f));
    r[1] = SIMD_ADD(SIMD_ADD(odd0, odd1), r[3]);
}

static SIMD_TARGET void SIMD_NAME(dct8)(SIMD_VECTOR *r) {
    SIMD_VECTOR even[4], odd[4];
    even[0] = SIMD_ADD(r[0], r[7]);
    even[1] = SIMD_ADD(r[1], r[6]);
    even[2] = SIMD_ADD(r[2], r[5]);
    even[3] = SIMD_ADD(r[3], r[4]);
    odd[0] = SIMD_MUL(SIMD_SUB(r[0], r[7]), SIMD_SET(0.5097955791f));
    odd[1] = SIMD_MUL(SIMD_SUB(r[1], r[6]), SIMD_SET(0.6013448869f));
    odd[2] = SIMD_MUL(SIMD_SUB(r[2], r[5]), SIMD_SET(0.8999762231f));
    odd[3] = SIMD_MUL(SIMD_SUB(r[3], r[4]), SIMD_SET(2.5629154477f));
    SIMD_NAME(dct4)(even);
    SIMD_NAME(dct4)(odd);
    r[0] = even[0]; r[2] = even[1]; r[4] = even[2]; r[6] = even[3];
    r[1] = SIMD_ADD(odd[0], odd[1]);
    r[3] = SIMD_ADD(odd[1], odd[2]);
    r[5] = SIMD_ADD(odd[2], odd[3]);
    r[7] = odd[3];
}

static SIMD_TARGET void SIMD_NAME(transpose)(const float *input, float *output) {
#if SIMD_WIDTH == 8
    __m256 r0 = _mm256_loadu_ps(input), r1 = _mm256_loadu_ps(input + 8);
    __m256 r2 = _mm256_loadu_ps(input + 16), r3 = _mm256_loadu_ps(input + 24);
    __m256 r4 = _mm256_loadu_ps(input + 32), r5 = _mm256_loadu_ps(input + 40);
    __m256 r6 = _mm256_loadu_ps(input + 48), r7 = _mm256_loadu_ps(input + 56);
    __m256 t0 = _mm256_unpacklo_ps(r0, r1), t1 = _mm256_unpackhi_ps(r0, r1);
    __m256 t2 = _mm256_unpacklo_ps(r2, r3), t3 = _mm256_unpackhi_ps(r2, r3);
    __m256 t4 = _mm256_unpacklo_ps(r4, r5), t5 = _mm256_unpackhi_ps(r4, r5);
    __m256 t6 = _mm256_unpacklo_ps(r6, r7), t7 = _mm256_unpackhi_ps(r6, r7);
    r0 = _mm256_shuffle_ps(t0, t2, 0x44); r1 = _mm256_shuffle_ps(t0, t2, 0xee);
    r2 = _mm256_shuffle_ps(t1, t3, 0x44); r3 = _mm256_shuffle_ps(t1, t3, 0xee);
    r4 = _mm256_shuffle_ps(t4, t6, 0x44); r5 = _mm256_shuffle_ps(t4, t6, 0xee);
    r6 = _mm256_shuffle_ps(t5, t7, 0x44); r7 = _mm256_shuffle_ps(t5, t7, 0xee);
    _mm256_storeu_ps(output,      _mm256_permute2f128_ps(r0, r4, 0x20));
    _mm256_storeu_ps(output + 8,  _mm256_permute2f128_ps(r1, r5, 0x20));
    _mm256_storeu_ps(output + 16, _mm256_permute2f128_ps(r2, r6, 0x20));
    _mm256_storeu_ps(output + 24, _mm256_permute2f128_ps(r3, r7, 0x20));
    _mm256_storeu_ps(output + 32, _mm256_permute2f128_ps(r0, r4, 0x31));
    _mm256_storeu_ps(output + 40, _mm256_permute2f128_ps(r1, r5, 0x31));
    _mm256_storeu_ps(output + 48, _mm256_permute2f128_ps(r2, r6, 0x31));
    _mm256_storeu_ps(output + 56, _mm256_permute2f128_ps(r3, r7, 0x31));
#else
    for (int y = 0; y < 8; y += 4) {
        for (int x = 0; x < 8; x += 4) {
            SIMD_VECTOR r0 = SIMD_LOAD(input + y * 8 + x);
            SIMD_VECTOR r1 = SIMD_LOAD(input + (y + 1) * 8 + x);
            SIMD_VECTOR r2 = SIMD_LOAD(input + (y + 2) * 8 + x);
            SIMD_VECTOR r3 = SIMD_LOAD(input + (y + 3) * 8 + x);
#if defined(__aarch64__) || defined(_M_ARM64)
            float32x4x2_t a = vtrnq_f32(r0, r1), b = vtrnq_f32(r2, r3);
            r0 = vcombine_f32(vget_low_f32(a.val[0]), vget_low_f32(b.val[0]));
            r1 = vcombine_f32(vget_low_f32(a.val[1]), vget_low_f32(b.val[1]));
            r2 = vcombine_f32(vget_high_f32(a.val[0]), vget_high_f32(b.val[0]));
            r3 = vcombine_f32(vget_high_f32(a.val[1]), vget_high_f32(b.val[1]));
#else
            _MM_TRANSPOSE4_PS(r0, r1, r2, r3);
#endif
            SIMD_STORE(output + x * 8 + y, r0);
            SIMD_STORE(output + (x + 1) * 8 + y, r1);
            SIMD_STORE(output + (x + 2) * 8 + y, r2);
            SIMD_STORE(output + (x + 3) * 8 + y, r3);
        }
    }
#endif
}

static SIMD_TARGET SIMD_INTEGER SIMD_NAME(quantize)(SIMD_VECTOR value, float minimum, int32_t *output) {
#if defined(__aarch64__) || defined(_M_ARM64)
    SIMD_VECTOR magnitude = SIMD_ADD(vabsq_f32(value), SIMD_SET(0.5f));
    value = vbslq_f32(vcltq_f32(value, SIMD_ZERO()), vnegq_f32(magnitude), magnitude);
    value = SIMD_MIN(SIMD_MAX(value, SIMD_SET(minimum)), SIMD_SET(1023));
    int32x4_t quantized = vcvtq_s32_f32(value);
    // Only the first coefficient of a block may use -1024.
    if (minimum == -1024) quantized = vmaxq_s32(quantized, (int32x4_t){-1024,-1023,-1023,-1023});
    vst1q_s32(output, quantized);
    return quantized;
#elif SIMD_WIDTH == 8
    __m256 sign = _mm256_and_ps(value, _mm256_set1_ps(-0.0f));
    value = _mm256_xor_ps(_mm256_add_ps(_mm256_andnot_ps(_mm256_set1_ps(-0.0f), value), SIMD_SET(0.5f)), sign);
    __m256 limits = minimum == -1024 ? _mm256_setr_ps(-1024,-1023,-1023,-1023,-1023,-1023,-1023,-1023) : SIMD_SET(-1023);
    value = SIMD_MIN(SIMD_MAX(value, limits), SIMD_SET(1023));
    __m256i quantized = _mm256_cvttps_epi32(value);
    _mm256_storeu_si256((__m256i *)output, quantized);
    return quantized;
#else
    __m128 sign = _mm_and_ps(value, _mm_set1_ps(-0.0f));
    value = _mm_xor_ps(_mm_add_ps(_mm_andnot_ps(_mm_set1_ps(-0.0f), value), SIMD_SET(0.5f)), sign);
    __m128 limits = minimum == -1024 ? _mm_setr_ps(-1024,-1023,-1023,-1023) : SIMD_SET(-1023);
    value = SIMD_MIN(SIMD_MAX(value, limits), SIMD_SET(1023));
    __m128i quantized = _mm_cvttps_epi32(value);
    _mm_storeu_si128((__m128i *)output, quantized);
    return quantized;
#endif
}

static SIMD_TARGET unsigned SIMD_NAME(fdct)(const float *samples, int32_t *output, const float *reciprocal_quantization) {
    float columns[64], transposed[64];
    for (int x = 0; x < 8; x += SIMD_WIDTH) {
        SIMD_VECTOR r[8];
        for (int y = 0; y < 8; y++) r[y] = SIMD_LOAD(samples + y * 8 + x);
        SIMD_NAME(dct8)(r);
        for (int y = 0; y < 8; y++) SIMD_STORE(columns + y * 8 + x, r[y]);
    }
    SIMD_NAME(transpose)(columns, transposed);
    for (int x = 0; x < 8; x += SIMD_WIDTH) {
        SIMD_VECTOR r[8];
        for (int y = 0; y < 8; y++) r[y] = SIMD_LOAD(transposed + y * 8 + x);
        SIMD_NAME(dct8)(r);
        for (int y = 0; y < 8; y++) SIMD_STORE(columns + y * 8 + x, r[y]);
    }
#if defined(__aarch64__) || defined(_M_ARM64)
    SIMD_INTEGER last = vdupq_n_s32(0);
#elif SIMD_WIDTH == 8
    SIMD_INTEGER last = _mm256_setzero_si256();
#else
    SIMD_INTEGER last = _mm_setzero_si128();
#endif
    for (int i = 0; i < 64; i += SIMD_WIDTH) {
        SIMD_VECTOR value = SIMD_MUL(SIMD_LOAD(columns + i), SIMD_LOAD(reciprocal_quantization + i));
        SIMD_INTEGER quantized = SIMD_NAME(quantize)(value, i == 0 ? -1024 : -1023, output + i);
#if defined(__aarch64__) || defined(_M_ARM64)
        int32x4_t positions = vld1q_s32(scan_positions + i);
        positions = vbicq_s32(positions, vreinterpretq_s32_u32(vceqq_s32(quantized, vdupq_n_s32(0))));
        last = vmaxq_s32(last, positions);
#elif SIMD_WIDTH == 8
        __m256i positions = _mm256_loadu_si256((const __m256i *)(scan_positions + i));
        positions = _mm256_andnot_si256(_mm256_cmpeq_epi32(quantized, _mm256_setzero_si256()), positions);
        last = _mm256_max_epi32(last, positions);
#else
        __m128i positions = _mm_loadu_si128((const __m128i *)(scan_positions + i));
        positions = _mm_andnot_si128(_mm_cmpeq_epi32(quantized, _mm_setzero_si128()), positions);
        __m128i higher = _mm_cmpgt_epi32(positions, last);
        last = _mm_or_si128(_mm_and_si128(higher, positions), _mm_andnot_si128(higher, last));
#endif
    }
    int32_t lanes[SIMD_WIDTH];
#if defined(__aarch64__) || defined(_M_ARM64)
    vst1q_s32(lanes, last);
#elif SIMD_WIDTH == 8
    _mm256_storeu_si256((__m256i *)lanes, last);
#else
    _mm_storeu_si128((__m128i *)lanes, last);
#endif
    unsigned last_index = 0;
    for (int i = 0; i < SIMD_WIDTH; i++) if ((unsigned)lanes[i] > last_index) last_index = (unsigned)lanes[i];
    return last_index;
}

// Adjoint of dct4/dct8. Applying the same normalization to the quantizers
// gives the inverse orthonormal transform without a second set of constants.
static SIMD_TARGET void SIMD_NAME(inverse4)(SIMD_VECTOR *r) {
    SIMD_VECTOR even0 = SIMD_ADD(r[0], SIMD_MUL(r[2], SIMD_SET(0.7071067812f)));
    SIMD_VECTOR even1 = SIMD_SUB(r[0], SIMD_MUL(r[2], SIMD_SET(0.7071067812f)));
    SIMD_VECTOR shared = SIMD_MUL(SIMD_ADD(r[1], r[3]), SIMD_SET(0.7071067812f));
    SIMD_VECTOR odd0 = SIMD_MUL(SIMD_ADD(r[1], shared), SIMD_SET(0.5411961001f));
    SIMD_VECTOR odd1 = SIMD_MUL(SIMD_SUB(r[1], shared), SIMD_SET(1.3065629649f));
    r[0] = SIMD_ADD(even0, odd0); r[3] = SIMD_SUB(even0, odd0);
    r[1] = SIMD_ADD(even1, odd1); r[2] = SIMD_SUB(even1, odd1);
}

static SIMD_TARGET void SIMD_NAME(inverse8)(SIMD_VECTOR *r) {
    SIMD_VECTOR even[4] = {r[0], r[2], r[4], r[6]};
    SIMD_VECTOR odd[4] = {r[1], SIMD_ADD(r[1], r[3]), SIMD_ADD(r[3], r[5]), SIMD_ADD(r[5], r[7])};
    SIMD_NAME(inverse4)(even);
    SIMD_NAME(inverse4)(odd);
    odd[0] = SIMD_MUL(odd[0], SIMD_SET(0.5097955791f));
    odd[1] = SIMD_MUL(odd[1], SIMD_SET(0.6013448869f));
    odd[2] = SIMD_MUL(odd[2], SIMD_SET(0.8999762231f));
    odd[3] = SIMD_MUL(odd[3], SIMD_SET(2.5629154477f));
    r[0] = SIMD_ADD(even[0], odd[0]); r[7] = SIMD_SUB(even[0], odd[0]);
    r[1] = SIMD_ADD(even[1], odd[1]); r[6] = SIMD_SUB(even[1], odd[1]);
    r[2] = SIMD_ADD(even[2], odd[2]); r[5] = SIMD_SUB(even[2], odd[2]);
    r[3] = SIMD_ADD(even[3], odd[3]); r[4] = SIMD_SUB(even[3], odd[3]);
}

static SIMD_TARGET void SIMD_NAME(idct)(const int32_t *input, const float *quantization, float *output) {
    int32_t ac = 0;
    for (int i = 1; i < 64; i++) ac |= input[i];
    if (!ac) {
        float value = (float)input[0] * quantization[0] + 128;
        value = value <= 0 ? 0 : value >= 255 ? 255 : (float)(int)(value + 0.5f);
        for (int i = 0; i < 64; i++) output[i] = value;
        return;
    }
    float coefficients[64], columns[64], transposed[64];
    for (int i = 0; i < 64; i += SIMD_WIDTH) {
#if defined(__aarch64__) || defined(_M_ARM64)
        SIMD_VECTOR values = vcvtq_f32_s32(vld1q_s32(input + i));
#elif SIMD_WIDTH == 8
        SIMD_VECTOR values = _mm256_cvtepi32_ps(_mm256_loadu_si256((const __m256i *)(input + i)));
#else
        SIMD_VECTOR values = _mm_cvtepi32_ps(_mm_loadu_si128((const __m128i *)(input + i)));
#endif
        SIMD_STORE(coefficients + i, SIMD_MUL(values, SIMD_LOAD(quantization + i)));
    }
    for (int x = 0; x < 8; x += SIMD_WIDTH) {
        SIMD_VECTOR r[8];
        for (int y = 0; y < 8; y++) r[y] = SIMD_LOAD(coefficients + y * 8 + x);
        SIMD_NAME(inverse8)(r);
        for (int y = 0; y < 8; y++) SIMD_STORE(columns + y * 8 + x, r[y]);
    }
    SIMD_NAME(transpose)(columns, transposed);
    for (int x = 0; x < 8; x += SIMD_WIDTH) {
        SIMD_VECTOR r[8];
        for (int y = 0; y < 8; y++) r[y] = SIMD_LOAD(transposed + y * 8 + x);
        SIMD_NAME(inverse8)(r);
        for (int y = 0; y < 8; y++) SIMD_STORE(columns + y * 8 + x, r[y]);
    }
    SIMD_NAME(transpose)(columns, transposed);
    for (int i = 0; i < 64; i += SIMD_WIDTH) {
        SIMD_VECTOR values = SIMD_ADD(SIMD_LOAD(transposed + i), SIMD_SET(128.5f));
        values = SIMD_MIN(SIMD_MAX(values, SIMD_ZERO()), SIMD_SET(255));
#if defined(__aarch64__) || defined(_M_ARM64)
        values = vcvtq_f32_s32(vcvtq_s32_f32(values));
#elif SIMD_WIDTH == 8
        values = _mm256_cvtepi32_ps(_mm256_cvttps_epi32(values));
#else
        values = _mm_cvtepi32_ps(_mm_cvttps_epi32(values));
#endif
        SIMD_STORE(output + i, values);
    }
}
