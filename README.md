# AnyKVM

AnyKVM is a Go application for capturing and remotely controlling computers and
devices on macOS, Windows and Linux, configured through one YAML file.

- **Capture:** desktop screens, cameras/capture cards, VNC/RTSP/RDP/ARD,
  Android (scrcpy), and iPhone/iPad (USB, CoreDevice and AirPlay).
- **View and control:** browser, VNC, RDP, GameStream/Moonlight, or an optional
  SDL2 local viewer, with multiple targets and viewer takeover.
- **Input and audio:** local or serial/BLE/USB HID backends, source audio and
  supported system-audio capture.

Available backends depend on OS, drivers and hardware. The guides below describe
experimental protocol/device support and its limits.

## Quick start

Install the [build prerequisites](docs/build.md#requirements), then build for the
current host:

```sh
python3 tools/build.py --targets native
```

The executable is `build/releases/<os>-<arch>/anykvm` (`anykvm.exe` on Windows).
Use that executable for the commands below, or add its directory to PATH.

Generate a configuration and list available devices:

```sh
anykvm -gen-config config.yaml
anykvm -list-devices
```

Edit `config.yaml` to select capture, input and audio using the
[configuration guide](docs/configuration.md), then run:

```sh
anykvm -config config.yaml
```

Open [the Web viewer](http://127.0.0.1:8080/). Other listeners are disabled by
default. Platform capture/input permissions are covered in the setup guides.

## Documentation

- [Configuration, authentication and Web settings](docs/configuration.md).
- [Local capture, audio and input](docs/capture.md).
- [Android capture, audio and control](docs/android.md).
- Remote sources: [VNC/RTSP/RDP and Wake-on-LAN](docs/network-sources.md),
  [Apple Screen Sharing / ARD](docs/ard-client.md).
- iPhone/iPad: [USB and CoreDevice](docs/iphone-coredevice.md),
  [AirPlay](docs/iphone-airplay.md).
- Viewer access: [Web, VNC and local viewer](docs/viewing.md),
  [RDP server](docs/rdp-server.md), [GameStream/Moonlight](docs/gamestream.md).
- [Automatic startup and services](docs/service.md).
- [Builds, media libraries, development and tests](docs/build.md),
  [native capture buffers](docs/capture-native-buffers.md).
- [HID firmware, builds and shared protocol](firmware/README.md).

Remaining work and known issues are tracked in [TODO](TODO.md).
