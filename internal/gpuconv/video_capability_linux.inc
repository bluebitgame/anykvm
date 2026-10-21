// Driver declarations only; no session, picture allocation or trial encoding.
int bk_vk_encoder_supports(bk_vk *v) {
    VkVideoEncodeH265ProfileInfoKHR hevc = {.sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_H265_PROFILE_INFO_KHR,
                                            .stdProfileIdc = STD_VIDEO_H265_PROFILE_IDC_MAIN};
    VkVideoEncodeAV1ProfileInfoKHR av1 = {.sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_AV1_PROFILE_INFO_KHR,
                                          .stdProfile = STD_VIDEO_AV1_PROFILE_MAIN};
    VkVideoEncodeUsageInfoKHR usage = {.sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_USAGE_INFO_KHR,
                                       .pNext = v->video_codec == 1 ? (void *)&hevc : (void *)&av1,
                                       .videoUsageHints = VK_VIDEO_ENCODE_USAGE_STREAMING_BIT_KHR,
                                       .videoContentHints = VK_VIDEO_ENCODE_CONTENT_DESKTOP_BIT_KHR,
                                       .tuningMode = VK_VIDEO_ENCODE_TUNING_MODE_ULTRA_LOW_LATENCY_KHR};
    VkVideoProfileInfoKHR profile = {.sType = VK_STRUCTURE_TYPE_VIDEO_PROFILE_INFO_KHR,
                                     .pNext = &usage,
                                     .videoCodecOperation = v->video_codec == 1
                                                                ? VK_VIDEO_CODEC_OPERATION_ENCODE_H265_BIT_KHR
                                                                : VK_VIDEO_CODEC_OPERATION_ENCODE_AV1_BIT_KHR,
                                     .chromaSubsampling = VK_VIDEO_CHROMA_SUBSAMPLING_420_BIT_KHR,
                                     .lumaBitDepth = VK_VIDEO_COMPONENT_BIT_DEPTH_8_BIT_KHR,
                                     .chromaBitDepth = VK_VIDEO_COMPONENT_BIT_DEPTH_8_BIT_KHR};
    VkVideoEncodeH265CapabilitiesKHR hevc_caps = {.sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_H265_CAPABILITIES_KHR};
    VkVideoEncodeAV1CapabilitiesKHR av1_caps = {.sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_AV1_CAPABILITIES_KHR};
    VkVideoEncodeCapabilitiesKHR encode = {.sType = VK_STRUCTURE_TYPE_VIDEO_ENCODE_CAPABILITIES_KHR,
                                           .pNext = v->video_codec == 1 ? (void *)&hevc_caps : (void *)&av1_caps};
    VkVideoCapabilitiesKHR caps = {.sType = VK_STRUCTURE_TYPE_VIDEO_CAPABILITIES_KHR, .pNext = &encode};
    if (v->GetPhysicalDeviceVideoCapabilitiesKHR(v->physical, &profile, &caps) != VK_SUCCESS) {
        usage.tuningMode = VK_VIDEO_ENCODE_TUNING_MODE_LOW_LATENCY_KHR;
        if (v->GetPhysicalDeviceVideoCapabilitiesKHR(v->physical, &profile, &caps) != VK_SUCCESS)
            return 0;
    }
    if (!(encode.rateControlModes & VK_VIDEO_ENCODE_RATE_CONTROL_MODE_CBR_BIT_KHR) || caps.maxDpbSlots < 2 ||
        caps.maxActiveReferencePictures < 1 ||
        (v->video_codec == 1 ? !hevc_caps.maxPPictureL0ReferenceCount
                             : !av1_caps.maxSingleReferenceCount || !(av1_caps.singleReferenceNameMask & 127)))
        return 0;
    VkVideoProfileListInfoKHR profiles = {
        .sType = VK_STRUCTURE_TYPE_VIDEO_PROFILE_LIST_INFO_KHR, .profileCount = 1, .pProfiles = &profile};
    for (unsigned pass = 0; pass < 2; pass++) {
        VkPhysicalDeviceVideoFormatInfoKHR info = {.sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_VIDEO_FORMAT_INFO_KHR,
                                                   .pNext = &profiles,
                                                   .imageUsage = pass ? VK_IMAGE_USAGE_VIDEO_ENCODE_DPB_BIT_KHR
                                                                      : VK_IMAGE_USAGE_VIDEO_ENCODE_SRC_BIT_KHR |
                                                                            VK_IMAGE_USAGE_TRANSFER_DST_BIT};
        uint32_t count = 0;
        if (v->GetPhysicalDeviceVideoFormatPropertiesKHR(v->physical, &info, &count, NULL) != VK_SUCCESS || !count ||
            count > 64)
            return 0;
        VkVideoFormatPropertiesKHR formats[64] = {0};
        for (unsigned i = 0; i < count; i++)
            formats[i].sType = VK_STRUCTURE_TYPE_VIDEO_FORMAT_PROPERTIES_KHR;
        if (v->GetPhysicalDeviceVideoFormatPropertiesKHR(v->physical, &info, &count, formats) != VK_SUCCESS)
            return 0;
        int found = 0;
        for (unsigned i = 0; i < count; i++)
            if (formats[i].format == VK_FORMAT_G8_B8R8_2PLANE_420_UNORM &&
                formats[i].imageTiling == VK_IMAGE_TILING_OPTIMAL)
                found = 1;
        if (!found)
            return 0;
    }
    return 1;
}
