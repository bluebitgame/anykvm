#ifndef ANYKVM_SCREEN_DAMAGE_H
#define ANYKVM_SCREEN_DAMAGE_H
#include <stdint.h>
typedef struct { int x0, y0, x1, y1; } bk_screen_dirty_rect;
typedef struct {
    uint64_t stream, previous, sequence;
    int valid, count;
    const bk_screen_dirty_rect *rects;
} bk_screen_damage;
void bk_screen_set_damage(void *screen, int enabled);
int bk_screen_lease_damage(void *lease, bk_screen_damage *out);
#endif
