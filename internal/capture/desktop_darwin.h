#include <stdint.h>

typedef struct {
    int width, height, pixel_width, pixel_height, usable;
    double refresh_rate;
} bk_display_mode;

void *bk_display_open(int index, char *error, int error_size);
uint32_t bk_display_id(void *handle);
uint32_t bk_display_index_id(int index);
int bk_display_modes(void *handle);
int bk_display_mode_at(void *handle, int index, bk_display_mode *mode);
int bk_display_current(void *handle, bk_display_mode *mode);
int bk_display_set(void *handle, int index, char *error, int error_size);
int bk_display_restore(void *handle, char *error, int error_size);
void bk_display_free(void *handle);
