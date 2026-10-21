// Native Vulkan Video H.264/HEVC/AV1: one reference, no reordered pictures, bounded submission.
// Included by native_linux.c so conversion and encoding share the same device.
typedef struct {
    VkImage image;
    VkDeviceMemory memory;
    VkImageView view;
} video_image;
struct bk_vk_encoder {
    bk_vk *v;
    VkVideoSessionKHR session;
    VkVideoSessionParametersKHR parameters;
    VkDeviceMemory session_memory[32];
    uint32_t memory_count;
    video_image input, dpb[2];
    video_image nv12_images[64];
    bk_vk_source nv12_sources[64];
    uint64_t nv12_inodes[64], nv12_devices[64];
    unsigned nv12_count;
    VkCommandPool commands;
    VkCommandBuffer command;
    VkQueryPool feedback;
    VkSemaphore ready;
    buffer bitstream;
    VkVideoEncodeH264ProfileInfoKHR h264_profile;
    VkVideoEncodeH265ProfileInfoKHR h265_profile;
    VkVideoEncodeAV1ProfileInfoKHR av1_profile;
    VkVideoEncodeUsageInfoKHR usage;
    VkVideoProfileInfoKHR profile;
    VkVideoProfileListInfoKHR profiles;
    VkVideoCapabilitiesKHR caps;
    VkVideoEncodeCapabilitiesKHR encode_caps;
    VkVideoEncodeH264CapabilitiesKHR h264_caps;
    VkVideoEncodeH265CapabilitiesKHR h265_caps;
    VkVideoEncodeAV1CapabilitiesKHR av1_caps;
    VkVideoEncodeRateControlLayerInfoKHR layer;
    VkVideoEncodeH264RateControlInfoKHR h264_rc;
    VkVideoEncodeH265RateControlInfoKHR h265_rc;
    VkVideoEncodeAV1RateControlInfoKHR av1_rc;
    VkVideoEncodeRateControlInfoKHR rc;
    StdVideoEncodeH264ReferenceInfo previous;
    StdVideoEncodeH265ReferenceInfo h265_previous;
    StdVideoEncodeAV1ReferenceInfo av1_previous;
    StdVideoH265LevelIdc h265_level;
    StdVideoAV1Level av1_level;
    uint32_t ctb_log2, transform_log2, av1_sb_log2;
    StdVideoAV1TileInfo av1_tiles;
    uint16_t av1_col_starts[65], av1_row_starts[65], av1_tile_widths[64], av1_tile_heights[64];
    int codec;
    uint32_t width, height, aligned_width, aligned_height, gop, frame, slot;
    int initialized, input_initialized;
    uint8_t *headers;
    size_t headers_size;
};
static void video_image_free(bk_vk *v, video_image *i) {
    if (i->view)
        v->DestroyImageView(v->device, i->view, NULL);
    if (i->image)
        v->DestroyImage(v->device, i->image, NULL);
    if (i->memory)
        v->FreeMemory(v->device, i->memory, NULL);
}
static int video_image_new(bk_vk_encoder *e, video_image *i, VkImageUsageFlags usage, char *error, size_t capacity) {
    bk_vk *v = e->v;
    uint32_t families[] = {v->family, v->video_family};
    VkImageCreateInfo info = {.sType = VK_STRUCTURE_TYPE_IMAGE_CREATE_INFO,
                              .pNext = &e->profiles,
                              .imageType = VK_IMAGE_TYPE_2D,
                              .format = VK_FORMAT_G8_B8R8_2PLANE_420_UNORM,
                              .extent = {e->aligned_width, e->aligned_height, 1},
                              .mipLevels = 1,
                              .arrayLayers = (usage & VK_IMAGE_USAGE_VIDEO_ENCODE_DPB_BIT_KHR) ? 2 : 1,
                              .samples = VK_SAMPLE_COUNT_1_BIT,
                              .tiling = VK_IMAGE_TILING_OPTIMAL,
                              .usage = usage,
                              .sharingMode =
                                  v->family == v->video_family ? VK_SHARING_MODE_EXCLUSIVE : VK_SHARING_MODE_CONCURRENT,
                              .queueFamilyIndexCount = v->family == v->video_family ? 0 : 2,
                              .pQueueFamilyIndices = families};
    TRY(v->CreateImage(v->device, &info, NULL, &i->image));
    VkMemoryRequirements req;
    v->GetImageMemoryRequirements(v->device, i->image, &req);
    uint32_t type = memory_type(v, req.memoryTypeBits, VK_MEMORY_PROPERTY_DEVICE_LOCAL_BIT);
    if (type == UINT32_MAX) {
        snprintf(error, capacity, "no device-local Vulkan Video image memory");
        goto failed;
    }
    VkMemoryAllocateInfo alloc = {
        .sType = VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO, .allocationSize = req.size, .memoryTypeIndex = type};
    TRY(v->AllocateMemory(v->device, &alloc, NULL, &i->memory));
    TRY(v->BindImageMemory(v->device, i->image, i->memory, 0));
    VkImageViewCreateInfo view = {.sType = VK_STRUCTURE_TYPE_IMAGE_VIEW_CREATE_INFO,
                                  .image = i->image,
                                  .viewType =
                                      info.arrayLayers == 2 ? VK_IMAGE_VIEW_TYPE_2D_ARRAY : VK_IMAGE_VIEW_TYPE_2D,
                                  .format = info.format,
                                  .subresourceRange = {VK_IMAGE_ASPECT_COLOR_BIT, 0, 1, 0, info.arrayLayers}};
    TRY(v->CreateImageView(v->device, &view, NULL, &i->view));
    return 0;
failed:
    return -1;
}
void bk_vk_encoder_free(bk_vk_encoder *e) {
    if (!e)
        return;
    bk_vk *v = e->v;
    v->DeviceWaitIdle(v->device);
    if (e->ready)
        v->DestroySemaphore(v->device, e->ready, NULL);
    if (e->commands)
        v->DestroyCommandPool(v->device, e->commands, NULL);
    if (e->feedback)
        v->DestroyQueryPool(v->device, e->feedback, NULL);
    if (e->parameters)
        v->DestroyVideoSessionParametersKHR(v->device, e->parameters, NULL);
    if (e->session)
        v->DestroyVideoSessionKHR(v->device, e->session, NULL);
    for (uint32_t i = 0; i < e->memory_count; i++)
        if (e->session_memory[i])
            v->FreeMemory(v->device, e->session_memory[i], NULL);
    for(unsigned i=0;i<e->nv12_count;i++)video_image_free(v,&e->nv12_images[i]);
    video_image_free(v, &e->input);
    for (int i = 0; i < 2; i++)
        video_image_free(v, &e->dpb[i]);
    buffer_free(v, &e->bitstream);
    free(e->headers);
    free(e);
}
static StdVideoH264LevelIdc video_level(uint32_t w, uint32_t h, uint32_t num, uint32_t den, uint32_t bitrate) {
    struct {
        StdVideoH264LevelIdc id;
        uint32_t fs, mbps, kbps;
    } levels[] = {{STD_VIDEO_H264_LEVEL_IDC_3_1, 3600, 108000, 14000},
                  {STD_VIDEO_H264_LEVEL_IDC_3_2, 5120, 216000, 20000},
                  {STD_VIDEO_H264_LEVEL_IDC_4_0, 8192, 245760, 20000},
                  {STD_VIDEO_H264_LEVEL_IDC_4_1, 8192, 245760, 50000},
                  {STD_VIDEO_H264_LEVEL_IDC_4_2, 8704, 522240, 50000},
                  {STD_VIDEO_H264_LEVEL_IDC_5_0, 22080, 589824, 135000},
                  {STD_VIDEO_H264_LEVEL_IDC_5_1, 36864, 983040, 240000},
                  {STD_VIDEO_H264_LEVEL_IDC_5_2, 36864, 2073600, 240000},
                  {STD_VIDEO_H264_LEVEL_IDC_6_0, 139264, 4177920, 240000},
                  {STD_VIDEO_H264_LEVEL_IDC_6_1, 139264, 8355840, 480000},
                  {STD_VIDEO_H264_LEVEL_IDC_6_2, 139264, 16711680, 800000}};
    uint64_t mb = (uint64_t)((w + 15) / 16) * ((h + 15) / 16);
    for (unsigned i = 0; i < sizeof(levels) / sizeof(*levels); i++)
        if (mb <= levels[i].fs && mb * num <= (uint64_t)levels[i].mbps * den && bitrate <= levels[i].kbps)
            return levels[i].id;
    return STD_VIDEO_H264_LEVEL_IDC_INVALID;
}
#include "video_codecs_linux.inc"
#include "video_capability_linux.inc"
bk_vk_encoder *bk_vk_encoder_new(bk_vk *v, int width, int height, int num, int den, int bitrate, char *error,
                                 size_t capacity) {
    if (!v->video_queue) {
        snprintf(error, capacity, "Vulkan device was not opened with a video encode queue");
        return NULL;
    }
    bk_vk_encoder *e = calloc(1, sizeof(*e));
    if (!e)
        return NULL;
    e->v = v;
    e->codec = v->video_codec;
    e->width = width;
    e->height = height;
    e->gop = ((uint64_t)num * 2 + den - 1) / den;
    e->h264_profile = (VkVideoEncodeH264ProfileInfoKHR){.sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_H264_PROFILE_INFO_KHR,
                                                        .stdProfileIdc = STD_VIDEO_H264_PROFILE_IDC_BASELINE};
    e->h265_profile = (VkVideoEncodeH265ProfileInfoKHR){.sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_H265_PROFILE_INFO_KHR,
                                                        .stdProfileIdc = STD_VIDEO_H265_PROFILE_IDC_MAIN};
    e->av1_profile = (VkVideoEncodeAV1ProfileInfoKHR){.sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_AV1_PROFILE_INFO_KHR,
                                                      .stdProfile = STD_VIDEO_AV1_PROFILE_MAIN};
    e->usage = (VkVideoEncodeUsageInfoKHR){.sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_USAGE_INFO_KHR,
                                           .pNext = e->codec == 1   ? (void *)&e->h265_profile
                                                    : e->codec == 2 ? (void *)&e->av1_profile
                                                                    : (void *)&e->h264_profile,
                                           .videoUsageHints = VK_VIDEO_ENCODE_USAGE_STREAMING_BIT_KHR,
                                           .videoContentHints = VK_VIDEO_ENCODE_CONTENT_DESKTOP_BIT_KHR,
                                           .tuningMode = VK_VIDEO_ENCODE_TUNING_MODE_ULTRA_LOW_LATENCY_KHR};
    e->profile =
        (VkVideoProfileInfoKHR){.sType = VK_STRUCTURE_TYPE_VIDEO_PROFILE_INFO_KHR,
                                .pNext = &e->usage,
                                .videoCodecOperation = e->codec == 1   ? VK_VIDEO_CODEC_OPERATION_ENCODE_H265_BIT_KHR
                                                       : e->codec == 2 ? VK_VIDEO_CODEC_OPERATION_ENCODE_AV1_BIT_KHR
                                                                       : VK_VIDEO_CODEC_OPERATION_ENCODE_H264_BIT_KHR,
                                .chromaSubsampling = VK_VIDEO_CHROMA_SUBSAMPLING_420_BIT_KHR,
                                .lumaBitDepth = VK_VIDEO_COMPONENT_BIT_DEPTH_8_BIT_KHR,
                                .chromaBitDepth = VK_VIDEO_COMPONENT_BIT_DEPTH_8_BIT_KHR};
    e->profiles = (VkVideoProfileListInfoKHR){
        .sType = VK_STRUCTURE_TYPE_VIDEO_PROFILE_LIST_INFO_KHR, .profileCount = 1, .pProfiles = &e->profile};
    e->h264_caps.sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_H264_CAPABILITIES_KHR;
    e->h265_caps.sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_H265_CAPABILITIES_KHR;
    e->av1_caps.sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_AV1_CAPABILITIES_KHR;
    e->encode_caps.sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_CAPABILITIES_KHR;
    e->encode_caps.pNext = e->codec == 1   ? (void *)&e->h265_caps
                           : e->codec == 2 ? (void *)&e->av1_caps
                                           : (void *)&e->h264_caps;
    e->caps.sType = VK_STRUCTURE_TYPE_VIDEO_CAPABILITIES_KHR;
    e->caps.pNext = &e->encode_caps;
    VkResult status = v->GetPhysicalDeviceVideoCapabilitiesKHR(v->physical, &e->profile, &e->caps);
    if (status != VK_SUCCESS) {
        e->usage.tuningMode = VK_VIDEO_ENCODE_TUNING_MODE_LOW_LATENCY_KHR;
        TRY(v->GetPhysicalDeviceVideoCapabilitiesKHR(v->physical, &e->profile, &e->caps));
    }
    if (width < (int)e->caps.minCodedExtent.width || height < (int)e->caps.minCodedExtent.height ||
        width > (int)e->caps.maxCodedExtent.width || height > (int)e->caps.maxCodedExtent.height ||
        e->caps.maxDpbSlots < 2 || e->caps.maxActiveReferencePictures < 1 ||
        (e->codec == 0 && !e->h264_caps.maxPPictureL0ReferenceCount) ||
        (e->codec == 1 && !e->h265_caps.maxPPictureL0ReferenceCount) ||
        (e->codec == 2 && (!e->av1_caps.maxSingleReferenceCount || !(e->av1_caps.singleReferenceNameMask & 127))) ||
        !(e->encode_caps.rateControlModes & VK_VIDEO_ENCODE_RATE_CONTROL_MODE_CBR_BIT_KHR) ||
        (uint64_t)bitrate * 1000 > e->encode_caps.maxBitrate ||
        (e->encode_caps.supportedEncodeFeedbackFlags & (VK_VIDEO_ENCODE_FEEDBACK_BITSTREAM_BUFFER_OFFSET_BIT_KHR |
                                                        VK_VIDEO_ENCODE_FEEDBACK_BITSTREAM_BYTES_WRITTEN_BIT_KHR)) !=
            (VK_VIDEO_ENCODE_FEEDBACK_BITSTREAM_BUFFER_OFFSET_BIT_KHR |
             VK_VIDEO_ENCODE_FEEDBACK_BITSTREAM_BYTES_WRITTEN_BIT_KHR)) {
        snprintf(error, capacity,
                 "Vulkan Video lacks required extent, one-reference CBR or bitstream-feedback capabilities");
        goto failed;
    }
    StdVideoH264LevelIdc level = STD_VIDEO_H264_LEVEL_IDC_INVALID;
    if (e->codec == 0) {
        uint32_t gw = e->caps.pictureAccessGranularity.width, gh = e->caps.pictureAccessGranularity.height;
        if (gw < 16)
            gw = 16;
        if (gh < 16)
            gh = 16;
        e->aligned_width = (width + gw - 1) / gw * gw;
        e->aligned_height = (height + gh - 1) / gh * gh;
        level = video_level(width, height, num, den, bitrate);
        if (level == STD_VIDEO_H264_LEVEL_IDC_INVALID || level > e->h264_caps.maxLevelIdc) {
            snprintf(error, capacity, "Vulkan H.264 level cannot cover requested size/rate/bitrate");
            goto failed;
        }
    } else if (video_extra_configure(e, num, den, bitrate, error, capacity) < 0)
        goto failed;
    // Check actual input and DPB format support instead of assuming NV12 exists.
    for (int pass = 0; pass < 2; pass++) {
        VkPhysicalDeviceVideoFormatInfoKHR fi = {.sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_VIDEO_FORMAT_INFO_KHR,
                                                 .pNext = &e->profiles,
                                                 .imageUsage = pass ? VK_IMAGE_USAGE_VIDEO_ENCODE_DPB_BIT_KHR
                                                                    : VK_IMAGE_USAGE_VIDEO_ENCODE_SRC_BIT_KHR |
                                                                          VK_IMAGE_USAGE_TRANSFER_DST_BIT};
        uint32_t n = 0;
        TRY(v->GetPhysicalDeviceVideoFormatPropertiesKHR(v->physical, &fi, &n, NULL));
        if (!n || n > 64) {
            snprintf(error, capacity, "Vulkan Video returned no usable image formats");
            goto failed;
        }
        VkVideoFormatPropertiesKHR formats[64] = {0};
        for (uint32_t j = 0; j < n; j++)
            formats[j].sType = VK_STRUCTURE_TYPE_VIDEO_FORMAT_PROPERTIES_KHR;
        TRY(v->GetPhysicalDeviceVideoFormatPropertiesKHR(v->physical, &fi, &n, formats));
        int found = 0;
        for (uint32_t j = 0; j < n; j++)
            if (formats[j].format == VK_FORMAT_G8_B8R8_2PLANE_420_UNORM &&
                formats[j].imageTiling == VK_IMAGE_TILING_OPTIMAL)
                found = 1;
        if (!found) {
            snprintf(error, capacity, "Vulkan Video does not support NV12 input and reference pictures");
            goto failed;
        }
    }
    VkVideoEncodeH264SessionCreateInfoKHR hs = {.sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_H264_SESSION_CREATE_INFO_KHR,
                                                .useMaxLevelIdc = VK_TRUE,
                                                .maxLevelIdc = level};
    VkVideoEncodeH265SessionCreateInfoKHR h265_session = {
        .sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_H265_SESSION_CREATE_INFO_KHR,
        .useMaxLevelIdc = VK_TRUE,
        .maxLevelIdc = e->h265_level};
    VkVideoEncodeAV1SessionCreateInfoKHR av1_session = {.sType =
                                                            VK_STRUCTURE_TYPE_VIDEO_ENCODE_AV1_SESSION_CREATE_INFO_KHR,
                                                        .useMaxLevel = VK_TRUE,
                                                        .maxLevel = e->av1_level};
    VkVideoSessionCreateInfoKHR session = {.sType = VK_STRUCTURE_TYPE_VIDEO_SESSION_CREATE_INFO_KHR,
                                           .pNext = e->codec == 1   ? (void *)&h265_session
                                                    : e->codec == 2 ? (void *)&av1_session
                                                                    : (void *)&hs,
                                           .queueFamilyIndex = v->video_family,
                                           .pVideoProfile = &e->profile,
                                           .pictureFormat = VK_FORMAT_G8_B8R8_2PLANE_420_UNORM,
                                           .referencePictureFormat = VK_FORMAT_G8_B8R8_2PLANE_420_UNORM,
                                           .maxCodedExtent = {width, height},
                                           .maxDpbSlots = 2,
                                           .maxActiveReferencePictures = 1,
                                           .pStdHeaderVersion = &e->caps.stdHeaderVersion};
    TRY(v->CreateVideoSessionKHR(v->device, &session, NULL, &e->session));
    uint32_t count = 0;
    TRY(v->GetVideoSessionMemoryRequirementsKHR(v->device, e->session, &count, NULL));
    if (count > 32)
        goto failed;
    VkVideoSessionMemoryRequirementsKHR req[32] = {0};
    for (uint32_t i = 0; i < count; i++)
        req[i].sType = VK_STRUCTURE_TYPE_VIDEO_SESSION_MEMORY_REQUIREMENTS_KHR;
    TRY(v->GetVideoSessionMemoryRequirementsKHR(v->device, e->session, &count, req));
    e->memory_count = count;
    VkBindVideoSessionMemoryInfoKHR bindings[32] = {0};
    for (uint32_t i = 0; i < count; i++) {
        uint32_t type = memory_type(v, req[i].memoryRequirements.memoryTypeBits, VK_MEMORY_PROPERTY_DEVICE_LOCAL_BIT);
        if (type == UINT32_MAX)
            type = memory_type(v, req[i].memoryRequirements.memoryTypeBits, 0);
        if (type == UINT32_MAX)
            goto failed;
        VkMemoryAllocateInfo alloc = {.sType = VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO,
                                      .allocationSize = req[i].memoryRequirements.size,
                                      .memoryTypeIndex = type};
        TRY(v->AllocateMemory(v->device, &alloc, NULL, &e->session_memory[i]));
        bindings[i] = (VkBindVideoSessionMemoryInfoKHR){.sType = VK_STRUCTURE_TYPE_BIND_VIDEO_SESSION_MEMORY_INFO_KHR,
                                                        .memoryBindIndex = req[i].memoryBindIndex,
                                                        .memory = e->session_memory[i],
                                                        .memorySize = alloc.allocationSize};
    }
    TRY(v->BindVideoSessionMemoryKHR(v->device, e->session, count, bindings));
    if (e->codec == 0) {
        StdVideoH264SequenceParameterSetVui vui = {.video_format = 5,
                                                   .colour_primaries = 1,
                                                   .transfer_characteristics = 1,
                                                   .matrix_coefficients = 1,
                                                   .num_units_in_tick = den,
                                                   .time_scale = num * 2,
                                                   .max_dec_frame_buffering = 1};
        vui.flags.video_signal_type_present_flag = 1;
        vui.flags.color_description_present_flag = 1;
        vui.flags.timing_info_present_flag = 1;
        vui.flags.fixed_frame_rate_flag = 1;
        vui.flags.bitstream_restriction_flag = 1;
        StdVideoH264SequenceParameterSet sps = {.profile_idc = STD_VIDEO_H264_PROFILE_IDC_BASELINE,
                                                .level_idc = level,
                                                .chroma_format_idc = STD_VIDEO_H264_CHROMA_FORMAT_IDC_420,
                                                .log2_max_frame_num_minus4 = 8,
                                                .pic_order_cnt_type = STD_VIDEO_H264_POC_TYPE_0,
                                                .log2_max_pic_order_cnt_lsb_minus4 = 8,
                                                .max_num_ref_frames = 1,
                                                .pic_width_in_mbs_minus1 = (width + 15) / 16 - 1,
                                                .pic_height_in_map_units_minus1 = (height + 15) / 16 - 1,
                                                .frame_crop_right_offset = ((16 - width % 16) % 16) / 2,
                                                .frame_crop_bottom_offset = ((16 - height % 16) % 16) / 2,
                                                .pSequenceParameterSetVui = &vui};
        sps.flags.constraint_set0_flag = 1;
        sps.flags.constraint_set1_flag = 1;
        sps.flags.frame_mbs_only_flag = 1;
        sps.flags.direct_8x8_inference_flag = 1;
        sps.flags.frame_cropping_flag = width % 16 || height % 16;
        sps.flags.vui_parameters_present_flag = 1;
        StdVideoH264PictureParameterSet pps = {0};
        pps.flags.deblocking_filter_control_present_flag = 1;
        VkVideoEncodeH264SessionParametersAddInfoKHR add = {
            .sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_H264_SESSION_PARAMETERS_ADD_INFO_KHR,
            .stdSPSCount = 1,
            .pStdSPSs = &sps,
            .stdPPSCount = 1,
            .pStdPPSs = &pps};
        VkVideoEncodeH264SessionParametersCreateInfoKHR hp = {
            .sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_H264_SESSION_PARAMETERS_CREATE_INFO_KHR,
            .maxStdSPSCount = 1,
            .maxStdPPSCount = 1,
            .pParametersAddInfo = &add};
        VkVideoSessionParametersCreateInfoKHR pc = {.sType = VK_STRUCTURE_TYPE_VIDEO_SESSION_PARAMETERS_CREATE_INFO_KHR,
                                                    .pNext = &hp,
                                                    .videoSession = e->session};
        TRY(v->CreateVideoSessionParametersKHR(v->device, &pc, NULL, &e->parameters));
        VkVideoEncodeH264SessionParametersGetInfoKHR hg = {
            .sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_H264_SESSION_PARAMETERS_GET_INFO_KHR,
            .writeStdSPS = 1,
            .writeStdPPS = 1};
        VkVideoEncodeSessionParametersGetInfoKHR get = {
            .sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_SESSION_PARAMETERS_GET_INFO_KHR,
            .pNext = &hg,
            .videoSessionParameters = e->parameters};
        TRY(v->GetEncodedVideoSessionParametersKHR(v->device, &get, NULL, &e->headers_size, NULL));
        if (!e->headers_size || e->headers_size > 65536)
            goto failed;
        e->headers = malloc(e->headers_size);
        if (!e->headers)
            goto failed;
        TRY(v->GetEncodedVideoSessionParametersKHR(v->device, &get, NULL, &e->headers_size, e->headers));
    } else if (video_extra_parameters(e, num, den, error, capacity) < 0)
        goto failed;
    if (video_image_new(e, &e->input, VK_IMAGE_USAGE_VIDEO_ENCODE_SRC_BIT_KHR | VK_IMAGE_USAGE_TRANSFER_DST_BIT, error,
                        capacity) < 0)
        goto failed;
    if (video_image_new(e, &e->dpb[0], VK_IMAGE_USAGE_VIDEO_ENCODE_DPB_BIT_KHR, error, capacity) < 0)
        goto failed;
    // Profile-compatible encoded output buffer, persistently host mapped.
    VkBufferCreateInfo bc = {.sType = VK_STRUCTURE_TYPE_BUFFER_CREATE_INFO,
                             .pNext = &e->profiles,
                             .size = (VkDeviceSize)width * height * 3 + 65536,
                             .usage = VK_BUFFER_USAGE_VIDEO_ENCODE_DST_BIT_KHR};
    uint64_t alignment = e->caps.minBitstreamBufferSizeAlignment;
    bc.size = (bc.size + alignment - 1) / alignment * alignment;
    TRY(v->CreateBuffer(v->device, &bc, NULL, &e->bitstream.buffer));
    VkMemoryRequirements memory;
    v->GetBufferMemoryRequirements(v->device, e->bitstream.buffer, &memory);
    uint32_t type = memory_type(v, memory.memoryTypeBits,
                                VK_MEMORY_PROPERTY_HOST_VISIBLE_BIT | VK_MEMORY_PROPERTY_HOST_COHERENT_BIT);
    if (type == UINT32_MAX)
        type = memory_type(v, memory.memoryTypeBits, VK_MEMORY_PROPERTY_HOST_VISIBLE_BIT);
    if (type == UINT32_MAX)
        goto failed;
    VkMemoryAllocateInfo ba = {
        .sType = VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO, .allocationSize = memory.size, .memoryTypeIndex = type};
    TRY(v->AllocateMemory(v->device, &ba, NULL, &e->bitstream.memory));
    TRY(v->BindBufferMemory(v->device, e->bitstream.buffer, e->bitstream.memory, 0));
    TRY(v->MapMemory(v->device, e->bitstream.memory, 0, VK_WHOLE_SIZE, 0, &e->bitstream.mapped));
    e->bitstream.size = bc.size;
    e->bitstream.allocation = memory.size;
    e->bitstream.coherent = (v->memory.memoryTypes[type].propertyFlags & VK_MEMORY_PROPERTY_HOST_COHERENT_BIT) != 0;
    VkQueryPoolVideoEncodeFeedbackCreateInfoKHR feedback = {
        .sType = VK_STRUCTURE_TYPE_QUERY_POOL_VIDEO_ENCODE_FEEDBACK_CREATE_INFO_KHR,
        .pNext = &e->profile,
        .encodeFeedbackFlags = VK_VIDEO_ENCODE_FEEDBACK_BITSTREAM_BUFFER_OFFSET_BIT_KHR |
                               VK_VIDEO_ENCODE_FEEDBACK_BITSTREAM_BYTES_WRITTEN_BIT_KHR};
    VkQueryPoolCreateInfo qp = {.sType = VK_STRUCTURE_TYPE_QUERY_POOL_CREATE_INFO,
                                .pNext = &feedback,
                                .queryType = VK_QUERY_TYPE_VIDEO_ENCODE_FEEDBACK_KHR,
                                .queryCount = 1};
    TRY(v->CreateQueryPool(v->device, &qp, NULL, &e->feedback));
    VkCommandPoolCreateInfo pool = {.sType = VK_STRUCTURE_TYPE_COMMAND_POOL_CREATE_INFO,
                                    .queueFamilyIndex = v->video_family};
    TRY(v->CreateCommandPool(v->device, &pool, NULL, &e->commands));
    VkCommandBufferAllocateInfo cmd = {.sType = VK_STRUCTURE_TYPE_COMMAND_BUFFER_ALLOCATE_INFO,
                                       .commandPool = e->commands,
                                       .level = VK_COMMAND_BUFFER_LEVEL_PRIMARY,
                                       .commandBufferCount = 1};
    TRY(v->AllocateCommandBuffers(v->device, &cmd, &e->command));
    VkSemaphoreCreateInfo sem = {.sType = VK_STRUCTURE_TYPE_SEMAPHORE_CREATE_INFO};
    TRY(v->CreateSemaphore(v->device, &sem, NULL, &e->ready));
    e->layer =
        (VkVideoEncodeRateControlLayerInfoKHR){.sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_RATE_CONTROL_LAYER_INFO_KHR,
                                               .averageBitrate = (uint64_t)bitrate * 1000,
                                               .maxBitrate = (uint64_t)bitrate * 1000,
                                               .frameRateNumerator = num,
                                               .frameRateDenominator = den};
    e->h264_rc =
        (VkVideoEncodeH264RateControlInfoKHR){.sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_H264_RATE_CONTROL_INFO_KHR,
                                              .flags = VK_VIDEO_ENCODE_H264_RATE_CONTROL_REGULAR_GOP_BIT_KHR |
                                                       VK_VIDEO_ENCODE_H264_RATE_CONTROL_REFERENCE_PATTERN_FLAT_BIT_KHR,
                                              .gopFrameCount = e->gop,
                                              .idrPeriod = e->gop,
                                              .temporalLayerCount = 1};
    e->h265_rc =
        (VkVideoEncodeH265RateControlInfoKHR){.sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_H265_RATE_CONTROL_INFO_KHR,
                                              .flags = VK_VIDEO_ENCODE_H265_RATE_CONTROL_REGULAR_GOP_BIT_KHR |
                                                       VK_VIDEO_ENCODE_H265_RATE_CONTROL_REFERENCE_PATTERN_FLAT_BIT_KHR,
                                              .gopFrameCount = e->gop,
                                              .idrPeriod = e->gop,
                                              .subLayerCount = 1};
    e->av1_rc =
        (VkVideoEncodeAV1RateControlInfoKHR){.sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_AV1_RATE_CONTROL_INFO_KHR,
                                             .flags = VK_VIDEO_ENCODE_AV1_RATE_CONTROL_REGULAR_GOP_BIT_KHR |
                                                      VK_VIDEO_ENCODE_AV1_RATE_CONTROL_REFERENCE_PATTERN_FLAT_BIT_KHR,
                                             .gopFrameCount = e->gop,
                                             .keyFramePeriod = e->gop,
                                             .temporalLayerCount = 1};
    e->rc = (VkVideoEncodeRateControlInfoKHR){.sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_RATE_CONTROL_INFO_KHR,
                                              .pNext = e->codec == 1   ? (void *)&e->h265_rc
                                                       : e->codec == 2 ? (void *)&e->av1_rc
                                                                       : (void *)&e->h264_rc,
                                              .rateControlMode = VK_VIDEO_ENCODE_RATE_CONTROL_MODE_CBR_BIT_KHR,
                                              .layerCount = 1,
                                              .pLayers = &e->layer,
                                              .virtualBufferSizeInMs = (1000ull * den + num - 1) / num,
                                              .initialVirtualBufferSizeInMs = 0};
    return e;
failed:
    bk_vk_encoder_free(e);
    return NULL;
}
#include "video_nv12_linux.inc"
int bk_vk_encoder_encode(bk_vk_encoder *e, const bk_vk_source *source_pixels, const uint8_t *pixels, size_t pixel_size,
                         int force, uint8_t **output, size_t *output_size, char *error, size_t capacity) {
    bk_vk *v = e->v;
    bk_vk_image *converted = NULL;
    buffer upload = {0};
    VkFence fence = VK_NULL_HANDLE;
    int result = -1;
    int repeat = !source_pixels && !pixels && pixel_size == 0 && e->input_initialized;
    int direct = source_pixels && source_pixels->fourcc == 0x3231564e;
    video_image *input_picture = &e->input;
    if (direct && !(input_picture = video_nv12_cached(e, source_pixels, error, capacity)))
        return 1;
    *output = NULL;
    *output_size = 0;
    if (force || e->frame >= e->gop)
        e->frame = 0;
    int idr = e->frame == 0;
    uint32_t current = e->slot, previous = current ^ 1;
    VkBuffer input;
    uint32_t pitch;
    if (!direct && !repeat) {
    if (source_pixels) {
        converted =
            convert(v, source_pixels, e->aligned_width, e->aligned_height, e->width, e->height, 1, error, capacity);
        if (!converted)
            goto failed;
        input = converted->output.buffer;
        pitch = converted->pitch;
    } else {
        if (!pixels || pixel_size != (size_t)e->width * e->height * 3 / 2) {
            snprintf(error, capacity, "invalid native Vulkan NV12 pixels");
            goto failed;
        }
        pitch = (e->aligned_width + 63) & ~63u;
        if (buffer_new(v, &upload, (VkDeviceSize)pitch * e->aligned_height * 3 / 2, VK_BUFFER_USAGE_TRANSFER_SRC_BIT, 1,
                       error, capacity) < 0)
            goto failed;
        memset(upload.mapped, 16, (size_t)pitch * e->aligned_height);
        memset((uint8_t *)upload.mapped + (size_t)pitch * e->aligned_height, 128,
               (size_t)pitch * e->aligned_height / 2);
        for (uint32_t y = 0; y < e->height; y++)
            memcpy((uint8_t *)upload.mapped + (size_t)y * pitch, pixels + (size_t)y * e->width, e->width);
        for (uint32_t y = 0; y < e->height / 2; y++)
            memcpy((uint8_t *)upload.mapped + (size_t)(e->aligned_height + y) * pitch,
                   pixels + (size_t)e->width * e->height + (size_t)y * e->width, e->width);
        if (!upload.coherent) {
            VkMappedMemoryRange r = {
                .sType = VK_STRUCTURE_TYPE_MAPPED_MEMORY_RANGE, .memory = upload.memory, .size = VK_WHOLE_SIZE};
            TRY(v->FlushMappedMemoryRanges(v->device, 1, &r));
        }
        input = upload.buffer;
    }
    if (begin(v, error, capacity) < 0)
        goto failed;
    VkImageMemoryBarrier into = {.sType = VK_STRUCTURE_TYPE_IMAGE_MEMORY_BARRIER,
                                 .srcAccessMask = 0,
                                 .dstAccessMask = VK_ACCESS_TRANSFER_WRITE_BIT,
                                 .oldLayout =
                                     e->input_initialized ? VK_IMAGE_LAYOUT_GENERAL : VK_IMAGE_LAYOUT_UNDEFINED,
                                 .newLayout = VK_IMAGE_LAYOUT_TRANSFER_DST_OPTIMAL,
                                 .srcQueueFamilyIndex = VK_QUEUE_FAMILY_IGNORED,
                                 .dstQueueFamilyIndex = VK_QUEUE_FAMILY_IGNORED,
                                 .image = e->input.image,
                                 .subresourceRange = {VK_IMAGE_ASPECT_COLOR_BIT, 0, 1, 0, 1}};
    v->CmdPipelineBarrier(v->command, VK_PIPELINE_STAGE_TOP_OF_PIPE_BIT, VK_PIPELINE_STAGE_TRANSFER_BIT, 0, 0, NULL, 0,
                          NULL, 1, &into);
    VkBufferImageCopy planes[2] = {{.bufferRowLength = pitch,
                                    .imageSubresource = {VK_IMAGE_ASPECT_PLANE_0_BIT, 0, 0, 1},
                                    .imageExtent = {e->aligned_width, e->aligned_height, 1}},
                                   {.bufferOffset = (VkDeviceSize)pitch * e->aligned_height,
                                    .bufferRowLength = pitch / 2,
                                    .imageSubresource = {VK_IMAGE_ASPECT_PLANE_1_BIT, 0, 0, 1},
                                    .imageExtent = {e->aligned_width / 2, e->aligned_height / 2, 1}}};
    v->CmdCopyBufferToImage(v->command, input, e->input.image, VK_IMAGE_LAYOUT_TRANSFER_DST_OPTIMAL, 2, planes);
    into.srcAccessMask = VK_ACCESS_TRANSFER_WRITE_BIT;
    into.dstAccessMask = 0;
    into.oldLayout = VK_IMAGE_LAYOUT_TRANSFER_DST_OPTIMAL;
    into.newLayout = VK_IMAGE_LAYOUT_GENERAL;
    v->CmdPipelineBarrier(v->command, VK_PIPELINE_STAGE_TRANSFER_BIT, VK_PIPELINE_STAGE_BOTTOM_OF_PIPE_BIT, 0, 0, NULL,
                          0, NULL, 1, &into);
    TRY(v->EndCommandBuffer(v->command));
    VkSubmitInfo copy = {.sType = VK_STRUCTURE_TYPE_SUBMIT_INFO,
                         .commandBufferCount = 1,
                         .pCommandBuffers = &v->command,
                         .signalSemaphoreCount = 1,
                         .pSignalSemaphores = &e->ready};
    TRY(v->QueueSubmit(v->queue, 1, &copy, VK_NULL_HANDLE));
    }
    TRY(v->ResetCommandPool(v->device, e->commands, 0));
    VkCommandBufferBeginInfo cb = {.sType = VK_STRUCTURE_TYPE_COMMAND_BUFFER_BEGIN_INFO,
                                   .flags = VK_COMMAND_BUFFER_USAGE_ONE_TIME_SUBMIT_BIT};
    TRY(v->BeginCommandBuffer(e->command, &cb));
    VkImageMemoryBarrier2 barriers[2] = {0};
    for (int i = 0; i < 2; i++)
        barriers[i] = (VkImageMemoryBarrier2){
            .sType = VK_STRUCTURE_TYPE_IMAGE_MEMORY_BARRIER_2,
            .srcStageMask = i && e->initialized ? VK_PIPELINE_STAGE_2_VIDEO_ENCODE_BIT_KHR : VK_PIPELINE_STAGE_2_NONE,
            .srcAccessMask = i && e->initialized ? VK_ACCESS_2_VIDEO_ENCODE_WRITE_BIT_KHR : 0,
            .dstStageMask = VK_PIPELINE_STAGE_2_VIDEO_ENCODE_BIT_KHR,
            .dstAccessMask = VK_ACCESS_2_VIDEO_ENCODE_READ_BIT_KHR | (i ? VK_ACCESS_2_VIDEO_ENCODE_WRITE_BIT_KHR : 0),
            .oldLayout = i ? (e->initialized ? VK_IMAGE_LAYOUT_VIDEO_ENCODE_DPB_KHR : VK_IMAGE_LAYOUT_UNDEFINED)
                           : VK_IMAGE_LAYOUT_GENERAL,
            .newLayout = i ? VK_IMAGE_LAYOUT_VIDEO_ENCODE_DPB_KHR : VK_IMAGE_LAYOUT_VIDEO_ENCODE_SRC_KHR,
            .srcQueueFamilyIndex = VK_QUEUE_FAMILY_IGNORED,
            .dstQueueFamilyIndex = VK_QUEUE_FAMILY_IGNORED,
            .image = i ? e->dpb[0].image : input_picture->image,
            .subresourceRange = {VK_IMAGE_ASPECT_COLOR_BIT, 0, 1, 0, i ? 2 : 1}};
    if(direct){barriers[0].srcQueueFamilyIndex=VK_QUEUE_FAMILY_FOREIGN_EXT;barriers[0].dstQueueFamilyIndex=v->video_family;}
    VkDependencyInfo dependency = {
        .sType = VK_STRUCTURE_TYPE_DEPENDENCY_INFO, .imageMemoryBarrierCount = 2, .pImageMemoryBarriers = barriers};
    v->CmdPipelineBarrier2(e->command, &dependency);
    v->CmdResetQueryPool(e->command, e->feedback, 0, 1);
    StdVideoH264PictureType type = idr ? STD_VIDEO_H264_PICTURE_TYPE_IDR : STD_VIDEO_H264_PICTURE_TYPE_P;
    StdVideoEncodeH264ReferenceInfo reconstructed = {
        .primary_pic_type = type, .FrameNum = e->frame, .PicOrderCnt = (int32_t)e->frame * 2};
    VkVideoEncodeH264DpbSlotInfoKHR dpb_info[2] = {
        {.sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_H264_DPB_SLOT_INFO_KHR, .pStdReferenceInfo = &reconstructed},
        {.sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_H264_DPB_SLOT_INFO_KHR, .pStdReferenceInfo = &e->previous}};
    VkVideoPictureResourceInfoKHR resources[2] = {{.sType = VK_STRUCTURE_TYPE_VIDEO_PICTURE_RESOURCE_INFO_KHR,
                                                   .codedExtent = {e->width, e->height},
                                                   .baseArrayLayer = current,
                                                   .imageViewBinding = e->dpb[0].view},
                                                  {.sType = VK_STRUCTURE_TYPE_VIDEO_PICTURE_RESOURCE_INFO_KHR,
                                                   .codedExtent = {e->width, e->height},
                                                   .baseArrayLayer = previous,
                                                   .imageViewBinding = e->dpb[0].view}};
    VkVideoReferenceSlotInfoKHR slots[2] = {{.sType = VK_STRUCTURE_TYPE_VIDEO_REFERENCE_SLOT_INFO_KHR,
                                             .pNext = &dpb_info[0],
                                             .slotIndex = -1,
                                             .pPictureResource = &resources[0]},
                                            {.sType = VK_STRUCTURE_TYPE_VIDEO_REFERENCE_SLOT_INFO_KHR,
                                             .pNext = &dpb_info[1],
                                             .slotIndex = previous,
                                             .pPictureResource = &resources[1]}};
    video_extra_picture extra;
    video_extra_prepare(e,idr,previous,&extra,slots);
    VkVideoEncodeH264GopRemainingFrameInfoKHR remaining = {
        .sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_H264_GOP_REMAINING_FRAME_INFO_KHR,
        .useGopRemainingFrames = e->h264_caps.requiresGopRemainingFrames,
        .gopRemainingI = idr ? 1 : 0,
        .gopRemainingP = e->gop - e->frame - (idr ? 1 : 0)};
    VkVideoEncodeH264RateControlInfoKHR begin_h264 = e->h264_rc;
    begin_h264.pNext = &remaining;
    VkVideoEncodeRateControlInfoKHR begin_rc = e->rc;
    begin_rc.pNext = e->codec?extra.rate:(void *)&begin_h264;
    VkVideoBeginCodingInfoKHR start = {.sType = VK_STRUCTURE_TYPE_VIDEO_BEGIN_CODING_INFO_KHR,
                                       .pNext = e->initialized ? (void *)&begin_rc : e->codec?extra.remaining:(void *)&remaining,
                                       .videoSession = e->session,
                                       .videoSessionParameters = e->parameters,
                                       .referenceSlotCount = idr ? 1 : 2,
                                       .pReferenceSlots = slots};
    v->CmdBeginVideoCodingKHR(e->command, &start);
    if (!e->initialized) {
        VkVideoEncodeQualityLevelInfoKHR quality = {
            .sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_QUALITY_LEVEL_INFO_KHR, .pNext = &e->rc, .qualityLevel = 0};
        VkVideoCodingControlInfoKHR control = {.sType = VK_STRUCTURE_TYPE_VIDEO_CODING_CONTROL_INFO_KHR,
                                               .pNext = &quality,
                                               .flags = VK_VIDEO_CODING_CONTROL_RESET_BIT_KHR |
                                                        VK_VIDEO_CODING_CONTROL_ENCODE_RATE_CONTROL_BIT_KHR |
                                                        VK_VIDEO_CODING_CONTROL_ENCODE_QUALITY_LEVEL_BIT_KHR};
        v->CmdControlVideoCodingKHR(e->command, &control);
    }
    StdVideoEncodeH264ReferenceListsInfo lists = {0};
    memset(lists.RefPicList0, 255, sizeof(lists.RefPicList0));
    memset(lists.RefPicList1, 255, sizeof(lists.RefPicList1));
    if (!idr)
        lists.RefPicList0[0] = previous;
    StdVideoEncodeH264PictureInfo picture = {.flags = {.IdrPicFlag = idr, .is_reference = 1},
                                             .primary_pic_type = type,
                                             .frame_num = e->frame,
                                             .PicOrderCnt = (int32_t)e->frame * 2,
                                             .pRefLists = idr ? NULL : &lists};
    StdVideoEncodeH264SliceHeader header = {.slice_type =
                                                idr ? STD_VIDEO_H264_SLICE_TYPE_I : STD_VIDEO_H264_SLICE_TYPE_P};
    VkVideoEncodeH264NaluSliceInfoKHR slice = {.sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_H264_NALU_SLICE_INFO_KHR,
                                               .pStdSliceHeader = &header};
    VkVideoEncodeH264PictureInfoKHR h264 = {.sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_H264_PICTURE_INFO_KHR,
                                            .naluSliceEntryCount = 1,
                                            .pNaluSliceEntries = &slice,
                                            .pStdPictureInfo = &picture};
    slots[0].slotIndex = current;
    VkVideoEncodeInfoKHR encode = {.sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_INFO_KHR,
                                   .pNext = e->codec?extra.picture:(void *)&h264,
                                   .dstBuffer = e->bitstream.buffer,
                                   .dstBufferRange = e->bitstream.size,
                                   .srcPictureResource = {.sType = VK_STRUCTURE_TYPE_VIDEO_PICTURE_RESOURCE_INFO_KHR,
                                                          .codedExtent = {e->width, e->height},
                                                          .imageViewBinding = input_picture->view},
                                   .pSetupReferenceSlot = &slots[0],
                                   .referenceSlotCount = idr ? 0 : 1,
                                   .pReferenceSlots = &slots[1]};
    v->CmdBeginQuery(e->command, e->feedback, 0, 0);
    v->CmdEncodeVideoKHR(e->command, &encode);
    v->CmdEndQuery(e->command, e->feedback, 0);
    VkVideoEndCodingInfoKHR end = {.sType = VK_STRUCTURE_TYPE_VIDEO_END_CODING_INFO_KHR};
    v->CmdEndVideoCodingKHR(e->command, &end);
    barriers[0].srcStageMask = VK_PIPELINE_STAGE_2_VIDEO_ENCODE_BIT_KHR;
    barriers[0].srcAccessMask = VK_ACCESS_2_VIDEO_ENCODE_READ_BIT_KHR;
    barriers[0].dstStageMask = VK_PIPELINE_STAGE_2_NONE;
    barriers[0].dstAccessMask = 0;
    barriers[0].oldLayout = VK_IMAGE_LAYOUT_VIDEO_ENCODE_SRC_KHR;
    barriers[0].newLayout = VK_IMAGE_LAYOUT_GENERAL;
    if(direct){barriers[0].srcQueueFamilyIndex=v->video_family;barriers[0].dstQueueFamilyIndex=VK_QUEUE_FAMILY_FOREIGN_EXT;}
    VkBufferMemoryBarrier2 coded = {.sType = VK_STRUCTURE_TYPE_BUFFER_MEMORY_BARRIER_2,
                                    .srcStageMask = VK_PIPELINE_STAGE_2_VIDEO_ENCODE_BIT_KHR,
                                    .srcAccessMask = VK_ACCESS_2_VIDEO_ENCODE_WRITE_BIT_KHR,
                                    .dstStageMask = VK_PIPELINE_STAGE_2_HOST_BIT,
                                    .dstAccessMask = VK_ACCESS_2_HOST_READ_BIT,
                                    .srcQueueFamilyIndex = VK_QUEUE_FAMILY_IGNORED,
                                    .dstQueueFamilyIndex = VK_QUEUE_FAMILY_IGNORED,
                                    .buffer = e->bitstream.buffer,
                                    .size = VK_WHOLE_SIZE};
    dependency.imageMemoryBarrierCount = 1;
    dependency.bufferMemoryBarrierCount = 1;
    dependency.pBufferMemoryBarriers = &coded;
    v->CmdPipelineBarrier2(e->command, &dependency);
    TRY(v->EndCommandBuffer(e->command));
    VkFenceCreateInfo fc = {.sType = VK_STRUCTURE_TYPE_FENCE_CREATE_INFO};
    TRY(v->CreateFence(v->device, &fc, NULL, &fence));
    VkPipelineStageFlags wait_stage = VK_PIPELINE_STAGE_ALL_COMMANDS_BIT;
    VkSubmitInfo job = {.sType = VK_STRUCTURE_TYPE_SUBMIT_INFO,
                        .waitSemaphoreCount = (direct || repeat)?0:1,
                        .pWaitSemaphores = &e->ready,
                        .pWaitDstStageMask = &wait_stage,
                        .commandBufferCount = 1,
                        .pCommandBuffers = &e->command};
    TRY(v->QueueSubmit(v->video_queue, 1, &job, fence));
    TRY(v->WaitForFences(v->device, 1, &fence, VK_TRUE, 5000000000ull));
    uint64_t feedback[2] = {0};
    TRY(v->GetQueryPoolResults(v->device, e->feedback, 0, 1, sizeof(feedback), feedback, sizeof(feedback),
                               VK_QUERY_RESULT_64_BIT | VK_QUERY_RESULT_WAIT_BIT));
    if (feedback[0] > e->bitstream.size || !feedback[1] || feedback[1] > e->bitstream.size - feedback[0]) {
        snprintf(error, capacity, "Vulkan Video returned invalid or empty bitstream feedback");
        goto failed;
    }
    if (!e->bitstream.coherent) {
        VkMappedMemoryRange range = {
            .sType = VK_STRUCTURE_TYPE_MAPPED_MEMORY_RANGE, .memory = e->bitstream.memory, .size = VK_WHOLE_SIZE};
        TRY(v->InvalidateMappedMemoryRanges(v->device, 1, &range));
    }
    size_t prefix = idr ? e->headers_size : 0;
    *output_size = prefix + feedback[1];
    *output = malloc(*output_size);
    if (!*output)
        goto failed;
    if (prefix)
        memcpy(*output, e->headers, prefix);
    memcpy(*output + prefix, (uint8_t *)e->bitstream.mapped + feedback[0], feedback[1]);
    if(e->codec==1)e->h265_previous=extra.h265_reconstructed;
    else if(e->codec==2)e->av1_previous=extra.av1_reconstructed;
    else e->previous = reconstructed;
    e->frame++;
    e->slot ^= 1;
    e->initialized = 1;
    if(!direct)e->input_initialized = 1;
    result = 0;
failed:
    if (result)
        v->DeviceWaitIdle(v->device);
    if (fence)
        v->DestroyFence(v->device, fence, NULL);
    buffer_free(v, &upload);
    bk_vk_image_free(converted);
    return result;
}
