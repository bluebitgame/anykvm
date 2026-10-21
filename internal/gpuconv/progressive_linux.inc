#include "progressive_layout.h"
struct bk_vk_progressive {
    bk_vk *v;
    VkPipeline entropy, index, pack;
    buffer buffers[6], status, readback;
    VkDescriptorSetLayout surface_descriptor_layout;
    VkPipelineLayout surface_layout;
    VkPipeline surface_pipeline;
    VkDescriptorPool surface_pool;
    VkDescriptorSet surface_descriptor;
    buffer current,previous,dirty,dirty_readback,surface_parameters;
    uint32_t surface_width,surface_height,prepared,previous_valid;
};
void bk_vk_progressive_free(bk_vk_progressive *p) {
    if (!p) return;
    bk_vk *v=p->v;
    v->DeviceWaitIdle(v->device);
    for (int i=0;i<6;i++) buffer_free(v,&p->buffers[i]);
    buffer_free(v,&p->status); buffer_free(v,&p->readback);
    buffer_free(v,&p->current);buffer_free(v,&p->previous);buffer_free(v,&p->dirty);
    buffer_free(v,&p->dirty_readback);buffer_free(v,&p->surface_parameters);
    if(p->surface_pool)v->DestroyDescriptorPool(v->device,p->surface_pool,NULL);
    if(p->surface_pipeline)v->DestroyPipeline(v->device,p->surface_pipeline,NULL);
    if(p->surface_layout)v->DestroyPipelineLayout(v->device,p->surface_layout,NULL);
    if(p->surface_descriptor_layout)v->DestroyDescriptorSetLayout(v->device,p->surface_descriptor_layout,NULL);
    if (p->entropy) v->DestroyPipeline(v->device,p->entropy,NULL);
    if (p->index) v->DestroyPipeline(v->device,p->index,NULL);
    if (p->pack) v->DestroyPipeline(v->device,p->pack,NULL);
    free(p);
}
bk_vk_progressive *bk_vk_progressive_new(bk_vk *v,const void *entropy,size_t entropy_bytes,
    const void *index,size_t index_bytes,const void *pack,size_t pack_bytes,char *error,size_t capacity) {
    bk_vk_progressive *p=calloc(1,sizeof(*p));
    if (!p) { snprintf(error,capacity,"Progressive allocation failed"); return NULL; }
    p->v=v;
    if (jpeg_create_pipeline(v,entropy,entropy_bytes,"encodeTiles",&p->entropy,error,capacity)<0 ||
        jpeg_create_pipeline(v,index,index_bytes,"indexSegments",&p->index,error,capacity)<0 ||
        jpeg_create_pipeline(v,pack,pack_bytes,"packSegments",&p->pack,error,capacity)<0) {
        bk_vk_progressive_free(p); return NULL;
    }
    return p;
}
static void progressive_transfer_barrier(bk_vk *v) {
    VkMemoryBarrier barrier={.sType=VK_STRUCTURE_TYPE_MEMORY_BARRIER,.srcAccessMask=VK_ACCESS_SHADER_WRITE_BIT,
        .dstAccessMask=VK_ACCESS_TRANSFER_READ_BIT};
    v->CmdPipelineBarrier(v->command,VK_PIPELINE_STAGE_COMPUTE_SHADER_BIT,VK_PIPELINE_STAGE_TRANSFER_BIT,0,1,&barrier,0,NULL,0,NULL);
}
static void progressive_host_barrier(bk_vk *v) {
    VkMemoryBarrier barrier={.sType=VK_STRUCTURE_TYPE_MEMORY_BARRIER,.srcAccessMask=VK_ACCESS_TRANSFER_WRITE_BIT,
        .dstAccessMask=VK_ACCESS_HOST_READ_BIT};
    v->CmdPipelineBarrier(v->command,VK_PIPELINE_STAGE_TRANSFER_BIT,VK_PIPELINE_STAGE_HOST_BIT,0,1,&barrier,0,NULL,0,NULL);
}
int bk_vk_progressive_run(bk_vk_progressive *p,const uint8_t *input,size_t input_bytes,const uint32_t *meta,
    uint8_t **output,size_t *output_bytes,uint32_t *lengths,char *error,size_t capacity) {
    if (!p||!input||!output||!output_bytes||!lengths||!bk_progressive_validate(meta,input_bytes)) {
        snprintf(error,capacity,"invalid Progressive compute input"); return -1;
    }
    *output=NULL; *output_bytes=0;
    if(meta[6]&&(!p->prepared||meta[6]!=p->surface_width*4||meta[7]!=p->surface_height)) {
        snprintf(error,capacity,"Progressive surface is not prepared");return -1;
    }
    bk_vk *v=p->v;
    VkDeviceSize sizes[6]={input_bytes,64,(VkDeviceSize)meta[5]*12288*4,
        BK_PROGRESSIVE_SEGMENTS*BK_PROGRESSIVE_SEGMENT_BYTES,BK_PROGRESSIVE_INFO_BYTES,
        BK_PROGRESSIVE_SEGMENTS*BK_PROGRESSIVE_SEGMENT_BYTES};
    VkDescriptorBufferInfo infos[6]; VkWriteDescriptorSet writes[6];
    for (unsigned i=0;i<6;i++) {
        VkBufferUsageFlags usage=VK_BUFFER_USAGE_STORAGE_BUFFER_BIT;
        if (i>=4) usage|=VK_BUFFER_USAGE_TRANSFER_SRC_BIT;
        if (decode_buffer_resize(v,&p->buffers[i],sizes[i],i<2,usage,error,capacity)<0) return -1;
        infos[i]=(VkDescriptorBufferInfo){.buffer=p->buffers[i].buffer,.range=p->buffers[i].size};
        if(i==0&&meta[6])infos[i]=(VkDescriptorBufferInfo){.buffer=p->current.buffer,.range=p->current.size};
        writes[i]=(VkWriteDescriptorSet){.sType=VK_STRUCTURE_TYPE_WRITE_DESCRIPTOR_SET,.dstSet=v->descriptor,.dstBinding=i,
            .descriptorType=VK_DESCRIPTOR_TYPE_STORAGE_BUFFER,.descriptorCount=1,.pBufferInfo=&infos[i]};
    }
    if (decode_buffer_resize(v,&p->status,BK_PROGRESSIVE_INFO_BYTES,1,VK_BUFFER_USAGE_TRANSFER_DST_BIT,error,capacity)<0) return -1;
    memcpy(p->buffers[0].mapped,input,input_bytes); memcpy(p->buffers[1].mapped,meta,64);
    if (flush_buffer(v,&p->buffers[0],error,capacity)<0 || flush_buffer(v,&p->buffers[1],error,capacity)<0) return -1;
    v->UpdateDescriptorSets(v->device,6,writes,0,NULL);
    if (begin(v,error,capacity)<0) return -1;
    // Include coefficient writes from earlier submissions as well as host input.
    VkMemoryBarrier host={.sType=VK_STRUCTURE_TYPE_MEMORY_BARRIER,
        .srcAccessMask=VK_ACCESS_HOST_WRITE_BIT|VK_ACCESS_SHADER_WRITE_BIT,
        .dstAccessMask=VK_ACCESS_SHADER_READ_BIT|VK_ACCESS_SHADER_WRITE_BIT};
    v->CmdPipelineBarrier(v->command,VK_PIPELINE_STAGE_HOST_BIT|VK_PIPELINE_STAGE_COMPUTE_SHADER_BIT,
        VK_PIPELINE_STAGE_COMPUTE_SHADER_BIT,0,1,&host,0,NULL,0,NULL);
    v->CmdBindDescriptorSets(v->command,VK_PIPELINE_BIND_POINT_COMPUTE,v->layout,0,1,&v->descriptor,0,NULL);
    if (meta[4]!=2) {
        v->CmdBindPipeline(v->command,VK_PIPELINE_BIND_POINT_COMPUTE,v->pipeline);
        v->CmdDispatch(v->command,meta[0],3,1); jpeg_compute_barrier(v);
    }
    v->CmdBindPipeline(v->command,VK_PIPELINE_BIND_POINT_COMPUTE,p->entropy);
    v->CmdDispatch(v->command,meta[0],3,1); jpeg_compute_barrier(v);
    v->CmdBindPipeline(v->command,VK_PIPELINE_BIND_POINT_COMPUTE,p->index);
    v->CmdDispatch(v->command,1,1,1); jpeg_compute_barrier(v);
    v->CmdBindPipeline(v->command,VK_PIPELINE_BIND_POINT_COMPUTE,p->pack);
    v->CmdDispatch(v->command,meta[0]*6,1,1);
    progressive_transfer_barrier(v);
    VkBufferCopy copy={.size=BK_PROGRESSIVE_INFO_BYTES};
    v->CmdCopyBuffer(v->command,p->buffers[4].buffer,p->status.buffer,1,&copy);
    progressive_host_barrier(v);
    if (submit(v,-1,error,capacity)<0 || invalidate_buffer(v,&p->status,error,capacity)<0) return -1;
    uint32_t *status=p->status.mapped, bytes=status[48];
    if (status[49] || bytes>sizes[5] || !bytes || bytes%4) {
        snprintf(error,capacity,"Progressive segment overflow or invalid size"); return -1;
    }
    // Only compact entropy data crosses back to the CPU; coefficients stay resident.
    if (decode_buffer_resize(v,&p->readback,bytes,1,VK_BUFFER_USAGE_TRANSFER_DST_BIT,error,capacity)<0 ||
        begin(v,error,capacity)<0) return -1;
    progressive_transfer_barrier(v);
    copy.size=bytes; v->CmdCopyBuffer(v->command,p->buffers[5].buffer,p->readback.buffer,1,&copy);
    progressive_host_barrier(v);
    if (submit(v,-1,error,capacity)<0 || invalidate_buffer(v,&p->readback,error,capacity)<0) return -1;
    uint8_t *result=malloc(bytes);
    if (!result) { snprintf(error,capacity,"Progressive output allocation failed"); return -1; }
    memcpy(result,p->readback.mapped,bytes); memcpy(lengths,status,BK_PROGRESSIVE_SEGMENTS*4);
    *output=result; *output_bytes=bytes; return 0;
}

