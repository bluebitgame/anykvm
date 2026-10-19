# Configuration and settings

AnyKVM reads capture targets, input backends and network services from YAML.
Use the [quick start](../README.md#quick-start) to build and run it.

## Configuration files and device discovery

Running `anykvm` without `-config` creates `config.yaml` beside the executable
if absent, with Web on `127.0.0.1:8080` and other services disabled. The working
directory does not change this default configuration location.

To generate or select a configuration explicitly:

```sh
anykvm -gen-config /path/to/config.yaml
anykvm -list-devices
anykvm -list-devices -list-ios
anykvm -config /path/to/config.yaml
```

`-gen-config` writes a reference from the same defaults as first startup, without
starting services. An explicitly selected configuration must already exist.
`-list-ios` includes macOS USB iPhone/iPad devices. Edit YAML and restart, or use
the Web settings editor described below.

## Local desktop example

```yaml
web:
  http: "127.0.0.1:8080"
  https: ""
vnc:
  addr: ""
rdp:
  addr: ""
gamestream:
  addr: ""
hotkey_mod: ctrl+alt
targets:
  - name: Desktop
    video:
      source: screen:0
      mode: "@30"
      cursor: client
    audio:
      source: auto
    hid:
      device: auto
      mouse_mode: absolute
```

Listener options live under `web`, `vnc`, `rdp` and `gamestream`; an empty address
disables that listener. `targets` defines controlled endpoints. Use
`video.source` for capture and `hid.device` for input. One controlling viewer
session owns the active target at a time.

`audio.source: auto` uses source audio when available, or platform-default audio
for local screens. An explicit audio device overrides it; an empty string
disables audio. `hid.device: auto` uses source input or local screen input where
supported; an empty string disables input. VNC/RDP/ARD and Android capture use
their own input session whenever input is enabled, even with an external HID
selector. External HID backends are used for sources without their own input,
such as capture cards and AirPlay. The USB/CoreDevice hybrid has its own setup.

[Capture setup](capture.md) defines `video.mode`, cursor behavior, audio and
platform permissions.

For other sources, see [VNC/RTSP/RDP](network-sources.md), [ARD](ard-client.md),
[Android](android.md), [iPhone USB/CoreDevice](iphone-coredevice.md) and
[AirPlay](iphone-airplay.md).
Viewer-specific options are covered in [Web/VNC/local viewing](viewing.md),
[RDP](rdp-server.md) and [GameStream](gamestream.md).

## Authentication and TLS

Set `web.username`/`web.password` for viewer authentication, `vnc.password` for
VNC, and a separate `rdp.username`/`rdp.password` for RDP. Top-level
`tls_cert`/`tls_key` are shared by HTTPS/VNC TLS/RDP; local certificates are
generated when absent. GameStream uses certificate pairing.

Store configurations containing secrets privately. The full schema is defined
in [config.go](../internal/app/config.go).

## Web settings editor

Set `admin_password` to enable `/settings/` with username `admin`, independently
of viewer credentials. The editor reports whether changes apply immediately,
on a new connection or after restart.

For background startup with a fixed configuration, see [services](service.md).

Pico adapters have three output families, built separately for Pico 1 and Pico 2.
See the [Pico wiring guide](../firmware/pico-common/README.md) for twelve UART/CDC
connection combinations. Use `hid:<server-serial-port>` for USB HID or PS/2;
PS/2 requires `mouse_mode: relative`. Native CDC + native HID self-control must
use the same computer for both ends of the USB connection.
