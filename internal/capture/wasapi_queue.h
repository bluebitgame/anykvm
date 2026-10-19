#ifndef BK_WASAPI_QUEUE_H
#define BK_WASAPI_QUEUE_H
#include <stdint.h>
#include <stddef.h>
#include <string.h>
#define BK_PCM_QUEUE_BYTES (48000u * 4u / 10u)
typedef struct {
    uint8_t data[BK_PCM_QUEUE_BYTES];
    size_t start, count;
    uint64_t epoch;
} bk_pcm_queue;
static inline void bk_pcm_queue_reset(bk_pcm_queue *q) {
    q->start = q->count = 0;
    q->epoch++;
}
// All queue operations require the caller's lock. Every unit is a complete
// stereo S16LE frame, so wrapping and overflow cannot separate sample channels.
static inline int bk_pcm_queue_push(bk_pcm_queue *q, const uint8_t *data, size_t size, int silent,
                                    int discontinuity) {
    if (size % 4 || (!silent && !data && size))
        return -1;
    if (discontinuity)
        bk_pcm_queue_reset(q);
    if (size > BK_PCM_QUEUE_BYTES) {
        size_t skip = size - BK_PCM_QUEUE_BYTES;
        if (data)
            data += skip;
        size = BK_PCM_QUEUE_BYTES;
        bk_pcm_queue_reset(q);
    }
    if (q->count + size > BK_PCM_QUEUE_BYTES) {
        size_t drop = q->count + size - BK_PCM_QUEUE_BYTES;
        q->start = (q->start + drop) % BK_PCM_QUEUE_BYTES;
        q->count -= drop;
        q->epoch++;
    }
    size_t offset = (q->start + q->count) % BK_PCM_QUEUE_BYTES;
    size_t first = BK_PCM_QUEUE_BYTES - offset;
    if (first > size)
        first = size;
    if (silent) {
        memset(q->data + offset, 0, first);
        memset(q->data, 0, size - first);
    } else if (size) {
        memcpy(q->data + offset, data, first);
        memcpy(q->data, data + first, size - first);
    }
    q->count += size;
    return 0;
}
static inline size_t bk_pcm_queue_read(bk_pcm_queue *q, uint8_t *data, size_t capacity,
                                       uint64_t *epoch) {
    size_t size = q->count < capacity ? q->count : capacity;
    size -= size % 4;
    size_t first = BK_PCM_QUEUE_BYTES - q->start;
    if (first > size)
        first = size;
    memcpy(data, q->data + q->start, first);
    memcpy(data + first, q->data, size - first);
    q->start = (q->start + size) % BK_PCM_QUEUE_BYTES;
    q->count -= size;
    *epoch = q->epoch;
    return size;
}
#endif