// A child has its own pipelines/descriptors/command pool, while its immutable
// input Image and Go Converter lease retain the original GPU device/loader.
// All parent/child queue access is serialized by Converter.mu in Go.
bk_vk *bk_vk_progressive_context(bk_vk_image *image,const void *spirv,size_t bytes,char *error,size_t capacity) {
    if(!image||image->nv12){snprintf(error,capacity,"Progressive requires a BGRA GPU snapshot");return NULL;}
    bk_vk *parent=image->context,*v=calloc(1,sizeof(*v));if(!v)return NULL;
    *v=*parent;v->jpeg_buffers=NULL;v->library=NULL;v->instance=VK_NULL_HANDLE;v->borrowed_device=1;
    v->descriptor_layout=VK_NULL_HANDLE;v->layout=VK_NULL_HANDLE;v->pipeline=VK_NULL_HANDLE;
    v->jpeg_entropy=v->jpeg_index=v->jpeg_pack=VK_NULL_HANDLE;
    v->descriptor_pool=VK_NULL_HANDLE;v->descriptor=VK_NULL_HANDLE;
    v->commands=VK_NULL_HANDLE;v->command=VK_NULL_HANDLE;v->sampler=VK_NULL_HANDLE;
    VkPhysicalDeviceProperties2 properties={.sType=VK_STRUCTURE_TYPE_PHYSICAL_DEVICE_PROPERTIES_2};
    v->GetPhysicalDeviceProperties2(v->physical,&properties);
    if(properties.properties.limits.maxPerStageDescriptorStorageBuffers<6||properties.properties.limits.maxDescriptorSetStorageBuffers<6){
        snprintf(error,capacity,"capture GPU lacks six compute storage bindings");goto failed;
    }
    VkDescriptorSetLayoutBinding bindings[6]={0};
    for(uint32_t i=0;i<6;i++)bindings[i]=(VkDescriptorSetLayoutBinding){.binding=i,.descriptorType=VK_DESCRIPTOR_TYPE_STORAGE_BUFFER,.descriptorCount=1,.stageFlags=VK_SHADER_STAGE_COMPUTE_BIT};
    VkDescriptorSetLayoutCreateInfo dl={.sType=VK_STRUCTURE_TYPE_DESCRIPTOR_SET_LAYOUT_CREATE_INFO,.bindingCount=6,.pBindings=bindings};
    TRY(v->CreateDescriptorSetLayout(v->device,&dl,NULL,&v->descriptor_layout));
    VkPipelineLayoutCreateInfo pl={.sType=VK_STRUCTURE_TYPE_PIPELINE_LAYOUT_CREATE_INFO,.setLayoutCount=1,.pSetLayouts=&v->descriptor_layout};
    TRY(v->CreatePipelineLayout(v->device,&pl,NULL,&v->layout));
    if(jpeg_create_pipeline(v,spirv,bytes,"main",&v->pipeline,error,capacity)<0)goto failed;
    VkDescriptorPoolSize size={VK_DESCRIPTOR_TYPE_STORAGE_BUFFER,6};
    VkDescriptorPoolCreateInfo pool={.sType=VK_STRUCTURE_TYPE_DESCRIPTOR_POOL_CREATE_INFO,.maxSets=1,.poolSizeCount=1,.pPoolSizes=&size};
    TRY(v->CreateDescriptorPool(v->device,&pool,NULL,&v->descriptor_pool));
    VkDescriptorSetAllocateInfo set={.sType=VK_STRUCTURE_TYPE_DESCRIPTOR_SET_ALLOCATE_INFO,.descriptorPool=v->descriptor_pool,.descriptorSetCount=1,.pSetLayouts=&v->descriptor_layout};
    TRY(v->AllocateDescriptorSets(v->device,&set,&v->descriptor));
    VkCommandPoolCreateInfo cp={.sType=VK_STRUCTURE_TYPE_COMMAND_POOL_CREATE_INFO,.queueFamilyIndex=v->family};
    TRY(v->CreateCommandPool(v->device,&cp,NULL,&v->commands));
    VkCommandBufferAllocateInfo cb={.sType=VK_STRUCTURE_TYPE_COMMAND_BUFFER_ALLOCATE_INFO,.commandPool=v->commands,.level=VK_COMMAND_BUFFER_LEVEL_PRIMARY,.commandBufferCount=1};
    TRY(v->AllocateCommandBuffers(v->device,&cb,&v->command));
    return v;
failed:bk_vk_close(v);return NULL;
}

