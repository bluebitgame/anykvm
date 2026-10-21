//go:build linux && cgo

#define VK_NO_PROTOTYPES
#include "native_linux.h"
#include "third_party/vulkan/vulkan_core.h"
#include "drm_match_linux.h"
#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include "dma_buf_compat_linux.h"
#include <poll.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <unistd.h>

#define FUNCTIONS(X)                                                                                                   \
    X(DestroyInstance)                                                                                                 \
    X(EnumeratePhysicalDevices) X(GetPhysicalDeviceProperties2) X(GetPhysicalDeviceMemoryProperties) X(                \
        GetPhysicalDeviceQueueFamilyProperties2) X(EnumerateDeviceExtensionProperties)                                 \
        X(GetPhysicalDeviceFeatures2) X(GetPhysicalDeviceImageFormatProperties2)  X(                     \
            GetPhysicalDeviceExternalFenceProperties) X(CreateDevice) X(DestroyDevice) X(GetDeviceQueue)               \
            X(CreateBuffer) X(DestroyBuffer) X(GetBufferMemoryRequirements) X(AllocateMemory) X(FreeMemory) X(         \
                BindBufferMemory) X(MapMemory) X(UnmapMemory) X(InvalidateMappedMemoryRanges)                          \
                X(FlushMappedMemoryRanges) X(CreateImage) X(DestroyImage) X(GetImageMemoryRequirements) X(             \
                    BindImageMemory) X(CreateImageView) X(DestroyImageView) X(CreateSampler) X(DestroySampler)         \
                    X(CreateDescriptorSetLayout) X(DestroyDescriptorSetLayout) X(CreatePipelineLayout) X(              \
                        DestroyPipelineLayout) X(CreateShaderModule) X(DestroyShaderModule) X(CreateComputePipelines)  \
                        X(DestroyPipeline) X(CreateDescriptorPool) X(DestroyDescriptorPool) X(                         \
                            AllocateDescriptorSets) X(UpdateDescriptorSets) X(CreateCommandPool) X(DestroyCommandPool) \
                            X(AllocateCommandBuffers) X(ResetCommandPool) X(BeginCommandBuffer) X(EndCommandBuffer) X( \
                                CmdPipelineBarrier) X(CmdBindPipeline) X(CmdBindDescriptorSets) X(CmdDispatch)         \
                                X(CmdDispatchIndirect) X(CmdFillBuffer) X(CmdCopyBuffer) X(CmdCopyBufferToImage) X(CreateFence) X(DestroyFence) X(QueueSubmit) \
                                    X(WaitForFences) X(QueueWaitIdle) X(DeviceWaitIdle)               \
                                        X(GetMemoryFdPropertiesKHR) X(GetFenceFdKHR)                                   \
                                            X(GetPhysicalDeviceVideoCapabilitiesKHR) X(                                \
                                                GetPhysicalDeviceVideoFormatPropertiesKHR) X(CreateVideoSessionKHR)    \
                                                X(DestroyVideoSessionKHR) X(GetVideoSessionMemoryRequirementsKHR)      \
                                                    X(BindVideoSessionMemoryKHR) X(CreateVideoSessionParametersKHR)    \
                                                        X(DestroyVideoSessionParametersKHR)                            \
                                                            X(GetEncodedVideoSessionParametersKHR)                     \
                                                                X(CmdBeginVideoCodingKHR) X(CmdEndVideoCodingKHR)      \
                                                                    X(CmdControlVideoCodingKHR) X(CmdEncodeVideoKHR)   \
                                                                        X(CreateQueryPool) X(DestroyQueryPool)         \
                                                                            X(GetQueryPoolResults)                     \
                                                                                X(CmdResetQueryPool) X(CmdBeginQuery)  \
                                                                                    X(CmdEndQuery)                     \
                                                                                        X(CmdPipelineBarrier2)         \
                                                                                            X(CreateSemaphore)         \
                                                                                                X(DestroySemaphore)
