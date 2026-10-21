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

For raw sources, JPEG output sends only changed regions; initial frames, resize
and full-refresh requests send the whole picture. `vnc.pixel_tolerance: 0`
(default) uses exact pixel comparisons.

Tight output classifies changed raw-pixel blocks on the same 64×64 grid.
Exactly uniform RGB blocks use lossless Tight Fill (`0x80`); adjacent blocks of
the same color merge into larger rectangles. Alpha is ignored. Other blocks
use JPEG (`0x90`), bounded to 2048×256 rectangles. Unchanged blocks are skipped
before classification. Compressed baseline YCbCr/grayscale JPEG also supports
Fill: a coefficient-only Huffman probe proves all AC coefficients are zero and
each component's DC is constant, then derives the color from dequantized DC.
No pixel image, upsampling or IDCT is performed. For self-generated GPU JPEG,
the existing 64×64 change scan also checks exact RGB uniformity per 16×16 MCU,
reusing each texture sample. Uniform MCUs calculate quantized DC directly with
the same color conversion and signed rounding as the full AAN DCT, then skip
both DCT and Huffman encoding. They enter only the changed-record queue; a
separate DCT queue excludes them before indirect transform dispatch,
so an all-solid update schedules zero DCT groups. Nonuniform MCUs still run DCT;
a post-DCT AC-mask/DC check compacts a separate Huffman queue, excluding MCUs
that quantization made constant before entropy dispatch. Both kinds of solid
MCUs therefore schedule no Huffman work. A solid MCU emits only
its 12-byte record (index, tag and packed 33-bit quantized DC color descriptor):
no Huffman coding, entropy-buffer access or entropy payload readback occurs.
CPU snapshots derive RGB once and retain the exact DCs for unchanged MCUs. Tight
classifies these records at 16×16 MCU granularity and sends Fill directly, even
inside an otherwise textured 64×64 dirty tile. Only if a JPEG consumer needs
that MCU does cropping synthesize its small DC/EOB stream on the CPU, preserving
the original coefficients and byte-identical JPEG output without pixels or
requantization. Standalone GPU JPEG output keeps its complete entropy encoding.
Other regions keep lossless JPEG crops. Progressive/unsupported scans, uncertain
color metadata and invalid entropy conservatively remain JPEG.

`vnc.jpeg_quality: 0` (Auto) retains the source MJPEG quantization and uses the
viewer-negotiated quality for raw sources. Self-generated GPU JPEG is compared
on a 64×64 grid. For the exclusive VNC session, Windows D3D11 and Linux Vulkan
read back only changed MCU records and keep an immutable CPU compressed snapshot;
no full-frame JPEG is packed on the GPU or transferred to the CPU. VNC packages
rectangles from those segments, including a full refresh from cached segments
when requested. A compact GPU work list drives indirect encoding dispatches and
packing indexes, so unchanged tiles have no subsequent encoding tasks or
metadata scan. A static frame launches no encoding or packing thread groups;
Windows Desktop Duplication metadata further restricts change detection to
candidate tiles from dirty regions and both ends of moves. Skipped acquisitions
or missing metadata fall back to full comparison. The compressed data is split by copying independently coded MCU
bytes, without re-encoding. Tight additionally entropy-probes changed blocks for
Fill as described above. Adjacent dirty tiles are merged; unchanged
regions are not sent. External MJPEG attempts TurboJPEG lossless coefficient-domain
cropping, with one source coefficient decode shared by all tile crops. Only
changed tiles are sent, without a pixel decode, DCT or requantization. This
optional runtime is searched in `ANYKVM_JPEG_LIBDIR`, executable `lib/`, executable
directory, standard macOS Homebrew locations and platform library paths. It can
also be used in FFmpeg builds. If unavailable or unsupported, full-frame JPEG
passthrough is retained; byte-identical frames are still suppressed. Compressed
domain comparisons are exact and do not apply `vnc.pixel_tolerance`.

A nonzero `vnc.jpeg_quality` forces that quality, including decoding and
re-encoding MJPEG. Reconnect after changing it.
Tight JPEG requires viewer JPEG support. Web UI quality levels differ from the
numeric YAML encoder quality.

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
