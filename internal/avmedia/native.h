#ifndef ANYKVM_AVMEDIA_H
#define ANYKVM_AVMEDIA_H
#include <stdint.h>
#include <stddef.h>
typedef struct bm_session bm_session;
typedef struct bm_media bm_media;
bm_media *bm_media_alloc(void);
int bm_media_available(void);
int bm_has_encoder(const char *);
int bm_media_open(bm_media*,const char*,char*,size_t);
int bm_media_read(bm_media*,uint8_t**,int*,int*,int*,int*,int64_t*,int64_t*,uint8_t**,int*,int*,int*,char*,size_t);
int64_t bm_media_duration(bm_media*);
int bm_media_seek(bm_media*,int64_t,char*,size_t);
void bm_media_interrupt(bm_media*);
void bm_media_close(bm_media*);
int bm_load(const char *dir,char *err,size_t len);
bm_session *bm_encoder(const char *name,const char *device,int w,int h,int fps_num,int fps_den,int bitrate,int audio_ms,char *err,size_t len);
bm_session *bm_decoder(const char *name,char *err,size_t len);
bm_session *bm_jpeg_encoder(int w,int h,int quality,char *err,size_t len);
bm_session *bm_audio_decoder(const char *name,const uint8_t *config,size_t size,int rate,int channels,char *err,size_t len);
bm_session *bm_source(const char *format,const char *url,const char *options,int audio,const char *passthrough,char *err,size_t len);
int bm_encode(bm_session*,const uint8_t*,size_t,int,int,const char*,int,uint8_t**,int*,char*,size_t);
int bm_decode(bm_session*,const uint8_t*,size_t,uint8_t**,int*,int*,int*,char*,size_t);
int bm_read(bm_session*,uint8_t**,int*,int*,int*,char*,size_t);
int bm_source_codec(bm_session*);
void bm_source_rate(bm_session*,int*,int*);
void bm_interrupt(bm_session*);
void bm_close(bm_session*);
char *bm_devices(const char *format);
#endif
