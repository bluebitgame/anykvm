#include "internal/spice/native_linux.h"
#include <assert.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

// Minimal GLib/channel stand-ins keep the production callbacks runnable on the
// build host. Only scheduling and allocation are substituted.
typedef int gint;
typedef void *gpointer;
typedef struct {
} SpiceChannel;
enum {
  SPICE_SURFACE_FMT_32_xRGB = 32,
  SPICE_SURFACE_FMT_32_ARGB = 96,
  SPICE_SURFACE_FMT_16_555 = 16,
  SPICE_SURFACE_FMT_16_565 = 80
};
struct bk_spice {
  bk_spice_frame frame;
  int failed, bitmap_dirty, bitmap_reset, bitmap_format, bitmap_stride;
  uint8_t *bitmap_data;
  char error[512];
};
#define g_try_malloc malloc
#define g_free free
static void fail(bk_spice *s, const char *error) {
  s->failed = 1;
  snprintf(s->error, sizeof(s->error), "%s", error);
}
static void call(void (*fn)(void *), void *arg) { fn(arg); }
#include "internal/spice/native_remote_linux.h"

int main(void) {
  bk_spice s = {0};
  bk_spice_bitmap first = {0}, second = {0};
  char error[512] = {0};
  assert(bk_spice_bitmap_next(&s, &first, error, sizeof(error)) ==
         BK_SPICE_BITMAP_IDLE);
  // Polling takes owned snapshots and coalesces intervening damage.
  uint32_t *pixels = malloc(8);
  assert(pixels);
  pixels[0] = 0x00112233;
  pixels[1] = 0x00445566;
  bitmap_create(NULL, SPICE_SURFACE_FMT_32_xRGB, 2, 1, 8, -1, pixels, &s);
  assert(bk_spice_bitmap_next(&s, &first, error, sizeof(error)) ==
         BK_SPICE_BITMAP_FRAME);
  assert(first.width == 2 && first.height == 1 && first.size == 8);
  assert(first.data[0] == 0x33 && first.data[3] == 255);
  pixels[0] = 0x00778899;
  bitmap_invalidate(NULL, 0, 0, 1, 1, &s);
  bitmap_invalidate(NULL, 1, 0, 1, 1, &s);
  assert(bk_spice_bitmap_next(&s, &second, error, sizeof(error)) ==
         BK_SPICE_BITMAP_FRAME);
  assert(second.data[0] == 0x99 && first.data[0] == 0x33);
  bk_spice_bitmap_release(&second);
  assert(bk_spice_bitmap_next(&s, &second, error, sizeof(error)) ==
         BK_SPICE_BITMAP_IDLE);

  // Removal invalidates the primary without invalidating prior snapshots.
  bitmap_destroy(NULL, &s);
  free(pixels); // any following poll must not touch the old allocation
  assert(!s.bitmap_data && !s.frame.width && !s.frame.height);
  bitmap_invalidate(NULL, 0, 0, 1, 1, &s);
  assert(bk_spice_bitmap_next(&s, &second, error, sizeof(error)) ==
         BK_SPICE_BITMAP_RESET);
  assert(bk_spice_bitmap_next(&s, &second, error, sizeof(error)) ==
         BK_SPICE_BITMAP_IDLE);
  assert(first.data[0] == 0x33);
  bk_spice_bitmap_release(&first);

  uint16_t rgb = 0xf800;
  bitmap_create(NULL, SPICE_SURFACE_FMT_16_565, 1, 1, 2, -1, &rgb, &s);
  assert(bk_spice_bitmap_next(&s, &first, error, sizeof(error)) ==
         BK_SPICE_BITMAP_FRAME);
  assert(first.data[2] == 255 && first.data[3] == 255);
  bk_spice_bitmap_release(&first);
  // An immediate replacement supersedes a pending removal notification.
  bitmap_destroy(NULL, &s);
  bitmap_create(NULL, SPICE_SURFACE_FMT_16_565, 1, 1, 2, -1, &rgb, &s);
  assert(bk_spice_bitmap_next(&s, &first, error, sizeof(error)) ==
         BK_SPICE_BITMAP_FRAME);
  bk_spice_bitmap_release(&first);
  bitmap_destroy(NULL, &s);

  // Reject invalid primary metadata before reading any pixels.
  bk_spice bad = {0};
  bitmap_create(NULL, SPICE_SURFACE_FMT_16_565, 2, 1, 2, -1, &rgb, &bad);
  assert(bad.failed);
  assert(bk_spice_bitmap_next(&bad, &first, error, sizeof(error)) ==
         BK_SPICE_BITMAP_ERROR);
  assert(error[0]);
  bad = (bk_spice){0};
  bitmap_create(NULL, SPICE_SURFACE_FMT_32_xRGB, 8193, 1, 8193 * 4, -1, &rgb,
                &bad);
  assert(bad.failed);
  bad = (bk_spice){0};
  bitmap_create(NULL, 999, 1, 1, 4, -1, &rgb, &bad);
  assert(bad.failed);
  return 0;
}
