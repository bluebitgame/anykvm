# Experimental ARD capture client

`video.source: ard://host[:port]` connects to Mac Screen Sharing as a capture
source with remote keyboard/mouse input. The implementation is in `internal/ard`.
Its High Performance path receives single-picture HEVC and decodes it to BGRA.

## Configuration

Add this target to a configuration with the desired VNC, Web, GameStream or RDP
listeners, then run `anykvm -config /path/to/config.yaml`:

```yaml
targets:
  - name: Remote Mac
    video:
      source: ard://mac.example:5900
      mode: "@60"
    ard:
      username: mac-account
      password_env: ANYKVM_MAC_PASSWORD
      codec: hevc
      display_mode: mirror
      timeout: 10s
      fallback: deny
```

The default port is 5900; bracket IPv6 addresses. The URI accepts only a host
and port. Put credentials in `ard.username` and either `password_env` or
`password`. Environment passwords are read on every open. Each credential is
limited to 63 UTF-8 bytes without NUL. Configurations containing passwords are
saved with private Unix permissions.

- `codec: auto` (default) and `hevc` select experimental HEVC. High Performance
  H.264 is not implemented: `h264` fails with `fallback: deny`, while
  `fallback: vnc` starts an ordinary RFB session directly.
- `display_mode: mirror` is the default. Private displays are unsupported.
  Requested dimensions do not resize the remote mirror desktop; `@60` requests
  an update cap, while delivered geometry and cadence follow the remote source.
- `timeout` defaults to 10 seconds and bounds startup through the first picture.
- `fallback: deny` is the default. `vnc` permits a new ordinary RFB connection
  when initial High Performance negotiation is unsupported or times out.
  Authentication failure, private-display requests and active-session failures
  never trigger fallback. The fallback pixel/input transport is plaintext.

The HEVC path uses the optional FFmpeg software decoder. Follow
[FFmpeg build/runtime setup](build.md#native-and-optional-codecs).

## Supported behavior and limits

Apple account authentication types 30 and 33, keyed media negotiation,
AES-CBC/ComCryption control records and authenticated SRTP HEVC reception are
implemented. An audio offer is required during negotiation, but ARD audio
forwarding is not implemented.

The receiver accepts single NAL, aggregation and fragmentation packets, with
bounded reordering and replay/integrity checks. Damaged pictures are discarded;
dependent output waits for an IRAP with VPS/SPS/PPS. SRTCP reports, FIR and a
rate-limited media restart provide recovery. Source RTP timing is retained;
RTCP sender-clock synchronization and encoded downstream passthrough are absent.

Input supports VNC keysyms, absolute/relative movement, three buttons and vertical
wheel. Generic RFB cursor shapes can reach VNC/RDP output; Apple cached alpha
cursors, remote cursor position and Web/GameStream cursor-shape transport remain pending.
Closing releases held input and all workers; reopening creates fresh session
state. An idle desktop is distinguished from a terminated stream.

Only one physical display and complete pictures up to 8,912,896 luma samples are
supported. Compound/tiled HEVC, private displays, High Performance H.264 and
hardware decode are unsupported. HEVC 4:4:4 compatibility remains unverified.

## Validation

After [preparing build inputs](build.md):

```sh
go test -race ./internal/ard ./internal/bridge ./internal/vnc ./internal/app
ANYKVM_TEST_AVMEDIA=1 ANYKVM_FFMPEG_LIBDIR=/absolute/path/to/ffmpeg9/lib \
  go test -tags ffmpeg -race ./internal/ard -run TestHEVCEncryptedCaptureRoundTrip -v
```

Tests cover credential ciphers, SRTP tampering/replay/reordering, HEVC assembly,
fallback, lifecycle, input release and synthetic encrypted capture. Real-Mac
validation is still needed for HEVC negotiation/4:4:4, codec parameters, cursor
behavior, idle/resize, loss recovery, reconnect, takeover and downstream output.

Remaining work is in [TODO](../TODO.md#capture-sources-ard-client).

## References

- [rootshell-vnc](https://github.com/kitknox/rootshell-vnc)
