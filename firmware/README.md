# AnyKVM firmware and serial HID protocol v1

## Firmware directories

All maintained microcontroller application sources live here:

- [`pico-common/`](pico-common/README.md): shared Pico 1/2 application, UART/CDC
  transports, pin plan and wiring for twelve connection combinations.
- [`pico-hid/`](pico-hid/README.md): native USB CDC + HID; UART control or
  same-computer CDC self-control (`pico`, `pico2`).
- [`pico-pio-usb/`](pico-pio-usb/README.md): UART/CDC control and GPIO PIO USB HID
  output (`pico-pio-usb`, `pico2-pio-usb`).
- [`pico-ps2/`](pico-ps2/README.md): UART/CDC control and GPIO PS/2 keyboard/mouse
  output (`pico-ps2`, `pico2-ps2`), relative mouse only.
- [`rp2350-usb-a/`](rp2350-usb-a/README.md): Waveshare RP2350-USB-A, with USB-C CDC
  control and PIO USB-A keyboard/mouse output.
- [`nrf52840-ble/`](nrf52840-ble/README.md): USB CDC control and BLE HID output,
  using Zephyr / nRF Connect SDK.
- [`esp32s3-ble/`](esp32s3-ble/README.md): USB CDC control and BLE HID output,
  using the ESP32 Arduino core.
- `AnyKVMProtocol/`: shared C/C++ protocol and input state library.
- `tests/`: native shared-library tests.

Pico is a board name: Pico 1 uses RP2040; Pico 2 uses RP2350. SDK board names
remain `pico` and `pico2`. The RP2350-USB-A firmware has a different USB topology
and a separate build from `pico-hid`.

Maintainer hardware acceptance is complete for RP2350-USB-A. The six Pico 1/2 builds
pass; physical acceptance of their twelve connection paths remains pending.
See the board-specific guides for the recorded validation scope.

Build products, toolchains and local test reports belong in the Git-ignored
`build/` tree.

## Build

From the repository root, with Python 3.9+ (including venv/pip), Git and a host
C/C++ compiler installed:

```sh
python3 tools/build_firmware.py --list-targets
python3 tools/build_firmware.py --targets pico pico2 pico-pio-usb pico2-pio-usb pico-ps2 pico2-ps2
python3 tools/build_firmware.py --targets rp2350-usb-a
python3 tools/build_firmware.py --targets nrf52840-ble esp32s3-ble
python3 tools/build_firmware.py --targets all
```

The eleven targets are `pico`, `pico2`, `pico-pio-usb`, `pico2-pio-usb`,
`pico-ps2`, `pico2-ps2`, `rp2350-usb-a`, `nrf52840-ble` (XIAO),
`nrf52840-feather`, `nrf52840-dongle` and `esp32s3-ble` (XIAO). Omitting `--targets`
builds all eleven. The script compiles firmware without flashing a board.

SDKs are prepared only for selected targets. Versions are pinned in
[`tools/build_firmware.py`](../tools/build_firmware.py): Pico SDK 2.3.1, ARM GNU
13.3.Rel1, nRF Connect SDK 2.5.3, Arduino CLI 1.3.1 and ESP32 Arduino Core 3.3.2.
Downloads are checksum-verified; Git dependencies use fixed revisions.
SDK setup does not install packages into the user's Arduino or Nordic workspace.

- `build/firmware/sdk/`: Pico, PIO USB, Nordic and Arduino SDKs.
- `build/firmware/toolchains/` and `python/`: ARM compiler and local Python tools.
- `build/firmware/cache/`: package caches and temporary files. Verified archives
  share `build/toolchains/downloads/` with the desktop builder.
- `build/firmware/work/<target>/`: intermediate build files.
- `build/firmware/releases/<target>/`: firmware images and SHA-256 metadata.

All of these paths are ignored by Git. The host compiler is used to build Pico
SDK utilities: Apple Command Line Tools on macOS, GCC/G++ on Linux, or MinGW-w64
on Windows. The script prepares CMake, Ninja, west and the ARM cross compiler.

Use `--dry-run` to preview commands without downloading or writing files,
`--prepare-only` to prepare dependencies, and `--offline` to reuse a prepared
environment. `--fresh` reconfigures the selected builds; `--jobs N` limits build
parallelism. Failed targets return a nonzero exit status while other targets
continue, and their previously published images remain available.

## Release packages

GitHub Actions builds all eleven targets when firmware sources or build/package
scripts differ from the previous published ancestor Release. The first Release
includes firmware; unchanged firmware is omitted from later Releases.

Each `firmware-<target>.zip` is a separate attachment on the desktop Release,
with images, SHA-256 metadata, source revision and board guides. Use the package
for your exact board and follow its flashing instructions. Release publication
waits for all required desktop and firmware builds to pass.

## Shared protocol

All maintained Pico UART, Pico USB-A, nRF52840 and ESP32-S3 adapters use this
protocol. Flash updated firmware when updating from earlier firmware. The host
requires a version/capability response and rejects unversioned devices.

The shared C library is in `AnyKVMProtocol/src/`. `AnyKVMProtocol.h` implements
framing; `AnyKVMDevice.h` implements commands and HID input state. Platform
adapters supply report transmission, status, mouse mode and optional BLE profiles.
The Go host uses one serial driver for USB, PS/2 and BLE adapters. `hid:DEVICE` selects
an adapter; `ble:DEVICE[:SLOT]` additionally requires BLE output. Existing
`nrf52840:` selectors remain accepted as an alias for BLE selection.

## Transport

UART/CDC uses 115200 baud, 8N1 and raw binary bytes. Both requests and responses
are eight bytes: `33 command data[4] crc_hi crc_lo`. CRC16/MODBUS covers the first
six bytes (initial 0xffff, reflected polynomial 0xa001). CRC byte order is big
endian. Invalid CRCs and partial frames are discarded with sliding-window
resynchronization; they never execute commands or generate uncorrelated ACKs.

