#ifndef BK_V4L2_CAPTURE_H
#define BK_V4L2_CAPTURE_H
#include <stddef.h>
#include <stdint.h>
typedef struct bk_v4l_capture bk_v4l_capture;
char *bk_v4l_modes(const char *,char *,size_t);
int bk_v4l_candidate(const char *);
bk_v4l_capture *bk_v4l_open(const char *,int,int,double,const char *,char *,size_t);
void bk_v4l_close(bk_v4l_capture *);
int bk_v4l_width(bk_v4l_capture *);
int bk_v4l_height(bk_v4l_capture *);
int bk_v4l_rate_num(bk_v4l_capture *);
int bk_v4l_rate_den(bk_v4l_capture *);
const char *bk_v4l_format(bk_v4l_capture *);
int bk_v4l_read(bk_v4l_capture *,int,uint8_t **,size_t *,int *,char *,size_t);
int bk_v4l_done(bk_v4l_capture *,int,char *,size_t);
int bk_v4l_convert(bk_v4l_capture *,uint8_t *,size_t,uint8_t *,size_t,char *,size_t);
int bk_v4l_nv12(bk_v4l_capture *,int,size_t,int *,unsigned *,size_t *);
void bk_v4l_keyframe(bk_v4l_capture *);
#endif
