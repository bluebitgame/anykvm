// Codec-specific policy and syntax. Submission, memory, NV12 and synchronization
// remain in video_linux.inc. Main profiles are SDR 8-bit 4:2:0, one reference.
#include "video_levels.h"
static int video_av1_tiles(bk_vk_encoder *e, char *error, size_t capacity) {
    uint32_t sb = 1u << e->av1_sb_log2, mi_sb = sb / 4;
    uint32_t mi_cols = ((e->width + 7) / 8) * 2, mi_rows = ((e->height + 7) / 8) * 2;
    uint32_t sb_cols = (mi_cols + mi_sb - 1) / mi_sb, sb_rows = (mi_rows + mi_sb - 1) / mi_sb;
    uint32_t max_w = e->av1_caps.maxTileSize.width / sb, max_h = e->av1_caps.maxTileSize.height / sb;
    if (!max_w || !max_h)
        goto failed;
    if (max_w > 4096 / sb)
        max_w = 4096 / sb;
    uint32_t cols = 1, rows = 1, tile_w = sb_cols, tile_h = sb_rows;
    while (tile_w > max_w && cols < 64) {
        cols *= 2;
        tile_w = (sb_cols + cols - 1) / cols;
    }
    while ((tile_h > max_h || (uint64_t)tile_w * tile_h * sb * sb > 4096ull * 2304) && rows < 64) {
        rows *= 2;
        tile_h = (sb_rows + rows - 1) / rows;
    }
    cols = (sb_cols + tile_w - 1) / tile_w;
    rows = (sb_rows + tile_h - 1) / tile_h;
    if (tile_w > max_w || tile_h > max_h || cols > 64 || rows > 64 || cols > e->av1_caps.maxTiles.width ||
        rows > e->av1_caps.maxTiles.height)
        goto failed;
    for (uint32_t i = 0; i <= cols; i++)
        e->av1_col_starts[i] = i == cols ? mi_cols : i * tile_w * mi_sb;
    for (uint32_t i = 0; i <= rows; i++)
        e->av1_row_starts[i] = i == rows ? mi_rows : i * tile_h * mi_sb;
    for (uint32_t i = 0; i < cols; i++) {
        uint32_t width = (e->av1_col_starts[i + 1] - e->av1_col_starts[i] + mi_sb - 1) / mi_sb;
        e->av1_tile_widths[i] = width - 1;
        if (cols > 1 && width * sb < e->av1_caps.minTileSize.width)
            goto failed;
    }
    for (uint32_t i = 0; i < rows; i++) {
        uint32_t height = (e->av1_row_starts[i + 1] - e->av1_row_starts[i] + mi_sb - 1) / mi_sb;
        e->av1_tile_heights[i] = height - 1;
        if (rows > 1 && height * sb < e->av1_caps.minTileSize.height)
            goto failed;
    }
    e->av1_tiles = (StdVideoAV1TileInfo){.flags = {.uniform_tile_spacing_flag = 1},
                                         .TileCols = cols,
                                         .TileRows = rows,
                                         .tile_size_bytes_minus_1 = 3,
                                         .pMiColStarts = e->av1_col_starts,
                                         .pMiRowStarts = e->av1_row_starts,
                                         .pWidthInSbsMinus1 = e->av1_tile_widths,
                                         .pHeightInSbsMinus1 = e->av1_tile_heights};
    return 0;
failed:
    snprintf(error, capacity, "Vulkan AV1 cannot tile the requested extent with this device");
    return -1;
}
static int video_extra_configure(bk_vk_encoder *e, int num, int den, int bitrate, char *error, size_t capacity) {
    uint32_t alignment = 16;
    if (e->codec == 1) {
        e->h265_level = video_h265_level(e->width, e->height, num, den, bitrate);
        if (e->h265_level == STD_VIDEO_H265_LEVEL_IDC_INVALID || e->h265_level > e->h265_caps.maxLevelIdc ||
            !e->h265_caps.maxSliceSegmentCount || !e->h265_caps.maxSubLayerCount)
            goto failed;
        e->ctb_log2 = (e->h265_caps.ctbSizes & VK_VIDEO_ENCODE_H265_CTB_SIZE_64_BIT_KHR)   ? 6
                      : (e->h265_caps.ctbSizes & VK_VIDEO_ENCODE_H265_CTB_SIZE_32_BIT_KHR) ? 5
                      : (e->h265_caps.ctbSizes & VK_VIDEO_ENCODE_H265_CTB_SIZE_16_BIT_KHR) ? 4
                                                                                           : 0;
        e->transform_log2 =
            (e->h265_caps.transformBlockSizes & VK_VIDEO_ENCODE_H265_TRANSFORM_BLOCK_SIZE_32_BIT_KHR)   ? 5
            : (e->h265_caps.transformBlockSizes & VK_VIDEO_ENCODE_H265_TRANSFORM_BLOCK_SIZE_16_BIT_KHR) ? 4
            : (e->h265_caps.transformBlockSizes & VK_VIDEO_ENCODE_H265_TRANSFORM_BLOCK_SIZE_8_BIT_KHR)  ? 3
                                                                                                        : 2;
        if (!e->ctb_log2 || !(e->h265_caps.transformBlockSizes & VK_VIDEO_ENCODE_H265_TRANSFORM_BLOCK_SIZE_4_BIT_KHR))
            goto failed;
        if (e->transform_log2 > e->ctb_log2)
            e->transform_log2 = e->ctb_log2;
        alignment = 1u << e->ctb_log2;
    } else {
        e->av1_level = video_av1_level(e->width, e->height, num, den, bitrate);
        if (e->av1_level == STD_VIDEO_AV1_LEVEL_INVALID || e->av1_level > e->av1_caps.maxLevel ||
            !e->av1_caps.maxOperatingPoints || !e->av1_caps.maxTemporalLayerCount || !e->av1_caps.maxSpatialLayerCount)
            goto failed;
        e->av1_sb_log2 = (e->av1_caps.superblockSizes & VK_VIDEO_ENCODE_AV1_SUPERBLOCK_SIZE_64_BIT_KHR)    ? 6
                         : (e->av1_caps.superblockSizes & VK_VIDEO_ENCODE_AV1_SUPERBLOCK_SIZE_128_BIT_KHR) ? 7
                                                                                                           : 0;
        if (!e->av1_sb_log2)
            goto failed;
        // AV1 has no SPS conformance window. Do not silently change the
        // negotiated dimensions when a driver pads coded pictures.
        uint32_t aw = e->av1_caps.codedPictureAlignment.width, ah = e->av1_caps.codedPictureAlignment.height;
        if (!aw || !ah || e->width % aw || e->height % ah) {
            snprintf(error, capacity, "Vulkan AV1 requires coded size alignment %ux%u", aw, ah);
            return -1;
        }
        if (video_av1_tiles(e, error, capacity) < 0)
            return -1;
        alignment = 1u << e->av1_sb_log2;
    }
    uint32_t gw = e->caps.pictureAccessGranularity.width, gh = e->caps.pictureAccessGranularity.height;
    if (gw < alignment)
        gw = alignment;
    if (gh < alignment)
        gh = alignment;
    e->aligned_width = (e->width + gw - 1) / gw * gw;
    e->aligned_height = (e->height + gh - 1) / gh * gh;
    return 0;
failed:
    snprintf(error, capacity, "Vulkan %s level/CBR/reference/block capabilities cannot cover requested output",
             e->codec == 1 ? "HEVC" : "AV1");
    return -1;
}
static int video_extra_parameters(bk_vk_encoder *e, int num, int den, char *error, size_t capacity) {
    bk_vk *v = e->v;
    VkVideoSessionParametersCreateInfoKHR create = {.sType = VK_STRUCTURE_TYPE_VIDEO_SESSION_PARAMETERS_CREATE_INFO_KHR,
                                                    .videoSession = e->session};
    VkVideoEncodeH265SessionParametersGetInfoKHR h265_get = {
        .sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_H265_SESSION_PARAMETERS_GET_INFO_KHR,
        .writeStdVPS = 1,
        .writeStdSPS = 1,
        .writeStdPPS = 1};
    if (e->codec == 1) {
        StdVideoH265ProfileTierLevel tier = {.flags = {.general_progressive_source_flag = 1,
                                                       .general_non_packed_constraint_flag = 1,
                                                       .general_frame_only_constraint_flag = 1},
                                             .general_profile_idc = STD_VIDEO_H265_PROFILE_IDC_MAIN,
                                             .general_level_idc = e->h265_level};
        StdVideoH265DecPicBufMgr dpb = {0};
        dpb.max_dec_pic_buffering_minus1[0] = 1;
        StdVideoH265VideoParameterSet vps = {
            .flags = {.vps_temporal_id_nesting_flag = 1, .vps_timing_info_present_flag = 1},
            .vps_num_units_in_tick = den,
            .vps_time_scale = num,
            .pDecPicBufMgr = &dpb,
            .pProfileTierLevel = &tier};
        StdVideoH265SequenceParameterSetVui vui = {.flags = {.video_signal_type_present_flag = 1,
                                                             .colour_description_present_flag = 1,
                                                             .vui_timing_info_present_flag = 1},
                                                   .video_format = 5,
                                                   .colour_primaries = 1,
                                                   .transfer_characteristics = 1,
                                                   .matrix_coeffs = 1,
                                                   .vui_num_units_in_tick = den,
                                                   .vui_time_scale = num};
        StdVideoH265SequenceParameterSet sps = {
            .flags = {.sps_temporal_id_nesting_flag = 1, .vui_parameters_present_flag = 1},
            .chroma_format_idc = STD_VIDEO_H265_CHROMA_FORMAT_IDC_420,
            .pic_width_in_luma_samples = (e->width + 7) & ~7u,
            .pic_height_in_luma_samples = (e->height + 7) & ~7u,
            .log2_max_pic_order_cnt_lsb_minus4 = 12,
            .log2_min_luma_coding_block_size_minus3 = 0,
            .log2_diff_max_min_luma_coding_block_size = e->ctb_log2 - 3,
            .log2_min_luma_transform_block_size_minus2 = 0,
            .log2_diff_max_min_luma_transform_block_size = e->transform_log2 - 2,
            .max_transform_hierarchy_depth_inter = e->ctb_log2 - 2,
            .max_transform_hierarchy_depth_intra = e->ctb_log2 - 2,
            .pProfileTierLevel = &tier,
            .pDecPicBufMgr = &dpb,
            .pSequenceParameterSetVui = &vui};
        sps.conf_win_right_offset = (sps.pic_width_in_luma_samples - e->width) / 2;
        sps.conf_win_bottom_offset = (sps.pic_height_in_luma_samples - e->height) / 2;
        sps.flags.conformance_window_flag = sps.conf_win_right_offset || sps.conf_win_bottom_offset;
        StdVideoH265PictureParameterSet pps = {
            .flags = {.deblocking_filter_control_present_flag = 1, .pps_loop_filter_across_slices_enabled_flag = 1},
            .init_qp_minus26 = 0};
        VkVideoEncodeH265SessionParametersAddInfoKHR add = {
            .sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_H265_SESSION_PARAMETERS_ADD_INFO_KHR,
            .stdVPSCount = 1,
            .pStdVPSs = &vps,
            .stdSPSCount = 1,
            .pStdSPSs = &sps,
            .stdPPSCount = 1,
            .pStdPPSs = &pps};
        VkVideoEncodeH265SessionParametersCreateInfoKHR params = {
            .sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_H265_SESSION_PARAMETERS_CREATE_INFO_KHR,
            .maxStdVPSCount = 1,
            .maxStdSPSCount = 1,
            .maxStdPPSCount = 1,
            .pParametersAddInfo = &add};
        create.pNext = &params;
        TRY(v->CreateVideoSessionParametersKHR(v->device, &create, NULL, &e->parameters));
    } else {
        StdVideoAV1ColorConfig color = {.flags = {.color_description_present_flag = 1},
                                        .BitDepth = 8,
                                        .subsampling_x = 1,
                                        .subsampling_y = 1,
                                        .color_primaries = STD_VIDEO_AV1_COLOR_PRIMARIES_BT_709,
                                        .transfer_characteristics = STD_VIDEO_AV1_TRANSFER_CHARACTERISTICS_BT_709,
                                        .matrix_coefficients = STD_VIDEO_AV1_MATRIX_COEFFICIENTS_BT_709,
                                        .chroma_sample_position = STD_VIDEO_AV1_CHROMA_SAMPLE_POSITION_UNKNOWN};
        StdVideoAV1TimingInfo timing = {
            .flags = {.equal_picture_interval = 1}, .num_units_in_display_tick = den, .time_scale = num};
        StdVideoAV1SequenceHeader sequence = {
            .flags = {.use_128x128_superblock = e->av1_sb_log2 == 7, .timing_info_present_flag = 1},
            .seq_profile = STD_VIDEO_AV1_PROFILE_MAIN,
            .frame_width_bits_minus_1 = video_log2_ceil(e->width) - 1,
            .frame_height_bits_minus_1 = video_log2_ceil(e->height) - 1,
            .max_frame_width_minus_1 = e->width - 1,
            .max_frame_height_minus_1 = e->height - 1,
            .seq_force_integer_mv = 0,
            .seq_force_screen_content_tools = 0,
            .pColorConfig = &color,
            .pTimingInfo = &timing};
        StdVideoEncodeAV1OperatingPointInfo op = {.seq_level_idx = e->av1_level};
        VkVideoEncodeAV1SessionParametersCreateInfoKHR params = {
            .sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_AV1_SESSION_PARAMETERS_CREATE_INFO_KHR,
            .pStdSequenceHeader = &sequence,
            .stdOperatingPointCount = 1,
            .pStdOperatingPoints = &op};
        create.pNext = &params;
        TRY(v->CreateVideoSessionParametersKHR(v->device, &create, NULL, &e->parameters));
    }
    VkVideoEncodeSessionParametersGetInfoKHR get = {.sType =
                                                        VK_STRUCTURE_TYPE_VIDEO_ENCODE_SESSION_PARAMETERS_GET_INFO_KHR,
                                                    .pNext = e->codec == 1 ? &h265_get : NULL,
                                                    .videoSessionParameters = e->parameters};
    TRY(v->GetEncodedVideoSessionParametersKHR(v->device, &get, NULL, &e->headers_size, NULL));
    if (!e->headers_size || e->headers_size > 65536)
        goto failed;
    e->headers = malloc(e->headers_size);
    if (!e->headers)
        goto failed;
    TRY(v->GetEncodedVideoSessionParametersKHR(v->device, &get, NULL, &e->headers_size, e->headers));
    return 0;
failed:
    return -1;
}

