#include <stddef.h>

int bk_tj_load(const char *path);
int bk_tj_encode(const unsigned char *pixels, int width, int height, int quality,
                 unsigned char **data, unsigned long *size, char *error, size_t error_size);
int bk_tj_decode(const unsigned char *data, size_t size, unsigned char *pixels,
                 int width, int height, char *error, size_t error_size);
void bk_tj_free(unsigned char *data);
