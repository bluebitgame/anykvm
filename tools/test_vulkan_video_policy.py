"""Exercise native codec level/crop policy on any C build host, without a GPU."""
import pathlib
import shutil
import subprocess
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parent.parent


class VulkanVideoPolicyTests(unittest.TestCase):
    def test_native_codec_levels(self):
        compiler = shutil.which("cc") or shutil.which("clang")
        if not compiler:
            self.skipTest("C compiler unavailable")
        source = r'''
#include <assert.h>
#include "video_levels.h"
int main(void) {
    assert(video_h265_level(638,478,30000,1001,4000)==STD_VIDEO_H265_LEVEL_IDC_3_0);
    assert(video_h265_level(1280,720,30,1,4000)==STD_VIDEO_H265_LEVEL_IDC_3_1);
    assert(video_h265_level(1920,1080,60,1,12000)==STD_VIDEO_H265_LEVEL_IDC_4_1);
    assert(video_h265_level(3840,2160,60,1,40000)==STD_VIDEO_H265_LEVEL_IDC_5_1);
    assert(video_h265_level(1280,720,30,1,20000)==STD_VIDEO_H265_LEVEL_IDC_4_1);
    assert(video_h265_level(8192,8192,240,1,1000000)==STD_VIDEO_H265_LEVEL_IDC_INVALID);
    // SPS pads to coding blocks before the level's picture-size constraint.
    assert(video_h265_level(736,500,30,1,6000)==STD_VIDEO_H265_LEVEL_IDC_3_0);
    assert(video_av1_level(640,480,30000,1001,4000)==STD_VIDEO_AV1_LEVEL_3_0);
    assert(video_av1_level(1280,720,30,1,4000)==STD_VIDEO_AV1_LEVEL_3_1);
    assert(video_av1_level(1920,1080,60,1,20000)==STD_VIDEO_AV1_LEVEL_4_1);
    assert(video_av1_level(3840,2160,60,1,40000)==STD_VIDEO_AV1_LEVEL_5_1);
    assert(video_av1_level(1280,720,30,1,20000)==STD_VIDEO_AV1_LEVEL_4_1);
    assert(video_av1_level(8192,8192,240,1,1000000)==STD_VIDEO_AV1_LEVEL_INVALID);
    assert(video_log2_ceil(640)==10 && video_log2_ceil(480)==9);
    return 0;
}
'''
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory)
            (path / "test.c").write_text(source)
            subprocess.run([compiler, "-std=c11", "-I", str(ROOT / "internal/gpuconv"),
                            str(path / "test.c"), "-o", str(path / "test")], check=True)
            subprocess.run([str(path / "test")], check=True)