// All nested pointers live in this per-submit object through command recording.
typedef struct {
    StdVideoEncodeH265ReferenceInfo h265_reconstructed;
    VkVideoEncodeH265DpbSlotInfoKHR h265_dpb[2];
    VkVideoEncodeH265GopRemainingFrameInfoKHR h265_remaining;
    VkVideoEncodeH265RateControlInfoKHR h265_rate;
    StdVideoEncodeH265ReferenceListsInfo h265_lists;
    StdVideoH265ShortTermRefPicSet h265_rps;
    StdVideoEncodeH265PictureInfo h265_picture;
    StdVideoEncodeH265SliceSegmentHeader h265_header;
    VkVideoEncodeH265NaluSliceSegmentInfoKHR h265_slice;
    VkVideoEncodeH265PictureInfoKHR h265;
    StdVideoEncodeAV1ReferenceInfo av1_reconstructed;
    VkVideoEncodeAV1DpbSlotInfoKHR av1_dpb[2];
    VkVideoEncodeAV1GopRemainingFrameInfoKHR av1_remaining;
    VkVideoEncodeAV1RateControlInfoKHR av1_rate;
    StdVideoAV1Quantization av1_quant;
    StdVideoAV1LoopFilter av1_filter;
    StdVideoAV1Segmentation av1_segmentation;
    StdVideoAV1CDEF av1_cdef;
    StdVideoAV1LoopRestoration av1_restoration;
    StdVideoAV1GlobalMotion av1_motion;
    StdVideoEncodeAV1PictureInfo av1_picture;
    VkVideoEncodeAV1PictureInfoKHR av1;
    void *remaining, *rate, *picture;
} video_extra_picture;
static void video_extra_prepare(bk_vk_encoder *e, int idr, uint32_t previous, video_extra_picture *p,
                                VkVideoReferenceSlotInfoKHR slots[2]) {
    memset(p, 0, sizeof(*p));
    if (e->codec == 1) {
        StdVideoH265PictureType type = idr ? STD_VIDEO_H265_PICTURE_TYPE_IDR : STD_VIDEO_H265_PICTURE_TYPE_P;
        p->h265_reconstructed = (StdVideoEncodeH265ReferenceInfo){.pic_type = type, .PicOrderCntVal = e->frame};
        p->h265_dpb[0] =
            (VkVideoEncodeH265DpbSlotInfoKHR){.sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_H265_DPB_SLOT_INFO_KHR,
                                              .pStdReferenceInfo = &p->h265_reconstructed};
        p->h265_dpb[1] = (VkVideoEncodeH265DpbSlotInfoKHR){
            .sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_H265_DPB_SLOT_INFO_KHR, .pStdReferenceInfo = &e->h265_previous};
        slots[0].pNext = &p->h265_dpb[0];
        slots[1].pNext = &p->h265_dpb[1];
        p->h265_remaining = (VkVideoEncodeH265GopRemainingFrameInfoKHR){
            .sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_H265_GOP_REMAINING_FRAME_INFO_KHR,
            .useGopRemainingFrames = e->h265_caps.requiresGopRemainingFrames,
            .gopRemainingI = idr ? 1 : 0,
            .gopRemainingP = e->gop - e->frame - (idr ? 1 : 0)};
        p->h265_rate = e->h265_rc;
        p->h265_rate.pNext = &p->h265_remaining;
        memset(p->h265_lists.RefPicList0, 255, sizeof(p->h265_lists.RefPicList0));
        memset(p->h265_lists.RefPicList1, 255, sizeof(p->h265_lists.RefPicList1));
        if (!idr) {
            p->h265_lists.RefPicList0[0] = previous;
            p->h265_rps.num_negative_pics = 1;
            p->h265_rps.used_by_curr_pic_s0_flag = 1;
        }
        p->h265_picture = (StdVideoEncodeH265PictureInfo){
            .flags = {.is_reference = 1, .IrapPicFlag = idr, .pic_output_flag = 1, .no_output_of_prior_pics_flag = idr},
            .pic_type = type,
            .PicOrderCntVal = e->frame,
            .pRefLists = &p->h265_lists,
            .pShortTermRefPicSet = idr ? NULL : &p->h265_rps};
        p->h265_header = (StdVideoEncodeH265SliceSegmentHeader){
            .flags = {.first_slice_segment_in_pic_flag = 1, .slice_loop_filter_across_slices_enabled_flag = 1},
            .slice_type = idr ? STD_VIDEO_H265_SLICE_TYPE_I : STD_VIDEO_H265_SLICE_TYPE_P,
            .MaxNumMergeCand = 5};
        p->h265_slice = (VkVideoEncodeH265NaluSliceSegmentInfoKHR){
            .sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_H265_NALU_SLICE_SEGMENT_INFO_KHR,
            .pStdSliceSegmentHeader = &p->h265_header};
        p->h265 = (VkVideoEncodeH265PictureInfoKHR){.sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_H265_PICTURE_INFO_KHR,
                                                    .naluSliceSegmentEntryCount = 1,
                                                    .pNaluSliceSegmentEntries = &p->h265_slice,
                                                    .pStdPictureInfo = &p->h265_picture};
        p->remaining = &p->h265_remaining;
        p->rate = &p->h265_rate;
        p->picture = &p->h265;
    } else if (e->codec == 2) {
        StdVideoAV1FrameType type = idr ? STD_VIDEO_AV1_FRAME_TYPE_KEY : STD_VIDEO_AV1_FRAME_TYPE_INTER;
        p->av1_reconstructed = (StdVideoEncodeAV1ReferenceInfo){.frame_type = type};
        p->av1_dpb[0] = (VkVideoEncodeAV1DpbSlotInfoKHR){.sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_AV1_DPB_SLOT_INFO_KHR,
                                                         .pStdReferenceInfo = &p->av1_reconstructed};
        p->av1_dpb[1] = (VkVideoEncodeAV1DpbSlotInfoKHR){.sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_AV1_DPB_SLOT_INFO_KHR,
                                                         .pStdReferenceInfo = &e->av1_previous};
        slots[0].pNext = &p->av1_dpb[0];
        slots[1].pNext = &p->av1_dpb[1];
        p->av1_remaining = (VkVideoEncodeAV1GopRemainingFrameInfoKHR){
            .sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_AV1_GOP_REMAINING_FRAME_INFO_KHR,
            .useGopRemainingFrames = e->av1_caps.requiresGopRemainingFrames,
            .gopRemainingIntra = idr ? 1 : 0,
            .gopRemainingPredictive = e->gop - e->frame - (idr ? 1 : 0)};
        p->av1_rate = e->av1_rc;
        p->av1_rate.pNext = &p->av1_remaining;
        p->av1_quant.base_q_idx = 128;
        for (unsigned i = 0; i < STD_VIDEO_AV1_NUM_REF_FRAMES; i++) {
            p->av1_motion.gm_params[i][2] = 1 << 16;
            p->av1_motion.gm_params[i][5] = 1 << 16;
        }
        p->av1_picture =
            (StdVideoEncodeAV1PictureInfo){.flags = {.error_resilient_mode = 1, .show_frame = 1},
                                           .frame_type = type,
                                           .primary_ref_frame = STD_VIDEO_AV1_PRIMARY_REF_NONE,
                                           .refresh_frame_flags = idr ? 255 : 1,
                                           .render_width_minus_1 = e->width - 1,
                                           .render_height_minus_1 = e->height - 1,
                                           .interpolation_filter = STD_VIDEO_AV1_INTERPOLATION_FILTER_EIGHTTAP,
                                           .TxMode = STD_VIDEO_AV1_TX_MODE_SELECT,
                                           .pTileInfo = &e->av1_tiles,
                                           .pQuantization = &p->av1_quant,
                                           .pLoopFilter = &p->av1_filter,
                                           .pSegmentation = &p->av1_segmentation,
                                           .pCDEF = &p->av1_cdef,
                                           .pLoopRestoration = &p->av1_restoration,
                                           .pGlobalMotion = &p->av1_motion};
        p->av1 = (VkVideoEncodeAV1PictureInfoKHR){
            .sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_AV1_PICTURE_INFO_KHR,
            .predictionMode = idr ? VK_VIDEO_ENCODE_AV1_PREDICTION_MODE_INTRA_ONLY_KHR
                                  : VK_VIDEO_ENCODE_AV1_PREDICTION_MODE_SINGLE_REFERENCE_KHR,
            .rateControlGroup = idr ? VK_VIDEO_ENCODE_AV1_RATE_CONTROL_GROUP_INTRA_KHR
                                    : VK_VIDEO_ENCODE_AV1_RATE_CONTROL_GROUP_PREDICTIVE_KHR,
            .pStdPictureInfo = &p->av1_picture};
        for (unsigned i = 0; i < 7; i++)
            p->av1.referenceNameSlotIndices[i] =
                !idr && (e->av1_caps.singleReferenceNameMask & (1u << i)) ? (int32_t)previous : -1;
        p->remaining = &p->av1_remaining;
        p->rate = &p->av1_rate;
        p->picture = &p->av1;
    }
}
