// Exercise buffer reuse and failed growth without a Vulkan driver.
#include "../native_linux.c"

static unsigned created, destroyed, allocated, released, unmapped;
static int fail_allocation;

static VKAPI_ATTR VkResult VKAPI_CALL create_buffer(VkDevice device, const VkBufferCreateInfo *info,
    const VkAllocationCallbacks *allocator, VkBuffer *handle) {
    (void)device; (void)info; (void)allocator;
    *handle = (VkBuffer)(uintptr_t)++created;
    return VK_SUCCESS;
}
static VKAPI_ATTR void VKAPI_CALL destroy_buffer(VkDevice device, VkBuffer handle, const VkAllocationCallbacks *allocator) {
    (void)device; (void)handle; (void)allocator;
    destroyed++;
}
static VKAPI_ATTR void VKAPI_CALL requirements(VkDevice device, VkBuffer handle, VkMemoryRequirements *req) {
    (void)device; (void)handle;
    *req = (VkMemoryRequirements){.size = 4096, .alignment = 4, .memoryTypeBits = 1};
}
static VKAPI_ATTR VkResult VKAPI_CALL allocate_memory(VkDevice device, const VkMemoryAllocateInfo *info,
    const VkAllocationCallbacks *allocator, VkDeviceMemory *memory) {
    (void)device; (void)allocator;
    if (fail_allocation) return VK_ERROR_OUT_OF_DEVICE_MEMORY;
    *memory = (VkDeviceMemory)(uintptr_t)malloc(info->allocationSize);
    if (!*memory) return VK_ERROR_OUT_OF_HOST_MEMORY;
    allocated++;
    return VK_SUCCESS;
}
static VKAPI_ATTR void VKAPI_CALL free_memory(VkDevice device, VkDeviceMemory memory, const VkAllocationCallbacks *allocator) {
    (void)device; (void)allocator;
    free((void *)(uintptr_t)memory);
    released++;
}
static VKAPI_ATTR VkResult VKAPI_CALL bind_memory(VkDevice device, VkBuffer handle, VkDeviceMemory memory, VkDeviceSize offset) {
    (void)device; (void)handle; (void)memory; (void)offset;
    return VK_SUCCESS;
}
static VKAPI_ATTR VkResult VKAPI_CALL map_memory(VkDevice device, VkDeviceMemory memory, VkDeviceSize offset,
    VkDeviceSize size, VkMemoryMapFlags flags, void **mapped) {
    (void)device; (void)offset; (void)size; (void)flags;
    *mapped = (void *)(uintptr_t)memory;
    return VK_SUCCESS;
}
static VKAPI_ATTR void VKAPI_CALL unmap_memory(VkDevice device, VkDeviceMemory memory) {
    (void)device; (void)memory;
    unmapped++;
}
#define CHECK(condition) do { if (!(condition)) { fprintf(stderr, "check failed at line %d\n", __LINE__); return 1; } } while (0)

int main(void) {
    bk_vk context = {
        .CreateBuffer = create_buffer, .DestroyBuffer = destroy_buffer,
        .GetBufferMemoryRequirements = requirements, .AllocateMemory = allocate_memory,
        .FreeMemory = free_memory, .BindBufferMemory = bind_memory,
        .MapMemory = map_memory, .UnmapMemory = unmap_memory
    };
    context.memory.memoryTypeCount = 1;
    context.memory.memoryTypes[0].propertyFlags = VK_MEMORY_PROPERTY_DEVICE_LOCAL_BIT |
        VK_MEMORY_PROPERTY_HOST_VISIBLE_BIT | VK_MEMORY_PROPERTY_HOST_COHERENT_BIT;
    context.jpeg_buffers = calloc(1, sizeof(*context.jpeg_buffers));
    CHECK(context.jpeg_buffers);
    buffer *b = &context.jpeg_buffers->items[JPEG_PARAMETERS];
    char error[256] = {0};
    VkBufferUsageFlags usage = VK_BUFFER_USAGE_UNIFORM_BUFFER_BIT;
    CHECK(jpeg_buffer_reserve(&context, b, 512, usage, 1, error, sizeof(error)) == 0);
    VkBuffer first = b->buffer;
    void *mapped = b->mapped;
    memset(mapped, 0x5a, 512);
    CHECK(jpeg_buffer_reserve(&context, b, 256, usage, 1, error, sizeof(error)) == 0);
    CHECK(jpeg_buffer_reserve(&context, b, 512, usage, 1, error, sizeof(error)) == 0);
    CHECK(b->buffer == first && b->mapped == mapped && created == 1 && allocated == 1);
    CHECK(((uint8_t *)mapped)[511] == 0x5a);
    fail_allocation = 1;
    CHECK(jpeg_buffer_reserve(&context, b, 1024, usage, 1, error, sizeof(error)) < 0);
    CHECK(b->buffer == first && b->mapped == mapped && b->size == 512);
    CHECK(((uint8_t *)mapped)[511] == 0x5a);
    fail_allocation = 0;
    CHECK(jpeg_buffer_reserve(&context, b, 1024, usage, 1, error, sizeof(error)) == 0);
    CHECK(b->buffer != first && b->size == 1024 && unmapped == 1 && released == 1);
    b = &context.jpeg_buffers->items[JPEG_OUTPUT];
    CHECK(jpeg_buffer_reserve(&context, b, 2048, VK_BUFFER_USAGE_STORAGE_BUFFER_BIT, 0, error, sizeof(error)) == 0);
    CHECK(!b->mapped);
    jpeg_buffers_free(&context);
    jpeg_buffers_free(&context);
    CHECK(!context.jpeg_buffers && created == destroyed && allocated == released && unmapped == 2);
    return 0;
}
