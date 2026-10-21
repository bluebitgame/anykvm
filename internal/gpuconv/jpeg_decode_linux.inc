// Baseline JPEG decoder. No DMA-BUF import or Vulkan Video extensions needed.
struct bk_vk_jpeg_decoder {
    bk_vk *v;
    VkPipeline idct, pack;
    buffer buffers[6], output_readback, status_readback;
};
void bk_vk_jpeg_decoder_free(bk_vk_jpeg_decoder *d) {
    if (!d) return;
    bk_vk *v=d->v;
    v->DeviceWaitIdle(v->device);
    for (int i=0;i<6;i++) buffer_free(v,&d->buffers[i]);
    buffer_free(v,&d->output_readback); buffer_free(v,&d->status_readback);
    if (d->idct) v->DestroyPipeline(v->device,d->idct,NULL);
    if (d->pack) v->DestroyPipeline(v->device,d->pack,NULL);
    free(d);
}
bk_vk_jpeg_decoder *bk_vk_jpeg_decoder_new(bk_vk *v,const void *idct,size_t idct_bytes,const void *pack,size_t pack_bytes,char *error,size_t capacity) {
    bk_vk_jpeg_decoder *d=calloc(1,sizeof(*d));
    if (!d) return NULL;
    d->v=v;
    if (jpeg_create_pipeline(v,idct,idct_bytes,"inverseDCT",&d->idct,error,capacity)<0 ||
        jpeg_create_pipeline(v,pack,pack_bytes,"packNV12",&d->pack,error,capacity)<0) {
        bk_vk_jpeg_decoder_free(d); return NULL;
    }
    return d;
}
static int decode_buffer_resize(bk_vk *v,buffer *b,VkDeviceSize size,int host,VkBufferUsageFlags usage,char *error,size_t capacity) {
    if (b->size>=size) return 0;
    buffer_free(v,b);
    return buffer_new(v,b,size,usage,host,error,capacity);
}
int bk_vk_jpeg_decoder_decode(bk_vk_jpeg_decoder *d,const uint8_t *input,size_t input_bytes,const uint32_t *meta,size_t meta_bytes,
                             uint8_t *output,size_t output_bytes,char *error,size_t capacity) {
    if (!d||!input||!meta||!output||meta_bytes<2056*4||input_bytes<4||input_bytes>128u*1024*1024||
        meta_bytes>128u*1024*1024||meta[2]<2||meta[3]<2||meta[2]>8192||meta[3]>8192||
        output_bytes!=(size_t)meta[2]*meta[3]*3/2) return -1;
    uint64_t workspace=(uint64_t)meta[6]*meta[7]*meta[8]*64*4;
    if (!workspace||workspace>128u*1024*1024) return -1;
    bk_vk *v=d->v;
    VkDeviceSize sizes[6]={input_bytes,meta_bytes,workspace,workspace,(output_bytes+3)&~(size_t)3,4};
    VkDescriptorBufferInfo infos[6]; VkWriteDescriptorSet writes[6];
    for (unsigned i=0;i<6;i++) {
        VkBufferUsageFlags usage=VK_BUFFER_USAGE_STORAGE_BUFFER_BIT;
        if (i==4||i==5) usage|=VK_BUFFER_USAGE_TRANSFER_SRC_BIT;
        if (decode_buffer_resize(v,&d->buffers[i],sizes[i],i<2||i==5,usage,error,capacity)<0) goto failed;
        infos[i]=(VkDescriptorBufferInfo){.buffer=d->buffers[i].buffer,.range=d->buffers[i].size};
        writes[i]=(VkWriteDescriptorSet){.sType=VK_STRUCTURE_TYPE_WRITE_DESCRIPTOR_SET,.dstSet=v->descriptor,
            .dstBinding=i,.descriptorType=VK_DESCRIPTOR_TYPE_STORAGE_BUFFER,.descriptorCount=1,.pBufferInfo=&infos[i]};
    }
    if (decode_buffer_resize(v,&d->output_readback,sizes[4],1,VK_BUFFER_USAGE_TRANSFER_DST_BIT,error,capacity)<0 ||
        decode_buffer_resize(v,&d->status_readback,4,1,VK_BUFFER_USAGE_TRANSFER_DST_BIT,error,capacity)<0) goto failed;
    memcpy(d->buffers[0].mapped,input,input_bytes); memcpy(d->buffers[1].mapped,meta,meta_bytes); memset(d->buffers[5].mapped,0,4);
    if (flush_buffer(v,&d->buffers[0],error,capacity)<0 || flush_buffer(v,&d->buffers[1],error,capacity)<0 ||
        flush_buffer(v,&d->buffers[5],error,capacity)<0) goto failed;
    v->UpdateDescriptorSets(v->device,6,writes,0,NULL);
    if (begin(v,error,capacity)<0) goto failed;
    VkMemoryBarrier host={.sType=VK_STRUCTURE_TYPE_MEMORY_BARRIER,.srcAccessMask=VK_ACCESS_HOST_WRITE_BIT,
        .dstAccessMask=VK_ACCESS_SHADER_READ_BIT|VK_ACCESS_SHADER_WRITE_BIT};
    v->CmdPipelineBarrier(v->command,VK_PIPELINE_STAGE_HOST_BIT,VK_PIPELINE_STAGE_COMPUTE_SHADER_BIT,0,1,&host,0,NULL,0,NULL);
    v->CmdBindDescriptorSets(v->command,VK_PIPELINE_BIND_POINT_COMPUTE,v->layout,0,1,&v->descriptor,0,NULL);
    v->CmdBindPipeline(v->command,VK_PIPELINE_BIND_POINT_COMPUTE,v->pipeline);
    v->CmdDispatch(v->command,(meta[9]+63)/64,1,1);
    jpeg_compute_barrier(v);
    v->CmdBindPipeline(v->command,VK_PIPELINE_BIND_POINT_COMPUTE,d->idct);
    v->CmdDispatch(v->command,meta[6]*meta[8],meta[7],1);
    jpeg_compute_barrier(v);
    v->CmdBindPipeline(v->command,VK_PIPELINE_BIND_POINT_COMPUTE,d->pack);
    uint32_t groups=(uint32_t)((sizes[4]/4+63)/64);
    v->CmdDispatch(v->command,groups<256?groups:256,(uint32_t)((sizes[4]/4+16383)/16384),1);
    VkMemoryBarrier transfer={.sType=VK_STRUCTURE_TYPE_MEMORY_BARRIER,.srcAccessMask=VK_ACCESS_SHADER_WRITE_BIT,.dstAccessMask=VK_ACCESS_TRANSFER_READ_BIT};
    v->CmdPipelineBarrier(v->command,VK_PIPELINE_STAGE_COMPUTE_SHADER_BIT,VK_PIPELINE_STAGE_TRANSFER_BIT,0,1,&transfer,0,NULL,0,NULL);
    VkBufferCopy copy={.size=sizes[4]};
    v->CmdCopyBuffer(v->command,d->buffers[4].buffer,d->output_readback.buffer,1,&copy);
    copy.size=4; v->CmdCopyBuffer(v->command,d->buffers[5].buffer,d->status_readback.buffer,1,&copy);
    VkMemoryBarrier read={.sType=VK_STRUCTURE_TYPE_MEMORY_BARRIER,.srcAccessMask=VK_ACCESS_TRANSFER_WRITE_BIT,.dstAccessMask=VK_ACCESS_HOST_READ_BIT};
    v->CmdPipelineBarrier(v->command,VK_PIPELINE_STAGE_TRANSFER_BIT,VK_PIPELINE_STAGE_HOST_BIT,0,1,&read,0,NULL,0,NULL);
    if (submit(v,-1,error,capacity)<0 || invalidate_buffer(v,&d->status_readback,error,capacity)<0) goto failed;
    if (*(uint32_t *)d->status_readback.mapped) { snprintf(error,capacity,"invalid JPEG entropy"); return -1; }
    if (invalidate_buffer(v,&d->output_readback,error,capacity)<0) goto failed;
    memcpy(output,d->output_readback.mapped,output_bytes); return 0;
failed:
    return -1;
}