class VulkanVideoSyntaxTests(unittest.TestCase):
    def test_parameters_and_reference_chains_without_driver(self):
        compiler = shutil.which("cc") or shutil.which("clang")
        if not compiler:
            self.skipTest("C compiler unavailable")
        source = r'''
#define VK_NO_PROTOTYPES
#include <assert.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include "third_party/vulkan/vulkan_core.h"
typedef struct {
    VkDevice device;
    PFN_vkCreateVideoSessionParametersKHR CreateVideoSessionParametersKHR;
    PFN_vkGetEncodedVideoSessionParametersKHR GetEncodedVideoSessionParametersKHR;
} bk_vk;
// Only fields consumed by the codec policy/syntax helpers. Resource allocation
// and GPU submission are covered separately by TestNativeVulkanVideo.
typedef struct {
    bk_vk *v; int codec;
    uint32_t width,height,aligned_width,aligned_height,gop,frame,ctb_log2,transform_log2,av1_sb_log2;
    VkVideoCapabilitiesKHR caps;
    VkVideoEncodeH265CapabilitiesKHR h265_caps;
    VkVideoEncodeAV1CapabilitiesKHR av1_caps;
    StdVideoH265LevelIdc h265_level;StdVideoAV1Level av1_level;
    StdVideoAV1TileInfo av1_tiles;
    uint16_t av1_col_starts[65],av1_row_starts[65],av1_tile_widths[64],av1_tile_heights[64];
    VkVideoSessionKHR session;VkVideoSessionParametersKHR parameters;
    uint8_t *headers;size_t headers_size;
    VkVideoEncodeH265RateControlInfoKHR h265_rc;
    VkVideoEncodeAV1RateControlInfoKHR av1_rc;
    StdVideoEncodeH265ReferenceInfo h265_previous;
    StdVideoEncodeAV1ReferenceInfo av1_previous;
} bk_vk_encoder;
#define TRY(call) do {if((call)!=VK_SUCCESS)goto failed;}while(0)
#include "video_codecs_linux.inc"
static int codec;
static VKAPI_ATTR VkResult VKAPI_CALL create_parameters(VkDevice device,const VkVideoSessionParametersCreateInfoKHR *create,
    const VkAllocationCallbacks *allocator,VkVideoSessionParametersKHR *parameters) {
    (void)device;(void)allocator;
    if(codec==1) {
        const VkVideoEncodeH265SessionParametersCreateInfoKHR *params=create->pNext;
        assert(params->sType==VK_STRUCTURE_TYPE_VIDEO_ENCODE_H265_SESSION_PARAMETERS_CREATE_INFO_KHR);
        const StdVideoH265SequenceParameterSet *sps=params->pParametersAddInfo->pStdSPSs;
        assert(sps->pic_width_in_luma_samples==640 && sps->pic_height_in_luma_samples==480);
        assert(sps->conf_win_right_offset==1 && sps->conf_win_bottom_offset==1 && sps->flags.conformance_window_flag);
        assert(sps->chroma_format_idc==STD_VIDEO_H265_CHROMA_FORMAT_IDC_420 && sps->bit_depth_luma_minus8==0);
        assert(sps->pSequenceParameterSetVui->matrix_coeffs==1 && sps->pDecPicBufMgr->max_num_reorder_pics[0]==0);
        assert(params->pParametersAddInfo->stdVPSCount==1 && params->pParametersAddInfo->stdPPSCount==1);
    } else {
        const VkVideoEncodeAV1SessionParametersCreateInfoKHR *params=create->pNext;
        assert(params->sType==VK_STRUCTURE_TYPE_VIDEO_ENCODE_AV1_SESSION_PARAMETERS_CREATE_INFO_KHR);
        const StdVideoAV1SequenceHeader *sequence=params->pStdSequenceHeader;
        assert(sequence->max_frame_width_minus_1==639 && sequence->max_frame_height_minus_1==479);
        assert(sequence->seq_profile==STD_VIDEO_AV1_PROFILE_MAIN && sequence->pColorConfig->BitDepth==8);
        assert(sequence->pColorConfig->subsampling_x && sequence->pColorConfig->subsampling_y && !sequence->pColorConfig->flags.color_range);
        assert(params->stdOperatingPointCount==1 && params->pStdOperatingPoints->seq_tier==0);
    }
    *parameters=(VkVideoSessionParametersKHR)(uintptr_t)1;return VK_SUCCESS;
}
static VKAPI_ATTR VkResult VKAPI_CALL get_parameters(VkDevice device,const VkVideoEncodeSessionParametersGetInfoKHR *get,
    VkVideoEncodeSessionParametersFeedbackInfoKHR *feedback,size_t *size,void *data) {
    (void)device;(void)feedback;
    if(codec==1) {
        const VkVideoEncodeH265SessionParametersGetInfoKHR *info=get->pNext;
        assert(info->writeStdVPS && info->writeStdSPS && info->writeStdPPS);
    } else assert(!get->pNext);
    if(data)memset(data,0,*size);else *size=16;
    return VK_SUCCESS;
}
int main(void) {
    bk_vk v={.CreateVideoSessionParametersKHR=create_parameters,.GetEncodedVideoSessionParametersKHR=get_parameters};
    char error[512]={0};
    bk_vk_encoder e={.v=&v,.gop=60,.caps={.pictureAccessGranularity={1,1}}};
    e.h265_caps=(VkVideoEncodeH265CapabilitiesKHR){.maxLevelIdc=STD_VIDEO_H265_LEVEL_IDC_6_2,.maxSliceSegmentCount=1,.maxSubLayerCount=1,
        .ctbSizes=VK_VIDEO_ENCODE_H265_CTB_SIZE_64_BIT_KHR,
        .transformBlockSizes=VK_VIDEO_ENCODE_H265_TRANSFORM_BLOCK_SIZE_4_BIT_KHR|VK_VIDEO_ENCODE_H265_TRANSFORM_BLOCK_SIZE_32_BIT_KHR};
    e.av1_caps=(VkVideoEncodeAV1CapabilitiesKHR){.maxLevel=STD_VIDEO_AV1_LEVEL_6_2,.maxOperatingPoints=1,.maxTemporalLayerCount=1,.maxSpatialLayerCount=1,
        .codedPictureAlignment={8,8},.superblockSizes=VK_VIDEO_ENCODE_AV1_SUPERBLOCK_SIZE_64_BIT_KHR,
        .minTileSize={64,64},.maxTileSize={4096,4096},.maxTiles={64,64},.singleReferenceNameMask=1};
    for(codec=1;codec<=2;codec++) {
        e.codec=codec;e.width=codec==1?638:640;e.height=codec==1?478:480;
        assert(video_extra_configure(&e,30000,1001,4000,error,sizeof(error))==0);
        assert(video_extra_parameters(&e,30000,1001,error,sizeof(error))==0);
        free(e.headers);e.headers=NULL;e.headers_size=0;
        video_extra_picture picture;VkVideoReferenceSlotInfoKHR slots[2]={0};
        for(int key=0;key<=1;key++) {
            e.frame=key?0:1;
            video_extra_prepare(&e,key,1,&picture,slots);
            if(codec==1) {
                assert(slots[0].pNext==&picture.h265_dpb[0] && slots[1].pNext==&picture.h265_dpb[1]);
                assert(picture.h265.pStdPictureInfo->pRefLists==&picture.h265_lists && picture.h265_header.flags.first_slice_segment_in_pic_flag);
                assert(picture.h265_picture.flags.IrapPicFlag==key);
                if(!key)assert(picture.h265_lists.RefPicList0[0]==1 && picture.h265_rps.num_negative_pics==1 && picture.h265_rps.used_by_curr_pic_s0_flag);
                assert(picture.h265_rate.pNext==&picture.h265_remaining);
            } else {
                assert(slots[0].pNext==&picture.av1_dpb[0] && slots[1].pNext==&picture.av1_dpb[1]);
                assert(picture.av1_picture.pTileInfo==&e.av1_tiles && picture.av1_picture.flags.show_frame && picture.av1_picture.flags.error_resilient_mode);
                assert(picture.av1_picture.primary_ref_frame==STD_VIDEO_AV1_PRIMARY_REF_NONE);
                assert(picture.av1_picture.refresh_frame_flags==(key?255:1));
                assert(picture.av1.referenceNameSlotIndices[0]==(key?-1:1));
                for(int i=1;i<7;i++)assert(picture.av1.referenceNameSlotIndices[i]==-1);
                assert(picture.av1_rate.pNext==&picture.av1_remaining);
            }
        }
    }
    e.codec=2;e.width=638;e.height=478;
    assert(video_extra_configure(&e,30,1,4000,error,sizeof(error))<0);
    e.width=7680;e.height=4320;
    assert(video_extra_configure(&e,60,1,100000,error,sizeof(error))==0);
    assert(e.av1_tiles.TileCols==2 && e.av1_tiles.TileRows==2);
    assert(e.av1_tiles.pMiColStarts[2]==1920 && e.av1_tiles.pMiRowStarts[2]==1080);
    e.av1_caps.maxTiles.width=1;
    assert(video_extra_configure(&e,60,1,100000,error,sizeof(error))<0);
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
