#ifndef BK_CAPTURE_WINDOWS_H
#define BK_CAPTURE_WINDOWS_H
#include <stdint.h>
#include <stddef.h>
typedef struct bk_win_capture bk_win_capture;
typedef struct bk_win_frame bk_win_frame;
typedef struct {
    int64_t present, acquire_start, acquired, copy_submitted, now, frequency;
    int repeated;
    uint32_t accumulated_frames;
} bk_win_timing;
void bk_win_frame_timing(bk_win_frame *, bk_win_timing *);
uint64_t bk_win_process_cpu_100ns(void);
bk_win_capture *bk_win_open(const char *, int, int, int, int, int, char *, size_t);
bk_win_capture *bk_win_open_video(const char *, int, int, double, const char *, int, int, char *, size_t);
char *bk_win_modes(const char *, char *, size_t);
const char *bk_win_input_format(bk_win_capture *);
bk_win_frame *bk_win_next(bk_win_capture *, int, char *, size_t);
bk_win_frame *bk_win_repeat(bk_win_capture *);
void bk_win_close(bk_win_capture *);
void bk_win_frame_free(bk_win_frame *);
bk_win_frame *bk_win_frame_snapshot(bk_win_frame *,char *,size_t);
uintptr_t bk_win_frame_handle(bk_win_frame *);
int bk_win_frame_width(bk_win_frame *);
int bk_win_frame_height(bk_win_frame *);
int bk_win_frame_rotation(bk_win_frame *);
int bk_win_frame_pitch(bk_win_frame *);
int bk_win_frame_kind(bk_win_frame *);
void *bk_win_frame_sample(bk_win_frame *);
int bk_win_frame_borrow(bk_win_frame *,uint8_t **,size_t *,int *,char *,size_t);
int bk_win_frame_data(bk_win_frame *, uint8_t **, size_t *, char *, size_t);
int bk_win_rate_num(bk_win_capture *);
int bk_win_rate_den(bk_win_capture *);
int bk_win_adapter(bk_win_capture *, uint64_t *, uint32_t *);
char *bk_win_devices(int, char *, size_t);
#endif
