# Web, VNC and local viewer

Configure listeners and credentials using the
[configuration guide](configuration.md). For other viewers, see
[RDP](rdp-server.md) and [GameStream/Moonlight](gamestream.md).

## Browser access

The browser uses the configured `web.http`/`web.https` listener. The default is
[http://127.0.0.1:8080/](http://127.0.0.1:8080/).

H.264/HEVC/AV1 need compatible WebCodecs support on localhost or HTTPS; JPEG
works without WebCodecs. `?codec=h264`, `hevc`, `av1` or `jpeg` selects a path
where supported. AV1 also requires `web.codecs.av1: true` and an available
source/encoder.

The toolbar toggles fullscreen. Ctrl+Alt+Shift+X (Control+Option+Shift+X on macOS)
toggles video-only fullscreen. Escape forwarding depends on browser keyboard-lock
support/permission.

## Buffering and diagnostics

`web.buffer_frames` controls the shared decoded display queue for H.264, HEVC
and AV1: 0=auto (one picture), or 1..8. Capacity 1 presents the latest decoded
picture on the next browser refresh. Larger values use source timestamps to
pace a bounded queue, dropping stale decoded pictures when needed. Compressed
prediction references stay ordered. Changes apply to new Web connections.

Add `?debug=video`, or `&debug=video` after a codec parameter, for H.264/HEVC
received/decoded/drawn FPS, bitrate, decoder pressure, display-queue depth,
drops and recovery. AV1 shares the display queue but has no dedicated decoder
diagnostics in this report. `pendingOutput` counts submitted pictures awaiting
output, separately from the WebCodecs input queue. Timing measures browser
processing and canvas submission, excluding physical display and end-to-end
latency. Quiet sender intervals alone do not trigger decoder recovery.

## VNC access

Enable `vnc.addr`, for example `:5901`, for VNC access. `vnc.tls: true` enables
VeNCrypt TLS with the shared certificate. Set `vnc.password` for authentication.

Audio requires a viewer with QEMU Audio support.

For using a remote VNC desktop as a capture source, see
[VNC upstream sources](network-sources.md#vnc-support-and-wake-on-lan).

## Video quality and media support

`web.bitrate`, `vnc.bitrate` and `rdp.bitrate` are host encoder targets in kbps;
passthrough retains source bitrate. JPEG quality is configured separately per
service. [Build and media runtime](build.md#native-and-optional-codecs) describes
native codecs and optional FFmpeg paths.

## Local viewer

Build with `--tags viewer`, then set top-level `viewer: true` in YAML. The SDL2
window starts on the first target; `hotkey_mod` plus 1–9/0 switches targets
(default Ctrl+Alt). Configured network services also run. Closing the viewer
exits AnyKVM. Default/headless builds need no SDL2.

See [build options](build.md#desktop-build-script) for platform dependencies
and combining the `viewer` and `ffmpeg` tags.
