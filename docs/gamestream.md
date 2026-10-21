# Experimental GameStream / Moonlight access

AnyKVM implements pairing, target app lists, launch/resume/cancel, RTSP,
ENet control, H.264 and optional HEVC/AV1 video with UDP FEC, plus stereo Opus.
One viewer controls capture at a time; a new VNC/Web/RDP session takes ownership
and closes GameStream. CGO is required; pinned ENet/nanors sources are restored
by the [build scripts](build.md), without a system ENet dependency.

## Pairing and access

Add the GameStream listener to a
[configured target setup](configuration.md#local-desktop-example):

```yaml
gamestream:
  addr: ":47989"
  state: ""
  codecs:
    hevc: false
    av1: false
  profile: constrained_baseline
  lowdelay: true
  pacing_mbps: 0
```

Run `anykvm -config /path/to/config.yaml -gamestream-pair` in a terminal:

1. Manually add the server's LAN IPv4 address in Moonlight; automatic discovery
   is not implemented.
2. Start pairing and enter Moonlight's four-digit PIN in the AnyKVM terminal.
   Each attempt expires after two minutes.
3. Start with 720p / 30 FPS / H.264 / stereo / 5–10 Mbps and launch a target.
   Quit the current app before choosing another. AnyKVM target-switch hotkeys
   also work; the session's negotiated output size stays fixed.

After quitting, wait until Moonlight reflects the stopped session before
launching again. Immediate relaunch while the client still shows the previous
session state can fail. This state-synchronization issue remains in
[TODO](../TODO.md#gamestream).

Later runs omit `-gamestream-pair`; existing paired clients still authenticate.
Background services must omit the interactive pairing flag. `state.json` beside
the executable holds host identity/private key and paired certificates, with
private Unix permissions. Keep it across restarts; deleting it requires pairing
again. `gamestream.state` selects another file, with explicit relative paths
resolved from the working directory. VNC/Web passwords do not grant access.
Target names must be unique/nonempty; renaming changes their stable app IDs.

## Ports and codecs

Default TCP ports are 47989 (HTTP), 47984 (HTTPS) and 48010 (RTSP). UDP ports are
47998 (video), 47999 (control) and 48000 (audio). A custom HTTP base port uses
HTTPS -5, RTSP +21, and UDP +9/+10/+11. Avoid port conflicts with other servers
and the Web listener. Other AnyKVM services may remain enabled.

H.264 is always available in negotiation. HEVC/AV1 are independently disabled
by default; enable the corresponding `gamestream.codecs` entry and restart.
They require a compatible encoded source or hardware encoder; see
[codec backends](build.md#native-and-optional-codecs). Raw/JPEG inputs use hardware
encoding for HEVC/AV1, with no software fallback.

Codec availability is checked during negotiation. If encoding fails, the session
does not switch codecs automatically; disable the failing codec and reconnect.
Browser codec settings are independent.

Video is SDR 4:2:0; HDR/Main10, surround audio and gamepads are unsupported.

For native macOS H.264, `profile` accepts constrained_baseline (default),
baseline, main or high. `lowdelay` defaults to true and controls low-latency rate
control. Native paths use one temporal layer, disable LTR/reordering and request
one reference buffer on supported native Intel Macs. These settings do not
rewrite compressed sources or affect HEVC/other encoders. Client compatibility
depends on the selected encoder/profile; check nondefault combinations on the
intended client.

## Video and recovery

Desktop sources supporting output scaling use negotiated resolution/FPS/bitrate.
Capture cards, USB iPhone video and direct compressed sources retain their source
size; match the client resolution/aspect when its decoder or pointer mapping
requires it. Capture-mode selection remains independent of output negotiation.

Compatible encoded frames pass through without transcoding, retaining source
bitrate and ordered prediction references. Raw/MJPEG or incompatible formats
need an encoder. AirPlay uses a separate [compatibility path](iphone-airplay.md#gamestream-test);
CoreDevice has its own [native/USB policies](iphone-coredevice.md#web-and-gamestream).
Absolute input compensates for known video padding; the host cannot force the
client's local window geometry or decoder selection.

Fixed-rate capture cards may use an integer frame divisor when it matches the
requested rate within 0.1% (60→30, 59.94→29.97 for a 30 FPS request). Other rates
retain source delivery; no frames are duplicated to raise FPS. USB iPhone capture
uses arrival-time pacing because nominal AVFoundation rates can differ from
actual delivery. Capture timestamps retain source intervals in RTP.

Recovery first requests a keyframe from the encoder/source. A source without
feedback may use bounded capture restarts only when its backend permits it;
passive AirPlay is not restarted to force recovery. Direct encoded sources need
usable periodic/recovery pictures. Very large pictures can exceed bounded FEC
packetization and end the session with a diagnostic.

`pacing_mbps` limits short UDP bursts, not video quality bitrate: 0 uses an
800 Mbps ceiling. It does not detect link capacity. The negotiated client total
bitrate reserves FEC/audio/protocol overhead before setting the encoder budget;
already encoded passthrough retains its original bitrate.

## Audio and input

Audio follows the [target configuration](configuration.md#local-desktop-example).
Disabled or absent source audio creates neither an encoder nor synthetic
silence. Missing Opus support leaves video/input usable; see
[Opus dependencies](build.md#native-and-optional-codecs).

Input supports keyboard, relative/absolute movement, three buttons and vertical
wheel, releasing held state on disconnect. Unicode text injection and clipboard
are unsupported. Client OS is detected only where evidence exists; device names
and IP addresses are not OS identifiers. Assign an OS to a paired certificate
using `gamestream.client_os`, keyed by its SHA-256 `client_cert` fingerprint from
the launch log. Values are darwin, macos, windows, linux, android, ios or unknown.
Assignments override User-Agent detection, do not authorize an unpaired client
and do not remap shortcuts by default.

Target covers use embedded source-platform artwork; see
[cover art](gamestream-cover-art.md) for assets, selection and rendering.

## Validation

After preparing build inputs:

```sh
go test -race ./internal/gamestream ./internal/bridge
ANYKVM_FFMPEG_LIBDIR=/path/to/ffmpeg9/lib ANYKVM_TEST_AVMEDIA=1 \
  go test -tags ffmpeg -race ./internal/avmedia ./internal/gamestream
```

On Windows, `ANYKVM_TEST_WINDOWS_VENDOR=nvenc,amf,qsv` (installed GPUs only)
and `ANYKVM_TEST_VENDOR_HEVC=1` enable `TestWindowsVendorNative` to test HEVC
without FFmpeg libraries. Optional `ANYKVM_TEST_FFMPEG` supplies a test-only independent decoder. Tests
cover pairing, encrypted RTSP/control, FEC, input release, takeover and synthetic
media. Maintainer real-client testing is complete across desktop, Android and
iOS for pairing, target listing, H.264/HEVC playback, audio, target switching
and reconnection. Immediate relaunch during the stale client-state window
remains an open issue. Logs beginning `gamestream:` report negotiation, encoder,
input timing and stop reasons.

## References

- [Sunshine](https://github.com/LizardByte/Sunshine)
