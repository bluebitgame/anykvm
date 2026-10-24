# Linux SPICE upstream (experimental)

AnyKVM receives SPICE consoles through a local socket or a remote TCP listener:

- `spice:/absolute/path.sock` captures local SPICE GL using DMA-BUF and the same
  GPU conversion/encoding pipeline as Linux `screen`. This targets
  Venus/virtio-gpu desktops; the downstream viewer selects the output format.
- `spice://host:port` receives a remote desktop as decoded BGRA frames for the
  existing downstream display and encoding paths.

Both modes share the playback audio, input and cursor channels. Real VM
compatibility testing is still pending.

## Build

SPICE requires Linux, CGO and the optional `spice` build tag. Install the usual
[build requirements](build.md), then on Debian/Ubuntu:

```sh
sudo apt install libspice-client-glib-2.0-dev libegl1-mesa-dev libgbm-dev libdrm-dev
python3 tools/build.py --targets native --tags spice
```

Prebuilt releases do not include SPICE support. The build script does not bundle
these extra libraries; cross builds require matching target libraries.

Local GL capture additionally needs access to a render node whose GPU/driver
supports EGL/Vulkan DMA-BUF interoperability and the requested GPU processing
path (Vulkan Video for video encoding, Vulkan Compute for JPEG or GPU surfaces).
Use the same physical GPU for QEMU and AnyKVM initially. Remote TCP capture uses
the host/port connection and does not open a render node.

## Remote TCP configuration

Connect to the VM's SPICE listener using its host and explicit port:

```yaml
targets:
  - name: Remote VM
    video:
      source: spice://192.0.2.10:5900
      mode: "@30"
      cursor: client
    spice:
      password_env: VM_SPICE_PASSWORD
      timeout: 30s
    hid:
      device: auto
    audio:
      source: auto
```

IPv6 addresses use brackets, for example `spice://[2001:db8::10]:5900`.

The SPICE library decodes display updates into a CPU framebuffer. AnyKVM copies
owned BGRA snapshots at the configured maximum FPS and follows guest resolution
changes. Updates accumulate between reads, so the final update before an idle
desktop is retained. GameStream uses its downstream encoder. This remote path
includes CPU pixel processing; local GL supplies DMA-BUF images to the shared
desktop GPU pipeline.

A static desktop stays idle. A refresh request resends the latest snapshot.
Removing the primary display clears both its cached image and input dimensions,
then starts a fresh timeout while waiting for a replacement display.

## Local GL configuration

Add these options to a complete QEMU command, adjusting the socket and GPU paths:

```sh
-device virtio-vga-gl,hostmem=4G,blob=true,venus=true \
-spice unix=on,addr=/run/user/1000/anykvm-vm.spice,gl=on,rendernode=/dev/dri/renderD128 \
-device qemu-xhci -device usb-tablet
```

For guest playback audio, also add:

```sh
-audiodev spice,id=spiceaudio -device intel-hda -device hda-output,audiodev=spiceaudio
```

