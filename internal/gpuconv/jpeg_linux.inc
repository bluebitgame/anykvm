#include "jpeg_layout.h"

enum jpeg_buffer_index {
    JPEG_COEFFICIENTS,
    JPEG_SEGMENTS,
    JPEG_SEGMENT_INFO,
    JPEG_OUTPUT,
    JPEG_ARGUMENTS,
    JPEG_HEADER,
    JPEG_PARAMETERS,
    JPEG_LENGTH,
    JPEG_READBACK,
    JPEG_BUFFER_COUNT
};

struct jpeg_buffers {
    buffer items[JPEG_BUFFER_COUNT];
};

static void jpeg_buffers_free(bk_vk *v) {
    if (!v->jpeg_buffers) return;
    for (unsigned i = 0; i < JPEG_BUFFER_COUNT; i++) {
        buffer_free(v, &v->jpeg_buffers->items[i]);
    }
    free(v->jpeg_buffers);
    v->jpeg_buffers = NULL;
}

static int jpeg_buffer_reserve(bk_vk *v, buffer *b, VkDeviceSize size, VkBufferUsageFlags usage,
                               int host, char *error, size_t capacity) {
    if (b->buffer && b->size >= size) return 0;
    buffer replacement = {0};
    if (buffer_new(v, &replacement, size, usage, host, error, capacity) < 0) return -1;
    buffer_free(v, b);
    *b = replacement;
    return 0;
}

static int jpeg_create_pipeline(bk_vk *v, const void *spirv, size_t size, const char *entry,
                          VkPipeline *pipeline, char *error, size_t capacity) {
    VkShaderModule shader = VK_NULL_HANDLE;
    if (!spirv || !size || size % 4) {
        snprintf(error, capacity, "invalid JPEG SPIR-V");
        return -1;
    }
    uint32_t *aligned = malloc(size);
    if (!aligned) return -1;
    memcpy(aligned, spirv, size);
    VkShaderModuleCreateInfo module = {.sType = VK_STRUCTURE_TYPE_SHADER_MODULE_CREATE_INFO, .codeSize = size, .pCode = aligned};
    VkResult code = v->CreateShaderModule(v->device, &module, NULL, &shader);
    free(aligned);
    if (code != VK_SUCCESS) return vk_error(error, capacity, "JPEG shader module", code);
    VkComputePipelineCreateInfo info = {
        .sType = VK_STRUCTURE_TYPE_COMPUTE_PIPELINE_CREATE_INFO, .layout = v->layout,
        .stage = {.sType = VK_STRUCTURE_TYPE_PIPELINE_SHADER_STAGE_CREATE_INFO,
                  .stage = VK_SHADER_STAGE_COMPUTE_BIT, .module = shader, .pName = entry}
    };
    code = v->CreateComputePipelines(v->device, VK_NULL_HANDLE, 1, &info, NULL, pipeline);
    v->DestroyShaderModule(v->device, shader, NULL);
    return code == VK_SUCCESS ? 0 : vk_error(error, capacity, "JPEG compute pipeline", code);
}

int bk_vk_jpeg_init(bk_vk *v, const void *entropy, size_t entropy_size, const void *index, size_t index_size,
                    const void *pack, size_t pack_size, char *error, size_t capacity) {
    if (jpeg_create_pipeline(v, entropy, entropy_size, "encodeMCUs", &v->jpeg_entropy, error, capacity) < 0 ||
        jpeg_create_pipeline(v, index, index_size, "indexSegments", &v->jpeg_index, error, capacity) < 0 ||
        jpeg_create_pipeline(v, pack, pack_size, "packJPEG", &v->jpeg_pack, error, capacity) < 0)
        return -1;
    return 0;
}

static int flush_buffer(bk_vk *v, buffer *b, char *error, size_t capacity) {
    if (!b->coherent) {
        VkMappedMemoryRange range = {.sType = VK_STRUCTURE_TYPE_MAPPED_MEMORY_RANGE, .memory = b->memory, .size = VK_WHOLE_SIZE};
        TRY(v->FlushMappedMemoryRanges(v->device, 1, &range));
    }
    return 0;
failed:
    return -1;
}

