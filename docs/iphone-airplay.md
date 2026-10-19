# Experimental AirPlay capture

AnyKVM receives iPhone/iPad Screen Mirroring, companion audio and legacy-paired
standalone audio in process. Ordinary MP4/HLS URL playback uses the Web viewer.

## Configuration

Add an AirPlay target and enable the desired viewer listener:

```yaml
web:
  http: ":8088"
targets:
  - name: iPhone AirPlay
    video:
      source: airplay:AnyKVM-AirPlay-Test
      mode: "1920x1080@60"
    airplay:
      addr: ":7100"
      interface: ""
    audio:
      source: auto
```

`airplay` advertises `AnyKVM`; `airplay:NAME` sets the receiver name, not a sender
selector. Names must contain 1–63 UTF-8 bytes. The control port defaults to 7000;
7100 avoids the usual macOS AirPlay Receiver port. `interface` can select an
active multicast interface such as `en0`; it must match the bind address.
Multiple receivers need distinct names and ports.

`video.mode` requests a display size and FPS cap in `/info`; the sender chooses
actual dimensions, aspect, bitrate and cadence. Empty dimensions request
1920x1080. Changing the request requires restarting capture and reconnecting
Screen Mirroring. No fixed sender Mbps limit or sender IDR request is implemented.

`audio.source: auto` follows AirPlay audio; an empty string disables it and an
explicit device selects external audio. AirPlay provides no keyboard/mouse
backend. Configure a supported external HID device only if input is needed.

Discovery publishes `_airplay._tcp` and `_raop._tcp`, using system Bonjour on
macOS and Go mDNS elsewhere. Allow local multicast UDP 5353, the configured TCP
control port, an ephemeral TCP video port and ephemeral UDP timing/audio ports.
Automatic selection excludes VPN tunnels; an explicit interface overrides it.

## Web test

Follow [build preparation](build.md), then build and generate a local configuration:

```sh
go build -o build/anykvm ./cmd/anykvm
./build/anykvm -gen-config build/airplay.yaml
```

Replace its `web` and `targets` blocks with the example above, leaving the other
listeners disabled. Start it with `./build/anykvm -config build/airplay.yaml`.

1. Put the phone and server on the same LAN. In Control Center → Screen
   Mirroring, select **AnyKVM-AirPlay-Test**.
2. Open `http://localhost:8088/?codec=h264`. The receiver is advertised at server
   startup, so the viewer may also connect first.
3. Check motion, orientation changes, an idle pause, Stop Mirroring and reconnect.
   Click the speaker control if browser autoplay blocks sound.

H.264 uses browser WebCodecs passthrough, preserving source presentation timing
without host video decoding. Use localhost or HTTPS for WebCodecs. Plain LAN
HTTP normally uses JPEG fallback; macOS decodes/encodes that path natively,
while other platforms need the optional FFmpeg backend.

