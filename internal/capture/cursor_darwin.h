#ifndef CURSOR_DARWIN_H
#define CURSOR_DARWIN_H

#include <stdint.h>

typedef struct {
    uint8_t *pixels;
    int      width;
    int      height;
    int      hot_x;
    int      hot_y;
    int      source_width;
    int      source_height;
} cursor_result_t;

int              cursor_available(void);
cursor_result_t *cursor_capture(void);
cursor_result_t *cursor_capture_native(void);
void             cursor_free(cursor_result_t *cr);

#endif
