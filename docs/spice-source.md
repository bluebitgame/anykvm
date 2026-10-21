# Linux SPICE GL upstream (experimental)

AnyKVM can capture a local QEMU SPICE GL console on Linux and encode it as
H.264 using Vulkan Video. This targets Venus/virtio-gpu desktops and keeps
video pixels on the GPU. Real GPU/VM compatibility testing is still pending.

## Requirements and build

- Linux with CGO and the optional `spice` build tag.
- A local SPICE GL Unix socket and permission to access its render node.
- A GPU/driver supporting Vulkan 1.3 H.264 encoding and EGL/Vulkan DMA-BUF
  interoperability. Use the same physical GPU for QEMU and AnyKVM initially.

Install the usual [build requirements](build.md), then on Debian/Ubuntu:

```sh
sudo apt install libspice-client-glib-2.0-dev libegl1-mesa-dev libgbm-dev libdrm-dev
python3 tools/build.py --targets native --tags spice
```

Prebuilt releases do not include SPICE support. The build script does not bundle
these extra libraries; cross builds require matching target libraries.

## Configure the VM

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

Use a private socket directory and configure SPICE ticket authentication.
The guest needs compatible virtio-gpu and Mesa Venus drivers. See the
[QEMU guide](https://www.qemu.org/docs/master/system/devices/virtio/virtio-gpu.html)
and [Mesa requirements](https://docs.mesa3d.org/drivers/venus.html).

## Configure AnyKVM

```yaml
targets:
  - name: Linux VM
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

Set `VM_SPICE_PASSWORD` to the VM's SPICE ticket before starting AnyKVM.
Use either `password_env` or `password`, not both. Match `render_node` to QEMU.
`timeout` waits for the first GL frame (default 15 seconds, maximum 5 minutes).
Set `audio.source: ""` to disable playback audio.

## Supported features and limits

- **Video:** H.264 only, with negotiated size/FPS/bitrate for GameStream.
  Unsupported GPU paths fail without CPU capture or software encoding fallback.
  Downstream viewers may still require decoding or conversion.
- **Audio:** guest playback; microphone redirection is unsupported.
  Audio failure does not stop video.
- **Input:** keyboard, three mouse buttons and wheel. Absolute input requires
  client mouse mode and a guest tablet. Pause is unsupported.
- **Cursor:** VNC and RDP receive source cursor shapes. Web, Moonlight and the
  local viewer do not; GPU cursor composition is unsupported.
- **Display:** one scanout on display channel 0. No pre-GL boot framebuffer,
  multi-monitor selection, clipboard, USB redirection or migration support.
  AnyKVM does not create or manage the VM.

## Validation

On the Linux GPU host with a running VM, run:

```sh
ANYKVM_SPICE_TEST_SOCKET=/run/user/1000/anykvm-vm.spice \
ANYKVM_SPICE_TEST_RENDER_NODE=/dev/dri/renderD128 \
ANYKVM_SPICE_TEST_PASSWORD_ENV=VM_SPICE_PASSWORD \
go test -tags spice -v ./internal/spice -run '^TestNativeSPICEDMABUF$' -count=1
```

The test checks H.264 output and idle keyframe recovery. Also check picture
quality, resize, audio, cursor and reconnection with the intended viewer.
Verify Venus inside the guest separately; passing build or unit tests does not
establish hardware compatibility or latency.
