// Exercise the production DXGI metadata reader with a synthetic duplication API.
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <limits.h>
typedef uint32_t UINT;
typedef int32_t LONG,HRESULT;
#define FAILED(hr) ((hr)<0)
typedef struct { LONG left,top,right,bottom; } RECT,bk_win_rect;
typedef struct { LONG x,y; } POINT;
typedef struct { POINT SourcePoint; RECT DestinationRect; } DXGI_OUTDUPL_MOVE_RECT;
typedef struct {UINT TotalMetadataBufferSize;} DXGI_OUTDUPL_FRAME_INFO;
typedef struct {void *duplication;} bk_win_capture;
typedef struct {bk_win_rect source,destination;} bk_win_move;
typedef struct {const bk_win_move *moves;size_t move_count;const bk_win_rect *rects;size_t count;int width,height,valid;} damage;
typedef struct {damage damage;bk_win_rect *damage_rects;bk_win_move *damage_moves;} bk_win_frame;
DXGI_OUTDUPL_MOVE_RECT moves[1]={{{10,20},{70,80,100,110}}};
RECT dirties[1]={{1,2,3,4}};
int failure=0;
HRESULT IDXGIOutputDuplication_GetFrameMoveRects(void *p,UINT size,DXGI_OUTDUPL_MOVE_RECT *out,UINT *bytes){
 *bytes=sizeof(moves);if(failure==1)return -1;
 if(size<*bytes)return -1;memcpy(out,moves,*bytes);return 0;
}
HRESULT IDXGIOutputDuplication_GetFrameDirtyRects(void *p,UINT size,RECT *out,UINT *bytes){
 *bytes=sizeof(dirties);if(failure==2)return -1;
 if(failure==3){*bytes=size+1;return 0;}
 if(size<*bytes)return -1;memcpy(out,dirties,*bytes);return 0;
}
#include "desktop_damage.inc"
int main(void){
 bk_win_capture capture={0};DXGI_OUTDUPL_FRAME_INFO info={sizeof(moves)+sizeof(dirties)};
 bk_win_frame frame={0};
 desktop_damage(&capture,&frame,&info,193,131);
 if(!frame.damage.valid||frame.damage.count!=3||frame.damage.width!=193||frame.damage.height!=131)return 1;
 RECT expected[3]={{70,80,100,110},{10,20,40,50},{1,2,3,4}};
 if(memcmp(expected,frame.damage.rects,sizeof(expected)))return 2;
 if(frame.damage.move_count!=1 || memcmp(&frame.damage.moves[0].source,&expected[1],sizeof(RECT)) || memcmp(&frame.damage.moves[0].destination,&expected[0],sizeof(RECT)))return 7;
 free(frame.damage_rects);free(frame.damage_moves);
 for(int mode=1;mode<=3;mode++){
  memset(&frame,0,sizeof(frame));failure=mode;
  desktop_damage(&capture,&frame,&info,193,131);
  if(frame.damage.valid||frame.damage_rects||frame.damage.count)return 3;
 }
 failure=0;moves[0].SourcePoint.x=INT_MAX;
 memset(&frame,0,sizeof(frame));desktop_damage(&capture,&frame,&info,193,131);
 if(frame.damage.valid||frame.damage_rects)return 4;
 moves[0].SourcePoint.x=10;dirties[0].right=194;
 memset(&frame,0,sizeof(frame));desktop_damage(&capture,&frame,&info,193,131);
 if(frame.damage.valid||frame.damage_rects)return 5;
 memset(&frame,0,sizeof(frame));info.TotalMetadataBufferSize=0;
 desktop_damage(&capture,&frame,&info,193,131);
 if(frame.damage.valid||frame.damage_rects)return 6;
 return 0;
}
