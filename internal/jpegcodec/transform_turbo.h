#include <stddef.h>
typedef struct { int x,y,w,h; unsigned char *data; unsigned long size; } bk_jpeg_tile;
int bk_jt_load(const char *path);
int bk_jt_tiles(const unsigned char *data, size_t size, int tile_size,
                bk_jpeg_tile **tiles, int *count, int *width, int *height, char *error, size_t error_size);
void bk_jt_tiles_free(bk_jpeg_tile *tiles, int count);
