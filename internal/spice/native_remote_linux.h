#ifndef ANYKVM_SPICE_NATIVE_REMOTE_LINUX_H
#define ANYKVM_SPICE_NATIVE_REMOTE_LINUX_H

#include "native_bitmap.h"

// All access to the borrowed primary surface stays on the GLib thread. Damage
// is coalesced until polling; consumers receive independent full-frame copies.
static int bitmap_pixel_bits(int format) {
  switch (format) {
  case SPICE_SURFACE_FMT_32_xRGB:
  case SPICE_SURFACE_FMT_32_ARGB:
    return 32;
  case SPICE_SURFACE_FMT_16_555:
    return 15;
  case SPICE_SURFACE_FMT_16_565:
    return 16;
  default:
    return 0;
  }
}

static void bitmap_create(SpiceChannel *channel, gint format, gint width,
                          gint height, gint stride, gint shmid, gpointer pixels,
                          gpointer data) {
  (void)channel;
  (void)shmid;
  bk_spice *s = data;
  int bits = bitmap_pixel_bits(format);
  int bytes = bits == 32 ? 4 : 2;
  int64_t row_bytes = stride < 0 ? -(int64_t)stride : stride;
  if (!pixels || width < 1 || height < 1 || width > 8192 || height > 8192 ||
      row_bytes < (int64_t)width * bytes || !bits) {
    fail(s, "invalid SPICE primary surface format, dimensions or stride");
    return;
  }
  s->frame.width = width;
  s->frame.height = height;
  s->bitmap_format = format;
  s->bitmap_stride = stride;
  s->bitmap_data = pixels;
  s->bitmap_dirty = 1;
  s->bitmap_reset = 0;
}
static void bitmap_destroy(SpiceChannel *channel, gpointer data) {
  (void)channel;
  bk_spice *s = data;
  s->bitmap_data = NULL;
  s->bitmap_dirty = 0;
  s->bitmap_reset = 1;
  s->frame.width = s->frame.height = 0;
}
static void bitmap_invalidate(SpiceChannel *channel, gint x, gint y, gint width,
                              gint height, gpointer data) {
  (void)channel;
  (void)x;
  (void)y;
  (void)width;
  (void)height;
  bk_spice *s = data;
  if (s->bitmap_data)
    s->bitmap_dirty = 1;
}
typedef struct {
  bk_spice *s;
  bk_spice_bitmap *bitmap;
  char *error;
  size_t cap;
  int result;
} bitmap_poll_job;
static void next_bitmap(void *arg) {
  bitmap_poll_job *j = arg;
  bk_spice *s = j->s;
  if (s->failed) {
    snprintf(j->error, j->cap, "%s", s->error);
    j->result = BK_SPICE_BITMAP_ERROR;
    return;
  }
  if (s->bitmap_reset) {
    s->bitmap_reset = 0;
    j->result = BK_SPICE_BITMAP_RESET; // the previous primary and its cached
                                       // snapshot are invalid
    return;
  }
  if (!s->bitmap_dirty || !s->bitmap_data)
    return;
  size_t size = (size_t)s->frame.width * s->frame.height * 4;
  uint8_t *pixels = g_try_malloc(size);
  if (!pixels) {
    snprintf(j->error, j->cap, "cannot allocate SPICE bitmap snapshot");
    j->result = BK_SPICE_BITMAP_ERROR;
    return;
  }
  int bits = bitmap_pixel_bits(s->bitmap_format);
  bk_spice_bitmap_copy(pixels, s->bitmap_data, s->frame.width, s->frame.height,
                       s->bitmap_stride, bits);
  *j->bitmap = (bk_spice_bitmap){.data = pixels,
                                 .size = size,
                                 .width = s->frame.width,
                                 .height = s->frame.height};
  s->bitmap_dirty = 0;
  j->result = BK_SPICE_BITMAP_FRAME;
}
int bk_spice_bitmap_next(bk_spice *s, bk_spice_bitmap *bitmap, char *error,
                         size_t cap) {
  bitmap_poll_job j = {.s = s,
                       .bitmap = bitmap,
                       .error = error,
                       .cap = cap,
                       .result = BK_SPICE_BITMAP_IDLE};
  call(next_bitmap, &j);
  return j.result;
}
void bk_spice_bitmap_release(bk_spice_bitmap *bitmap) {
  g_free(bitmap->data);
  bitmap->data = NULL;
}
#endif