static int invalidate_buffer(bk_vk *v, buffer *b, char *error, size_t capacity) {
    if (!b->coherent) {
        VkMappedMemoryRange range = {.sType = VK_STRUCTURE_TYPE_MAPPED_MEMORY_RANGE, .memory = b->memory, .size = VK_WHOLE_SIZE};
        TRY(v->InvalidateMappedMemoryRanges(v->device, 1, &range));
    }
    return 0;
failed:
    return -1;
}

static void jpeg_compute_barrier(bk_vk *v) {
    VkMemoryBarrier memory = {.sType = VK_STRUCTURE_TYPE_MEMORY_BARRIER,
        .srcAccessMask = VK_ACCESS_SHADER_WRITE_BIT, .dstAccessMask = VK_ACCESS_SHADER_READ_BIT | VK_ACCESS_SHADER_WRITE_BIT};
    v->CmdPipelineBarrier(v->command, VK_PIPELINE_STAGE_COMPUTE_SHADER_BIT, VK_PIPELINE_STAGE_COMPUTE_SHADER_BIT,
                          0, 1, &memory, 0, NULL, 0, NULL);
}

static void jpeg_bind_resources(bk_vk *v, const source *input, buffer buffers[JPEG_BUFFER_COUNT]) {
    VkDescriptorImageInfo image = {.imageView = input->view, .imageLayout = VK_IMAGE_LAYOUT_SHADER_READ_ONLY_OPTIMAL};
    VkDescriptorBufferInfo infos[8] = {0};
    VkWriteDescriptorSet writes[8] = {0};
    // Binding order matches jpeg420.hlsl; binding 0 is the source image.
    const unsigned resources[] = {
        [1] = JPEG_COEFFICIENTS,
        [2] = JPEG_PARAMETERS,
        [3] = JPEG_SEGMENTS,
        [4] = JPEG_SEGMENT_INFO,
        [5] = JPEG_OUTPUT,
        [6] = JPEG_HEADER,
        [7] = JPEG_ARGUMENTS
    };
    for (uint32_t binding = 0; binding < 8; binding++) {
        writes[binding] = (VkWriteDescriptorSet){.sType = VK_STRUCTURE_TYPE_WRITE_DESCRIPTOR_SET,
            .dstSet = v->descriptor, .dstBinding = binding, .descriptorCount = 1,
            .descriptorType = binding == 0 ? VK_DESCRIPTOR_TYPE_SAMPLED_IMAGE :
                binding == 2 ? VK_DESCRIPTOR_TYPE_UNIFORM_BUFFER : VK_DESCRIPTOR_TYPE_STORAGE_BUFFER};
        if (binding == 0) {
            writes[binding].pImageInfo = &image;
        } else {
            buffer *b = &buffers[resources[binding]];
            infos[binding] = (VkDescriptorBufferInfo){.buffer = b->buffer, .range = b->size};
            writes[binding].pBufferInfo = &infos[binding];
        }
    }
    v->UpdateDescriptorSets(v->device, 8, writes, 0, NULL);
}