int bk_vk_progressive_surface_init(bk_vk_progressive *p,const void *spirv,size_t bytes,char *error,size_t capacity) {
    bk_vk *v=p->v;
    VkDescriptorSetLayoutBinding bindings[5]={0};
    for(uint32_t i=0;i<5;i++)bindings[i]=(VkDescriptorSetLayoutBinding){.binding=i,.descriptorType=VK_DESCRIPTOR_TYPE_STORAGE_BUFFER,.descriptorCount=1,.stageFlags=VK_SHADER_STAGE_COMPUTE_BIT};
    VkDescriptorSetLayoutCreateInfo dl={.sType=VK_STRUCTURE_TYPE_DESCRIPTOR_SET_LAYOUT_CREATE_INFO,.bindingCount=5,.pBindings=bindings};
    TRY(v->CreateDescriptorSetLayout(v->device,&dl,NULL,&p->surface_descriptor_layout));
    VkPipelineLayoutCreateInfo pl={.sType=VK_STRUCTURE_TYPE_PIPELINE_LAYOUT_CREATE_INFO,.setLayoutCount=1,.pSetLayouts=&p->surface_descriptor_layout};
    TRY(v->CreatePipelineLayout(v->device,&pl,NULL,&p->surface_layout));
    bk_vk view=*v;view.layout=p->surface_layout;
    if(jpeg_create_pipeline(&view,spirv,bytes,"main",&p->surface_pipeline,error,capacity)<0)goto failed;
    VkDescriptorPoolSize size={VK_DESCRIPTOR_TYPE_STORAGE_BUFFER,5};
    VkDescriptorPoolCreateInfo pool={.sType=VK_STRUCTURE_TYPE_DESCRIPTOR_POOL_CREATE_INFO,.maxSets=1,.poolSizeCount=1,.pPoolSizes=&size};
    TRY(v->CreateDescriptorPool(v->device,&pool,NULL,&p->surface_pool));
    VkDescriptorSetAllocateInfo set={.sType=VK_STRUCTURE_TYPE_DESCRIPTOR_SET_ALLOCATE_INFO,.descriptorPool=p->surface_pool,.descriptorSetCount=1,.pSetLayouts=&p->surface_descriptor_layout};
    TRY(v->AllocateDescriptorSets(v->device,&set,&p->surface_descriptor));return 0;
failed:return -1;
}
int bk_vk_progressive_surface_prepare(bk_vk_progressive *p,bk_vk_image *image,const uint32_t *parameters,uint32_t *flags,char *error,size_t capacity) {
    if(!p||!image||!parameters||!flags||!p->surface_pipeline||!bk_progressive_surface_validate(parameters)||
       image->context->device!=p->v->device||image->nv12||image->width!=parameters[0]||image->height!=parameters[1]||parameters[3]) {
        snprintf(error,capacity,"invalid GPU surface context or geometry");return -1;
    }
    bk_vk *v=p->v;uint32_t values[12];memcpy(values,parameters,sizeof(values));values[2]=image->pitch;
    uint32_t w=values[4],h=values[5],tiles=((w+63)/64)*((h+63)/64);p->prepared=0;
    if(p->surface_width!=w||p->surface_height!=h) {
        buffer_free(v,&p->current);buffer_free(v,&p->previous);p->previous_valid=0;
        p->surface_width=w;p->surface_height=h;
    }
    if(!p->previous_valid)values[10]=1;
    VkDeviceSize pixels=(VkDeviceSize)w*h*4;
    if(decode_buffer_resize(v,&p->current,pixels,0,VK_BUFFER_USAGE_STORAGE_BUFFER_BIT|VK_BUFFER_USAGE_TRANSFER_SRC_BIT,error,capacity)<0||
       decode_buffer_resize(v,&p->previous,pixels,0,VK_BUFFER_USAGE_STORAGE_BUFFER_BIT|VK_BUFFER_USAGE_TRANSFER_DST_BIT,error,capacity)<0||
       decode_buffer_resize(v,&p->dirty,tiles*4,0,VK_BUFFER_USAGE_STORAGE_BUFFER_BIT|VK_BUFFER_USAGE_TRANSFER_SRC_BIT,error,capacity)<0||
       decode_buffer_resize(v,&p->dirty_readback,tiles*4,1,VK_BUFFER_USAGE_TRANSFER_DST_BIT,error,capacity)<0||
       decode_buffer_resize(v,&p->surface_parameters,48,1,VK_BUFFER_USAGE_STORAGE_BUFFER_BIT,error,capacity)<0)return -1;
    memcpy(p->surface_parameters.mapped,values,48);
    if(flush_buffer(v,&p->surface_parameters,error,capacity)<0)return -1;
    buffer *buffers[]={&image->output,&p->surface_parameters,&p->current,&p->previous,&p->dirty};
    VkDescriptorBufferInfo infos[5];VkWriteDescriptorSet writes[5];
    for(uint32_t i=0;i<5;i++) {
        infos[i]=(VkDescriptorBufferInfo){.buffer=buffers[i]->buffer,.range=buffers[i]->size};
        writes[i]=(VkWriteDescriptorSet){.sType=VK_STRUCTURE_TYPE_WRITE_DESCRIPTOR_SET,.dstSet=p->surface_descriptor,.dstBinding=i,.descriptorType=VK_DESCRIPTOR_TYPE_STORAGE_BUFFER,.descriptorCount=1,.pBufferInfo=&infos[i]};
    }
    v->UpdateDescriptorSets(v->device,5,writes,0,NULL);
    if(begin(v,error,capacity)<0)return -1;
    VkMemoryBarrier ready={.sType=VK_STRUCTURE_TYPE_MEMORY_BARRIER,.srcAccessMask=VK_ACCESS_SHADER_WRITE_BIT|VK_ACCESS_HOST_WRITE_BIT|VK_ACCESS_TRANSFER_WRITE_BIT,.dstAccessMask=VK_ACCESS_SHADER_READ_BIT|VK_ACCESS_SHADER_WRITE_BIT};
    v->CmdPipelineBarrier(v->command,VK_PIPELINE_STAGE_COMPUTE_SHADER_BIT|VK_PIPELINE_STAGE_HOST_BIT|VK_PIPELINE_STAGE_TRANSFER_BIT,VK_PIPELINE_STAGE_COMPUTE_SHADER_BIT,0,1,&ready,0,NULL,0,NULL);
    v->CmdBindPipeline(v->command,VK_PIPELINE_BIND_POINT_COMPUTE,p->surface_pipeline);
    v->CmdBindDescriptorSets(v->command,VK_PIPELINE_BIND_POINT_COMPUTE,p->surface_layout,0,1,&p->surface_descriptor,0,NULL);
    v->CmdDispatch(v->command,(w+63)/64,(h+63)/64,1);
    progressive_transfer_barrier(v);
    VkBufferCopy copy={.size=tiles*4};v->CmdCopyBuffer(v->command,p->dirty.buffer,p->dirty_readback.buffer,1,&copy);
    progressive_host_barrier(v);
    if(submit(v,-1,error,capacity)<0||invalidate_buffer(v,&p->dirty_readback,error,capacity)<0)return -1;
    memcpy(flags,p->dirty_readback.mapped,tiles*4);p->prepared=1;return 0;
}
int bk_vk_progressive_surface_commit(bk_vk_progressive *p,char *error,size_t capacity) {
    if(!p||!p->prepared){snprintf(error,capacity,"no prepared Progressive surface");return -1;}
    bk_vk *v=p->v;if(begin(v,error,capacity)<0)return -1;
    progressive_transfer_barrier(v);
    VkBufferCopy copy={.size=(VkDeviceSize)p->surface_width*p->surface_height*4};
    v->CmdCopyBuffer(v->command,p->current.buffer,p->previous.buffer,1,&copy);
    VkMemoryBarrier ready={.sType=VK_STRUCTURE_TYPE_MEMORY_BARRIER,.srcAccessMask=VK_ACCESS_TRANSFER_WRITE_BIT,.dstAccessMask=VK_ACCESS_SHADER_READ_BIT};
    v->CmdPipelineBarrier(v->command,VK_PIPELINE_STAGE_TRANSFER_BIT,VK_PIPELINE_STAGE_COMPUTE_SHADER_BIT,0,1,&ready,0,NULL,0,NULL);
    if(submit(v,-1,error,capacity)<0)return -1;
    p->previous_valid=1;return 0;
}
