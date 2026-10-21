#include <sys/stat.h>
// Included after video_image helpers; all imported objects are frame-local.
static int video_nv12_import(bk_vk_encoder *e,const bk_vk_source *f,video_image *i,char *error,size_t capacity){
 bk_vk *v=e->v;int fd=-1;
 if(f->fourcc!=0x3231564e || f->width!=e->width || f->height!=e->height || f->planes!=2 || f->modifier || f->flip_y ||
    f->pitches[0]!=f->pitch || f->pitches[1]!=f->pitch || f->pitch<f->width || !f->size)goto failed;
 VkPhysicalDeviceExternalImageFormatInfo external={.sType=VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_EXTERNAL_IMAGE_FORMAT_INFO,.pNext=&e->profiles,.handleType=VK_EXTERNAL_MEMORY_HANDLE_TYPE_DMA_BUF_BIT_EXT};
 VkPhysicalDeviceImageDrmFormatModifierInfoEXT modifier={.sType=VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_IMAGE_DRM_FORMAT_MODIFIER_INFO_EXT,.pNext=&external,.drmFormatModifier=0,.sharingMode=VK_SHARING_MODE_EXCLUSIVE};
 VkPhysicalDeviceImageFormatInfo2 query={.sType=VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_IMAGE_FORMAT_INFO_2,.pNext=&modifier,.format=VK_FORMAT_G8_B8R8_2PLANE_420_UNORM,.type=VK_IMAGE_TYPE_2D,.tiling=VK_IMAGE_TILING_DRM_FORMAT_MODIFIER_EXT,.usage=VK_IMAGE_USAGE_VIDEO_ENCODE_SRC_BIT_KHR};
 VkExternalImageFormatProperties props={.sType=VK_STRUCTURE_TYPE_EXTERNAL_IMAGE_FORMAT_PROPERTIES};
 VkImageFormatProperties2 result={.sType=VK_STRUCTURE_TYPE_IMAGE_FORMAT_PROPERTIES_2,.pNext=&props};
 TRY(v->GetPhysicalDeviceImageFormatProperties2(v->physical,&query,&result));
 if(!(props.externalMemoryProperties.externalMemoryFeatures&VK_EXTERNAL_MEMORY_FEATURE_IMPORTABLE_BIT))goto failed;
 VkSubresourceLayout planes[2]={{.offset=f->offsets[0],.rowPitch=f->pitches[0]},{.offset=f->offsets[1],.rowPitch=f->pitches[1]}};
 VkExternalMemoryImageCreateInfo sharing={.sType=VK_STRUCTURE_TYPE_EXTERNAL_MEMORY_IMAGE_CREATE_INFO,.pNext=&e->profiles,.handleTypes=external.handleType};
 VkImageDrmFormatModifierExplicitCreateInfoEXT layout={.sType=VK_STRUCTURE_TYPE_IMAGE_DRM_FORMAT_MODIFIER_EXPLICIT_CREATE_INFO_EXT,.pNext=&sharing,.drmFormatModifier=0,.drmFormatModifierPlaneCount=2,.pPlaneLayouts=planes};
 VkImageCreateInfo info={.sType=VK_STRUCTURE_TYPE_IMAGE_CREATE_INFO,.pNext=&layout,.imageType=VK_IMAGE_TYPE_2D,.format=query.format,
 .extent={f->width,f->height,1},.mipLevels=1,.arrayLayers=1,.samples=VK_SAMPLE_COUNT_1_BIT,.tiling=query.tiling,.usage=query.usage,.sharingMode=VK_SHARING_MODE_EXCLUSIVE};
 TRY(v->CreateImage(v->device,&info,NULL,&i->image));
 VkMemoryRequirements req;v->GetImageMemoryRequirements(v->device,i->image,&req);
 VkMemoryFdPropertiesKHR fdprops={.sType=VK_STRUCTURE_TYPE_MEMORY_FD_PROPERTIES_KHR};TRY(v->GetMemoryFdPropertiesKHR(v->device,external.handleType,f->fd,&fdprops));
 uint32_t type=memory_type(v,req.memoryTypeBits&fdprops.memoryTypeBits,0);if(type==UINT32_MAX||f->size<req.size)goto failed;
 fd=fcntl(f->fd,F_DUPFD_CLOEXEC,0);if(fd<0)goto failed;
 VkMemoryDedicatedAllocateInfo dedicated={.sType=VK_STRUCTURE_TYPE_MEMORY_DEDICATED_ALLOCATE_INFO,.image=i->image};
 VkImportMemoryFdInfoKHR imported={.sType=VK_STRUCTURE_TYPE_IMPORT_MEMORY_FD_INFO_KHR,.pNext=&dedicated,.handleType=external.handleType,.fd=fd};
 VkMemoryAllocateInfo alloc={.sType=VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO,.pNext=&imported,.allocationSize=f->size,.memoryTypeIndex=type};
 TRY(v->AllocateMemory(v->device,&alloc,NULL,&i->memory));fd=-1;TRY(v->BindImageMemory(v->device,i->image,i->memory,0));
 VkImageViewCreateInfo view={.sType=VK_STRUCTURE_TYPE_IMAGE_VIEW_CREATE_INFO,.image=i->image,.viewType=VK_IMAGE_VIEW_TYPE_2D,.format=query.format,.subresourceRange={VK_IMAGE_ASPECT_COLOR_BIT,0,1,0,1}};
 TRY(v->CreateImageView(v->device,&view,NULL,&i->view));return 0;
failed:
 if(fd>=0)close(fd);video_image_free(v,i);memset(i,0,sizeof(*i));
 snprintf(error,capacity,"capture DMA-BUF is incompatible with Vulkan Video input");return -1;
}

// FD numbers are reused after close. Cache by the DMA-BUF allocation identity,
// retaining imported memory until encoder shutdown, never by the numeric FD.
static video_image *video_nv12_cached(bk_vk_encoder *e,const bk_vk_source *f,char *error,size_t capacity){
 struct stat st;if(fstat(f->fd,&st)<0)return NULL;
 for(unsigned j=0;j<e->nv12_count;j++){
  const bk_vk_source *old=&e->nv12_sources[j];
  if(e->nv12_inodes[j]==(uint64_t)st.st_ino && e->nv12_devices[j]==(uint64_t)st.st_dev && old->size==f->size &&
   old->width==f->width && old->height==f->height && old->pitch==f->pitch && old->offsets[0]==f->offsets[0] && old->offsets[1]==f->offsets[1])return &e->nv12_images[j];
 }
 if(e->nv12_count>=64){snprintf(error,capacity,"NV12 import cache limit reached");return NULL;}
 unsigned j=e->nv12_count;
 if(video_nv12_import(e,f,&e->nv12_images[j],error,capacity)<0)return NULL;
 e->nv12_inodes[j]=st.st_ino;e->nv12_devices[j]=st.st_dev;e->nv12_sources[j]=*f;e->nv12_count++;
 return &e->nv12_images[j];
}