struct bk_vk {
    void *library;
    PFN_vkGetInstanceProcAddr get;
    VkInstance instance;
    VkPhysicalDevice physical;
    VkDevice device;
    VkQueue queue;
    uint32_t family, video_family, major, minor;
    int video_codec;
    int borrowed_device; // Child compute context; the Converter lease owns the device/loader.
    VkQueue video_queue;
    VkPhysicalDeviceMemoryProperties memory;
    VkDescriptorSetLayout descriptor_layout;
    VkPipelineLayout layout;
    VkPipeline pipeline;
    VkPipeline jpeg_entropy, jpeg_index, jpeg_pack, jpeg_compare, jpeg_work, jpeg_select_entropy, jpeg_prepare_entropy;
    VkPipeline jpeg_scan_groups, jpeg_add_offsets;
    struct jpeg_buffers *jpeg_buffers;
    VkDescriptorPool descriptor_pool;
    VkDescriptorSet descriptor;
    VkCommandPool commands;
    VkCommandBuffer command;
    VkSampler sampler;
#define FIELD(name) PFN_vk##name name;
    FUNCTIONS(FIELD)
#undef FIELD
};
typedef struct {
    VkBuffer buffer;
    VkDeviceMemory memory;
    VkDeviceSize size, allocation;
    void *mapped;
    int coherent;
} buffer;
struct bk_vk_image {
    bk_vk *context;
    buffer output, staging;
    uint32_t width, height, pitch;
    int nv12, readback_ready;
};
typedef struct {
    VkImage image;
    VkDeviceMemory memory;
    VkImageView view;
} source;
static int vk_error(char *error, size_t capacity, const char *operation, VkResult code) {
    snprintf(error, capacity, "%s failed (Vulkan %d)", operation, code);
    return -1;
}
#define TRY(call)                                                                                                      \
    do {                                                                                                               \
        VkResult code = (call);                                                                                        \
        if (code != VK_SUCCESS) {                                                                                      \
            vk_error(error, capacity, #call, code);                                                                    \
            goto failed;                                                                                               \
        }                                                                                                              \
    } while (0)
static uint32_t memory_type(bk_vk *v, uint32_t bits, VkMemoryPropertyFlags need) {
    for (uint32_t i = 0; i < v->memory.memoryTypeCount; i++)
        if ((bits & (1u << i)) && (v->memory.memoryTypes[i].propertyFlags & need) == need)
            return i;
    return UINT32_MAX;
}
static int extensions(bk_vk *v, VkPhysicalDevice device, const char **names, uint32_t count) {
    uint32_t n = 0;
    if (v->EnumerateDeviceExtensionProperties(device, NULL, &n, NULL) != VK_SUCCESS || n > 2048)
        return 0;
    VkExtensionProperties *properties = calloc(n, sizeof(*properties));
    if (!properties)
        return 0;
    int ok = v->EnumerateDeviceExtensionProperties(device, NULL, &n, properties) == VK_SUCCESS;
    for (uint32_t i = 0; i < count && ok; i++) {
        int found = 0;
        for (uint32_t j = 0; j < n; j++)
            if (!strcmp(names[i], properties[j].extensionName))
                found = 1;
        if (!found)
            ok = 0;
    }
    free(properties);
    return ok;
}
static void buffer_free(bk_vk *v, buffer *b) {
    if (b->mapped)
        v->UnmapMemory(v->device, b->memory);
    if (b->buffer)
        v->DestroyBuffer(v->device, b->buffer, NULL);
    if (b->memory)
        v->FreeMemory(v->device, b->memory, NULL);
    memset(b, 0, sizeof(*b));
}
static int buffer_new(bk_vk *v, buffer *b, VkDeviceSize size, VkBufferUsageFlags usage, int host,
                      char *error, size_t capacity) {
    VkBufferCreateInfo info = {.sType = VK_STRUCTURE_TYPE_BUFFER_CREATE_INFO,
                               .size = size,
                               .usage = usage,
                               .sharingMode = VK_SHARING_MODE_EXCLUSIVE,
                               .pNext = NULL};
    TRY(v->CreateBuffer(v->device, &info, NULL, &b->buffer));
    VkMemoryRequirements req;
    v->GetBufferMemoryRequirements(v->device, b->buffer, &req);
    uint32_t type = memory_type(v, req.memoryTypeBits,
                                host ? VK_MEMORY_PROPERTY_HOST_VISIBLE_BIT | VK_MEMORY_PROPERTY_HOST_COHERENT_BIT
                                     : VK_MEMORY_PROPERTY_DEVICE_LOCAL_BIT);
    if (type == UINT32_MAX)
        type = memory_type(v, req.memoryTypeBits, host ? VK_MEMORY_PROPERTY_HOST_VISIBLE_BIT : 0);
    if (type == UINT32_MAX) {
        snprintf(error, capacity, "Vulkan has no compatible buffer memory");
        goto failed;
    }
    VkMemoryAllocateInfo alloc = {.sType = VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO,
                                  .pNext = NULL,
                                  .allocationSize = req.size,
                                  .memoryTypeIndex = type};
    TRY(v->AllocateMemory(v->device, &alloc, NULL, &b->memory));
    TRY(v->BindBufferMemory(v->device, b->buffer, b->memory, 0));
    b->size = size;
    b->allocation = req.size;
    b->coherent = (v->memory.memoryTypes[type].propertyFlags & VK_MEMORY_PROPERTY_HOST_COHERENT_BIT) != 0;
    if (host)
        TRY(v->MapMemory(v->device, b->memory, 0, VK_WHOLE_SIZE, 0, &b->mapped));
    return 0;
failed:
    buffer_free(v, b);
    return -1;
}
static int begin(bk_vk *v, char *error, size_t capacity) {
    TRY(v->ResetCommandPool(v->device, v->commands, 0));
    VkCommandBufferBeginInfo info = {.sType = VK_STRUCTURE_TYPE_COMMAND_BUFFER_BEGIN_INFO,
                                     .flags = VK_COMMAND_BUFFER_USAGE_ONE_TIME_SUBMIT_BIT};
    TRY(v->BeginCommandBuffer(v->command, &info));
    return 0;
failed:
    return -1;
}
static int wait_fd(int fd, int timeout) {
    if (fd < 0)
        return 0;
    struct pollfd p = {.fd = fd, .events = POLLIN};
    int r;
    do {
        r = poll(&p, 1, timeout);
    } while (r < 0 && errno == EINTR);
    return r > 0 && (p.revents & POLLIN) ? 0 : -1;
}
static int submit(bk_vk *v, int dma_fd, char *error, size_t capacity) {
    VkFence fence = VK_NULL_HANDLE;
    int sync_fd = -1;
    TRY(v->EndCommandBuffer(v->command));
    VkExportFenceCreateInfo export = {.sType = VK_STRUCTURE_TYPE_EXPORT_FENCE_CREATE_INFO,
                                      .handleTypes = VK_EXTERNAL_FENCE_HANDLE_TYPE_SYNC_FD_BIT};
    VkFenceCreateInfo info = {.sType = VK_STRUCTURE_TYPE_FENCE_CREATE_INFO, .pNext = dma_fd >= 0 ? &export : NULL};
    TRY(v->CreateFence(v->device, &info, NULL, &fence));
    VkSubmitInfo job = {
        .sType = VK_STRUCTURE_TYPE_SUBMIT_INFO, .commandBufferCount = 1, .pCommandBuffers = &v->command};
    TRY(v->QueueSubmit(v->queue, 1, &job, fence));
    if (dma_fd >= 0) {
        VkFenceGetFdInfoKHR fd_info = {.sType = VK_STRUCTURE_TYPE_FENCE_GET_FD_INFO_KHR,
                                       .fence = fence,
                                       .handleType = VK_EXTERNAL_FENCE_HANDLE_TYPE_SYNC_FD_BIT};
        TRY(v->GetFenceFdKHR(v->device, &fd_info, &sync_fd));
        // Export transfers the fence payload. Wait on the sync-file, not the
        // reset VkFence. Publish our read fence before the compositor reuses it.
        if (sync_fd >= 0) {
            struct dma_buf_import_sync_file imported = {.flags = DMA_BUF_SYNC_READ, .fd = sync_fd};
            if (ioctl(dma_fd, DMA_BUF_IOCTL_IMPORT_SYNC_FILE, &imported) < 0 || wait_fd(sync_fd, 5000) < 0) {
                snprintf(error, capacity, "Vulkan DMA-BUF read fence failed: %s", strerror(errno));
                goto failed;
            }
        }
    } else
        TRY(v->WaitForFences(v->device, 1, &fence, VK_TRUE, 5000000000ull));
    if (sync_fd >= 0)
        close(sync_fd);
    v->DestroyFence(v->device, fence, NULL);
    return 0;
failed:
    // Destruction must never race submitted GPU work, including error paths.
    v->QueueWaitIdle(v->queue);
    if (sync_fd >= 0)
        close(sync_fd);
    if (fence)
        v->DestroyFence(v->device, fence, NULL);
    return -1;
}
static void jpeg_buffers_free(bk_vk *v);

void bk_vk_close(bk_vk *v) {
    if (!v)
        return;
    if (v->device) {
        v->DeviceWaitIdle(v->device);
        jpeg_buffers_free(v);
        if (v->commands)
            v->DestroyCommandPool(v->device, v->commands, NULL);
        if (v->descriptor_pool)
            v->DestroyDescriptorPool(v->device, v->descriptor_pool, NULL);
        if (v->sampler)
            v->DestroySampler(v->device, v->sampler, NULL);
        if (v->pipeline)
            v->DestroyPipeline(v->device, v->pipeline, NULL);
        if (v->jpeg_select_entropy) v->DestroyPipeline(v->device, v->jpeg_select_entropy, NULL);
        if (v->jpeg_prepare_entropy) v->DestroyPipeline(v->device, v->jpeg_prepare_entropy, NULL);
        if (v->jpeg_work) v->DestroyPipeline(v->device, v->jpeg_work, NULL);
        if (v->jpeg_compare) v->DestroyPipeline(v->device, v->jpeg_compare, NULL);
        if (v->jpeg_entropy) v->DestroyPipeline(v->device, v->jpeg_entropy, NULL);
        if (v->jpeg_index) v->DestroyPipeline(v->device, v->jpeg_index, NULL);
        if (v->jpeg_scan_groups) v->DestroyPipeline(v->device, v->jpeg_scan_groups, NULL);
        if (v->jpeg_add_offsets) v->DestroyPipeline(v->device, v->jpeg_add_offsets, NULL);
        if (v->jpeg_pack) v->DestroyPipeline(v->device, v->jpeg_pack, NULL);
        if (v->layout)
            v->DestroyPipelineLayout(v->device, v->layout, NULL);
        if (v->descriptor_layout)
            v->DestroyDescriptorSetLayout(v->device, v->descriptor_layout, NULL);
        if (!v->borrowed_device) v->DestroyDevice(v->device, NULL);
    }
    if (v->instance && v->DestroyInstance)
        v->DestroyInstance(v->instance, NULL);
    if (v->library)
        dlclose(v->library);
    free(v);
}
bk_vk *bk_vk_open(uint32_t major, uint32_t minor, const void *spirv, size_t size, int mode, char *error,
                  size_t capacity) {
    int query = mode == BK_VK_QUERY_HEVC || mode == BK_VK_QUERY_AV1;
    int video = query || mode == BK_VK_VIDEO || mode == BK_VK_VIDEO_HEVC || mode == BK_VK_VIDEO_AV1;
    int codec = mode == BK_VK_VIDEO_HEVC || mode == BK_VK_QUERY_HEVC ? 1 : mode == BK_VK_VIDEO_AV1 || mode == BK_VK_QUERY_AV1 ? 2 : 0;
    VkVideoCodecOperationFlagBitsKHR operation = codec == 1   ? VK_VIDEO_CODEC_OPERATION_ENCODE_H265_BIT_KHR
                                                 : codec == 2 ? VK_VIDEO_CODEC_OPERATION_ENCODE_AV1_BIT_KHR
                                                              : VK_VIDEO_CODEC_OPERATION_ENCODE_H264_BIT_KHR;
    int jpeg = mode == BK_VK_JPEG;
    int decode = mode == BK_VK_JPEG_DECODE || mode == BK_VK_PROGRESSIVE;
    bk_vk *v = calloc(1, sizeof(*v));
    if (!v)
        return NULL;
    v->video_codec = codec;
    VkShaderModule shader = VK_NULL_HANDLE;
    v->library = dlopen("libvulkan.so.1", RTLD_NOW | RTLD_LOCAL);
    if (!v->library) {
        snprintf(error, capacity, "libvulkan.so.1 is unavailable");
        goto failed;
    }
    v->get = (void *)dlsym(v->library, "vkGetInstanceProcAddr");
    if (!v->get)
        goto failed;
    PFN_vkCreateInstance create = (void *)v->get(NULL, "vkCreateInstance");
    VkApplicationInfo application = {
        .sType = VK_STRUCTURE_TYPE_APPLICATION_INFO, .pApplicationName = "AnyKVM", .apiVersion = VK_API_VERSION_1_3};
    VkInstanceCreateInfo instance = {.sType = VK_STRUCTURE_TYPE_INSTANCE_CREATE_INFO, .pApplicationInfo = &application};
    TRY(create(&instance, NULL, &v->instance));
#define LOAD(name) v->name = (void *)v->get(v->instance, "vk" #name);
    FUNCTIONS(LOAD)
#undef LOAD
    uint32_t n = 0;
    TRY(v->EnumeratePhysicalDevices(v->instance, &n, NULL));
    if (!n || n > 64) {
        snprintf(error, capacity, "Vulkan found no physical GPU");
        goto failed;
    }
    VkPhysicalDevice devices[64];
    TRY(v->EnumeratePhysicalDevices(v->instance, &n, devices));
    const char *required[] = {
        VK_EXT_PHYSICAL_DEVICE_DRM_EXTENSION_NAME,     VK_KHR_EXTERNAL_MEMORY_FD_EXTENSION_NAME,
        VK_EXT_EXTERNAL_MEMORY_DMA_BUF_EXTENSION_NAME, VK_EXT_IMAGE_DRM_FORMAT_MODIFIER_EXTENSION_NAME,
        VK_EXT_QUEUE_FAMILY_FOREIGN_EXTENSION_NAME,    VK_KHR_EXTERNAL_FENCE_FD_EXTENSION_NAME};
    const char *video_extensions[] = {VK_KHR_VIDEO_QUEUE_EXTENSION_NAME, VK_KHR_VIDEO_ENCODE_QUEUE_EXTENSION_NAME,
                                      codec == 1   ? VK_KHR_VIDEO_ENCODE_H265_EXTENSION_NAME
                                      : codec == 2 ? VK_KHR_VIDEO_ENCODE_AV1_EXTENSION_NAME
                                                   : VK_KHR_VIDEO_ENCODE_H264_EXTENSION_NAME};
    // Try exact matches first, regardless of Vulkan enumeration order. Only
    // compute consumers may use the split vc4/v3d display/render topology.
    for (int rank = 1; rank <= 2 && !v->physical; rank++) {
        for (uint32_t i = 0; i < n; i++) {
            if (video && !extensions(v, devices[i], video_extensions, 3))
                continue;
            if (video) {
                VkPhysicalDeviceVideoEncodeAV1FeaturesKHR av1 = {
                    .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_VIDEO_ENCODE_AV1_FEATURES_KHR};
                VkPhysicalDeviceVulkan13Features sync = {.sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_VULKAN_1_3_FEATURES,
                    .pNext = codec == 2 ? &av1 : NULL};
                VkPhysicalDeviceVulkan11Features yuv = {.sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_VULKAN_1_1_FEATURES, .pNext = &sync};
                VkPhysicalDeviceFeatures2 features = {.sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_FEATURES_2, .pNext = &yuv};
                v->GetPhysicalDeviceFeatures2(devices[i], &features);
                if (!sync.synchronization2 || !yuv.samplerYcbcrConversion || (codec == 2 && !av1.videoEncodeAV1))
                    continue;
            }
            if (!decode && !extensions(v, devices[i], required, sizeof(required) / sizeof(*required)))
                continue;
            VkPhysicalDeviceDrmPropertiesEXT drm = {.sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_DRM_PROPERTIES_EXT};
            VkPhysicalDeviceProperties2 properties = {.sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_PROPERTIES_2,
                                                      .pNext = &drm};
            v->GetPhysicalDeviceProperties2(devices[i], &properties);
            if ((jpeg || decode) && (properties.properties.limits.maxPerStageDescriptorStorageBuffers < 6 ||
                                     properties.properties.limits.maxDescriptorSetStorageBuffers < 6))
                continue;
            if (properties.properties.apiVersion < VK_API_VERSION_1_3 ||
                (decode && properties.properties.deviceType == VK_PHYSICAL_DEVICE_TYPE_CPU) ||
                (decode ? rank != 1 : bk_drm_match(major, minor, &drm, &properties.properties, !video) != rank))
                continue;
            uint32_t families = 0;
            v->GetPhysicalDeviceQueueFamilyProperties2(devices[i], &families, NULL);
            if (!families || families > 64)
                continue;
            VkQueueFamilyProperties2 queues[64] = {0};
            VkQueueFamilyVideoPropertiesKHR codecs[64] = {0};
            for (uint32_t j = 0; j < families; j++) {
                queues[j].sType = VK_STRUCTURE_TYPE_QUEUE_FAMILY_PROPERTIES_2;
                codecs[j].sType = VK_STRUCTURE_TYPE_QUEUE_FAMILY_VIDEO_PROPERTIES_KHR;
                if (video)
                    queues[j].pNext = &codecs[j];
            }
            v->GetPhysicalDeviceQueueFamilyProperties2(devices[i], &families, queues);
            uint32_t vf = UINT32_MAX;
            for (uint32_t j = 0; j < families; j++)
                if ((queues[j].queueFamilyProperties.queueFlags & VK_QUEUE_VIDEO_ENCODE_BIT_KHR) &&
                    (codecs[j].videoCodecOperations & operation)) {
                    vf = j;
                    break;
                }
            if (video && vf == UINT32_MAX)
                continue;
            v->video_family = video ? vf : UINT32_MAX;
            for (uint32_t j = 0; j < families; j++)
                if (queues[j].queueFamilyProperties.queueFlags & VK_QUEUE_COMPUTE_BIT) {
                    v->physical = devices[i];
                    v->family = j;
                    v->major = drm.hasPrimary ? drm.primaryMajor : drm.renderMajor;
                    v->minor = drm.hasPrimary ? drm.primaryMinor : drm.renderMinor;
                    break;
                }
            if (v->physical)
                break;
        }
    }
    if (!v->physical) {
        if (decode)
            snprintf(error, capacity, "no hardware Vulkan 1.3 GPU supports %s",
                     mode == BK_VK_PROGRESSIVE ? "Progressive compute encoding" : "JPEG compute decoding");
        else
            snprintf(error, capacity,
                     "no Vulkan 1.3 GPU associated with DRM %u:%u supports DMA-BUF/modifier/foreign-queue/sync-file interop",
                     major, minor);
        goto failed;
    }
    // Capability negotiation needs only instance/physical-device declarations.
    // Do not create a device, shader pipeline or video session for this query.
    if (query) return v;
    float priority = 1;
    VkDeviceQueueCreateInfo queues[2] = {{.sType = VK_STRUCTURE_TYPE_DEVICE_QUEUE_CREATE_INFO,
                                          .queueFamilyIndex = v->family,
                                          .queueCount = 1,
                                          .pQueuePriorities = &priority},
                                         {.sType = VK_STRUCTURE_TYPE_DEVICE_QUEUE_CREATE_INFO,
                                          .queueFamilyIndex = v->video_family,
                                          .queueCount = 1,
                                          .pQueuePriorities = &priority}};
    const char *enabled[16];
    uint32_t extension_count = 0;
    for (unsigned i = 0; !decode && i < sizeof(required) / sizeof(*required); i++)
        enabled[extension_count++] = required[i];
    if (video)
        for (int i = 0; i < 3; i++)
            enabled[extension_count++] = video_extensions[i];
    VkPhysicalDeviceVideoEncodeAV1FeaturesKHR av1_features = {
        .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_VIDEO_ENCODE_AV1_FEATURES_KHR, .videoEncodeAV1 = VK_TRUE};
    VkPhysicalDeviceVulkan13Features features = {.sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_VULKAN_1_3_FEATURES,
                                                 .synchronization2 = video,
                                                 .pNext = video && codec == 2 ? &av1_features : NULL};
    VkPhysicalDeviceVulkan11Features features11 = {.sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_VULKAN_1_1_FEATURES,
                                                   .pNext = &features,
                                                   .samplerYcbcrConversion = video};
    VkDeviceCreateInfo device = {.sType = VK_STRUCTURE_TYPE_DEVICE_CREATE_INFO,
                                 .queueCreateInfoCount = video && v->video_family != v->family ? 2 : 1,
                                 .pQueueCreateInfos = queues,
                                 .pNext = video ? &features11 : NULL,
                                 .enabledExtensionCount = extension_count,
                                 .ppEnabledExtensionNames = enabled};
    TRY(v->CreateDevice(v->physical, &device, NULL, &v->device));
    v->GetDeviceQueue(v->device, v->family, 0, &v->queue);
    v->GetPhysicalDeviceMemoryProperties(v->physical, &v->memory);
    if (video)
        v->GetDeviceQueue(v->device, v->video_family, 0, &v->video_queue);
    VkPhysicalDeviceExternalFenceInfo fi = {.sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_EXTERNAL_FENCE_INFO,
                                            .handleType = VK_EXTERNAL_FENCE_HANDLE_TYPE_SYNC_FD_BIT};
    VkExternalFenceProperties fp = {.sType = VK_STRUCTURE_TYPE_EXTERNAL_FENCE_PROPERTIES};
    v->GetPhysicalDeviceExternalFenceProperties(v->physical, &fi, &fp);
    if (!decode && !(fp.externalFenceFeatures & VK_EXTERNAL_FENCE_FEATURE_EXPORTABLE_BIT)) {
        snprintf(error, capacity, "Vulkan GPU cannot export sync-file fences");
        goto failed;
    }
    VkDescriptorType descriptor_types[] = {VK_DESCRIPTOR_TYPE_SAMPLED_IMAGE, VK_DESCRIPTOR_TYPE_SAMPLER,
                                           VK_DESCRIPTOR_TYPE_STORAGE_BUFFER, VK_DESCRIPTOR_TYPE_UNIFORM_BUFFER};
    VkDescriptorType jpeg_types[] = {VK_DESCRIPTOR_TYPE_SAMPLED_IMAGE,  VK_DESCRIPTOR_TYPE_STORAGE_BUFFER,
                                     VK_DESCRIPTOR_TYPE_UNIFORM_BUFFER, VK_DESCRIPTOR_TYPE_STORAGE_BUFFER,
                                     VK_DESCRIPTOR_TYPE_STORAGE_BUFFER, VK_DESCRIPTOR_TYPE_STORAGE_BUFFER,
                                     VK_DESCRIPTOR_TYPE_STORAGE_BUFFER, VK_DESCRIPTOR_TYPE_STORAGE_BUFFER,
                                     VK_DESCRIPTOR_TYPE_STORAGE_BUFFER, VK_DESCRIPTOR_TYPE_STORAGE_BUFFER};
    uint32_t binding_count = decode ? 6 : jpeg ? 10 : 4;
    VkDescriptorSetLayoutBinding bindings[10] = {0};
    VkDescriptorPoolSize sizes[10] = {0};
    for (uint32_t i = 0; i < binding_count; i++) {
        VkDescriptorType type = decode ? VK_DESCRIPTOR_TYPE_STORAGE_BUFFER : jpeg ? jpeg_types[i] : descriptor_types[i];
        bindings[i] = (VkDescriptorSetLayoutBinding){
            .binding = i, .descriptorType = type, .descriptorCount = 1, .stageFlags = VK_SHADER_STAGE_COMPUTE_BIT};
        sizes[i] = (VkDescriptorPoolSize){type, 1};
    }
    VkDescriptorSetLayoutCreateInfo dl = {.sType = VK_STRUCTURE_TYPE_DESCRIPTOR_SET_LAYOUT_CREATE_INFO,
                                          .bindingCount = binding_count,
                                          .pBindings = bindings};
    TRY(v->CreateDescriptorSetLayout(v->device, &dl, NULL, &v->descriptor_layout));
    VkPipelineLayoutCreateInfo pl = {.sType = VK_STRUCTURE_TYPE_PIPELINE_LAYOUT_CREATE_INFO,
                                     .setLayoutCount = 1,
                                     .pSetLayouts = &v->descriptor_layout};
    TRY(v->CreatePipelineLayout(v->device, &pl, NULL, &v->layout));
    if (!size || size % 4) {
        snprintf(error, capacity, "invalid embedded compute SPIR-V");
        goto failed;
    }
    uint32_t *aligned = malloc(size);
    if (!aligned)
        goto failed;
    memcpy(aligned, spirv, size);
    VkShaderModuleCreateInfo sm = {
        .sType = VK_STRUCTURE_TYPE_SHADER_MODULE_CREATE_INFO, .codeSize = size, .pCode = aligned};
    VkResult code = v->CreateShaderModule(v->device, &sm, NULL, &shader);
    free(aligned);
    if (code != VK_SUCCESS) {
        vk_error(error, capacity, "CreateShaderModule", code);
        goto failed;
    }
    VkComputePipelineCreateInfo cp = {.sType = VK_STRUCTURE_TYPE_COMPUTE_PIPELINE_CREATE_INFO,
                                      .layout = v->layout,
                                      .stage = {.sType = VK_STRUCTURE_TYPE_PIPELINE_SHADER_STAGE_CREATE_INFO,
                                                .stage = VK_SHADER_STAGE_COMPUTE_BIT,
                                                .module = shader,
                                                .pName = "main"}};
    TRY(v->CreateComputePipelines(v->device, VK_NULL_HANDLE, 1, &cp, NULL, &v->pipeline));
    v->DestroyShaderModule(v->device, shader, NULL);
    shader = VK_NULL_HANDLE;
    VkDescriptorPoolCreateInfo pool = {
        .sType = VK_STRUCTURE_TYPE_DESCRIPTOR_POOL_CREATE_INFO, .maxSets = 1, .poolSizeCount = binding_count, .pPoolSizes = sizes};
    TRY(v->CreateDescriptorPool(v->device, &pool, NULL, &v->descriptor_pool));
    VkDescriptorSetAllocateInfo ds = {.sType = VK_STRUCTURE_TYPE_DESCRIPTOR_SET_ALLOCATE_INFO,
                                      .descriptorPool = v->descriptor_pool,
                                      .descriptorSetCount = 1,
                                      .pSetLayouts = &v->descriptor_layout};
    TRY(v->AllocateDescriptorSets(v->device, &ds, &v->descriptor));
    VkSamplerCreateInfo sampler = {.sType = VK_STRUCTURE_TYPE_SAMPLER_CREATE_INFO,
                                   .magFilter = VK_FILTER_LINEAR,
                                   .minFilter = VK_FILTER_LINEAR,
                                   .addressModeU = VK_SAMPLER_ADDRESS_MODE_CLAMP_TO_EDGE,
                                   .addressModeV = VK_SAMPLER_ADDRESS_MODE_CLAMP_TO_EDGE,
                                   .addressModeW = VK_SAMPLER_ADDRESS_MODE_CLAMP_TO_EDGE,
                                   .maxLod = 0};
    TRY(v->CreateSampler(v->device, &sampler, NULL, &v->sampler));
    VkCommandPoolCreateInfo pc = {.sType = VK_STRUCTURE_TYPE_COMMAND_POOL_CREATE_INFO, .queueFamilyIndex = v->family};
    TRY(v->CreateCommandPool(v->device, &pc, NULL, &v->commands));
    VkCommandBufferAllocateInfo cb = {.sType = VK_STRUCTURE_TYPE_COMMAND_BUFFER_ALLOCATE_INFO,
                                      .commandPool = v->commands,
                                      .level = VK_COMMAND_BUFFER_LEVEL_PRIMARY,
                                      .commandBufferCount = 1};
    TRY(v->AllocateCommandBuffers(v->device, &cb, &v->command));
    return v;
failed:
    if (shader && v->device)
        v->DestroyShaderModule(v->device, shader, NULL);
    bk_vk_close(v);
    return NULL;
}
void bk_vk_drm_device(bk_vk *v, uint32_t *major, uint32_t *minor) {
    *major = v->major;
    *minor = v->minor;
}
static void source_free(bk_vk *v, source *s) {
    if (s->view)
        v->DestroyImageView(v->device, s->view, NULL);
    if (s->image)
        v->DestroyImage(v->device, s->image, NULL);
    if (s->memory)
        v->FreeMemory(v->device, s->memory, NULL);
}
static int source_import(bk_vk *v, const bk_vk_source *f, source *s, char *error, size_t capacity) {
    int fd = -1;
    VkFormat format;
    switch (f->fourcc) {
    case 0x34325258:
    case 0x34325241:
        format = VK_FORMAT_B8G8R8A8_UNORM;
        break;
    case 0x34324258:
    case 0x34324241:
        format = VK_FORMAT_R8G8B8A8_UNORM;
        break;
    default:
        snprintf(error, capacity, "unsupported Vulkan DMA-BUF RGB format 0x%08x", f->fourcc);
        return -1;
    }
    VkPhysicalDeviceExternalImageFormatInfo external = {
        .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_EXTERNAL_IMAGE_FORMAT_INFO,
        .handleType = VK_EXTERNAL_MEMORY_HANDLE_TYPE_DMA_BUF_BIT_EXT};
    VkPhysicalDeviceImageDrmFormatModifierInfoEXT modifier = {
        .sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_IMAGE_DRM_FORMAT_MODIFIER_INFO_EXT,
        .pNext = &external,
        .drmFormatModifier = f->modifier,
        .sharingMode = VK_SHARING_MODE_EXCLUSIVE};
    VkPhysicalDeviceImageFormatInfo2 query = {.sType = VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_IMAGE_FORMAT_INFO_2,
                                              .pNext = &modifier,
                                              .format = format,
                                              .type = VK_IMAGE_TYPE_2D,
                                              .tiling = VK_IMAGE_TILING_DRM_FORMAT_MODIFIER_EXT,
                                              .usage = VK_IMAGE_USAGE_SAMPLED_BIT};
    VkExternalImageFormatProperties properties = {.sType = VK_STRUCTURE_TYPE_EXTERNAL_IMAGE_FORMAT_PROPERTIES};
    VkImageFormatProperties2 result = {.sType = VK_STRUCTURE_TYPE_IMAGE_FORMAT_PROPERTIES_2, .pNext = &properties};
    TRY(v->GetPhysicalDeviceImageFormatProperties2(v->physical, &query, &result));
    if (!(properties.externalMemoryProperties.externalMemoryFeatures & VK_EXTERNAL_MEMORY_FEATURE_IMPORTABLE_BIT)) {
        snprintf(error, capacity, "DRM framebuffer modifier is not Vulkan-importable");
        goto failed;
    }
    uint32_t planes = f->planes ? f->planes : 1;
    if (planes > 4) {
        snprintf(error, capacity, "invalid DRM modifier plane count");
        goto failed;
    }
    VkSubresourceLayout plane[4] = {0};
    for (uint32_t i = 0; i < planes; i++) {
        plane[i].offset = f->planes ? f->offsets[i] : f->offset;
        plane[i].rowPitch = f->planes ? f->pitches[i] : f->pitch;
    }
    VkExternalMemoryImageCreateInfo sharing = {.sType = VK_STRUCTURE_TYPE_EXTERNAL_MEMORY_IMAGE_CREATE_INFO,
                                               .handleTypes = external.handleType};
    VkImageDrmFormatModifierExplicitCreateInfoEXT layout = {
        .sType = VK_STRUCTURE_TYPE_IMAGE_DRM_FORMAT_MODIFIER_EXPLICIT_CREATE_INFO_EXT,
        .pNext = &sharing,
        .drmFormatModifier = f->modifier,
        .drmFormatModifierPlaneCount = planes,
        .pPlaneLayouts = plane};
    VkImageCreateInfo image = {.sType = VK_STRUCTURE_TYPE_IMAGE_CREATE_INFO,
                               .pNext = &layout,
                               .imageType = VK_IMAGE_TYPE_2D,
                               .format = format,
                               .extent = {f->width, f->height, 1},
                               .mipLevels = 1,
                               .arrayLayers = 1,
                               .samples = VK_SAMPLE_COUNT_1_BIT,
                               .tiling = VK_IMAGE_TILING_DRM_FORMAT_MODIFIER_EXT,
                               .usage = VK_IMAGE_USAGE_SAMPLED_BIT,
                               .sharingMode = VK_SHARING_MODE_EXCLUSIVE,
                               .initialLayout = VK_IMAGE_LAYOUT_UNDEFINED};
    TRY(v->CreateImage(v->device, &image, NULL, &s->image));
    VkMemoryRequirements req;
    v->GetImageMemoryRequirements(v->device, s->image, &req);
    VkMemoryFdPropertiesKHR fd_properties = {.sType = VK_STRUCTURE_TYPE_MEMORY_FD_PROPERTIES_KHR};
    TRY(v->GetMemoryFdPropertiesKHR(v->device, external.handleType, f->fd, &fd_properties));
    uint32_t type = memory_type(v, req.memoryTypeBits & fd_properties.memoryTypeBits, 0);
    if (type == UINT32_MAX || (f->size && f->size < req.size)) {
        snprintf(error, capacity, "incompatible Vulkan DMA-BUF allocation");
        goto failed;
    }
    fd = fcntl(f->fd, F_DUPFD_CLOEXEC, 0);
    if (fd < 0)
        goto failed;
    VkMemoryDedicatedAllocateInfo dedicated = {.sType = VK_STRUCTURE_TYPE_MEMORY_DEDICATED_ALLOCATE_INFO,
                                               .image = s->image};
    VkImportMemoryFdInfoKHR import = {.sType = VK_STRUCTURE_TYPE_IMPORT_MEMORY_FD_INFO_KHR,
                                      .pNext = &dedicated,
                                      .handleType = external.handleType,
                                      .fd = fd};
    VkMemoryAllocateInfo alloc = {.sType = VK_STRUCTURE_TYPE_MEMORY_ALLOCATE_INFO,
                                  .pNext = &import,
                                  .allocationSize = f->size ? f->size : req.size,
                                  .memoryTypeIndex = type};
    TRY(v->AllocateMemory(v->device, &alloc, NULL, &s->memory));
    fd = -1;
    TRY(v->BindImageMemory(v->device, s->image, s->memory, 0));
    VkImageViewCreateInfo view = {.sType = VK_STRUCTURE_TYPE_IMAGE_VIEW_CREATE_INFO,
                                  .image = s->image,
                                  .viewType = VK_IMAGE_VIEW_TYPE_2D,
                                  .format = format,
                                  .subresourceRange = {VK_IMAGE_ASPECT_COLOR_BIT, 0, 1, 0, 1}};
    TRY(v->CreateImageView(v->device, &view, NULL, &s->view));
    return 0;
failed:
    if (fd >= 0)
        close(fd);
    return -1;
}
static bk_vk_image *convert(bk_vk *v, const bk_vk_source *f, int width, int height, int visible_width,
                            int visible_height, int nv12, char *error, size_t capacity) {
    bk_vk_image *out = calloc(1, sizeof(*out));
    source input = {0};
    buffer parameters = {0};
    if (!out)
        return NULL;
    out->context = v;
    out->width = width;
    out->height = height;
    out->nv12 = nv12;
    out->pitch = nv12 ? ((width + 63) & ~63) : width * 4;
    struct dma_buf_export_sync_file sync = {.flags = DMA_BUF_SYNC_READ, .fd = -1};
    if (ioctl(f->fd, DMA_BUF_IOCTL_EXPORT_SYNC_FILE, &sync) < 0) {
        snprintf(error, capacity, "DMA-BUF write-fence export: %s", strerror(errno));
        goto failed;
    }
    int waited = wait_fd(sync.fd, 1000);
    if (sync.fd >= 0)
        close(sync.fd);
    if (waited < 0) {
        snprintf(error, capacity, "DMA-BUF producer fence timed out");
        goto failed;
    }
    if (source_import(v, f, &input, error, capacity) < 0)
        goto failed;
    if (buffer_new(v, &out->output, (VkDeviceSize)out->pitch * height * (nv12 ? 3 : 2) / 2,
                   VK_BUFFER_USAGE_STORAGE_BUFFER_BIT | VK_BUFFER_USAGE_TRANSFER_SRC_BIT, 0, error, capacity) < 0)
        goto failed;
    uint32_t p[16] = {f->width,      f->height,      width,       height, f->x,          f->y,
                      f->crop_width, f->crop_height, 0,           0,      visible_width, visible_height,
                      out->pitch,    nv12,           f->rotation, f->flip_y};
    uint32_t sw = f->rotation == 90 || f->rotation == 270 ? f->crop_height : f->crop_width,
             sh = f->rotation == 90 || f->rotation == 270 ? f->crop_width : f->crop_height;
    if (!sw || !sh) {
        snprintf(error, capacity, "empty Vulkan source crop");
        goto failed;
    }
    if ((uint64_t)sw * visible_height > (uint64_t)sh * visible_width) {
        p[11] = (uint64_t)visible_width * sh / sw;
        if (nv12)
            p[11] &= ~1u;
        p[9] = (visible_height - p[11]) / 2;
        if (nv12)
            p[9] &= ~1u;
    } else {
        p[10] = (uint64_t)visible_height * sw / sh;
        if (nv12)
            p[10] &= ~1u;
        p[8] = (visible_width - p[10]) / 2;
        if (nv12)
            p[8] &= ~1u;
    }
    if (!p[10] || !p[11]) {
        snprintf(error, capacity, "Vulkan aspect fit is empty");
        goto failed;
    }
    if (buffer_new(v, &parameters, sizeof(p), VK_BUFFER_USAGE_UNIFORM_BUFFER_BIT, 1, error, capacity) < 0)
        goto failed;
    memcpy(parameters.mapped, p, sizeof(p));
    if (!parameters.coherent) {
        VkMappedMemoryRange range = {
            .sType = VK_STRUCTURE_TYPE_MAPPED_MEMORY_RANGE, .memory = parameters.memory, .size = VK_WHOLE_SIZE};
        TRY(v->FlushMappedMemoryRanges(v->device, 1, &range));
    }
    VkDescriptorImageInfo image = {.imageView = input.view, .imageLayout = VK_IMAGE_LAYOUT_SHADER_READ_ONLY_OPTIMAL},
                          sampler = {.sampler = v->sampler};
    VkDescriptorBufferInfo destination = {.buffer = out->output.buffer, .range = out->output.size},
                           uniform = {.buffer = parameters.buffer, .range = sizeof(p)};
    VkWriteDescriptorSet writes[4] = {{.sType = VK_STRUCTURE_TYPE_WRITE_DESCRIPTOR_SET,
                                       .dstSet = v->descriptor,
                                       .dstBinding = 0,
                                       .descriptorCount = 1,
                                       .descriptorType = VK_DESCRIPTOR_TYPE_SAMPLED_IMAGE,
                                       .pImageInfo = &image},
                                      {.sType = VK_STRUCTURE_TYPE_WRITE_DESCRIPTOR_SET,
                                       .dstSet = v->descriptor,
                                       .dstBinding = 1,
                                       .descriptorCount = 1,
                                       .descriptorType = VK_DESCRIPTOR_TYPE_SAMPLER,
                                       .pImageInfo = &sampler},
                                      {.sType = VK_STRUCTURE_TYPE_WRITE_DESCRIPTOR_SET,
                                       .dstSet = v->descriptor,
                                       .dstBinding = 2,
                                       .descriptorCount = 1,
                                       .descriptorType = VK_DESCRIPTOR_TYPE_STORAGE_BUFFER,
                                       .pBufferInfo = &destination},
                                      {.sType = VK_STRUCTURE_TYPE_WRITE_DESCRIPTOR_SET,
                                       .dstSet = v->descriptor,
                                       .dstBinding = 3,
                                       .descriptorCount = 1,
                                       .descriptorType = VK_DESCRIPTOR_TYPE_UNIFORM_BUFFER,
                                       .pBufferInfo = &uniform}};
    v->UpdateDescriptorSets(v->device, 4, writes, 0, NULL);
    if (begin(v, error, capacity) < 0)
        goto failed;
    VkImageMemoryBarrier acquire = {.sType = VK_STRUCTURE_TYPE_IMAGE_MEMORY_BARRIER,
                                    .srcAccessMask = VK_ACCESS_MEMORY_WRITE_BIT,
                                    .dstAccessMask = VK_ACCESS_SHADER_READ_BIT,
                                    .oldLayout = VK_IMAGE_LAYOUT_GENERAL,
                                    .newLayout = VK_IMAGE_LAYOUT_SHADER_READ_ONLY_OPTIMAL,
                                    .srcQueueFamilyIndex = VK_QUEUE_FAMILY_FOREIGN_EXT,
                                    .dstQueueFamilyIndex = v->family,
                                    .image = input.image,
                                    .subresourceRange = {VK_IMAGE_ASPECT_COLOR_BIT, 0, 1, 0, 1}};
    v->CmdPipelineBarrier(v->command, VK_PIPELINE_STAGE_ALL_COMMANDS_BIT, VK_PIPELINE_STAGE_COMPUTE_SHADER_BIT, 0, 0,
                          NULL, 0, NULL, 1, &acquire);
    v->CmdBindPipeline(v->command, VK_PIPELINE_BIND_POINT_COMPUTE, v->pipeline);
    v->CmdBindDescriptorSets(v->command, VK_PIPELINE_BIND_POINT_COMPUTE, v->layout, 0, 1, &v->descriptor, 0, NULL);
    v->CmdDispatch(v->command, (width + 31) / 32, (height + 15) / 16, 1);
    VkImageMemoryBarrier release = acquire;
    release.srcAccessMask = VK_ACCESS_SHADER_READ_BIT;
    release.dstAccessMask = 0;
    release.oldLayout = VK_IMAGE_LAYOUT_SHADER_READ_ONLY_OPTIMAL;
    release.newLayout = VK_IMAGE_LAYOUT_GENERAL;
    release.srcQueueFamilyIndex = v->family;
    release.dstQueueFamilyIndex = VK_QUEUE_FAMILY_FOREIGN_EXT;
    VkBufferMemoryBarrier ready = {.sType = VK_STRUCTURE_TYPE_BUFFER_MEMORY_BARRIER,
                                   .srcAccessMask = VK_ACCESS_SHADER_WRITE_BIT,
                                   .dstAccessMask = VK_ACCESS_MEMORY_READ_BIT,
                                   .srcQueueFamilyIndex = VK_QUEUE_FAMILY_IGNORED,
                                   .dstQueueFamilyIndex = VK_QUEUE_FAMILY_IGNORED,
                                   .buffer = out->output.buffer,
                                   .size = VK_WHOLE_SIZE};
    v->CmdPipelineBarrier(v->command, VK_PIPELINE_STAGE_COMPUTE_SHADER_BIT, VK_PIPELINE_STAGE_ALL_COMMANDS_BIT, 0, 0,
                          NULL, 1, &ready, 1, &release);
    if (submit(v, f->fd, error, capacity) < 0)
        goto failed;
    source_free(v, &input);
    buffer_free(v, &parameters);
    return out;
failed:
    source_free(v, &input);
    buffer_free(v, &parameters);
    bk_vk_image_free(out);
    return NULL;
}

void bk_vk_image_free(bk_vk_image *i) {
    if (!i)
        return;
    buffer_free(i->context, &i->staging);
    buffer_free(i->context, &i->output);
    free(i);
}

uint32_t bk_vk_image_pitch(bk_vk_image *i) {
    return i->pitch;
}

int bk_vk_readback(bk_vk_image *i, void **data, size_t *size, char *error, size_t capacity) {
    bk_vk *v = i->context;
    if (!i->readback_ready) {
        if (!i->staging.buffer &&
            buffer_new(v, &i->staging, i->output.size, VK_BUFFER_USAGE_TRANSFER_DST_BIT, 1, error, capacity) < 0)
            return -1;
        if (begin(v, error, capacity) < 0)
            return -1;
        VkBufferCopy copy = {.size = i->output.size};
        v->CmdCopyBuffer(v->command, i->output.buffer, i->staging.buffer, 1, &copy);
        VkBufferMemoryBarrier ready = {.sType = VK_STRUCTURE_TYPE_BUFFER_MEMORY_BARRIER,
                                       .srcAccessMask = VK_ACCESS_TRANSFER_WRITE_BIT,
                                       .dstAccessMask = VK_ACCESS_HOST_READ_BIT,
                                       .srcQueueFamilyIndex = VK_QUEUE_FAMILY_IGNORED,
                                       .dstQueueFamilyIndex = VK_QUEUE_FAMILY_IGNORED,
                                       .buffer = i->staging.buffer,
                                       .size = VK_WHOLE_SIZE};
        v->CmdPipelineBarrier(v->command, VK_PIPELINE_STAGE_TRANSFER_BIT, VK_PIPELINE_STAGE_HOST_BIT, 0,
                              0, NULL, 1, &ready, 0, NULL);
        if (submit(v, -1, error, capacity) < 0)
            return -1;
        if (!i->staging.coherent) {
            VkMappedMemoryRange range = {.sType = VK_STRUCTURE_TYPE_MAPPED_MEMORY_RANGE,
                                         .memory = i->staging.memory, .size = VK_WHOLE_SIZE};
            TRY(v->InvalidateMappedMemoryRanges(v->device, 1, &range));
        }
        i->readback_ready = 1;
    }
    *data = i->staging.mapped;
    *size = i->output.size;
    return 0;
failed:
    return -1;
}

bk_vk_image *bk_vk_convert(bk_vk *v, const bk_vk_source *f, int width, int height, int nv12, char *error,
                           size_t capacity) {
    return convert(v, f, width, height, width, height, nv12, error, capacity);
}
#include "jpeg_linux.inc"
#include "video_linux.inc"

#include "jpeg_decode_linux.inc"
#include "progressive_linux.inc"