Use a private socket directory and configure SPICE ticket authentication. The
guest needs compatible virtio-gpu and Mesa Venus drivers; see the [QEMU guide](https://www.qemu.org/docs/master/system/devices/virtio/virtio-gpu.html)
and [Mesa requirements](https://docs.mesa3d.org/drivers/venus.html).

Point AnyKVM at that socket and render node:

```yaml
targets:
  - name: Local VM
    video:
      source: spice:/run/user/1000/anykvm-vm.spice
      mode: "@30"
      cursor: client
    spice:
      render_node: /dev/dri/renderD128
      timeout: 60s
      password_env: VM_SPICE_PASSWORD
    hid:
      device: auto
    audio:
      source: auto
```

Local capture follows the viewer's negotiated format, like Linux `screen`:

- H.264, HEVC or AV1 uses the shared video encoder with DMA-BUF input. Codec
  availability depends on the Vulkan GPU/driver, including in builds with
  FFmpeg enabled. GameStream supplies size/FPS/bitrate.
- JPEG and VNC JPEG regions use Vulkan Compute encoding. Only compressed output
  is read back; desktop pixels stay on the GPU.
- RDP can receive an owned GPU surface for its GPU processing path.
- A viewer that explicitly requests BGRA pixels uses GPU conversion followed by
  pixel readback, recorded in `CPUReadbacks`.

Video/JPEG GPU failures stop the current capture session. An explicit BGRA
request uses the pixel-readback path described above. Idle refresh uses owned
video input or output storage, so an acknowledged SPICE scanout is never reused.

## Settings and channels

- **Authentication:** set `VM_SPICE_PASSWORD` before starting AnyKVM. Choose
  either `password_env` or `password`; the environment is resolved on each open.
- **Timeout:** defaults to 15 seconds and accepts up to 5 minutes. It waits for
  the first usable frame; remote display removal starts a new wait window.
- **Render node:** used only for local GL capture. Match it to QEMU's `rendernode`.
- **Audio:** `audio.source: auto` receives guest playback. Set it to `""` to
  disable playback. Audio failure does not stop video; microphone redirection
  is not part of this source.
- **Input:** keyboard, three mouse buttons and wheel. Absolute input requires
  client mouse mode and a guest tablet. Pause is not implemented.
- **Cursor:** use `video.cursor: client`. VNC and RDP receive source cursor
  shapes; Web, Moonlight and the local viewer do not consume this cursor API.
  GPU cursor composition is not implemented.

Both modes select display channel 0 and one desktop. Local GL starts at the
first usable GPU scanout; remote TCP follows the primary framebuffer, including
boot displays. VM management, multi-monitor selection, clipboard, USB redirection
and migration remain outside this source.

## Validation

### Offline checks

```sh
go test -race ./internal/spice
go test -race ./internal/capture -run DMABUF
go test -race ./internal/app -run SPICE
python3 tools/test_spice_bitmap.py
python3 tools/test_spice_layout.py
python3 tools/test_spice_build_tags.py
```

The Go tests cover format routing, codec capabilities, frame ownership, pacing,
resize, idle refresh and format changes across reopen. They also check that KMS
and local SPICE keep separate fallback/cache policies, that a deferred draw is
retained until processing or close, and that remote settings keep BGRA capture.
Mixed-session tests exercise video/audio/cursor isolation during failures,
blocked encoding and teardown.

The native C fixtures cover pixel formats, row padding, orientation and the
remote primary-display lifecycle. Run them with memory/undefined-behavior checks:

```sh
CC='clang -fsanitize=address,undefined -fno-omit-frame-pointer' python3 tools/test_spice_bitmap.py
```

Offline checks do not establish VM interoperability or latency.

### Remote VM check (pending)

With a running SPICE TCP VM and `VM_SPICE_PASSWORD` set:

```sh
ANYKVM_SPICE_TEST_REMOTE=192.0.2.10:5900 \
ANYKVM_SPICE_TEST_PASSWORD_ENV=VM_SPICE_PASSWORD \
go test -tags spice -v ./internal/spice -run '^TestNativeSPICERemote$' -count=1
```

This opt-in test checks bitmap capture, frame ownership and idle refresh without
a local GPU. Also verify boot, resize, audio, cursor, input and reconnect with the
intended viewer.

### Local GPU check (pending)

On a Linux GPU host with a running SPICE GL VM:

```sh
ANYKVM_SPICE_TEST_SOCKET=/run/user/1000/anykvm-vm.spice \
ANYKVM_SPICE_TEST_RENDER_NODE=/dev/dri/renderD128 \
ANYKVM_SPICE_TEST_PASSWORD_ENV=VM_SPICE_PASSWORD \
go test -tags spice -v ./internal/spice -run '^TestNativeSPICEDMABUF$' -count=1
```

This test checks H.264 output and idle keyframe recovery. Verify Venus separately
inside the guest, then check picture quality, resize, audio, cursor and reconnection
with the intended viewer.

## Shared implementation

Linux `screen` and local SPICE use
[`capture.DMABUFEncoder`](../internal/capture/dmabuf_encoder.go). It reuses the
format router in [`capture/media.go`](../internal/capture/media.go), then passes
borrowed images through [`dmabuf_frame.go`](../internal/capture/dmabuf_frame.go)
for video/JPEG encoding, GPU snapshots or explicit pixel conversion. The internal
router type is named `ffmpegDevice`; this path opens no FFmpeg capture session.

Each source retains its own acquisition and lifetime rules:

- **KMS (`screen`):** exports a DMA-BUF for the current desktop and closes its FD
  after synchronous processing. It uses the default pipeline options, permits
  pixel fallback and obtains a fresh source image for subsequent captures.
- **Local SPICE:** keeps the upstream draw leased during pacing and GPU work.
  It enables `RequireGPUEncoding` and `IdleRefresh`. HEVC/AV1 declarations come
  from the Vulkan path, including when FFmpeg is compiled in.
- **Remote SPICE:** receives owned BGRA snapshots through `remote_bitmap.go`.
  Its capability wrapper leaves direct encoding to downstream consumers.

### Draw and output ownership

`EncodeSource` borrows the source FD and producer draw/fence for the duration of
its call. The source remains responsible for releasing that lease. A completed
SPICE draw is acknowledged after GPU work; a draw deferred by pacing or a full
snapshot pool stays pending until a later capture or close.

Returned frames contain owned data or an owned GPU snapshot. Local SPICE keeps
an idle-refresh reference: video retains metadata and re-encodes the encoder's
own GPU input, while JPEG/pixel/surface output retains owned storage. Closing a
session drops its cache and resources; frames already held by downstream
consumers remain valid until their final buffer reference is released.

### Source files

- [`dmabuf_encoder.go`](../internal/capture/dmabuf_encoder.go): shared routing,
  source policies, session configuration and idle-refresh ownership.
- [`dmabuf_frame.go`](../internal/capture/dmabuf_frame.go): borrowed GPU frame
  adapter, conversion, JPEG/video input and bounded snapshot ownership.
- `capture/dmabuf_linux.go` and `dmabuf_other.go`: native JPEG construction and
  cached optional-codec declarations for each platform.
- [`spice/source.go`](../internal/spice/source.go): session lifecycle and local/
  remote dispatch. `local_gl.go` owns draw pacing, encoding and idle refresh;
  `video_options.go` forwards viewer settings to the shared pipeline.
- `spice/endpoint.go` and `options.go`: endpoint parsing and setting validation.
- `spice/remote.go` and `remote_bitmap.go`: remote capabilities, pacing, primary
  removal and owned bitmap snapshots.
- `spice/native_linux.go`: CGO adapter. `native_remote_linux.h` handles primary
  callbacks on the GLib thread; `native_bitmap.h` converts pixels.
- `spice/testdata/bitmap_*.c`: offline fixtures run by
  `tools/test_spice_bitmap.py`.
