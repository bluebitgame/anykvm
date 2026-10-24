# AnyKVM nRF52840 BLE HID Firmware (Zephyr / nRF Connect SDK)

Provides a BLE keyboard and absolute/relative mouse on an nRF52840. It
receives AnyKVM commands over USB CDC serial and forwards Bluetooth HID reports
to iOS/iPadOS or any other host that supports BLE HID.

Four profiles store separate BLE identities and bonds using Zephyr settings.
Switching profiles disconnects the current host and reconnects to the selected
one. USB CDC uses the [shared AnyKVM serial protocol](../README.md).

## Hardware

Supported build targets include Seeed XIAO nRF52840, Adafruit Feather nRF52840
Express and the nRF52840 Dongle (PCA10059).

## Build and flash

The [shared builder](../README.md#build) prepares nRF Connect SDK, west and the
ARM toolchain under `build/`. From the repository root:

```sh
python3 tools/build_firmware.py --targets nrf52840-xiao
python3 tools/build_firmware.py --targets nrf52840-feather nrf52840-dongle
```

These select Zephyr boards `xiao_ble`, `adafruit_feather_nrf52840` and
`nrf52840dongle_nrf52840`, respectively. Images are published under
`build/firmware/releases/<target>/`. Flash the HEX through a debugger or the
board's supported bootloader. Where a UF2 is produced, double-press RESET on
XIAO/Feather and copy it to the bootloader drive. Building does not flash hardware.

Release packages group all three boards in `firmware-nrf52840.zip`. Select the
`nrf52840-xiao/`, `nrf52840-feather/` or `nrf52840-dongle/` directory for your
board. This source directory is shared by all three targets.

## Connection topology

```text
macOS/Linux/Windows (AnyKVM) --USB CDC serial--> nRF52840 --BLE HID--> iPhone/iPad
                            (binary protocol)           (mouse + keyboard)
```

## iOS setup

- Keyboard: supported natively; ready to use after pairing.
- Mouse: Settings → Accessibility → Touch → AssistiveTouch (enable it to show the pointer).
- Pairing: select **AnyKVM** in iOS Bluetooth settings while the target profile is advertising.

## LED status (Seeed XIAO nRF52840: red/green/blue)

- **Blue:** solid when BLE is connected; blinks while advertising (200 ms when
  bonded, 500 ms when unbonded); off when disconnected and not advertising.
- **Green:** solid when AnyKVM has opened the serial port (USB DTR) and BLE is
  connected. Blue and green stay on during normal operation.
- **Red:** blinks rapidly if firmware initialization fails.

## Mac local-viewer mouse input

With macOS 11+ and an external GCMouse device detected at startup, relative mode
uses raw GCMouse deltas and ignores SDL motion. A built-in trackpad uses SDL
`XRel/YRel`, which is affected by the Mac tracking-speed setting. iOS
AssistiveTouch applies its own pointer acceleration in either case.

## Serial protocol

Uses the shared [AnyKVM v1 protocol](../README.md) and the same host
driver as Pico and ESP32-S3. Update firmware and the AnyKVM application together.
The common C library handles framing, resynchronization, keyboard state, buttons,
wheel movement, absolute coordinates, relative splitting, capabilities, and ACKs.
The Zephyr adapter supplies BLE reports and persistent profile management.

Use `ble:DEVICE:0` through `ble:DEVICE:3` for the four independent profiles.
`anykvm -pair ble:DEVICE:0` clears that profile's bond and starts pairing.
The target advertises as **AnyKVM**. Bond data survives power loss.

Both absolute and relative mouse reports are supported. Changing mouse mode on
this board rebuilds the GATT service and disconnects the target temporarily.
For iOS/iPadOS, start with `mouse_mode: relative` and enable AssistiveTouch;
absolute positioning requires validation on the target system.
