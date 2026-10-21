"""Verify driver declarations without requiring a GPU or encode entrypoints."""
import pathlib
import shutil
import subprocess
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parent.parent


class CodecCapabilitiesTests(unittest.TestCase):
    def test_vulkan_queries_do_not_need_an_encoder_or_picture_size(self):
        compiler = shutil.which("cc") or shutil.which("clang")
        if not compiler:
            self.skipTest("C compiler unavailable")
        source = r'''
#define VK_NO_PROTOTYPES
#include <assert.h>
#include <string.h>
#include "third_party/vulkan/vulkan_core.h"
// No device/session creation or encode function exists in this test dispatch.
typedef struct {
    int video_codec;
    VkPhysicalDevice physical;
    PFN_vkGetPhysicalDeviceVideoCapabilitiesKHR GetPhysicalDeviceVideoCapabilitiesKHR;
    PFN_vkGetPhysicalDeviceVideoFormatPropertiesKHR GetPhysicalDeviceVideoFormatPropertiesKHR;
} bk_vk;
#include "video_capability_linux.inc"
static int codec, missing_profile, missing_cbr, missing_nv12, low_latency_only, calls;
static VKAPI_ATTR VkResult VKAPI_CALL capabilities(VkPhysicalDevice device,
    const VkVideoProfileInfoKHR *profile, VkVideoCapabilitiesKHR *caps) {
    (void)device; calls++;
    assert(profile->chromaSubsampling==VK_VIDEO_CHROMA_SUBSAMPLING_420_BIT_KHR);
    assert(profile->lumaBitDepth==VK_VIDEO_COMPONENT_BIT_DEPTH_8_BIT_KHR);
    assert(profile->videoCodecOperation==(codec==1?VK_VIDEO_CODEC_OPERATION_ENCODE_H265_BIT_KHR:VK_VIDEO_CODEC_OPERATION_ENCODE_AV1_BIT_KHR));
    const VkVideoEncodeUsageInfoKHR *usage=profile->pNext;
    if(missing_profile || (low_latency_only && usage->tuningMode==VK_VIDEO_ENCODE_TUNING_MODE_ULTRA_LOW_LATENCY_KHR))
        return VK_ERROR_VIDEO_PROFILE_OPERATION_NOT_SUPPORTED_KHR;
    VkVideoEncodeCapabilitiesKHR *encode=caps->pNext;
    encode->rateControlModes=missing_cbr?0:VK_VIDEO_ENCODE_RATE_CONTROL_MODE_CBR_BIT_KHR;
    caps->maxDpbSlots=2;caps->maxActiveReferencePictures=1;
    // Unusual stream extents/alignment must not turn a general declaration into
    // a negative result. Actual stream dimensions are validated on stream open.
    caps->minCodedExtent=(VkExtent2D){2048,1024};
    caps->maxCodedExtent=(VkExtent2D){4096,4096};
    if(codec==1) ((VkVideoEncodeH265CapabilitiesKHR *)encode->pNext)->maxPPictureL0ReferenceCount=1;
    else {
        VkVideoEncodeAV1CapabilitiesKHR *av1=encode->pNext;
        av1->maxSingleReferenceCount=1;
        av1->singleReferenceNameMask=1;
        av1->codedPictureAlignment=(VkExtent2D){256,256};
    }
    return VK_SUCCESS;
}
static VKAPI_ATTR VkResult VKAPI_CALL formats(VkPhysicalDevice device,
    const VkPhysicalDeviceVideoFormatInfoKHR *info, uint32_t *count, VkVideoFormatPropertiesKHR *formats) {
    (void)device;
    assert(info->imageUsage & (VK_IMAGE_USAGE_VIDEO_ENCODE_DPB_BIT_KHR|VK_IMAGE_USAGE_VIDEO_ENCODE_SRC_BIT_KHR));
    if(!formats) {*count=1;return VK_SUCCESS;}
    formats[0].format=missing_nv12?VK_FORMAT_R8_UNORM:VK_FORMAT_G8_B8R8_2PLANE_420_UNORM;
    formats[0].imageTiling=VK_IMAGE_TILING_OPTIMAL;
    return VK_SUCCESS;
}
int main(void) {
    bk_vk v={.GetPhysicalDeviceVideoCapabilitiesKHR=capabilities,.GetPhysicalDeviceVideoFormatPropertiesKHR=formats};
    for(codec=1;codec<=2;codec++) {
        v.video_codec=codec;
        assert(bk_vk_encoder_supports(&v));
        low_latency_only=1; calls=0;
        assert(bk_vk_encoder_supports(&v) && calls==2);
        low_latency_only=0;missing_profile=1;
        assert(!bk_vk_encoder_supports(&v));
        missing_profile=0;missing_cbr=1;
        assert(!bk_vk_encoder_supports(&v));
        missing_cbr=0;missing_nv12=1;
        assert(!bk_vk_encoder_supports(&v));
        missing_nv12=0;
    }
    return 0;
}
'''
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory)
            (path / "test.c").write_text(source)
            subprocess.run([compiler, "-std=c11", "-I", str(ROOT / "internal/gpuconv"),
                            "-I", str(ROOT / "internal/gpuconv/third_party"),
                            str(path / "test.c"), "-o", str(path / "test")], check=True)
            subprocess.run([str(path / "test")], check=True)


if __name__ == "__main__":
    unittest.main()
