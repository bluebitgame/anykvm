#ifndef ANYKVM_SPICE_H
#define ANYKVM_SPICE_H

#include <stddef.h>
#include <stdint.h>

typedef struct bk_spice bk_spice;

// A successful next() lends both the FD and the upstream draw until done().
// The caller must finish GPU reads before done(); duplicating the FD is not
// enough.
typedef struct {
  int fd, flip_y;
  uint32_t width, height, format, pitch, offset, planes;
  uint32_t pitches[4], offsets[4], device_major, device_minor;
  uint64_t modifier;
} bk_spice_frame;

// Audio and cursor polling return owned buffers; release with the matching API.
typedef struct {
  uint8_t *data;
  size_t size;
  uint32_t rate, channels;
  uint64_t epoch, sample;
  uint16_t volume[2];
  int muted;
} bk_spice_audio;

typedef struct {
  uint8_t *rgba;
  uint32_t width, height, hot_x, hot_y;
  uint64_t serial;
  int known, visible, system_default, premultiplied;
} bk_spice_cursor;

// Lifecycle. All entry points dispatch synchronously to the owning GLib thread.
bk_spice *bk_spice_open(const char *path, const char *password,
                        const char *render_node, char *error, size_t cap);
void bk_spice_close(bk_spice *session);

// Video polling: -1 = failure, 0 = idle, 1 = borrowed frame.
int bk_spice_next(bk_spice *session, bk_spice_frame *frame, char *error,
                  size_t cap);
void bk_spice_done(bk_spice *session);

// Playback polling: -1 = failure, 0 = idle, 1 = owned PCM packet.
void bk_spice_audio_enable(bk_spice *session, int enabled);
int bk_spice_audio_next(bk_spice *session, bk_spice_audio *packet, char *error,
                        size_t cap);
int bk_spice_audio_available(bk_spice *session);
void bk_spice_audio_release(bk_spice_audio *packet);

// Cursor snapshots include visibility and a serial even when no pixels are
// present.
void bk_spice_cursor_get(bk_spice *session, bk_spice_cursor *cursor);
void bk_spice_cursor_release(bk_spice_cursor *cursor);

// Values match the Go input constants in input.go.
enum {
  BK_SPICE_KEY_PRESS,
  BK_SPICE_KEY_RELEASE,
  BK_SPICE_POSITION,
  BK_SPICE_MOTION,
  BK_SPICE_BUTTON_PRESS,
  BK_SPICE_BUTTON_RELEASE,
};
int bk_spice_input(bk_spice *session, int kind, int a, int b, int buttons);
#endif
