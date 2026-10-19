# Direct iPhone capture and input

AnyKVM supports two explicit USB video paths:

- `coredevice[:UDID]`: phone-native DisplayService HEVC
  over a CoreDevice tunnel, with Universal HID input.
- `ios-usb[:name-substring]`: macOS AVFoundation USB capture, optionally combined
  with standalone CoreDevice HID. Video uses the host's native media backend.

The selected backend is fixed. DisplayService startup errors do not switch to
AVFoundation. Protocol integration is Go; no Python worker or phone app is needed.

## Requirements and configuration

Unlock the phone, trust the host, enable Developer Mode and mount a compatible
Developer Disk Image, for example through Xcode. AnyKVM does not download or
mount developer images. DisplayService and supported Universal HID services must
be published by the device's OS/DDI. iOS 17.4 is the minimum for the USB tunnel
proxy, not a guarantee that these developer services are available.

Add a target to a configuration with the desired viewer listeners:

```yaml
targets:
  - name: iPhone
    video:
      source: coredevice
    coredevice:
      timeout: 60s
    hid:
      device: auto
      mouse_mode: absolute
    audio:
      source: auto
```

`coredevice.timeout` defaults to 60 seconds. With several phones, use
`coredevice:<UDID>`; `anykvm -list-devices -list-ios` lists selectors.
`hid.device: auto` uses source input; an empty string disables it. Other explicit
HID selectors are rejected for direct CoreDevice video. `audio.source: auto`
uses phone audio; general audio selection follows the
[configuration guide](configuration.md#local-desktop-example).

macOS/Linux can create a kernel tunnel through trusted USB CoreDeviceProxy.
Creating the interface requires root on macOS or network administration
privileges on Linux, where `ip` must be installed. To use an existing reachable
kernel tunnel instead, set `coredevice.rsd` inside that target to its RSD
endpoint, for example `'[fd00::2]:58783'`, using the tunnel's actual address/port.
Windows requires an existing tunnel via
`rsd`. TCP-only userspace proxies cannot deliver inbound RTP UDP. AnyKVM closes
only tunnels it created. See [build setup](build.md)
before building; run `sudo anykvm -config /absolute/path/to/config.yaml` on macOS
when automatic tunnel creation is needed.

## AVFoundation video with standalone CoreDevice input

On macOS, explicitly select USB video and CoreDevice HID:

```yaml
targets:
  - name: iPhone USB
    video:
      source: ios-usb
      mode: '@30'
    hid:
      device: coredevice
      mouse_mode: absolute
    coredevice:
      timeout: 60s
    audio:
      source: auto
```

This path opens AVFoundation video before HID and never negotiates DisplayService.
HID requires a supported Universal HID service and the actual `mainTouchscreen`
surface. A HID startup failure closes video and its tunnel. Unlock/trust,
Developer Mode, DDI and tunnel requirements still apply; surface enumeration
alone does not prove that apps accept injected events.

With several phones, pair `ios-usb:<name-substring>` with `coredevice:<UDID>`
for the same phone. AVFoundation names are not UDIDs. USB video retains source
dimensions and follows app rotation. NV12 can feed VideoToolbox directly; host
codec/bitrate settings apply. Requested FPS does not duplicate source frames.

Standalone HID polls SpringBoard orientation and maps touch back to the fixed
portrait touchscreen. Video dimensions gate rotation changes, and an active drag
is released before remapping. Unavailable orientation is retried with a warning;
automatic landscape touch mapping then remains unavailable. This mode avoids
requiring DisplayService. Physical input has passed testing with the maintainer's
macOS setup; other iOS/DDI combinations depend on the services the phone exposes.

## Web and GameStream

For DisplayService, use `?codec=hevc` for native HEVC. Native H.264 is
unsupported. `?codec=jpeg` needs the optional FFmpeg HEVC decoder; see
[media setup](build.md#native-and-optional-codecs). Native passthrough loads no
host decoder. DisplayService does not produce AV1.

[Web viewing requirements](viewing.md#browser-access) apply. Web rotates portrait
DisplayService buffers to match interface orientation,
mapping input back into encoded coordinates; landscape buffers are not rotated
again. Geometry/orientation changes release held input. AVFoundation video is
already upright and is not rotated again.

Follow [GameStream setup and pairing](gamestream.md#pairing-and-access).
Use HEVC for DisplayService, or a supported host codec
for USB video. Start at 30 FPS and match Moonlight's custom resolution to the
logged source dimensions when required by its decoder or pointer mapping.

GameStream does not apply Web presentation rotation to DisplayService video;
input uses encoded-picture axes. Select the USB hybrid for AVFoundation's upright
video. GameStream size/bitrate/FPS settings do not configure the phone's native
encoder. Host `gamestream.lowdelay` applies only to host encoding. Direct-stream
recovery requests fresh native video and uses bounded capture restarts if no
recovery frame arrives. Do not start a second DisplayService stream on the phone.

The last connecting viewer protocol takes control. Close Web before launching
Moonlight, or its reconnection can repeatedly take ownership. Use the shared
[Web diagnostics](viewing.md#buffering-and-diagnostics) for queues and recovery.

## Audio and input limits

`ios-usb` captures decoded 48 kHz stereo S16LE PCM through the same AVFoundation
session, if the USB device exposes an audio port. It does not open a microphone
or provide compressed AAC passthrough.

DisplayService negotiates a separate 48 kHz stereo AAC-ELD RTP stream, with
bounded reordering and RTCP feedback. Failed audio negotiation leaves video
available. Web forwards AAC when its exact format is supported, otherwise
receives PCM. GameStream uses Opus; macOS AudioToolbox decodes source AAC as
needed. Audio does not change video geometry or HID mapping.

Input supports one-finger touch/drag, wheel-to-swipe and held keyboard reports.
Closing releases touches and keys. Right/middle buttons, system buttons,
multitouch, Unicode text injection and clipboard are unsupported. Missing
DisplayService/HID services, error 9021 and rejected media startup fail explicitly.
There is no verified control here for the phone encoder's orientation, size or
FPS. Hardware decoder compatibility must be checked against the logged SPS
profile, chroma, depth and dimensions.

## Validation

After preparing build inputs:

```sh
go test -race ./internal/coredevice ./internal/app ./internal/wsmedia
```

Synthetic tests cover negotiation, malformed RTP, reordering/recovery, HID,
orientation and configuration. Maintainer physical-device testing is complete
on macOS for USB video/audio and CoreDevice video, input, audio, rotation and
reconnect. CoreDevice capture on Windows and Linux has not been physically
tested. The existing tunnel requirement on Windows still applies. macOS USB
capture also needs the corresponding system permissions.

## References

- [go-ios](https://github.com/danielpaulus/go-ios)
- [pymobiledevice3](https://github.com/doronz88/pymobiledevice3)
