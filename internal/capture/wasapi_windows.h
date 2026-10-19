#ifndef BK_WASAPI_WINDOWS_H
#define BK_WASAPI_WINDOWS_H
#include <stdint.h>
#include <stddef.h>
typedef struct bk_wasapi bk_wasapi;
bk_wasapi *bk_wasapi_open(const char *selector,char *error,size_t capacity);
size_t bk_wasapi_read(bk_wasapi *,uint8_t *,size_t,uint64_t *epoch);
int bk_wasapi_available(bk_wasapi *);
void bk_wasapi_close(bk_wasapi *);
char *bk_wasapi_devices(char *,size_t);
#endif
