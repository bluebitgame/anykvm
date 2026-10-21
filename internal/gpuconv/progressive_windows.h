#ifndef BK_PROGRESSIVE_WINDOWS_H
#define BK_PROGRESSIVE_WINDOWS_H
#include <stdint.h>
#include <stddef.h>
typedef struct bk_d3d_progressive bk_d3d_progressive;
bk_d3d_progressive *bk_d3d_progressive_new(const char *,size_t,uint64_t,int,char *,size_t);
int bk_d3d_progressive_run(bk_d3d_progressive *,const uint8_t *,size_t,const uint32_t *,uint8_t **,size_t *,uint32_t *,char *,size_t);
void bk_d3d_progressive_free(bk_d3d_progressive *);
int bk_d3d_progressive_surface_init(bk_d3d_progressive *,const char *,size_t,char *,size_t);
int bk_d3d_progressive_surface_prepare(bk_d3d_progressive *,uintptr_t,const uint32_t *,uint32_t *,char *,size_t);
int bk_d3d_progressive_surface_commit(bk_d3d_progressive *,char *,size_t);
#endif
