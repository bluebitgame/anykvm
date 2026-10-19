# Android

This guide covers Android capture, audio and control. The current integration
uses scrcpy over ADB.

## scrcpy over ADB

### Requirements and server discovery

Use `video.source: adb` or `adb:<serial>`, with ADB and a matching scrcpy server
supplied separately. `$ADB` overrides discovery; otherwise bundled
`platform-tools/adb` is preferred over PATH. Place
`scrcpy-server` or a versioned server beside AnyKVM. Relative `scrcpy.server` and
`SCRCPY_SERVER_PATH` values resolve from the executable directory.

scrcpy-server 3.0 or newer is required; older servers are rejected before launch.
Both the scrcpy 3.x video protocol and the 4.x/5.x session-packet protocol
are supported. AnyKVM selects the protocol from the server version, including
dimension changes on rotation and the scroll range introduced in 3.3.1.
The version must exactly match the server JAR; versioned filenames are detected
automatically, while an unversioned `scrcpy-server` may need `scrcpy.version`.

### Configuration

```yaml
targets:
  - name: Android
    video:
      source: adb
      mode: "@30"
    audio:
      source: auto
    scrcpy:
      server: scrcpy-server-v5.0.1
      version: "5.0.1"
      keep_active: true
      crop: ""
      gamestream_orientation: "90"
```

### Screen mirroring and virtual desktop

Empty mode mirrors the physical screen without an FPS cap; `@30` mirrors it at
up to 30 FPS. Only an explicit resolution, such as `1280x720` or `1280x720@30`,
creates an independent virtual desktop at that width and height (scrcpy-server
3.0 or newer). `auto` / `auto@30` and client-negotiated sizes keep physical-screen
mirroring. Virtual desktop dimensions remain fixed across client switches and
reopens; they do not follow client window sizes. Android may align dimensions
to encoder constraints. The virtual display is released when capture closes.

For example, change the target above to `video.mode: "1920x1080@30"` to enable
an independent desktop. System decorations and the device's secondary-display
launcher remain enabled. Devices without such a launcher may show an empty
display until an app is started on it; launcher availability depends on Android
and the device firmware.

Frames follow screen changes, not a guaranteed continuous cadence.
`crop: width:height:x:y` selects a rectangle on the captured display before
scaling/rotation. It applies to all viewers; the Android server validates bounds
and maps input back through the transform.

### GameStream orientation

`gamestream_orientation` applies only to GameStream: 0/90/180/270 rotates,
`@90` locks rotation, and scrcpy's supported flip/initial-lock forms are accepted.
It requires a server with capture-orientation support. Web/VNC retain normal
orientation. Restart/relaunch after changes. Rotation/cropping occur before
Android encoding, preserving H.264/H.265 passthrough.

### Keeping the device awake

`keep_active: true` requires scrcpy-server 4.0 or newer. It periodically reports
user activity to prevent sleep without changing global device settings, including
when running on battery. When omitted/false, AnyKVM keeps the existing
`stay_awake` behavior while charging.

### Video codecs and bitrate

`scrcpy.bitrate` is bits/sec. In GameStream it caps the negotiated encoder budget
when positive; zero adds no configured ceiling. Web/VNC use the configured value
or scrcpy's default. H.264 and H.265 passthrough need no FFmpeg. HEVC-capable Web
and GameStream clients can negotiate H.265; the Android device must provide
an H.265 encoder. Other clients continue using H.264. JPEG/raw conversion uses
H.264 and the optional FFmpeg backend, including on macOS. On servers 3.0 and
newer, keyframe recovery resets only video, preserving the audio connection.

### Android audio

`audio.source: auto` (the default) forwards Android system audio through the
same scrcpy session to the active client's audio sink. Audio uses an independent
socket and 48kHz stereo S16LE PCM, without FFmpeg. Set `audio.source: ''` to disable
it, or choose another audio source to use a host input instead. Device audio
uses scrcpy's default `output` source, routing playback away from the device.
It requires Android 11 or newer; Android 11 may require the screen unlocked when
starting capture. Device/app restrictions or audio errors leave video and input
usable. Audio queues are bounded and discard old blocks when no client consumes
them. Audio forwarding works with both physical-screen mirroring and the
independent desktop.

## Related guides

See [configuration and discovery](configuration.md) for target selection,
[Web/VNC viewing](viewing.md), [RDP](rdp-server.md) and
[GameStream/Moonlight](gamestream.md) for client setup.
