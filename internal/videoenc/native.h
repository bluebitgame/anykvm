#ifndef ANYKVM_VIDEO_ENCODER_H
#define ANYKVM_VIDEO_ENCODER_H
#include <stdint.h>
#include <stddef.h>
typedef struct bk_encoder bk_encoder;
bk_encoder *bk_encoder_open(const char *backend, const char *device, int width, int height, int fps_num, int fps_den, int bitrate, int hevc, int disable_low_latency, int h264_profile, char *error, size_t error_size);
int bk_encoder_submit(bk_encoder *, const uint8_t *data, int w, int h, int format, uint64_t id, int force_idr, char *error, size_t error_size);
int bk_encoder_submit_pixel(bk_encoder *, void *pixel, uint64_t id, int force_idr, char *error, size_t error_size);
int bk_encoder_receive(bk_encoder *, void **lease, const uint8_t **data, size_t *size, uint64_t *id, int *dropped, char *error, size_t error_size);
void bk_encoder_packet_release(void *lease);
int bk_encoder_flush(bk_encoder *, char *error, size_t error_size);
int bk_encoder_ready(bk_encoder *, char *error, size_t error_size);
void bk_encoder_close(bk_encoder *);
#endif