See [Web buffering and diagnostics](viewing.md#buffering-and-diagnostics) for
`web.buffer_frames` and `debug=video`. Changing codecs can reopen AirPlay capture
and require sender reconnection.

## GameStream test

Keep the AirPlay target above, enable the GameStream listener and follow
[GameStream pairing](gamestream.md#pairing-and-access). Select the AirPlay receiver
on the phone and launch **iPhone AirPlay** in Moonlight, starting with H.264 /
1080p / 60 FPS. Close other viewers to avoid competing takeovers.

GameStream decodes AirPlay H.264 and re-encodes it for Moonlight compatibility;
its bitrate setting controls the host encoder, not the phone. macOS uses
VideoToolbox. Recovery requests use the local encoder without restarting
AirPlay. A viewer waiting for the phone receives black waiting video after two
seconds, replaced by a fresh IDR when real pictures arrive. Source capture
intervals remain in RTP timing.

Launch the viewer promptly if mirroring connects first: a long wait can create
TCP backlog. Viewer detach ends the active phone session while discovery stays
available; process exit closes listeners and discovery. See
[GameStream ports](gamestream.md#ports-and-codecs) for network setup.

## Ordinary media URL/HLS playback

Legacy `POST /play` accepts HTTP(S) progressive MP4, HLS media/master playlists
and sender-mediated `mlhls://localhost/` / `nfhls://localhost/` FCUP resources.
These play only in Web. The server proxies containers on the authenticated Web
origin; Safari can use native HLS and other MSE browsers use bundled hls.js.
URL/HLS/FCUP playback needs no FFmpeg. Codec support depends on the
browser. VNC, RDP and GameStream receive mirroring, not URL containers.

Rate, scrub, playback-info and stop requests control the browser. PTTH reverse
connections carry playback events and FCUP resource requests; `/action` supplies
resource replies. Ownership is checked against sender/session/request IDs, and
stale feedback or replies are rejected. One URL or mirroring session owns the
receiver at a time. Matching paired senders can switch from RTP to URL playback.
Browser autoplay may require a click or begin muted.

The proxy supports relative playlist URLs, signed CDN queries, Range and HEAD,
without browser CORS or mixed-content requests. Only issued resources can be
fetched. Registries, FCUP caches and resource bodies are bounded; stop, viewer
disconnect and shutdown cancel pending work. DRM/FairPlay-protected media,
photos, advertisement transitions and app-specific property negotiation are
unsupported. YouTube App compatibility still needs device testing.

## Audio, dependencies and limits

- Mirroring implements H.264 with legacy Ed25519/X25519 pairing and FairPlay v3.
  HEVC mirroring, HomeKit/TLV8/PIN pairing and persistent client registration
  are unsupported.
- No sender PIN/password or allowlist is enforced. `web.password` and
  `vnc.password` protect their viewers only. Receiver keys survive capture
  reopen, but are regenerated when the process restarts.
- Type-96 audio accepts companion AAC-ELD (44.1 kHz stereo, 480 samples/packet)
  and standalone ALAC (44.1 kHz, 16-bit stereo, 352 samples/packet). Bounded
  retransmission/reordering skips unresolved gaps after 30 ms. A viewer must
  remain connected for audio delivery.
- Web forwards AAC-ELD when the browser accepts the exact format; otherwise it
  receives decoded/resampled 48 kHz stereo PCM. RDP receives PCM; GameStream
  encodes it to Opus. Sender volume/mute and FLUSH are handled. Stream-specific
  teardown can stop audio or mirroring independently while retaining pairing.
- macOS uses AudioToolbox, VideoToolbox and ImageIO for mirrored video/audio,
  JPEG and GameStream, without FFmpeg when system codecs work. Other platforms
  need a CGO `ffmpeg` build and FFmpeg 9 libraries for decoded output. See
  [media dependencies](build.md).
- Source preparation restores PlayFair's five C source files and headers from
  the pinned inputs in `tools/sources.lock.json`. CGO inclusion wrappers compile
  them, and the Go wrapper validates lengths and mode before entering C.
  PlayFair requires little-endian amd64/arm64; unsupported builds fail before
  opening the receiver or advertising discovery. Buffered AirPlay 2 audio and
  older ANNOUNCE/SDP audio sessions are unsupported.

## Validation

```sh
go test -race ./internal/airplay ./internal/wsmedia ./internal/app
```

Synthetic tests cover pairing, FairPlay, encrypted H.264/audio, teardown/reopen,
Web feedback, HLS/FCUP ownership and cancellation. Generate their inputs using
the [synthetic media guide](../internal/testdata/README.md). macOS system-codec checks use
`ANYKVM_TEST_SYSTEM_AIRPLAY=1`; FFmpeg checks additionally need `-tags ffmpeg`
and matching libraries. Maintainer testing confirmed the macOS AirPlay path,
including discovery, video/audio delivery, idle/reconnect and Web/GameStream
playback. Windows and Linux AirPlay capture have not been physically tested.

## References

- [OpenAirPlay specifications](https://github.com/openairplay/airplay-spec)
- [UxPlay](https://github.com/FDH2/UxPlay)
- [Apple Lossless Audio Codec](https://github.com/macosforge/alac)
