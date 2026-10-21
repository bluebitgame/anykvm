//go:build linux && cgo && spice

#include "native_linux.h"
#include "native_layout.h"
#include <EGL/egl.h>
#include <EGL/eglext.h>
#include <fcntl.h>
#include <gbm.h>
#include <spice-client.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/sysmacros.h>
#include <unistd.h>
#include <xf86drm.h>

// All spice-client-glib objects and EGL metadata operations live on one GLib
// thread. Go never holds a Go pointer in C and no callback calls into Go.
static GMainContext *context;
static gsize initialized;
static gpointer main_thread(gpointer unused) {
  (void)unused;
  g_main_loop_run(g_main_loop_new(context, FALSE));
  return NULL;
}
typedef struct {
  GMutex mu;
  GCond cond;
  void (*fn)(void *);
  void *arg;
  int done;
} job;
static gboolean dispatch(gpointer data) {
  job *j = data;
  j->fn(j->arg);
  g_mutex_lock(&j->mu);
  j->done = 1;
  g_cond_signal(&j->cond);
  g_mutex_unlock(&j->mu);
  return G_SOURCE_REMOVE;
}
static void call(void (*fn)(void *), void *arg) {
  if (g_once_init_enter(&initialized)) {
    context = g_main_context_default();
    g_thread_unref(g_thread_new("anykvm-spice", main_thread, NULL));
    g_once_init_leave(&initialized, 1);
  }
  job j = {.fn = fn, .arg = arg};
  g_mutex_init(&j.mu);
  g_cond_init(&j.cond);
  g_mutex_lock(&j.mu);
  // An attached source always dispatches on the owning loop, unlike invoke()
  // which can run immediately on the caller before the loop acquires context.
  GSource *source = g_idle_source_new();
  g_source_set_priority(source, G_PRIORITY_DEFAULT);
  g_source_set_callback(source, dispatch, &j, NULL);
  g_source_attach(source, context);
  g_source_unref(source);
  while (!j.done)
    g_cond_wait(&j.cond, &j.mu);
  g_mutex_unlock(&j.mu);
  g_cond_clear(&j.cond);
  g_mutex_clear(&j.mu);
}

struct bk_spice {
  SpiceSession *session;
  SpiceMainChannel *main;
  SpiceDisplayChannel *display;
  SpiceInputsChannel *inputs;
  SpicePlaybackChannel *playback;
  SpiceCursorChannel *cursor_channel;

  // Playback owns queued PCM; polling transfers one packet to the caller.
  GQueue audio_queue;
  size_t audio_bytes;
  int audio_enabled, audio_active, audio_failed, audio_rate, audio_channels,
      audio_reported;
  uint64_t audio_epoch, audio_samples;
  char audio_error[128];

  // Cursor pixels are independent of the GPU scanout.
  bk_spice_cursor cursor_state;

  int failed, pending, acquired, fd, inputs_ready, mouse_x, mouse_y;
  char error[512];
  bk_spice_frame frame;

  // EGL resolves scanout layout metadata without mapping desktop pixels.
  int render_fd;
  struct gbm_device *gbm;
  EGLDisplay egl;
  PFNEGLCREATEIMAGEKHRPROC create_image;
  PFNEGLDESTROYIMAGEKHRPROC destroy_image;
  PFNEGLEXPORTDMABUFIMAGEQUERYMESAPROC query_image;
  PFNEGLEXPORTDMABUFIMAGEMESAPROC export_image;
  const char *path, *password, *render_node;
};
static void fail(bk_spice *s, const char *reason) {
  if (!s->failed)
    snprintf(s->error, sizeof(s->error), "%s", reason);
  s->failed = 1;
}

// Media callbacks share bk_spice state and execute on the same GLib thread.
#include "native_media_linux.h"

