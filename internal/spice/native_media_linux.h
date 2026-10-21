#ifndef ANYKVM_SPICE_NATIVE_MEDIA_LINUX_H
#define ANYKVM_SPICE_NATIVE_MEDIA_LINUX_H
// Included after bk_spice is defined. All callbacks and polling jobs run on
// the same GLib context; audio/cursor state never races with channel teardown.
static void audio_clear(bk_spice *s) {
  bk_spice_audio *packet;
  while ((packet = g_queue_pop_head(&s->audio_queue))) {
    g_free(packet->data);
    g_free(packet);
  }
  s->audio_bytes = 0;
}
static void audio_fail(bk_spice *s, const char *reason) {
  audio_clear(s);
  s->audio_active = 0;
  s->audio_failed = 1;
  s->audio_reported = 0;
  snprintf(s->audio_error, sizeof(s->audio_error), "%s", reason);
}
static void playback_start(SpicePlaybackChannel *channel, gint format,
                           gint channels, gint rate, gpointer data) {
  (void)channel;
  bk_spice *s = data;
  audio_clear(s);
  s->audio_epoch++;
  s->audio_samples = 0;
  s->audio_failed = 0;
  s->audio_active = 0;
  if (format != SPICE_AUDIO_FMT_S16 || (channels != 1 && channels != 2) ||
      rate < 8000 || rate > 192000) {
    audio_fail(s, "unsupported SPICE playback format (requires S16 "
                  "mono/stereo, 8..192 kHz)");
    return;
  }
  s->audio_channels = channels;
  s->audio_rate = rate;
  s->audio_active = 1;
}
static void playback_stop(SpicePlaybackChannel *channel, gpointer data) {
  (void)channel;
  bk_spice *s = data;
  audio_clear(s);
  s->audio_active = 0;
  s->audio_samples = 0;
  s->audio_epoch++;
}
static void playback_data(SpicePlaybackChannel *channel, gpointer pcm,
                          gint size, gpointer data) {
  (void)channel;
  bk_spice *s = data;
  if (!s->audio_active || s->audio_failed)
    return;
  size_t budget = (size_t)s->audio_rate * s->audio_channels * 2 / 5; // 200 ms
  if (!pcm || size <= 0 || (size_t)size > budget ||
      size % (2 * s->audio_channels)) {
    audio_fail(s, "invalid or oversized SPICE playback PCM packet");
    return;
  }
  uint64_t first = s->audio_samples;
  s->audio_samples += size / (2 * s->audio_channels);
  if (!s->audio_enabled)
    return;
  // Keep latency/memory bounded when no audio consumer is reading. A dropped
  // packet leaves a sample-number gap, which resets Go's interpolator.
  while (s->audio_bytes + (size_t)size > budget ||
         s->audio_queue.length >= 32) {
    bk_spice_audio *old = g_queue_pop_head(&s->audio_queue);
    s->audio_bytes -= old->size;
    g_free(old->data);
    g_free(old);
  }
  bk_spice_audio *packet = g_new0(bk_spice_audio, 1);
  packet->data = g_memdup2(pcm, size);
  packet->size = size;
  packet->rate = s->audio_rate;
  packet->channels = s->audio_channels;
  packet->epoch = s->audio_epoch;
  packet->sample = first;
  g_queue_push_tail(&s->audio_queue, packet);
  s->audio_bytes += size;
}

typedef struct {
  bk_spice *s;
  int enabled;
} audio_enable_job;
static void set_audio(void *arg) {
  audio_enable_job *j = arg;
  audio_clear(j->s);
  j->s->audio_enabled = j->enabled;
  j->s->audio_samples = 0;
  j->s->audio_epoch++;
}
void bk_spice_audio_enable(bk_spice *s, int enabled) {
  audio_enable_job j = {s, !!enabled};
  call(set_audio, &j);
}
typedef struct {
  bk_spice *s;
  bk_spice_audio *packet;
  char *error;
  size_t cap;
  int result;
} audio_poll_job;
static void next_audio(void *arg) {
  audio_poll_job *j = arg;
  bk_spice *s = j->s;
  if (s->failed) {
    snprintf(j->error, j->cap, "%s", s->error);
    j->result = -1;
    return;
  }
  if (s->audio_failed) {
    if (!s->audio_reported) {
      snprintf(j->error, j->cap, "%s", s->audio_error);
      j->result = -1;
      s->audio_reported = 1;
    }
    return;
  }
  if (!s->audio_enabled || !s->audio_active)
    return;
  bk_spice_audio *packet = g_queue_pop_head(&s->audio_queue);
  if (!packet)
    return;
  s->audio_bytes -= packet->size;
  *j->packet = *packet; // transfer owned PCM bytes to the caller
  g_free(packet);
  j->packet->volume[0] = j->packet->volume[1] = 65535;
  gboolean mute = FALSE;
  guint channels = 0;
  guint16 *volume = NULL;
  if (s->playback) {
    g_object_get(s->playback, "mute", &mute, "nchannels", &channels, "volume",
                 &volume, NULL);
    // volume is borrowed from the channel and is only read on this thread.
    if (volume && channels >= 1) {
      j->packet->volume[0] = volume[0];
      j->packet->volume[1] = channels >= 2 ? volume[1] : volume[0];
    }
  }
  j->packet->muted = mute;
  j->result = 1;
}
int bk_spice_audio_next(bk_spice *s, bk_spice_audio *packet, char *error,
                        size_t cap) {
  audio_poll_job j = {.s = s, .packet = packet, .error = error, .cap = cap};
  call(next_audio, &j);
  return j.result;
}
static void audio_available(void *arg) {
  audio_enable_job *j = arg;
  j->enabled = j->s->audio_enabled && j->s->audio_active &&
               !j->s->audio_failed && !j->s->failed;
}
int bk_spice_audio_available(bk_spice *s) {
  audio_enable_job j = {.s = s};
  call(audio_available, &j);
  return j.enabled;
}
void bk_spice_audio_release(bk_spice_audio *packet) {
  g_free(packet->data);
  packet->data = NULL;
}

