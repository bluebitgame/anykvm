#ifndef ANYKVM_V4L2_ENCODER_H
#define ANYKVM_V4L2_ENCODER_H
#include <stddef.h>
#include <stdint.h>

typedef struct bk_pi_encoder bk_pi_encoder;
int bk_pi_candidate(const char *path);
bk_pi_encoder *bk_pi_open(const char *path, int width, int height,
                         int fps_num, int fps_den, int bitrate,
                         char *error, size_t error_size);
int bk_pi_encode(bk_pi_encoder *encoder, const uint8_t *nv12, size_t input_size,
                 int force_idr, uint8_t **output, size_t *output_size,
                 char *error, size_t error_size);
bk_pi_encoder *bk_pi_open_dma(const char *,int,int,int,int,int,unsigned,char *,size_t);
int bk_pi_encode_dma(bk_pi_encoder *,int,size_t,int,uint8_t **,size_t *,char *,size_t);
void bk_pi_close(bk_pi_encoder *encoder);
#endif
