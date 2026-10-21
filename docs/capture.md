# Capture, audio and input setup

Use the [configuration and discovery guide](configuration.md) to select each
target's `video.source`, `audio.source` and `hid.device`. The Web mode picker
reads device capabilities. Android setup is covered in the
[Android guide](android.md).

Startup logs show the selected codec and encoder, or `passthrough`.
During remote sessions, `AnyKVM: uplink=… Mbps` reports combined outgoing
traffic every five seconds. It excludes other applications and network headers.

## Modes, cursor and input

`video.mode` describes the input, independently of the viewer's output codec:

- Empty: device/source dimensions and cadence.
- `@30`: retain dimensions and request a 30 FPS cap.
- `1920x1080@59.94:nv12`: request size, rate and input format.
- `auto` / `auto@30`: follow client size where the source supports it.

Formats include NV12, full-range NV12, YUYV/UYVY, MJPEG and BGRA, depending on
the device. macOS AVFoundation, Windows Media Foundation and Linux V4L2 all
provide mode discovery. Explicit unsupported modes fail rather than silently
substituting another format/rate. Fractional rates retain rational timing.

`video.cursor: client` (default) excludes the local screen cursor and uses
separate cursor rendering where supported; `remote` includes it in screen video.
Capture cards use the RDP client's default cursor. Relative mouse mode hides
that client cursor. Cursors already embedded in HDMI/remote video remain part
of the pixels; GameStream cursor visibility is client-controlled.