static void cursor_changed(GObject *object, GParamSpec *property,
                           gpointer data) {
  (void)property;
  bk_spice *s = data;
  SpiceCursorShape *shape = NULL;
  g_object_get(object, "cursor", &shape, NULL);
  bk_spice_cursor *cursor = &s->cursor_state;
  g_clear_pointer(&cursor->rgba, g_free);
  cursor->known = cursor->visible = 1;
  cursor->serial++;
  cursor->system_default = 1;
  cursor->width = cursor->height = cursor->hot_x = cursor->hot_y = 0;
  // spice-client-glib normalizes supported protocol types into RGBA, including
  // cursor-cache references. The notify property (rather than the deprecated
  // cursor-set signal) retains the protocol type, including alpha semantics.
  if (shape && shape->data && shape->width && shape->height &&
      shape->width <= 256 && shape->height <= 256 &&
      (shape->type == SPICE_CURSOR_TYPE_ALPHA ||
       shape->type == SPICE_CURSOR_TYPE_MONO ||
       shape->type == SPICE_CURSOR_TYPE_COLOR4 ||
       shape->type == SPICE_CURSOR_TYPE_COLOR16 ||
       shape->type == SPICE_CURSOR_TYPE_COLOR32)) {
    cursor->rgba =
        g_memdup2(shape->data, (size_t)shape->width * shape->height * 4);
    cursor->width = shape->width;
    cursor->height = shape->height;
    cursor->hot_x = MIN(shape->hot_spot_x, shape->width - 1);
    cursor->hot_y = MIN(shape->hot_spot_y, shape->height - 1);
    cursor->premultiplied = shape->type == SPICE_CURSOR_TYPE_ALPHA;
    cursor->system_default = 0;
  }
  if (shape)
    g_boxed_free(SPICE_TYPE_CURSOR_SHAPE, shape);
}
static void cursor_hide(SpiceCursorChannel *channel, gpointer data) {
  (void)channel;
  bk_spice *s = data;
  s->cursor_state.known = 1;
  if (s->cursor_state.visible)
    s->cursor_state.serial++;
  s->cursor_state.visible = 0;
}
static void cursor_reset(SpiceCursorChannel *channel, gpointer data) {
  (void)channel;
  bk_spice *s = data;
  uint64_t serial = s->cursor_state.serial + 1;
  g_free(s->cursor_state.rgba);
  s->cursor_state = (bk_spice_cursor){.known = 1, .serial = serial};
}
static void cursor_move(SpiceCursorChannel *channel, gint x, gint y,
                        gpointer data) {
  (void)channel;
  (void)x;
  (void)y;
  bk_spice *s = data;
  if (!s->cursor_state.visible &&
      (s->cursor_state.rgba || s->cursor_state.system_default)) {
    s->cursor_state.visible = 1;
    s->cursor_state.serial++;
  }
}
typedef struct {
  bk_spice *s;
  bk_spice_cursor *cursor;
} cursor_poll_job;
static void get_cursor(void *arg) {
  cursor_poll_job *j = arg;
  *j->cursor = j->s->cursor_state;
  j->cursor->rgba = NULL;
  if (j->s->failed) {
    if (j->cursor->visible)
      j->cursor->serial++;
    j->cursor->visible = 0;
    return;
  }
  if (j->cursor->visible && j->s->cursor_state.rgba)
    j->cursor->rgba =
        g_memdup2(j->s->cursor_state.rgba,
                  (size_t)j->cursor->width * j->cursor->height * 4);
}
void bk_spice_cursor_get(bk_spice *s, bk_spice_cursor *cursor) {
  cursor_poll_job j = {s, cursor};
  call(get_cursor, &j);
}
void bk_spice_cursor_release(bk_spice_cursor *cursor) {
  g_free(cursor->rgba);
  cursor->rgba = NULL;
}
static void media_channel_destroy(SpiceSession *session, SpiceChannel *channel,
                                  gpointer data) {
  (void)session;
  bk_spice *s = data;
  if (SPICE_IS_PLAYBACK_CHANNEL(channel) &&
      s->playback == SPICE_PLAYBACK_CHANNEL(channel)) {
    g_signal_handlers_disconnect_by_data(channel, s);
    audio_fail(s, "SPICE playback channel removed");
    g_clear_object(&s->playback);
  }
  if (SPICE_IS_CURSOR_CHANNEL(channel) &&
      s->cursor_channel == SPICE_CURSOR_CHANNEL(channel)) {
    g_signal_handlers_disconnect_by_data(channel, s);
    cursor_reset(s->cursor_channel, s);
    g_clear_object(&s->cursor_channel);
  }
}

#endif
