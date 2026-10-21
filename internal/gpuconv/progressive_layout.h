#ifndef BK_PROGRESSIVE_LAYOUT_H
#define BK_PROGRESSIVE_LAYOUT_H
#include <stddef.h>
#include <stdint.h>
#define BK_PROGRESSIVE_SEGMENTS 48u
#define BK_PROGRESSIVE_SEGMENT_BYTES 32768u
#define BK_PROGRESSIVE_INFO_BYTES (112u * 4u)
// Validate before allocating or dispatching, even when called outside Go.
static int bk_progressive_validate(const uint32_t *m, size_t input_bytes) {
    if (!m || m[0]<1 || m[0]>8 || m[1]>1 || m[2]<6 || m[2]>15 || m[4]>2 ||
        m[5]<1 || m[5]>(128u*1024*1024)/(12288*4) ||
        (m[4] && (m[3]<1 || m[3]>4))) return 0;
    uint32_t limit=m[5];
    if (m[6]) {
        uint32_t width=m[6]/4;
        if (m[6]%4||width<1||width>8192||m[7]<1||m[7]>8192||
            (uint64_t)m[6]*m[7]>64u*1024*1024||m[1]||m[4]==2) return 0;
        limit=((width+63)/64)*((m[7]+63)/64);
        if (m[4]&&m[5]!=limit) return 0;
    }
    if (input_bytes != (m[4]==2||m[6]?4:m[0]*(m[1]?6144:16384))) return 0;
    for (uint32_t i=0;i<m[0];i++) {
        if (m[8+i]>=limit) return 0;
        for (uint32_t j=0;j<i;j++) if (m[8+i]==m[8+j]) return 0;
    }
    return 1;
}
static int bk_progressive_surface_validate(const uint32_t *p) {
    return p&&p[0]>=1&&p[0]<=8192&&p[1]>=1&&p[1]<=8192&&
        (uint64_t)p[0]*p[1]<=64u*1024*1024/4&&
        (p[3]==0||p[3]==90||p[3]==180||p[3]==270)&&
        p[4]>=1&&p[4]<=8192&&p[5]>=1&&p[5]<=8192&&
        (uint64_t)p[4]*p[5]<=64u*1024*1024/4&&
        p[8]>=1&&p[8]<=p[4]&&p[9]>=1&&p[9]<=p[5]&&
        p[6]<=p[4]-p[8]&&p[7]<=p[5]-p[9]&&p[10]<=1;
}
#endif