The same host driver is used on macOS, Windows and Linux. Typical device values
are `hid:cu.usbmodemXXXX`, `hid:COM5`, and `hid:ttyACM0`, respectively. Linux also
accepts an absolute `/dev/serial/by-id/...` path. The process must have permission
to open the selected serial device.

Windows opens COM ports exclusively, including ports numbered 10 and above. Its
AnyKVM control reader polls available bytes with a short idle backoff so a
synchronous idle read cannot hold up keyboard/mouse writes. Linux sets both the
baud flags and raw mode, opens without waiting for carrier detect, and bounds
idle control reads to 100 ms. Discovery retains its separate timeout policy.

## Commands

Unused payload bytes must be zero. All valid frames receive an `0x84` result.
Successful input commands receive only the result; queries and management
commands additionally return capabilities, ready, and status in that order.

- `0x01`, `0x05`, `0x22`: read-only query; all have identical semantics.
- `0x04`: mouse mode, `data[0]=0x08` absolute or `0x10` relative. Release held
  input before changing mode. Apply the mode before sending the result.
- `0x10`: release all keyboard keys, modifiers and mouse buttons. Preserve the
  current absolute position and reset the relative position anchor.
- `0x11`: key event, `data[0]=USB HID keyboard usage`, `data[1]=1` down or `0`
  up. Modifiers are usages 0xe0..0xe7. Repeated downs are idempotent; at most
  six ordinary keys are held. Additional downs are ignored when full.
- `0x12`: absolute position, unsigned X and Y as two big-endian uint16 values,
  each in 0..32767. Relative mode converts successive positions to deltas; the
  first position after reset/release anchors without movement.
- `0x13`: buttons. `data[0]` uses bits 7/6/5 to select left/right/middle and
  bits 3/2/1 for each selected button's down state. Unselected buttons retain
  their state. `data[1..3]` are reserved.
- `0x14`: vertical wheel, signed int8 in `data[1]`, clamped to -127..127.
- `0x15`: relative motion, signed int8 DX/DY in `data[0..1]`. Split -128 into
  multiple reports. Absolute mode accumulates and clamps to 0..32767.
- `0x20`: select profile `data[0]`. Release held input before switching.
- `0x21`: clear profile bond `data[0]`. Release held input first. Return success
  only when the old bond is removed; a new pairing must wait for success.
- `0x23`: advertise the current profile. Unsupported on USB-only adapters.
- `0x24`: reserved target-port selection for future combined-output firmware:
  `data[0]` is a logical physical-port index (0, 1, ...), other bytes zero.
  Port indices are independent of interface type: two ports may both be PS/2
  or both PIO USB, with the mapping defined by the future board/firmware.
  Current single-output builds return unsupported without changing held input.
  This selects a physical output over serial, not extra GPIO switch signals.

BLE connection loss resets held input and the coordinate anchor. Mode changes
and profile changes may temporarily disconnect the target; USB/UART control
remains available. Actual absolute pointer support depends on the target OS.

## Responses

- `0x80`: ready; `data[0]` is the profile count (USB=0, ESP32-S3=4, nRF52840=4).
- `0x81`: status in `data[0]`: slot in bits 0..1, connected bit 2, bonded bit 3,
  advertising bit 4, absolute mode bit 5. Other bytes are zero.
- `0x82`: asynchronous connection event, `data[0]=1` connected or `2`
  disconnected; `data[1]` is the profile slot.
- `0x83`: capabilities: `data[0]=1` protocol version, `data[1]` capability bits,
  `data[2]` profile count, `data[3]` model (1 Pico, 2 nRF52840, 3 ESP32-S3).
  Capability bits: 0 keyboard, 1 relative mouse, 2 absolute mouse, 3 BLE output,
  4 profile management, 5 USB output, 6 PS/2 output. UART and CDC are transports,
  not output types. PS/2 advertises keyboard/relative mouse only.
- `0x84`: command result; `data[0]` is the request command and `data[1]` is the
  result: 0 success, 1 unknown command, 2 invalid argument, 3 unsupported,
  4 busy, 5 platform error. Other bytes are zero.

The host serializes management requests and waits up to three seconds for their
results. It does not retry timed-out state changes automatically. Input requests
stream without waiting for results; the background reader drains their replies.
Discovery sends only queries and does not change the target's mode, bond or
profile. Firmware may release held input when the control connection closes.
Automatic serial discovery has a one-second budget per port and protocol,
including open, configuration, settling, and I/O. AnyKVM discovery waits up to
250 ms per status query with at most three attempts and 50 ms between attempts.
CH9329 identification waits up to 300 ms for its reply after the port settles.
Timed-out discovery closes its port; normal connected-device status and command
timeouts are independent of these discovery limits.

## Native tests

Run from the repository root (no attached hardware required):

```sh
cc -std=c11 -Wall -Wextra -Werror firmware/tests/protocol_test.c -o /tmp/anykvm-protocol-test
/tmp/anykvm-protocol-test
c++ -std=c++11 -Wall -Wextra -Werror -Ifirmware/AnyKVMProtocol/src \
  firmware/esp32s3-ble/tests/protocol_test.cpp -o /tmp/anykvm-input-test
/tmp/anykvm-input-test
go test ./internal/hid
```

Run the Go tests on Windows or Linux to exercise that host's platform-specific
regressions. The Windows tests check exclusive COM opening and native device
names. The Linux tests use a PTY to check raw mode, replacement of a stale 1200
baud setting, and bounded idle control reads. Cross-compiling a test executable
checks its build but does not execute these platform-specific tests.