static int jpeg_read_output(bk_vk *v, buffer buffers[JPEG_BUFFER_COUNT], size_t header_bytes, VkDeviceSize output_capacity,
                            uint8_t **jpeg, size_t *jpeg_bytes, char *error, size_t capacity) {
    uint32_t bytes;
    memcpy(&bytes, buffers[JPEG_LENGTH].mapped, sizeof(bytes));
    if (bytes < header_bytes + 2 || bytes > output_capacity) {
        snprintf(error, capacity, "GPU JPEG segment overflow or invalid output size");
        return -1;
    }
    VkDeviceSize copy_bytes = ((VkDeviceSize)bytes + 3) & ~(VkDeviceSize)3;
    // Leave headroom so small changes in compressed size do not allocate each frame.
    VkDeviceSize readback_capacity = (copy_bytes + 65535) & ~(VkDeviceSize)65535;
    if (jpeg_buffer_reserve(v, &buffers[JPEG_READBACK], readback_capacity, VK_BUFFER_USAGE_TRANSFER_DST_BIT, 1, error, capacity) < 0 ||
        begin(v, error, capacity) < 0) return -1;
    VkMemoryBarrier host_ready = {.sType = VK_STRUCTURE_TYPE_MEMORY_BARRIER,
        .srcAccessMask = VK_ACCESS_TRANSFER_WRITE_BIT, .dstAccessMask = VK_ACCESS_HOST_READ_BIT};
    VkBufferCopy copy = {.size = copy_bytes};
    v->CmdCopyBuffer(v->command, buffers[JPEG_OUTPUT].buffer, buffers[JPEG_READBACK].buffer, 1, &copy);
    v->CmdPipelineBarrier(v->command, VK_PIPELINE_STAGE_TRANSFER_BIT, VK_PIPELINE_STAGE_HOST_BIT,
                          0, 1, &host_ready, 0, NULL, 0, NULL);
    if (submit(v, -1, error, capacity) < 0 || invalidate_buffer(v, &buffers[JPEG_READBACK], error, capacity) < 0) return -1;
    *jpeg = malloc(bytes);
    if (!*jpeg) return -1;
    memcpy(*jpeg, buffers[JPEG_READBACK].mapped, bytes);
    *jpeg_bytes = bytes;
    return 0;
}

