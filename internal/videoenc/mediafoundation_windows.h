#ifndef ANYKVM_MEDIAFOUNDATION_WINDOWS_H
#define ANYKVM_MEDIAFOUNDATION_WINDOWS_H
#include <stddef.h>
#include <stdint.h>

typedef struct bk_mf_encoder bk_mf_encoder;
int bk_mf_supports(int codec);
bk_mf_encoder *bk_mf_open(int width, int height, int rate_num, int rate_den,
                         int bitrate, int codec, int hardware, char *error, size_t capacity);
int bk_mf_encode(bk_mf_encoder *encoder, const uint8_t *nv12, size_t size,
                 int force_idr, uint8_t **output, size_t *output_size,
                 char *error, size_t capacity);
int bk_mf_encode_sample(bk_mf_encoder *,void *,int,uint8_t **,size_t *,char *,size_t);
const uint8_t *bk_mf_header(bk_mf_encoder *encoder, size_t *size);
const uint8_t *bk_mf_probe(bk_mf_encoder *encoder, size_t *size);
const char *bk_mf_name(bk_mf_encoder *encoder);
void bk_mf_close(bk_mf_encoder *encoder);
#endif