static void channel_event(SpiceChannel *channel, SpiceChannelEvent event,
                          gpointer data) {
  bk_spice *s = data;
  if (SPICE_IS_PLAYBACK_CHANNEL(channel)) {
    if (event == SPICE_CHANNEL_CLOSED || event >= SPICE_CHANNEL_ERROR_CONNECT) {
      audio_fail(s, "SPICE playback channel closed or failed");
    }
    return; // Optional audio failure must not tear down GPU video.
  }
  if (SPICE_IS_CURSOR_CHANNEL(channel)) {
    if (event == SPICE_CHANNEL_CLOSED || event >= SPICE_CHANNEL_ERROR_CONNECT)
      cursor_reset(SPICE_CURSOR_CHANNEL(channel), s);
    return;
  }
  if (event == SPICE_CHANNEL_OPENED && SPICE_IS_INPUTS_CHANNEL(channel))
    s->inputs_ready = 1;
  if (event == SPICE_CHANNEL_OPENED && SPICE_IS_MAIN_CHANNEL(channel))
    spice_main_channel_request_mouse_mode(SPICE_MAIN_CHANNEL(channel),
                                          SPICE_MOUSE_MODE_CLIENT);
  if (event == SPICE_CHANNEL_SWITCHING)
    fail(s, "SPICE migration is unsupported by this GPU upstream");
  if (event == SPICE_CHANNEL_CLOSED || event >= SPICE_CHANNEL_ERROR_CONNECT)
    fail(s, "SPICE channel closed or connection/authentication failed");
}
static int layout(bk_spice *s, const SpiceGlScanout *scan) {
  if (!scan || scan->fd < 0 || !scan->width || !scan->height ||
      scan->width > 8192 || scan->height > 8192 || !scan->stride) {
    fail(s, "SPICE GL scanout is absent or invalid");
    return -1;
  }
  // SPICE supplies an implicit-layout FD, not a DRM modifier. Import via EGL
  // and re-export its actual layout; assuming modifier=0 corrupts tiled images.
  EGLint attrs[] = {EGL_WIDTH,
                    scan->width,
                    EGL_HEIGHT,
                    scan->height,
                    EGL_LINUX_DRM_FOURCC_EXT,
                    scan->format,
                    EGL_DMA_BUF_PLANE0_FD_EXT,
                    scan->fd,
                    EGL_DMA_BUF_PLANE0_OFFSET_EXT,
                    0,
                    EGL_DMA_BUF_PLANE0_PITCH_EXT,
                    scan->stride,
                    EGL_NONE};
  EGLImageKHR image = s->create_image(s->egl, EGL_NO_CONTEXT,
                                      EGL_LINUX_DMA_BUF_EXT, NULL, attrs);
  if (image == EGL_NO_IMAGE_KHR) {
    fail(s, "EGL could not import implicit SPICE DMA-BUF");
    return -1;
  }
  EGLint format = 0, planes = 0, strides[4] = {0}, offsets[4] = {0};
  EGLuint64KHR modifiers[4] = {0};
  int fds[4] = {-1, -1, -1, -1}, valid = 0;
  if (!s->query_image(s->egl, image, &format, &planes, modifiers) ||
      planes < 1 || planes > 4 ||
      modifiers[0] == UINT64_C(0x00ffffffffffffff)) {
    fail(s, "EGL cannot resolve the SPICE DMA-BUF modifier");
    goto end;
  }
  if (!s->export_image(s->egl, image, fds, strides, offsets)) {
    fail(s, "EGL cannot export SPICE DMA-BUF layout");
    goto end;
  }
  if (!bk_spice_layout_valid(planes, modifiers, fds, strides, offsets)) {
    fail(s, "invalid SPICE DMA-BUF layout or separate memory objects");
    goto end;
  }
  s->frame.width = scan->width;
  s->frame.height = scan->height;
  s->frame.format = format;
  s->frame.pitch = strides[0];
  s->frame.offset = offsets[0];
  s->frame.modifier = modifiers[0];
  s->frame.planes = planes;
  s->frame.flip_y = !scan->y0top;
  for (int i = 0; i < planes; i++) {
    s->frame.pitches[i] = strides[i];
    s->frame.offsets[i] = offsets[i];
  }
  s->fd = fds[0];
  s->frame.fd = fds[0];
  fds[0] = -1;
  valid = 1;
end:
  for (int i = 0; i < 4; i++)
    if (fds[i] >= 0)
      close(fds[i]);
  s->destroy_image(s->egl, image);
  return valid ? 0 : -1;
}
static void draw(SpiceDisplayChannel *display, guint x, guint y, guint w,
                 guint h, gpointer data) {
  (void)x;
  (void)y;
  (void)w;
  (void)h;
  bk_spice *s = data;
  if (s->pending) {
    fail(s, "SPICE sent another GL draw before DMA-BUF lease completion");
    spice_display_channel_gl_draw_done(display);
    return;
  }
  if (s->failed ||
      layout(s, spice_display_channel_get_gl_scanout(display)) < 0) {
    spice_display_channel_gl_draw_done(display);
    return;
  }
  s->pending = 1;
  // Ack is delayed until synchronous GPU conversion/encoding has completed.
}
static void channel_new(SpiceSession *session, SpiceChannel *channel,
                        gpointer data) {
  (void)session;
  bk_spice *s = data;
  int type = 0, id = 0;
  g_object_get(channel, "channel-type", &type, "channel-id", &id, NULL);
  if (type != SPICE_CHANNEL_MAIN && type != SPICE_CHANNEL_DISPLAY &&
      type != SPICE_CHANNEL_INPUTS && type != SPICE_CHANNEL_PLAYBACK &&
      type != SPICE_CHANNEL_CURSOR)
    return;
  if (type == SPICE_CHANNEL_MAIN && !s->main)
    s->main = g_object_ref(SPICE_MAIN_CHANNEL(channel));
  if (type == SPICE_CHANNEL_DISPLAY) {
    if (id != 0 || s->display)
      return;
    s->display = g_object_ref(SPICE_DISPLAY_CHANNEL(channel));
    g_signal_connect(channel, "gl-draw", G_CALLBACK(draw), s);
  }
  if (type == SPICE_CHANNEL_INPUTS) {
    if (id != 0 || s->inputs)
      return;
    s->inputs = g_object_ref(SPICE_INPUTS_CHANNEL(channel));
  }
  if (type == SPICE_CHANNEL_PLAYBACK) {
    if (id != 0 || s->playback)
      return;
    s->playback = g_object_ref(SPICE_PLAYBACK_CHANNEL(channel));
    s->audio_failed = 0;
    g_signal_connect(channel, "playback-start", G_CALLBACK(playback_start), s);
    g_signal_connect(channel, "playback-data", G_CALLBACK(playback_data), s);
    g_signal_connect(channel, "playback-stop", G_CALLBACK(playback_stop), s);
  }
  if (type == SPICE_CHANNEL_CURSOR) {
    if (id != 0 || s->cursor_channel)
      return;
    s->cursor_channel = g_object_ref(SPICE_CURSOR_CHANNEL(channel));
    g_signal_connect(channel, "notify::cursor", G_CALLBACK(cursor_changed), s);
    g_signal_connect(channel, "cursor-hide", G_CALLBACK(cursor_hide), s);
    g_signal_connect(channel, "cursor-reset", G_CALLBACK(cursor_reset), s);
    g_signal_connect(channel, "cursor-move", G_CALLBACK(cursor_move), s);
  }
  g_signal_connect(channel, "channel-event", G_CALLBACK(channel_event), s);
  if (type != SPICE_CHANNEL_MAIN)
    spice_channel_connect(channel);
}
static void open_session(void *arg) {
  bk_spice *s = arg;
  s->render_fd = open(s->render_node, O_RDWR | O_CLOEXEC);
  if (s->render_fd < 0) {
    fail(s, "cannot open spice.render_node");
    return;
  }
  drmDevicePtr device = NULL;
  struct stat st;
  if (drmGetDevice2(s->render_fd, 0, &device) != 0 ||
      !(device->available_nodes & (1 << DRM_NODE_PRIMARY)) ||
      stat(device->nodes[DRM_NODE_PRIMARY], &st) < 0) {
    if (device)
      drmFreeDevice(&device);
    fail(s, "cannot identify primary DRM device for SPICE render node");
    return;
  }
  s->frame.device_major = major(st.st_rdev);
  s->frame.device_minor = minor(st.st_rdev);
  drmFreeDevice(&device);
  s->gbm = gbm_create_device(s->render_fd);
  PFNEGLGETPLATFORMDISPLAYEXTPROC platform =
      (void *)eglGetProcAddress("eglGetPlatformDisplayEXT");
  if (!s->gbm || !platform ||
      (s->egl = platform(EGL_PLATFORM_GBM_KHR, s->gbm, NULL)) ==
          EGL_NO_DISPLAY ||
      !eglInitialize(s->egl, NULL, NULL)) {
    fail(s, "cannot initialize GBM/EGL for SPICE DMA-BUF");
    return;
  }
  const char *extensions = eglQueryString(s->egl, EGL_EXTENSIONS);
  if (!extensions || !strstr(extensions, "EGL_EXT_image_dma_buf_import") ||
      !strstr(extensions, "EGL_MESA_image_dma_buf_export")) {
    fail(s, "SPICE GPU input requires EGL DMA-BUF import and MESA export");
    return;
  }
  s->create_image = (void *)eglGetProcAddress("eglCreateImageKHR");
  s->destroy_image = (void *)eglGetProcAddress("eglDestroyImageKHR");
  s->query_image = (void *)eglGetProcAddress("eglExportDMABUFImageQueryMESA");
  s->export_image = (void *)eglGetProcAddress("eglExportDMABUFImageMESA");
  if (!s->create_image || !s->destroy_image || !s->query_image ||
      !s->export_image) {
    fail(s, "missing EGL DMA-BUF entrypoints");
    return;
  }
  s->session = spice_session_new();
  g_object_set(s->session, "unix-path", s->path, "password", s->password,
               "enable-audio", TRUE, "enable-usbredir", FALSE,
               "enable-smartcard", FALSE, NULL);
  g_signal_connect(s->session, "channel-new", G_CALLBACK(channel_new), s);
  g_signal_connect(s->session, "channel-destroy",
                   G_CALLBACK(media_channel_destroy), s);
  if (!spice_session_connect(s->session))
    fail(s, "SPICE connect failed");
}
bk_spice *bk_spice_open(const char *path, const char *password,
                        const char *render_node, char *error, size_t cap) {
  bk_spice *s = calloc(1, sizeof(*s));
  if (!s) {
    snprintf(error, cap, "cannot allocate SPICE session");
    return NULL;
  }
  s->fd = -1;
  s->render_fd = -1;
  s->egl = EGL_NO_DISPLAY;
  s->path = path;
  s->password = password;
  s->render_node = render_node;
  call(open_session, s);
  s->path = s->password = s->render_node = NULL;
  if (s->failed) {
    snprintf(error, cap, "%s", s->error);
    bk_spice_close(s);
    return NULL;
  }
  return s;
}
typedef struct {
  bk_spice *s;
  bk_spice_frame *frame;
  char *error;
  size_t cap;
  int result;
} next_job;
static void next_frame(void *arg) {
  next_job *j = arg;
  bk_spice *s = j->s;
  if (s->failed) {
    snprintf(j->error, j->cap, "%s", s->error);
    j->result = -1;
    return;
  }
  if (s->pending && !s->acquired) {
    *j->frame = s->frame;
    s->acquired = 1;
    j->result = 1;
  }
}
int bk_spice_next(bk_spice *s, bk_spice_frame *frame, char *error, size_t cap) {
  next_job j = {.s = s, .frame = frame, .error = error, .cap = cap};
  call(next_frame, &j);
  return j.result;
}
static void done_frame(void *arg) {
  bk_spice *s = arg;
  if (s->pending) {
    if (s->fd >= 0)
      close(s->fd);
    s->fd = -1;
    s->pending = s->acquired = 0;
    spice_display_channel_gl_draw_done(s->display);
  }
}
void bk_spice_done(bk_spice *s) { call(done_frame, s); }
static void close_session(void *arg) {
  bk_spice *s = arg;
  done_frame(s);
  if (s->session) {
    g_signal_handlers_disconnect_by_data(s->session, s);
    GList *channels = spice_session_get_channels(s->session);
    for (GList *c = channels; c; c = c->next)
      g_signal_handlers_disconnect_by_data(c->data, s);
    g_list_free(channels);
    spice_session_disconnect(s->session);
  }
  // Removed channels may no longer appear in session_get_channels(), but our
  // references keep them alive. Remove callbacks before freeing their user
  // data.
  if (s->main)
    g_signal_handlers_disconnect_by_data(s->main, s);
  if (s->display)
    g_signal_handlers_disconnect_by_data(s->display, s);
  if (s->inputs)
    g_signal_handlers_disconnect_by_data(s->inputs, s);
  if (s->playback)
    g_signal_handlers_disconnect_by_data(s->playback, s);
  if (s->cursor_channel)
    g_signal_handlers_disconnect_by_data(s->cursor_channel, s);
  audio_clear(s);
  g_free(s->cursor_state.rgba);
  if (s->playback)
    g_object_unref(s->playback);
  if (s->cursor_channel)
    g_object_unref(s->cursor_channel);
  if (s->main)
    g_object_unref(s->main);
  if (s->display)
    g_object_unref(s->display);
  if (s->inputs)
    g_object_unref(s->inputs);
  if (s->session)
    g_object_unref(s->session);
  // Each session owns a distinct GBM device / EGL display.
  if (s->egl != EGL_NO_DISPLAY)
    eglTerminate(s->egl);
  if (s->gbm)
    gbm_device_destroy(s->gbm);
  if (s->render_fd >= 0)
    close(s->render_fd);
  free(s);
}
void bk_spice_close(bk_spice *s) {
  if (s)
    call(close_session, s);
}
typedef struct {
  bk_spice *s;
  int kind, a, b, buttons, result;
} input_job;
static void send_input(void *arg) {
  input_job *j = arg;
  SpiceInputsChannel *c = j->s->inputs;
  if (j->s->failed || !c || !j->s->inputs_ready) {
    j->result = -1;
    return;
  }
  gint mode = SPICE_MOUSE_MODE_SERVER;
  if (j->s->main)
    g_object_get(j->s->main, "mouse-mode", &mode, NULL);
  switch (j->kind) {
  case BK_SPICE_KEY_PRESS:
    spice_inputs_channel_key_press(c, j->a);
    break;
  case BK_SPICE_KEY_RELEASE:
    spice_inputs_channel_key_release(c, j->a);
    break;
  case BK_SPICE_POSITION:
    if (mode != SPICE_MOUSE_MODE_CLIENT) {
      j->result = -1;
      break;
    }
    j->s->mouse_x = j->a;
    j->s->mouse_y = j->b;
    spice_inputs_channel_position(c, j->a, j->b, 0, j->buttons);
    break;
  case BK_SPICE_MOTION:
    if (mode == SPICE_MOUSE_MODE_CLIENT) {
      if (!j->s->frame.width || !j->s->frame.height) {
        j->result = -1;
        break;
      }
      // Client mouse mode is absolute. Relative viewer events move the
      // remembered guest position; server mode sends true relative motion.
      gint64 x = (gint64)j->s->mouse_x + j->a, y = (gint64)j->s->mouse_y + j->b;
      j->s->mouse_x = CLAMP(x, 0, (gint64)j->s->frame.width - 1);
      j->s->mouse_y = CLAMP(y, 0, (gint64)j->s->frame.height - 1);
      spice_inputs_channel_position(c, j->s->mouse_x, j->s->mouse_y, 0,
                                    j->buttons);
    } else {
      spice_inputs_channel_motion(c, j->a, j->b, j->buttons);
    }
    break;
  case BK_SPICE_BUTTON_PRESS:
    spice_inputs_channel_button_press(c, j->a, j->buttons);
    break;
  case BK_SPICE_BUTTON_RELEASE:
    spice_inputs_channel_button_release(c, j->a, j->buttons);
    break;
  }
}
int bk_spice_input(bk_spice *s, int kind, int a, int b, int buttons) {
  input_job j = {s, kind, a, b, buttons, 0};
  call(send_input, &j);
  return j.result;
}
