#include "wasapi_queue.h"
#include <assert.h>
#include <stdlib.h>

int main(void) {
    bk_pcm_queue q={0};uint64_t epoch=0;
    uint32_t *input=malloc(BK_PCM_QUEUE_BYTES*2),*output=malloc(BK_PCM_QUEUE_BYTES);
    assert(input && output);
    for(size_t i=0;i<BK_PCM_QUEUE_BYTES/2;i++)input[i]=(uint32_t)i+1;
    assert(bk_pcm_queue_push(&q,(uint8_t *)input,BK_PCM_QUEUE_BYTES,0,0)==0);
    assert(bk_pcm_queue_read(&q,(uint8_t *)output,7,&epoch)==4 && output[0]==1);
    assert(bk_pcm_queue_push(&q,(uint8_t *)(input+BK_PCM_QUEUE_BYTES/4),4,0,0)==0);
    assert(bk_pcm_queue_read(&q,(uint8_t *)output,BK_PCM_QUEUE_BYTES,&epoch)==BK_PCM_QUEUE_BYTES);
    for(size_t i=0;i<BK_PCM_QUEUE_BYTES/4;i++)assert(output[i]==i+2);
    // Overflow keeps the latest 100ms, not a growing backlog.
    assert(bk_pcm_queue_push(&q,(uint8_t *)input,BK_PCM_QUEUE_BYTES+400,0,0)==0);
    assert(bk_pcm_queue_read(&q,(uint8_t *)output,BK_PCM_QUEUE_BYTES,&epoch)==BK_PCM_QUEUE_BYTES && epoch>0);
    for(size_t i=0;i<BK_PCM_QUEUE_BYTES/4;i++)assert(output[i]==i+101);
    // A discontinuity discards buffered old-device samples; silent packets may
    // legally have a null data pointer and must contain exact PCM silence.
    assert(bk_pcm_queue_push(&q,(uint8_t *)input,400,0,0)==0);
    uint64_t old=q.epoch;
    assert(bk_pcm_queue_push(&q,NULL,80,1,1)==0);
    assert(bk_pcm_queue_read(&q,(uint8_t *)output,80,&epoch)==80 && epoch>old);
    for(size_t i=0;i<20;i++)assert(output[i]==0);
    assert(q.count==0);
    assert(bk_pcm_queue_push(&q,NULL,4,0,0)==-1);
    assert(bk_pcm_queue_push(&q,(uint8_t *)input,3,0,0)==-1);
    // Consumer backlog remains bounded across repeated wrap/overflow cycles.
    for(int n=0;n<1000;n++) {
        assert(bk_pcm_queue_push(&q,(uint8_t *)input,1000,0,0)==0);
        assert(bk_pcm_queue_read(&q,(uint8_t *)output,484,&epoch)==484);
        assert(q.count<=BK_PCM_QUEUE_BYTES && q.count%4==0 && q.start%4==0);
    }
    free(input);free(output);return 0;
}