// Intermediate buffers stay device-local; only the length and JPEG are read back.
int bk_vk_jpeg_encode(bk_vk *v, const bk_vk_source *f, int width, int height,
                      const uint32_t *quantization, const uint32_t *huffman,
                      const uint8_t *header, size_t header_bytes, uint8_t **jpeg, size_t *jpeg_bytes,
                      char *error, size_t capacity) {
    source input = {0};
    buffer *buffers = NULL;
    int result = -1;
    *jpeg = NULL;
    *jpeg_bytes = 0;
    if (!v || !v->jpeg_pack || !f || !quantization || !huffman || !header || header_bytes < 2 || header_bytes > 1024 ||
        width < 1 || height < 1 || width > 8192 || height > 8192) goto failed;
    uint32_t columns = (width + 15) / 16, rows = (height + 15) / 16;
    uint32_t count = columns * rows;
    if (count > BK_JPEG_STORAGE_LIMIT / BK_JPEG_SEGMENT_CAPACITY) goto failed;
    if (!v->jpeg_buffers) {
        v->jpeg_buffers = calloc(1, sizeof(*v->jpeg_buffers));
        if (!v->jpeg_buffers) goto failed;
    }
    buffers = v->jpeg_buffers->items;
    VkDeviceSize output_capacity = (header_bytes + (VkDeviceSize)count * BK_JPEG_SEGMENT_CAPACITY + 2 + 3) & ~(VkDeviceSize)3;

    struct dma_buf_export_sync_file sync = {.flags = DMA_BUF_SYNC_READ, .fd = -1};
    if (ioctl(f->fd, DMA_BUF_IOCTL_EXPORT_SYNC_FILE, &sync) < 0) {
        snprintf(error, capacity, "DMA-BUF producer fence export: %s", strerror(errno));
        goto failed;
    }
    int waited = wait_fd(sync.fd, 1000);
    if (sync.fd >= 0) close(sync.fd);
    if (waited < 0) { snprintf(error, capacity, "DMA-BUF producer fence timed out"); goto failed; }
    if (source_import(v, f, &input, error, capacity) < 0) goto failed;
    VkDeviceSize sizes[JPEG_BUFFER_COUNT] = {
        [JPEG_COEFFICIENTS] = (VkDeviceSize)count * BK_JPEG_COEFFICIENTS_PER_MCU * sizeof(int16_t),
        [JPEG_SEGMENTS] = (VkDeviceSize)count * BK_JPEG_SEGMENT_CAPACITY,
        [JPEG_SEGMENT_INFO] = (count * 2 + 1) * sizeof(uint32_t),
        [JPEG_OUTPUT] = output_capacity,
        [JPEG_ARGUMENTS] = 4 * sizeof(uint32_t),
        [JPEG_HEADER] = (header_bytes + 3) & ~(size_t)3,
        [JPEG_PARAMETERS] = sizeof(bk_jpeg_parameters),
        [JPEG_LENGTH] = sizeof(uint32_t)
    };
    for (unsigned i = 0; i < JPEG_READBACK; i++) {
        VkBufferUsageFlags usage = VK_BUFFER_USAGE_STORAGE_BUFFER_BIT;
        if (i == JPEG_PARAMETERS) usage = VK_BUFFER_USAGE_UNIFORM_BUFFER_BIT;
        if (i == JPEG_OUTPUT || i == JPEG_ARGUMENTS) usage |= VK_BUFFER_USAGE_TRANSFER_SRC_BIT;
        if (i == JPEG_ARGUMENTS) usage |= VK_BUFFER_USAGE_INDIRECT_BUFFER_BIT;
        if (i == JPEG_LENGTH) usage = VK_BUFFER_USAGE_TRANSFER_DST_BIT;
        int host_visible = i == JPEG_HEADER || i == JPEG_PARAMETERS || i == JPEG_LENGTH;
        if (jpeg_buffer_reserve(v, &buffers[i], sizes[i], usage, host_visible, error, capacity) < 0) goto failed;
    }
    bk_jpeg_parameters parameters = {
        .source_width = f->width, .source_height = f->height,
        .output_width = width, .output_height = height,
        .rotation_degrees = f->rotation, .mcu_columns = columns, .mcu_count = count,
        .header_bytes = (uint32_t)header_bytes, .crop_origin = {f->x, f->y}, .crop_size = {f->crop_width, f->crop_height}
    };
    bk_jpeg_prepare_quantization(&parameters, quantization);
    memcpy(parameters.huffman, huffman, sizeof(parameters.huffman));
    memcpy(buffers[JPEG_PARAMETERS].mapped, &parameters, sizeof(parameters));
    memset(buffers[JPEG_HEADER].mapped, 0, sizes[JPEG_HEADER]);
    memcpy(buffers[JPEG_HEADER].mapped, header, header_bytes);
    if (flush_buffer(v, &buffers[JPEG_PARAMETERS], error, capacity) < 0 ||
        flush_buffer(v, &buffers[JPEG_HEADER], error, capacity) < 0) goto failed;

    jpeg_bind_resources(v, &input, buffers);
    if (begin(v, error, capacity) < 0) goto failed;
    VkImageMemoryBarrier acquire = {.sType = VK_STRUCTURE_TYPE_IMAGE_MEMORY_BARRIER,
        .srcAccessMask = VK_ACCESS_MEMORY_WRITE_BIT, .dstAccessMask = VK_ACCESS_SHADER_READ_BIT,
        .oldLayout = VK_IMAGE_LAYOUT_GENERAL, .newLayout = VK_IMAGE_LAYOUT_SHADER_READ_ONLY_OPTIMAL,
        .srcQueueFamilyIndex = VK_QUEUE_FAMILY_FOREIGN_EXT, .dstQueueFamilyIndex = v->family,
        .image = input.image, .subresourceRange = {VK_IMAGE_ASPECT_COLOR_BIT, 0, 1, 0, 1}};
    v->CmdPipelineBarrier(v->command, VK_PIPELINE_STAGE_ALL_COMMANDS_BIT, VK_PIPELINE_STAGE_COMPUTE_SHADER_BIT,
                          0, 0, NULL, 0, NULL, 1, &acquire);
    v->CmdBindDescriptorSets(v->command, VK_PIPELINE_BIND_POINT_COMPUTE, v->layout, 0, 1, &v->descriptor, 0, NULL);
    v->CmdBindPipeline(v->command, VK_PIPELINE_BIND_POINT_COMPUTE, v->pipeline);
    v->CmdDispatch(v->command, columns, rows, 5);
    jpeg_compute_barrier(v);
    v->CmdBindPipeline(v->command, VK_PIPELINE_BIND_POINT_COMPUTE, v->jpeg_entropy);
    v->CmdDispatch(v->command, (count + 63) / 64, 1, 1);
    jpeg_compute_barrier(v);
    v->CmdBindPipeline(v->command, VK_PIPELINE_BIND_POINT_COMPUTE, v->jpeg_index);
    v->CmdDispatch(v->command, 1, 1, 1);
    jpeg_compute_barrier(v);
    VkBufferMemoryBarrier indirect_ready = {.sType = VK_STRUCTURE_TYPE_BUFFER_MEMORY_BARRIER,
        .srcAccessMask = VK_ACCESS_SHADER_WRITE_BIT, .dstAccessMask = VK_ACCESS_INDIRECT_COMMAND_READ_BIT,
        .srcQueueFamilyIndex = VK_QUEUE_FAMILY_IGNORED, .dstQueueFamilyIndex = VK_QUEUE_FAMILY_IGNORED,
        .buffer = buffers[JPEG_ARGUMENTS].buffer, .size = VK_WHOLE_SIZE};
    v->CmdPipelineBarrier(v->command, VK_PIPELINE_STAGE_COMPUTE_SHADER_BIT, VK_PIPELINE_STAGE_DRAW_INDIRECT_BIT,
                          0, 0, NULL, 1, &indirect_ready, 0, NULL);
    v->CmdBindPipeline(v->command, VK_PIPELINE_BIND_POINT_COMPUTE, v->jpeg_pack);
    v->CmdDispatchIndirect(v->command, buffers[JPEG_ARGUMENTS].buffer, 0);

    VkImageMemoryBarrier release = acquire;
    release.srcAccessMask = VK_ACCESS_SHADER_READ_BIT;
    release.dstAccessMask = 0;
    release.oldLayout = VK_IMAGE_LAYOUT_SHADER_READ_ONLY_OPTIMAL;
    release.newLayout = VK_IMAGE_LAYOUT_GENERAL;
    release.srcQueueFamilyIndex = v->family;
    release.dstQueueFamilyIndex = VK_QUEUE_FAMILY_FOREIGN_EXT;
    VkMemoryBarrier ready = {.sType = VK_STRUCTURE_TYPE_MEMORY_BARRIER,
        .srcAccessMask = VK_ACCESS_SHADER_WRITE_BIT, .dstAccessMask = VK_ACCESS_TRANSFER_READ_BIT};
    v->CmdPipelineBarrier(v->command, VK_PIPELINE_STAGE_COMPUTE_SHADER_BIT, VK_PIPELINE_STAGE_TRANSFER_BIT,
                          0, 1, &ready, 0, NULL, 1, &release);
    VkBufferCopy length_copy = {.srcOffset = 12, .size = 4};
    v->CmdCopyBuffer(v->command, buffers[JPEG_ARGUMENTS].buffer, buffers[JPEG_LENGTH].buffer, 1, &length_copy);
    VkMemoryBarrier host_ready = {.sType = VK_STRUCTURE_TYPE_MEMORY_BARRIER,
        .srcAccessMask = VK_ACCESS_TRANSFER_WRITE_BIT, .dstAccessMask = VK_ACCESS_HOST_READ_BIT};
    v->CmdPipelineBarrier(v->command, VK_PIPELINE_STAGE_TRANSFER_BIT, VK_PIPELINE_STAGE_HOST_BIT,
                          0, 1, &host_ready, 0, NULL, 0, NULL);
    if (submit(v, f->fd, error, capacity) < 0 || invalidate_buffer(v, &buffers[JPEG_LENGTH], error, capacity) < 0) goto failed;
    if (jpeg_read_output(v, buffers, header_bytes, output_capacity, jpeg, jpeg_bytes, error, capacity) < 0) goto failed;
    result = 0;
failed:
    if (result < 0 && !error[0]) snprintf(error, capacity, "Vulkan JPEG encoding failed");
    source_free(v, &input);
    return result;
}
