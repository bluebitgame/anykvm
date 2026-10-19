#ifndef ANYKVM_AIRPLAY_VIDEO_H
#define ANYKVM_AIRPLAY_VIDEO_H
#include <stddef.h>
#include <stdint.h>
typedef struct anykvm_airplay_video anykvm_airplay_video;
anykvm_airplay_video *anykvm_airplay_video_new(void);
int anykvm_airplay_video_configure(anykvm_airplay_video *,const uint8_t *,size_t,const uint8_t *,size_t);
int anykvm_airplay_video_decode(anykvm_airplay_video *,const uint8_t *,size_t,uint8_t **,int *,int *);
void anykvm_airplay_video_close(anykvm_airplay_video *);
#endif
