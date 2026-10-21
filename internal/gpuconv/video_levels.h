#ifndef ANYKVM_VIDEO_LEVELS_H
#define ANYKVM_VIDEO_LEVELS_H
#include "third_party/vk_video/vulkan_video_codec_av1std.h"
#include "third_party/vk_video/vulkan_video_codec_h265std.h"
#include <stdint.h>

static uint32_t video_log2_ceil(uint32_t n) {
    uint32_t bits = 0;
    for (uint32_t value = n - 1; value; value >>= 1)
        bits++;
    return bits;
}
static StdVideoH265LevelIdc video_h265_level(uint32_t w, uint32_t h, uint32_t num, uint32_t den, uint32_t kbps) {
    w = (w + 7) & ~7u;
    h = (h + 7) & ~7u;
    const struct {
        StdVideoH265LevelIdc id;
        uint32_t picture;
        uint64_t samples;
        uint32_t kbps;
    } levels[] = {{STD_VIDEO_H265_LEVEL_IDC_1_0, 36864, 552960, 128},
                  {STD_VIDEO_H265_LEVEL_IDC_2_0, 122880, 3686400, 1500},
                  {STD_VIDEO_H265_LEVEL_IDC_2_1, 245760, 7372800, 3000},
                  {STD_VIDEO_H265_LEVEL_IDC_3_0, 552960, 16588800, 6000},
                  {STD_VIDEO_H265_LEVEL_IDC_3_1, 983040, 33177600, 10000},
                  {STD_VIDEO_H265_LEVEL_IDC_4_0, 2228224, 66846720, 12000},
                  {STD_VIDEO_H265_LEVEL_IDC_4_1, 2228224, 133693440, 20000},
                  {STD_VIDEO_H265_LEVEL_IDC_5_0, 8912896, 267386880, 25000},
                  {STD_VIDEO_H265_LEVEL_IDC_5_1, 8912896, 534773760, 40000},
                  {STD_VIDEO_H265_LEVEL_IDC_5_2, 8912896, 1069547520, 60000},
                  {STD_VIDEO_H265_LEVEL_IDC_6_0, 35651584, 1069547520, 60000},
                  {STD_VIDEO_H265_LEVEL_IDC_6_1, 35651584, 2139095040, 120000},
                  {STD_VIDEO_H265_LEVEL_IDC_6_2, 35651584, 4278190080ull, 240000}};
    for (unsigned i = 0; i < sizeof(levels) / sizeof(*levels); i++)
        if ((uint64_t)w * h <= levels[i].picture && (uint64_t)w * w <= 8ull * levels[i].picture &&
            (uint64_t)h * h <= 8ull * levels[i].picture && (uint64_t)w * h * num <= levels[i].samples * den &&
            kbps <= levels[i].kbps)
            return levels[i].id;
    return STD_VIDEO_H265_LEVEL_IDC_INVALID;
}
static StdVideoAV1Level video_av1_level(uint32_t w, uint32_t h, uint32_t num, uint32_t den, uint32_t kbps) {
    // AOM AV1 Annex A, main tier, no hidden frames (display/decode cadence equal).
    const struct {
        StdVideoAV1Level id;
        uint32_t picture, w, h;
        uint64_t samples;
        uint32_t kbps;
    } levels[] = {{STD_VIDEO_AV1_LEVEL_2_0, 147456, 2048, 1152, 4423680, 1500},
                  {STD_VIDEO_AV1_LEVEL_2_1, 278784, 2816, 1584, 8363520, 3000},
                  {STD_VIDEO_AV1_LEVEL_3_0, 665856, 4352, 2448, 19975680, 6000},
                  {STD_VIDEO_AV1_LEVEL_3_1, 1065024, 5504, 3096, 31950720, 10000},
                  {STD_VIDEO_AV1_LEVEL_4_0, 2359296, 6144, 3456, 70778880, 12000},
                  {STD_VIDEO_AV1_LEVEL_4_1, 2359296, 6144, 3456, 141557760, 20000},
                  {STD_VIDEO_AV1_LEVEL_5_0, 8912896, 8192, 4352, 267386880, 30000},
                  {STD_VIDEO_AV1_LEVEL_5_1, 8912896, 8192, 4352, 534773760, 40000},
                  {STD_VIDEO_AV1_LEVEL_5_2, 8912896, 8192, 4352, 1069547520, 60000},
                  {STD_VIDEO_AV1_LEVEL_6_0, 35651584, 16384, 8704, 1069547520, 60000},
                  {STD_VIDEO_AV1_LEVEL_6_1, 35651584, 16384, 8704, 2139095040, 100000},
                  {STD_VIDEO_AV1_LEVEL_6_2, 35651584, 16384, 8704, 4278190080ull, 160000}};
    for (unsigned i = 0; i < sizeof(levels) / sizeof(*levels); i++)
        if ((uint64_t)w * h <= levels[i].picture && w <= levels[i].w && h <= levels[i].h &&
            (uint64_t)w * h * num <= levels[i].samples * den && kbps <= levels[i].kbps)
            return levels[i].id;
    return STD_VIDEO_AV1_LEVEL_INVALID;
}

#endif
