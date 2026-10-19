/* AnyKVM's own reduce-extrapolate DWT from MS-RDPEGFX 3.2.8.1.2.2.
 * Included with a four/eight-lane vector vocabulary by our C kernels.
 * Interior lifting is shared; all odd/first-level boundaries remain explicit. */
static ANYKVM_REDUCE_TARGET void ANYKVM_REDUCE_LINE(const int32_t *input, int32_t *low,
                                               int32_t *high, unsigned size, int simd) {
    unsigned n = size == 64 ? 31 : size / 2, lo = size == 64 ? 33 : (size + 1) / 2;
    unsigned i = 0;
#if ANYKVM_REDUCE_LANES
    if (simd) for (; i + ANYKVM_REDUCE_LANES <= n; i += ANYKVM_REDUCE_LANES) {
        ANYKVM_REDUCE_VEC even, odd;
        ANYKVM_REDUCE_EVEN_ODD(input + 2 * i, &even, &odd);
        ANYKVM_REDUCE_VEC next = ANYKVM_REDUCE_NEXT(even, input[2 * (i + ANYKVM_REDUCE_LANES)]);
        ANYKVM_REDUCE_STORE(high + i, ANYKVM_REDUCE_SHIFT(ANYKVM_REDUCE_ADD(
            ANYKVM_REDUCE_SUB(ANYKVM_REDUCE_SUB(ANYKVM_REDUCE_ADD(odd, odd), even), next), ANYKVM_REDUCE_CONST(2)), 2));
    }
#else
    (void)simd;
#endif
    for (; i < n; ++i) high[i] = (2 * input[2*i+1] - input[2*i] - input[2*i+2] + 2) >> 2;
    i = 0;
#if ANYKVM_REDUCE_LANES
    if (simd) for (; i + ANYKVM_REDUCE_LANES <= n; i += ANYKVM_REDUCE_LANES) {
        ANYKVM_REDUCE_VEC even, odd;
        ANYKVM_REDUCE_EVEN_ODD(input + 2*i, &even, &odd);
        ANYKVM_REDUCE_VEC current = ANYKVM_REDUCE_LOAD(high + i);
        ANYKVM_REDUCE_VEC sum = ANYKVM_REDUCE_ADD(ANYKVM_REDUCE_PREVIOUS(current, high[i ? i-1 : 0]), current);
        ANYKVM_REDUCE_STORE(low + i, ANYKVM_REDUCE_ADD(even, ANYKVM_REDUCE_SHIFT(ANYKVM_REDUCE_ADD(sum, ANYKVM_REDUCE_CONST(1)), 1)));
    }
#endif
    for (; i < lo; ++i) {
        int32_t value = 2*i < size ? input[2*i] : 2*input[63] - input[62];
        int32_t left = 0, right = 0;
        if (size == 64) {
            if (i <= n) left = high[i ? i-1 : 0];
            if (i < n) right = high[i];
        } else {
            left = high[i ? i-1 : 0]; right = high[i < n ? i : n-1];
        }
        low[i] = value + ((left + right + 1) >> 1);
    }
}

