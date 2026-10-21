// Exercise the native entropy functions independently of color conversion/DWT.
#include "../progressive_native.c"

int main(void) {
    char message[256];
    rfx_encoder *e = bk_rfx_open(message, sizeof(message));
    if (!e) return 1;
    // Force both single-byte and full-word writes through buffer growth.
    e->output = realloc(e->output, 1);
    e->capacity = 1;
    uint32_t request[4];
    while (fread(request, sizeof(request), 1, stdin) == 1) {
        unsigned kind = request[0], mode = request[1], count = request[2], extra = request[3];
        if (count > 4096 || !bk_rfx_set_simd(e, (int)mode)) return 2;
        e->length = 0; e->failed = 0; e->pending = 0; e->used = 0;
        if (kind == 0) {
            for (unsigned i = 0; i < count; i++) {
                uint32_t pair[2];
                if (fread(pair, sizeof(pair), 1, stdin) != 1 || pair[1] > 32) return 3;
                bits(e, pair[0], pair[1]);
            }
            finish_bits(e);
        } else {
            if (fread(e->coeffs, sizeof(int32_t), count, stdin) != count) return 3;
            if (kind == 1 && count == 4096) rlgr(e);
            else if (kind == 2 && extra >= 1 && extra <= 4) srl(e, count, extra);
            else return 4;
        }
        if (e->failed) return 5;
        uint32_t size = (uint32_t)e->length;
        fwrite(&size, sizeof(size), 1, stdout);
        fwrite(e->output, 1, size, stdout);
    }
    if (!feof(stdin)) return 6;
    // Grouped packets retain individual frame boundaries and owned byte sizes,
    // even when subsequent calls reuse/grow the same native output buffer.
    uint8_t *pixels = calloc(1024 * 64, 4);
    anykvm_rfx_rect rects[16];
    for (unsigned i = 0; i < 16; ++i)
        rects[i] = (anykvm_rfx_rect){(uint16_t)(i * 64), 0, 64, 64};
    size_t ends[2] = {8, 16}, lengths[2], size;
    const uint8_t *output;
    if (!pixels || !bk_rfx_run_packets(e, pixels, 1024 * 64 * 4, ANYKVM_RFX_BGRA,
            1024, 64, rects, 16, ends, 2, 9, 1, ANYKVM_RFX_SIMPLE, 0, NULL, 0,
            &output, &size, lengths, message, sizeof(message))) return 8;
    if (size != lengths[0] + lengths[1]) return 9;
    uint8_t *saved = malloc(size);
    if (!saved) return 10;
    memcpy(saved, output, size);
    size_t offset = 0;
    for (unsigned i = 0; i < 2; ++i) {
        if (!bk_rfx_run_pass(e, pixels, 1024 * 64 * 4, ANYKVM_RFX_BGRA,
                1024, 64, rects + 8 * i, 8, 9, i == 0, ANYKVM_RFX_SIMPLE, 0,
                NULL, 0, &output, &size, message, sizeof(message)) ||
            size != lengths[i] || memcmp(saved + offset, output, size)) return 11;
        offset += size;
    }
    ends[1] = 7;
    if (bk_rfx_run_packets(e, pixels, 1024 * 64 * 4, ANYKVM_RFX_BGRA,
            1024, 64, rects, 16, ends, 2, 9, 0, ANYKVM_RFX_SIMPLE, 0, NULL, 0,
            &output, &size, lengths, message, sizeof(message))) return 12;
    free(saved); free(pixels);
    // An eight-byte flush must reject seven bytes of remaining capacity.
    free(e->output);
    e->output = malloc(MAX_OUTPUT);
    e->capacity = MAX_OUTPUT; e->length = MAX_OUTPUT - 7;
    e->pending = 0; e->used = 0;
    bits(e, UINT32_MAX, 32); bits(e, UINT32_MAX, 32);
    if (!e->failed || e->length != MAX_OUTPUT - 7) return 7;
    bk_rfx_close(e);
    return 0;
}
