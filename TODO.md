# AnyKVM TODO

Implemented features are summarized in [README.md](README.md). This list tracks
remaining engineering work and known issues. Hardware/client acceptance status
remains in the linked guides; implemented features are not listed here solely
because physical validation is pending.

## Session recovery

- [ ] Fix video loss after sleep/wake during RDP use, covering upstream-source
  and downstream-server sessions.

## Capture sources: ARD client

Current scope and validation: [ARD client](docs/ard-client.md).

- [ ] Implement High Performance H.264 negotiation and reception.
- [ ] Add compound/tiled HEVC composition and private displays.
- [ ] Add Apple cached alpha cursors, remote cursor position and source audio.

## Capture sources: RDP client

Current AVC420, NLA/TLS, input and PCM audio support:
[network sources](docs/network-sources.md).

- [ ] Implement the licensing challenge exchange beyond STATUS_VALID_CLIENT.
- [ ] Add missing graphics/cache commands and decoders for legacy bitmap,
  ClearCodec/Planar/Progressive and AVC444/AVC444v2 updates.
- [ ] Add upstream cursor handling, clipboard, drives and multiple displays.

## Server protocols: RDP

Current implementation and repeatable checks: [RDP server](docs/rdp-server.md).

- [ ] Establish compatible HEVC capability negotiation, codec ID and payload
  framing, then implement RDP HEVC output.
- [ ] Add Progressive temporal sub-band differences and AVC444/AVC444v2.
  Traditional Bitmap RLE, CAVIDEO and NSCodec remain lower priority.
- [ ] Add multiple displays and session reconnection cookies.
- [ ] Benchmark ARM64 NEON pixel, Planar and Progressive kernels. Functional
  hardware testing is complete; performance measurements remain outstanding.

## GameStream

Current implementation, completed client checks and known relaunch issue:
[GameStream](docs/gamestream.md#validation).

- [ ] Fix immediate relaunch after quitting when the client has not yet reflected
  the stopped session state; launching again during this window currently fails.
- [ ] Recover from encoded IDR frames exceeding four FEC blocks without ending
  the session.
- [ ] Add congestion-driven bitrate adaptation.
- [ ] Add automatic discovery.
- [ ] Add local paired-client listing and revocation controls.

## Android (scrcpy)

- [x] Add device system audio forwarding through the shared scrcpy session,
  preserving video and input when device audio is unavailable.
