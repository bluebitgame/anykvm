# Experimental RDP graphics server

AnyKVM's Go RDP server publishes the active target through TLS and RDPGFX.
It supports AVC420 H.264, RemoteFX Progressive, Planar YCoCg420 and ClearCodec,
plus keyboard/mouse, cursor, audio, clipboard and client-drive access.
Authentication and graphics negotiation complete before control takeover.
For the upstream RDP capture source, see [network sources](network-sources.md).

## Connection and configuration

RDP is disabled by default. Add the listener to a
[configured target setup](configuration.md#local-desktop-example), with a
nonempty account/password:

```yaml
rdp:
  addr: ":3389"
  username: anykvm
  password: REPLACE_WITH_A_PRIVATE_PASSWORD
  codecs:
    h264: true
    progressive: true
    planar: true
    clearcodec: true
    hevc: false
  bitrate: 12000
  quant: 6
  refine: 2
  buffer_frames: 0
  auto_shortcut_mapping: true
  clipboard: true
  clipboard_options:
    text: 3
    richtext: 3
    images: 3
    files: 3
  drive:
    auto_mount: true
    permissions: 3
```

After [build preparation](build.md), run
`anykvm -config /path/to/config.yaml` and connect to port 3389. Listener and
account settings come from YAML. Credentials are AnyKVM credentials, independent
of OS login and VNC/Web accounts. Certificate selection follows the shared
[authentication and TLS settings](configuration.md#authentication-and-tls).

The server prefers NLA/CredSSP when HYBRID_EX/HYBRID is offered. CredSSP v5+
uses NTLMv2 with signing/sealing, MIC and TLS public-key binding. TLS-only clients
must provide Client Info credentials. Kerberos, smart cards, Remote Credential
Guard and an interactive Windows logon screen are unsupported. There is no
unencrypted RDP mode.

Successful login sends Save Session Info, including the authenticated identity
when Client Info is empty after NLA. This does not store client passwords or
create reconnect cookies. Native credential-dialog and saved-account behavior
remain client-side interoperability checks.

## Codec selection

The first permitted codec supported by a known client capability set wins:

1. **AVC420 H.264**: 8-bit YUV 4:2:0. Compatible Annex-B access units can pass
   through; other inputs require decoding/conversion and an H.264 encoder.
2. **RemoteFX Progressive**: lossy 64x64 tiles, available for eligible known
   capability sets without THINCLIENT. FIRST/UPGRADE refinement is enabled by
   default; see [Progressive testing](#progressive-local-testing).
3. **Planar**: lossy YCoCg420 with CLL=1, row differences and RDP 6.0 RLE.
   It prefers native NV12 input; its raw fallback remains subsampled YCoCg.
4. **ClearCodec**: lossless RGB, with residual/raw coding and pixel glyph/V-Bar
   caches. It shares Planar's baseline capability requirement, so select it
   explicitly when comparing these codecs.

All four are allowed by default. Disable the other entries under `rdp.codecs`
to force one. A client without an allowed codec is rejected; runtime errors do
not silently change the negotiated codec. Adaptive per-region codec mixing,
AVC444, regular RemoteFX CAVIDEO and traditional Bitmap Update are unsupported.

`rdp.bitrate` controls host H.264 encoding in kbps; 0 lets the encoder choose.
It does not change bitmap quality or externally encoded passthrough streams.
Native macOS H.264 screen and bitmap output need no FFmpeg. Compressed conversion
needs the matching decoder; see [media setup](build.md#native-and-optional-codecs).
Missing conversion support ends the session with a diagnostic.

### HEVC option and implementation status

`rdp.codecs.hevc` is reserved and defaults to `false`, independently of
`gamestream.codecs.hevc`. Enabling it with an active RDP listener fails with
`RDP HEVC output is not implemented; set rdp.codecs.hevc=false`.
Existing HEVC encoders or HEVC-to-H.264 conversion do not implement RDP HEVC
negotiation/framing. Compatible HEVC output remains future work.

## Video, display and flow control

AVC420 waits for an IDR with SPS/PPS at startup and refresh. Compatible profile,
level, chroma, bit depth and output geometry are checked before passthrough.
Prediction frames remain ordered. Capture backends with output scaling can
restart at client size and resume with a new IDR; other inputs are converted
and fitted with aspect-preserving black borders. Downscaling is allowed. Explicit
Target `video.mode` dimensions are an output ceiling, including after a client
resize or Target switch. Intersect that ceiling with the actual source size;
requests exceeding either edge use that bounded canvas. For example,
`1440x900@60` cannot produce `2560x1440` for a larger client. Omitting dimensions
(including `auto` and `@60`) adds no configured ceiling. In `auto`, a physical desktop mode change is attempted
first. Raw capture-card input can be reduced during encoding; device-compressed
H.264 passthrough keeps its encoded dimensions when within the Target ceiling;
larger compressed sources require conversion. H.264 output sizes are even and
alignment must also fit the configured ceiling.

On Retina displays, the source geometry records backing pixels: a desktop that
looks like 1440x900 can have a 2880x1800 rendering buffer at 2x. This is separate
from the panel's native resolution. Target output limits are encoded pixels;
an explicit 1440x900 limit is not multiplied by the Retina scale factor.

Bitmap codecs request pixels, compare a 64x64 grid and skip unchanged blocks.
ClearCodec/Planar coalesce fully dirty 128x128 groups; Progressive keeps 64x64
tiles. NV12 checks both Y and UV. Pixel snapshots can be replaced by newer ones;
compressed reference frames must be decoded in order. Refresh and resize reset
codec state and repaint static sources. Graphics transfers have an adaptive
ACK window and a five-second write/ACK timeout.
Output follows capture availability. Uncompressed updates may be replaced by
newer frames while waiting for the client; compressed frames remain ordered.
ACKs release individual frame IDs; suspending ACKs clears the window, and
resuming tracks only newly sent frames. The window starts at two frames and
adapts to source FPS, measured ACK latency and jitter, with a 32-frame ceiling.
Its extra flight allowance targets 50ms or one frame interval, whichever is
larger. Sustained queueing contracts the window to drain outstanding work, and
ACK throughput measurements account for a slower decoder. A 16MiB pending-data
threshold also stops new frames; a single frame may exceed that threshold.
RTT samples start before the final END_FRAME batch, excluding preceding server
encoding work. Stalled ACKs stop admission; no extra frame timer is used.
These bounds control server admission, not a guaranteed end-to-end latency:
transport buffers, individual frame size and client decoding also contribute.
All graphics codecs, including Progressive refinements, assign START_FRAME a
packed UTC time of day as required by MS-RDPEGFX. This is separate from source
playback PTS and diagnostic durations. Refresh requests wake idle pixel sources
using their retained latest picture.

Clients supporting scaled surface mapping receive source-sized surfaces within
Target limits, fitted to the requested desktop with matching pointer coordinates.
Other clients retain server-side sizing.
Display Control supports single-monitor updates; capture dimensions remain a
source preference. Local Mac desktop switching follows the
[automatic capture-mode policy](capture.md#macos-local-screens). RDP supplies
no refresh rate; capture follows the target's video setting or native cadence. Multiple monitors
and physical display rotation are unsupported.

`rdp.buffer_frames` limits native macOS H.264 captures submitted but incomplete:
0/omitted keeps two frames; explicit values are 1..8. The value survives RDP
resize/restart, and other capture backends currently ignore it. This is separate
from the graphics ACK window and [Web display buffering](viewing.md#buffering-and-diagnostics).
If the encoded-output queue overflows, playback resumes from a new keyframe.

## Input and cursor

Slow/fast-path input supports scan codes, Unicode (including surrogate pairs),
three mouse buttons and vertical wheel. Local macOS/Windows inject Unicode
text; other HID backends fall back to US-layout ASCII. Held keys/buttons are
released on disconnect or takeover. Horizontal scrolling and extra buttons are
unsupported.

On macOS, `rdp.auto_shortcut_mapping: true` maps left-Control C/X/V/A/Z/F/W/+/-
back to Command for clients reporting Mac/iOS. Other known client OS values
swap left Alt and left Win to Command/Option positions. Right modifiers and
unspecified client OS values retain their meanings. Set the option to `false`
and restart/reconnect to disable mapping. Other server OSes do not apply it.

Cursor shapes follow the capture source and are sent independently of video.
Use `video.cursor: client` to avoid a second cursor in captured pixels. Shapes
are scaled to output bounds and the negotiated 32x32 or 96x96 limit; empty shapes
hide the cursor and unavailable capture uses an arrow. Alpha, hotspot and display
scaling are preserved. 384x384 pointers are unsupported.

## Audio, clipboard and client drives

Enable local sound playback in the viewer to receive RDPSND 48 kHz stereo
S16LE PCM over the same TLS connection. Wave/Wave2, training, confirmations and
bounded queues are implemented. Audio rejection leaves video usable.
Audio source selection follows the [target configuration](configuration.md#local-desktop-example)
and [platform capture setup](capture.md). Microphone redirection is unsupported.

macOS supports clipboard text, HTML/RTF, images and file copy/paste through
`cliprdr`; other hosts need a supplied clipboard backend. Enable Clipboard in
the viewer and reconnect. `rdp.clipboard: false` disables all clipboard sharing.
The independent `clipboard_options` direction masks default to 3: 0=off,
1=client to server, 2=server to client, 3=both. Restart after policy changes.

Client-selected drives/folders use `rdpdr`, independently of clipboard policy.
`rdp.drive.permissions` uses 0=off, 1=read client files, 2=write client files,
3=both (default). A per-session loopback WebDAV endpoint exposes them.
`auto_mount: true` mounts/unmounts on macOS and Linux; Linux needs rclone/FUSE
and a writable mount root. Disconnect invalidates operations and closes the
endpoint. Remote permissions and viewer support still apply.

RemoteApp, printers, general USB redirection and session reconnect cookies are
unsupported.

### Clipboard and file transfer

For Mac-to-client file copies, copy in Finder and paste in the client's file
manager. Metadata is published first; file contents are read on demand as the
client requests byte ranges. Streaming file clipboard support is required.
Copies support nested/empty directories, empty files and Unicode names, up to
1,024 entries. Each file is limited to 2,147,483,647 bytes, with no combined
collection cap. Symbolic links, special files, ambiguous Windows names and names
of 260 or more UTF-16 units are rejected. Range requests are at most 4 MiB;
changed files, replacement offers and disconnect invalidate stale requests.

Client-to-Mac copies publish placeholders under
`$TMPDIR/anykvm-clipboard-<uid>/`. Finder's coordinated read triggers background
RDP fetching; the complete collection is ready before copying proceeds.
Repeated pastes reuse it. Readers bypassing file coordination are unsupported.
Failure/cancellation does not expose partial content. Replacement/disconnect
retires the offer; completed data is removed after readers finish and a
60-second quiet period. Normal exit flushes retired offers, and later offers
clean up abandoned crash directories.

### Redirected drive mounts

Each session's loopback WebDAV URL is logged. It requires no username/password
and rejects browser requests from other origins. macOS automatically mounts in
a private temporary session directory; Finder can show the client-named network
volume. Linux uses rclone/FUSE under
`$XDG_RUNTIME_DIR/anykvm/drives/<computer-name>` in graphical sessions, a private
temporary directory when no runtime directory exists, or `/mnt/<computer-name>`
when headless. Existing paths are never reused. The service account needs a
writable mount root; AnyKVM does not invoke sudo or prompt for mount permission.
Only the session's own mount/helper is stopped on disconnect.

Drives appear by drive letter or client-provided folder name. Files stream in
at most 256 KiB requests with 64-bit offsets and no whole-file staging; the
clipboard's 2 GiB limit does not apply. Directory listings are bounded to
8 MiB/20,000 entries and recursive removal to 128 levels. Reparse points,
cross-drive MOVE and self-copy aliases are rejected. WebDAV COPY requires both
read/write permission; directory metadata remains visible with either bit.
Linux sequential copies are supported, but random read/write editing may need
a local copy. Upload cancellation can leave partial remote files.

## Progressive local testing

To compare Progressive alone, configure and restart:

```yaml
rdp:
  codecs: {h264: false, progressive: true, planar: false, clearcodec: false}
  quant: 6
  refine: 2
```

`quant` accepts 6..15 (0/omitted uses 6). Larger values discard more detail and
usually reduce payload; they do not guarantee faster encoding or client FPS.
`refine` accepts 0..4 and defaults to 2. Zero sends complete SIMPLE tiles.
Values 1..4 enable the same three-stage profile: FIRST 50%, UPGRADE 75%, then
UPGRADE 100%, using Microsoft's [reference per-component/subband quantization
tables](https://github.com/microsoft/WindowsProtocolTestSuites/blob/main/ProtoSDK/MS-RDPEGFX/Codecs/RdpegfxTileUtils.cs).
Existing nonzero values remain accepted for configuration compatibility.
The percentages label quantization stages, not byte counts or resolution.
100% restores the configured base quantization; it does not override `quant`.

SIMPLE uses the original DWT; FIRST/UPGRADE uses reduce-extrapolate DWT. New
captures take priority while available gaps/ACK slots refine pending tiles.
Refinements leave one ACK-window slot and byte headroom for a fresh capture.
A one-frame congestion window is reserved entirely for fresh captures; idle
refinement wakes when the recovery hold expires. Background UPGRADE packets
are buffered before writing and advance tile quality only after a successful
write. A queued capture, refresh, or tighter ACK window discards unwritten
packets without advancing either CPU or GPU tile history. Already submitted
transport data remains ordered before the next FIRST.
Normal captures send FIRST only for tiles whose visible pixels changed, using
CPU comparison or GPU dirty-tile detection. After that check, unchanged tiles
can advance even while more captures are queued. One small packet may use the
remaining source-frame time (targeting 2ms of encoding); alongside changed
tiles it follows their FIRSTs within the same ACK frame. Changed tiles and move
destinations are excluded. Tile rotation prevents repeated FIRSTs from starving
an unchanged neighbor's 100% pass. Refinement pauses on congestion or exhausted
frame/byte budgets; identical captures retain completed quality without resends.
Replacement, refresh and resize invalidate stale refinements. Coefficient
caches are bounded to 128 MiB per connection; oversized grids use SIMPLE.
Temporal sub-band differences are not implemented.

Windows DXGI move metadata can produce `SurfaceToSurface` updates in CPU BGRA
and resident GPU Progressive modes. Copies require continuous acquisitions. The
server validates destination pixels against the last sent source (on the GPU for
resident capture). Pending source tiles are then refined from their cached old
coefficients, deduplicated across all verified copies, before the client picture
is snapshotted. Copies cover complete interior
64×64 destination tiles; partial edges or failed matches are encoded normally.
An unmapped scratch surface snapshots the old client picture before copies, so
self-overlap and mutually dependent moves cannot overwrite another move's source.
The scratch surface is recreated after layout changes.

Surface copies do not relocate Progressive decoder contexts. Copied destinations
cancel their old UPGRADE tasks and invalidate CPU coefficient entries; resident
GPU entries become unreachable until a new absolute FIRST overwrites them. No
UPGRADE is issued for a copied tile without that new FIRST. Source UPGRADEs
precede both the scratch snapshot and current-frame FIRST updates, so new pixels
cannot overwrite the coefficients needed for the old source. Both capture queue
replacement and ACK-window replacement discard move commands while accumulating
their source/destination damage; refresh, geometry changes and invalid metadata
also disable copies. Pixel and damage baselines commit only after the complete
frame writes successfully.

CGO builds use parallel SIMD encoding (AVX2/SSE2 on AMD64, NEON on ARM64),
with Go as the fallback. `ANYKVM_RDP_PROGRESSIVE_BACKEND` selects `auto`, `go`,
`native`, `native-scalar`, or AMD64-only `native-sse2`/`native-avx2`.

Experimental GPU encoding supports SIMPLE, FIRST and UPGRADE on Windows
(D3D11 feature level 11.0) and Linux (Vulkan 1.3). Set
`ANYKVM_RDP_PROGRESSIVE_BACKEND=gpu`, or explicitly `d3d11`/`vulkan`.
Forced GPU backends end the session on failure; `auto` uses CPU encoding.
macOS and builds without CGO use CPU backends.

Windows-host builds through `tools/build.py` or `tools/build_windows.ps1`
precompile all four Progressive encoding stages and the surface/dirty-detection
stage with FXC and embed DXBC. These shaders do not compile at runtime in those
binaries. Windows cross-builds from macOS/Linux retain runtime compilation.

Windows DXGI and Linux KMS sources can keep capture pixels on the GPU through
scaling, dirty detection and encoding. Other sources upload CPU pixels.
Performance and client compatibility still need validation on the target GPU.
After [build preparation](build.md#direct-go-builds), run:

```sh
ANYKVM_TEST_RDP_GPU=1 go test ./internal/rdp -run '^TestProgressiveGPUWireParity$' -count=1 -v
ANYKVM_TEST_RDP_GPU_SURFACE=1 go test ./internal/rdp -run '^TestProgressiveGPUSurfaceHardware$' -count=1 -v
```

The surface test requires desktop capture access and uses `screen`; override it
with `ANYKVM_TEST_RDP_GPU_SOURCE=screen:N`. Compare visible quality and fresh
frame rate on the client, not just encoder throughput or refinement counts.

## Shared pixel operations

`internal/pixelops` shares opaque BGRA comparisons, runs, BGR packing and the
64x64 dirty grid across RDP and VNC. AMD64 uses Go assembly with AVX2/SSE2 runtime
dispatch without CGO; ARM64 CGO uses NEON, otherwise Go. Alpha/padding are ignored
for opaque comparison; transparent cursors keep alpha-aware processing.

`internal/pixelconv` shares BGRA/NV12-to-YCoCg420 conversion and row differences
for Planar, with CGO SSE2/AVX2/NEON and Go fallback. Progressive's transform and
entropy kernels remain separate. Bounded-tail and protected-page tests cover
strided, unaligned and odd-sized buffers. VNC still uses 16px Hextile wire tiles.

## Connection troubleshooting

Logs distinguish X.224/TLS, authentication, activation and DRDYNVC/RDPGFX stages.
`authenticated RDPGFX session (codec=...)` confirms activation. Authentication
failures report identity/password presence without printing the password.

DVC negotiates versions 1, 2 or 3 in one implementation. Version 3 accepts bounded
RDP8 Lite compressed/mixed fragments; output uses ordinary DVC data and
uncompressed RDP8 graphics containers. DVC version does not select AVC or replace
graphics compression.

For H.264-only sessions, `no usable AVC420 capability advertised` refers to the
current advertisement. Inspect logged versions/flags and confirmed codec.
Known version 8.1 needs AVC420_ENABLED; ordinary version 10 sets must not set
AVC_DISABLED. Unknown versions are not selected.

Bitmap logs report output FPS, changed tiles and maximum
encoding/send/ACK waits. Progressive refinements have separate FPS counters.
Graphics logs also include pending frames/window, pending bytes, ACK RTT and
baseline, jitter, estimated flow FPS, and client-reported queued bytes.
ACK bursts are sampled as a group. Their reporting cadence (up to 250ms of
flight allowance, still bounded by 32 frames/16MiB) is separate from decoder
queue pressure. Queue snapshots are discounted for this reporting delay.
Consecutive single-frame reports retire old batch allowance, and an isolated
probe after desktop idle clears stale cadence and decoder-rate estimates.
Window changes use a 500ms stability period and one-frame dead band; sustained
pressure still drains immediately once detected. Late ACKs free their own slot
without replacing newer queue feedback. Resuming ACKs requires a frame sent
after the suspend boundary, so old in-flight ACKs cannot reopen that epoch.
The application-wide `AnyKVM: uplink=… Mbps` log reports total outgoing traffic
across all output protocols every five seconds, including RDP socket writes.
Idle desktops send few updates. Measure client display FPS separately from
source capture FPS and encoder throughput.

Maintainer investigation confirmed that the tested Android/iOS Windows App
builds hard-code AVC as disabled and advertise AVC_DISABLED. Use a mutually
supported bitmap codec for these clients; an H.264-only policy cannot connect.
This is a frustrating Microsoft client decision that needlessly strands mobile
users on bitmap codecs. Changing server bitrate, SPS or DVC negotiation cannot
enable a codec the client refuses to offer. The Mac Windows App AVC420 path has
passed functional testing.

Video can still be lost after sleep/wake during RDP use. This remains an open
[session recovery issue](../TODO.md#session-recovery).

## Validation

After build preparation:

```sh
go test -race ./internal/rdp ./internal/bridge ./internal/app
ANYKVM_TEST_CREDSSP_PYTHON=/path/to/venv/bin/python \
  go test -race ./internal/rdp -run '^TestCredSSPIndependentClient$' -count=1 -v
ANYKVM_TEST_RDP_MEDIA=1 ANYKVM_FFMPEG_LIBDIR=/path/to/ffmpeg9/lib \
  go test -tags ffmpeg -race ./internal/rdp
```

The independent CredSSP test requires pyspnego in the specified environment.
Tests cover authentication, framing/bounds, input release, clipboard/drives,
codec/resize/ACK state and configuration. Independent decoder checks exercise
synthetic graphics and control. Maintainer testing
confirmed real Windows App display, physical input, Unicode, cursor, audio,
clipboard/file transfer and drive access using the codecs each client advertises.
Sleep/wake video recovery remains
unresolved as described above.

## References

- [MS-RDPBCGR](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-rdpbcgr/)
- [MS-CSSP](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-cssp/)
- [MS-RDPEDYC](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-rdpedyc/)
- [MS-RDPEGFX](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-rdpegfx/)
- [MS-RDPEGDI](https://learn.microsoft.com/en-us/openspecs/windows_protocols/ms-rdpegdi/)
- [FreeRDP](https://github.com/FreeRDP/FreeRDP)