`hid.wheel_scale` multiplies scrolling across services: 1=normal, 0=off,
negative=reversed, fractional values accumulate. Screen capture/input permissions
are OS/backend-specific. [RDP display/input](rdp-server.md#video-display-and-flow-control)
and [GameStream source sizing](gamestream.md#video-and-recovery) describe client
geometry and letterboxing.

## macOS local screens

`screen` / `screen:N` uses ScreenCaptureKit on macOS 12.3+. Grant Screen Recording
to the executable or launching terminal, plus Accessibility for local input.
VideoToolbox provides native H.264/HEVC; ImageIO handles JPEG. These paths do not
require FFmpeg. `audio.source: auto` captures system audio through the same
session on macOS 13+, excluding AnyKVM's own audio; an explicit device overrides
it and an empty source disables it.

Third-party loopback devices are user-managed: install the driver, configure OS
audio routing and select the exposed input with `audio.source`. AnyKVM has no
integrated loopback setup or automatic fallback, and none is planned.

Only `video.mode: auto[@FPS]` changes the physical desktop mode to follow the
client. RDP uses client size/DPI and single-monitor Display Control; GameStream
uses negotiated resolution/FPS. Empty/`@FPS` retains native pixels, including
Retina backing pixels. Fixed dimensions request capture scaling without changing
the desktop mode. Aspect ratio is preserved with black borders when needed.
Without `@FPS`, capture follows the display's native cadence. Set `@FPS` to
limit the capture rate.

Auto selection uses existing eligible display modes, with client DPI affecting
logical size. Requests below 1080 rows use a 1x/100% mode. Unsupported requests
choose an eligible nearby mode or retain the current desktop and scale output.
It does not create or rotate displays. Disconnect, takeover, target switch and
shutdown restore the original mode; keyframe restarts retain the session mode.

### Wi-Fi and AWDL

Top-level `disable_awdl: true` temporarily suspends `awdl0` while viewers are
connected, checks for reactivation every two seconds, and restores the original
state on last disconnect/graceful exit. It defaults to false and affects
AirDrop/AWDL-dependent features. It does not change ordinary Wi-Fi connectivity.

The normal-user process uses noninteractive `sudo -n`; permission failure is
logged without stopping streaming. If using this option, authorize only the
required commands with `sudo visudo -f /etc/sudoers.d/anykvm-awdl`:

```sudoers
YOUR_USERNAME ALL=(root) NOPASSWD: /sbin/ifconfig awdl0 down, /sbin/ifconfig awdl0 up
```

Replace the username, keep root:wheel ownership/mode 0440, and verify with
`sudo visudo -c`. Revoke by removing that file and disabling the setting. After
a forced exit, restore AWDL if necessary with `sudo /sbin/ifconfig awdl0 up`.

## Windows screens and audio

`screen` / `screen:N` enumerates attached DXGI outputs. Native NVENC/AMF/QSV can
encode a shared D3D11 desktop texture after GPU NV12 conversion when adapter and
encoder match. Desktop MJPEG uses D3D11 Compute with
automatic CPU fallback; set `ANYKVM_WINDOWS_GPU_JPEG=0` to disable it.
Raw output and other incompatible encoder paths use CPU readback.
Hardware cursor composition and Windows Graphics Capture are absent.
Local keyboard/mouse input uses SendInput.

`audio.source: auto` on local screens uses native WASAPI loopback. Use `wasapi`
for the current multimedia playback device, `wasapi:<name>` to pin an output,
or `wasapi:id:<endpoint ID>` for an exact endpoint. The default selector follows
output changes/reconnects. It supplies 48 kHz stereo S16LE PCM without FFmpeg;
no playback produces no packets. Media Foundation microphone/capture-card input
names and indices are separate selectors.

Media Foundation H.264 hardware/software fallback remains available without
FFmpeg. See [codec backends](build.md#native-and-optional-codecs).

## Capture cards and cameras

Generate a local configuration with `anykvm -gen-config /path/to/config.yaml`
and replace its `targets` block, using the actual device/mode from discovery:

```yaml
targets:
  - name: Capture card
    video:
      source: /dev/video0
      mode: "1920x1080@60:nv12"
      cursor: client
    audio:
      source: ""
    hid:
      device: ""
```

This source is a Linux example. On Windows, use the device name or numeric index,
such as `"0"`; on macOS, use the AVFoundation device name. Keep audio/input
disabled until the card's separate audio input and external target's HID backend
are selected. Input resolution/format is separate from the output codec.
The generated Web listener is sufficient for viewing; enable [RDP](rdp-server.md)
or [GameStream](gamestream.md) separately when needed.

- **Windows:** Media Foundation provides video/audio and PCM conversion without
  FFmpeg. Metadata discovery may fail while another application exclusively
  owns the device. Native MJPEG can pass through and NV12 can feed compatible
  encoders directly; other formats may need Windows decoders/processors.
- **Linux:** single-planar V4L2 video and ALSA audio are native CGO backends.
  `/dev/video*` and stable `/dev/v4l/by-id/...` paths are supported. Metadata-only
  and codec M2M nodes are excluded; multi-planar camera pipelines are unsupported.
  libv4lconvert handles supported raw formats. H.264 passthrough needs no decoder;
  pixel/JPEG output from H.264 needs optional FFmpeg. `plughw:CARD=...,DEV=0`
  permits ALSA conversion to 48 kHz stereo PCM. Device permissions are required.
- **macOS:** AVFoundation handles device capture and native media conversion.
  USB iPhone video is covered in [CoreDevice/USB setup](iphone-coredevice.md).

[Native buffer and passthrough behavior](capture-native-buffers.md) explains
retention, copies, NV12 imports and hardware tests. [GameStream](gamestream.md#video-and-recovery)
explains source-size/rate preservation and encoded keyframe requirements.

## JPEG decoding for video output

Windows and Linux can use GPU decoding for baseline JPEG/MJPEG before native
video encoding, without FFmpeg. Unsupported JPEG formats or GPU failures use
the fallback decoder. macOS uses its system decoder. GPU decoding still involves
copies and does not guarantee lower latency.

## Linux desktops and hardware encoding

`screen` / `screen:N` captures active DRM/KMS primary planes, including under
Wayland. It does not use X11/Portal/PipeWire, change modes or acquire DRM master.
KMS export requires access to `/dev/dri/card*` and effective CAP_SYS_ADMIN;
render-node access alone is insufficient. AnyKVM does not grant permissions.

The native encoded path imports the framebuffer's DMA-BUF into Vulkan, scales
and converts to limited-range BT.709 NV12, then encodes with Vulkan Video without
raw pixel readback. Vulkan Video capture and encoding must use the same GPU.
Desktop MJPEG uses Vulkan Compute with CPU fallback; set
`ANYKVM_LINUX_GPU_JPEG=0` to disable it. Raw output uses explicit BGRA readback.
Tagged FFmpeg builds have a separate video encoder path.

Runtime needs `libvulkan.so.1`, a Vulkan 1.3 driver, DRM device/modifier and
DMA-BUF import support, foreign queue ownership and sync-file fences, plus
video encode queues and the selected codec's encode extension for Vulkan Video
output (not JPEG). See
[codec backends](build.md#native-and-optional-codecs). Native output uses CBR,
SDR 8-bit 4:2:0 and no frame reordering. AV1 dimensions must satisfy the driver's
alignment requirements. Shaders are embedded by the [build process](build.md).

The current KMS path handles SDR 8888 primary planes. Separate framebuffer
memory objects, fractional crops/reflection, HDR/10-bit, overlay/cursor
composition and Portal capture are unsupported. Mode changes or disconnection
stop capture so it can be reopened/re-enumerated.

Raspberry Pi 4/5 can use the separate `vc4-drm` display and `v3d` renderer for
GPU conversion and JPEG. This requires compatible Vulkan and DMA-BUF drivers;
GPU JPEG failures may fall back to CPU encoding. Use the hardware tests below
to verify the GPU path on the actual device.

The Raspberry Pi H.264 backend independently scans for a streaming V4L2 M2M
`bcm2835-codec` device accepting NV12/H.264. It requires that driver/firmware and
read/write permission to its node; it does not fix a `/dev/videoN` number.
Devices without this encoder need another supported native backend or optional
FFmpeg software encoding. Capture-card/CSI input configuration remains separate
from this encoding device.

## Hardware validation

After [build preparation](build.md#direct-go-builds), run tests on the actual host:

```sh
ANYKVM_TEST_VULKAN=1 ANYKVM_TEST_VULKAN_CODECS=h264,hevc,av1 \
  go test ./internal/videoenc -run '^TestNativeVulkanVideo$' -count=1 -v
ANYKVM_TEST_KMS=1 go test ./internal/capture -run '^TestLinuxKMSVulkanVideo$' -count=1 -v
ANYKVM_TEST_KMS_JPEG=1 go test ./internal/capture -run '^TestLinuxKMSVulkanJPEG$' -count=1 -v
ANYKVM_TEST_RPI_H264=1 go test ./internal/videoenc -run '^TestRaspberryPiNativeH264$' -count=1 -v
```

On Raspberry Pi, set `ANYKVM_TEST_KMS_RPI=1` for the KMS JPEG test and run with
DRM export permissions and effective CAP_SYS_ADMIN. The test requires GPU JPEG
without CPU fallback. Set `ANYKVM_TEST_VULKAN_CODECS` to codecs supported by your
GPU; the default is H.264.

On Windows, `ANYKVM_TEST_WASAPI=1` enables `TestWindowsWASAPILoopback`;
`ANYKVM_TEST_WINDOWS_NVENC=1` enables `TestWindowsDesktopNVENCTexture` (AMF/QSV
have equivalent variables/tests). Camera NV12/MJPEG tests and device-selection
variables are in [native buffer validation](capture-native-buffers.md#hardware-validation).
On macOS, `ANYKVM_TEST_SCREEN_AUDIO=1` enables `TestNativeScreenSystemAudio`;
grant capture/audio permission and play audible sound in another application.
Cross compilation and synthetic timing do not establish real driver/device
compatibility or end-to-end playback latency.
