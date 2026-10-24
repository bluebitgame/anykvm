# VNC, RTSP and RDP upstream sources

Set `targets[].video.source` to a VNC, RTSP or RDP URL to feed a remote source into
AnyKVM's viewer services. A target's `rdp` block configures its upstream
connection; the top-level `rdp` block configures the [AnyKVM listener](rdp-server.md).
For Apple Screen Sharing, see [ARD](ard-client.md). For Linux local QEMU
SPICE TCP and the local GL/DMA-BUF path, see [SPICE](spice-source.md).

## Configuration

```yaml
targets:
  - name: Camera
    video:
      source: rtsp://camera.example/live
    rtsp:
      username: user
      password_env: CAMERA_PASSWORD
      transport: tcp
      timeout: 10s
  - name: Remote desktop
    video:
      source: rdp://desktop.example:3389
      mode: "1920x1080@30"
    rdp:
      username: user
      password_env: DESKTOP_PASSWORD
      domain: EXAMPLE
      security: nla
      timeout: 15s
```

Omitted `audio.source` and `hid.device` default to `auto`, following the source's
available audio and input. Set either to an empty string to disable it. RTSP
provides audio but no input; RDP provides both through the video session.

RTSP and RDP URLs may contain `user:password@host`; percent-encode reserved
credential characters and bracket IPv6 addresses. Explicit credential fields
override URL credentials. `password_env` is resolved on every open and cannot
be combined with `password`. Saved source passwords select private file
permissions and are redacted/retained by the Web configuration editor.

## VNC support and Wake-on-LAN

For a remote VNC desktop, use `video.source: vnc://host:5900`. Automatic input
and available source audio share that connection:

```yaml
targets:
  - name: Remote VNC desktop
    video:
      source: vnc://host:5900
    audio:
      source: auto
    hid:
      device: auto
```

The client requests Raw, CopyRect and desktop-resize updates. Tight, Hextile and
ZRLE decoding are not negotiated by this source. Audio requires the upstream
server's QEMU Audio extension; video and input work without it. Relative movement
uses QEMU pointer mode when available or updates the remembered absolute pointer position otherwise. These source
limits are separate from the AnyKVM VNC listener's supported encodings.

Set `targets[].wol.mac` to wake an unreachable VNC source. Optional
`wol.broadcast` defaults to `255.255.255.255:9`; use your subnet broadcast or an
explicit destination, bracketing IPv6 when a port is included. An empty MAC
disables waking. Reachable targets connect directly; authentication failures
do not trigger wake packets. The Web target editor exposes these options.

## RTSP support

The Go client implements DESCRIBE/SETUP/PLAY, keepalive, Basic/Digest MD5
authentication and interleaved RTP/RTCP over TCP. `rtsps://` verifies TLS
certificates. Default ports are 554 for RTSP and 322 for RTSPS.

H.264 supports RFC 6184 modes 0/1: single NAL, STAP-A and FU-A. SDP parameter
sets determine dimensions. Packet gaps discard the damaged access unit and
suppress prediction frames until an IDR; complete units remain ordered.

Audio supports AAC-LC MPEG4-GENERIC/AAC-hbr (13/3/3-bit AU headers) and 8 kHz
mono G.711 PCMU/PCMA. AAC remains encoded for capable consumers; G.711 converts
to 48 kHz stereo PCM. An absent or unsupported audio track leaves video usable.
UDP, multicast, HEVC and generic HLS/IPTV playback are unsupported.

Connection and H.264 passthrough need no FFmpeg. Pixel/JPEG output needs H.264
decoding, using VideoToolbox on macOS or the optional FFmpeg backend elsewhere.
AAC-to-PCM uses AudioToolbox on macOS or optional FFmpeg.

## RDP support and limits

The experimental Go client uses NLA/CredSSP v5+ with NTLMv2 over verified TLS.
`security: tls` selects TLS-only login. `insecure_skip_verify: true` explicitly
allows an untrusted certificate for a controlled test server. A domain can also
be supplied as a percent-encoded `DOMAIN\user` URL username. Kerberos, legacy
RDP encryption and interactive authentication are unsupported.

Video uses RDPGFX 8.1 AVC420. The client decodes H.264 surfaces in order,
composes update rectangles into complete BGRA desktop snapshots and acknowledges
frames. Only complete snapshots can replace queued snapshots. macOS uses
VideoToolbox; other platforms need optional FFmpeg.

Input supports scan codes, modifiers, absolute/relative movement, three buttons
and wheel; closing releases held input. RDPSND receives 48 kHz stereo S16LE PCM,
with confirmations and bounded queues, without an audio codec dependency.

The source supports at most four surfaces, their mapping/deletion and desktop
reset. It requires licensing completion with `STATUS_VALID_CLIENT`; a full
licensing challenge exchange is absent. Legacy bitmap updates, AVC444,
ClearCodec/Planar/Progressive, unsupported graphics/cache operations, upstream
cursor shapes, clipboard and drives are not implemented in the client.

## Validation

After [preparing build inputs](build.md), run:

```sh
go test -race ./internal/rtsp ./internal/rdp ./internal/app
```

Local tests cover RTSP authentication, fragmented H.264/audio, RDP NLA/TLS,
H.264 composition, input and PCM. Maintainer testing confirmed real upstream RDP
video, input and audio through the downstream outputs, including resize,
reconnection and takeover. Video can still be lost after sleep/wake; this known
exception is tracked under [session recovery](../TODO.md#session-recovery).
Real-camera RTSP compatibility still needs testing. Remaining RDP implementation
work is tracked in [TODO](../TODO.md#capture-sources-rdp-client).