ANYKVM_REDUCE_TARGET void ANYKVM_REDUCE_DWT(const int32_t *input, int32_t *vertical,
                                       int32_t *bands, int32_t *coeffs, unsigned quantization, int simd) {
    int32_t low[33], high[32];
    const unsigned sizes[] = {64,33,17};
    size_t offset = 0;
    for (unsigned level = 0; level < 3; ++level) {
        unsigned size = sizes[level], n = size == 64 ? 31 : size/2, lo = size == 64 ? 33 : (size+1)/2;
        for (unsigned y = 0; y < n; ++y) {
            unsigned x = 0;
#if ANYKVM_REDUCE_LANES
            if (simd) for (; x + ANYKVM_REDUCE_LANES <= size; x += ANYKVM_REDUCE_LANES) {
                ANYKVM_REDUCE_VEC a = ANYKVM_REDUCE_LOAD(input + 2*y*size+x);
                ANYKVM_REDUCE_VEC b = ANYKVM_REDUCE_LOAD(input + (2*y+1)*size+x);
                ANYKVM_REDUCE_VEC c = ANYKVM_REDUCE_LOAD(input + (2*y+2)*size+x);
                ANYKVM_REDUCE_VEC value = ANYKVM_REDUCE_SUB(ANYKVM_REDUCE_SUB(ANYKVM_REDUCE_ADD(b,b),a),c);
                ANYKVM_REDUCE_STORE(vertical + (lo+y)*size+x, ANYKVM_REDUCE_SHIFT(ANYKVM_REDUCE_ADD(value,ANYKVM_REDUCE_CONST(2)),2));
            }
#endif
            for (; x < size; ++x) vertical[(lo+y)*size+x] =
                (2*input[(2*y+1)*size+x]-input[2*y*size+x]-input[(2*y+2)*size+x]+2)>>2;
        }
        for (unsigned y = 0; y < lo; ++y) {
            unsigned left = y ? y-1 : 0, right = y < n ? y : n-1, x = 0;
#if ANYKVM_REDUCE_LANES
            if (simd) for (; x + ANYKVM_REDUCE_LANES <= size; x += ANYKVM_REDUCE_LANES) {
                ANYKVM_REDUCE_VEC a = size == 64 && left >= n ? ANYKVM_REDUCE_CONST(0) : ANYKVM_REDUCE_LOAD(vertical + (lo+left)*size+x);
                ANYKVM_REDUCE_VEC b = size == 64 && y >= n ? ANYKVM_REDUCE_CONST(0) : ANYKVM_REDUCE_LOAD(vertical + (lo+right)*size+x);
                ANYKVM_REDUCE_VEC value;
                if (2*y < size) value = ANYKVM_REDUCE_LOAD(input + 2*y*size+x);
                else {
                    value = ANYKVM_REDUCE_LOAD(input + 63*size+x);
                    value = ANYKVM_REDUCE_SUB(ANYKVM_REDUCE_ADD(value,value),ANYKVM_REDUCE_LOAD(input + 62*size+x));
                }
                ANYKVM_REDUCE_STORE(vertical+y*size+x, ANYKVM_REDUCE_ADD(value, ANYKVM_REDUCE_SHIFT(
                    ANYKVM_REDUCE_ADD(ANYKVM_REDUCE_ADD(a,b),ANYKVM_REDUCE_CONST(1)),1)));
            }
#endif
            for (; x < size; ++x) {
                int32_t a = size == 64 && left >= n ? 0 : vertical[(lo+left)*size+x];
                int32_t b = size == 64 && y >= n ? 0 : vertical[(lo+right)*size+x];
                int32_t value = 2*y < size ? input[2*y*size+x] : 2*input[63*size+x]-input[62*size+x];
                vertical[y*size+x] = value + ((a+b+1)>>1);
            }
        }
        size_t hl = n*lo, lh = lo*n, hh = n*n, count = hl+lh+hh;
        for (unsigned y = 0; y < size; ++y) {
            ANYKVM_REDUCE_LINE(vertical+y*size,low,high,size,simd);
            if (y < lo) {
                memcpy(bands + count+y*lo,low,lo*sizeof(int32_t));
                memcpy(bands + y*n,high,n*sizeof(int32_t));
            } else {
                memcpy(bands + hl+(y-lo)*lo,low,lo*sizeof(int32_t));
                memcpy(bands + hl+lh+(y-lo)*n,high,n*sizeof(int32_t));
            }
        }
        memcpy(coeffs+offset,bands,count*sizeof(int32_t)); offset += count;
        input = bands+count;
    }
    memcpy(coeffs+offset,input,81*sizeof(int32_t));
    unsigned bits = quantization-1, i = 0;
    int32_t round = (int32_t)(1u << (bits-1));
#if ANYKVM_REDUCE_LANES
    if (simd) for (; i < 4096; i += ANYKVM_REDUCE_LANES)
        ANYKVM_REDUCE_STORE(coeffs+i, ANYKVM_REDUCE_QUANT_SHIFT(ANYKVM_REDUCE_ADD(ANYKVM_REDUCE_LOAD(coeffs+i),ANYKVM_REDUCE_CONST(round)),bits));
#endif
    for (; i < 4096; ++i) coeffs[i] = (coeffs[i]+round)>>bits;
}
#undef ANYKVM_REDUCE_TARGET
#undef ANYKVM_REDUCE_LINE
#undef ANYKVM_REDUCE_DWT
#undef ANYKVM_REDUCE_LANES
#undef ANYKVM_REDUCE_VEC
#undef ANYKVM_REDUCE_LOAD
#undef ANYKVM_REDUCE_STORE
#undef ANYKVM_REDUCE_CONST
#undef ANYKVM_REDUCE_ADD
#undef ANYKVM_REDUCE_SUB
#undef ANYKVM_REDUCE_SHIFT
#undef ANYKVM_REDUCE_QUANT_SHIFT
#undef ANYKVM_REDUCE_EVEN_ODD
#undef ANYKVM_REDUCE_NEXT
#undef ANYKVM_REDUCE_PREVIOUS
