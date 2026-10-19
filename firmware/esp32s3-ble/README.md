# XIAO ESP32-S3 BLE keyboard and mouse

Connect the XIAO ESP32-S3 to the AnyKVM computer by USB. The firmware receives
the [AnyKVM v1 serial protocol](../README.md) over USB CDC and sends
standard BLE HID reports to the controlled computer, tablet or phone.

Supports a six-key keyboard with modifiers, three mouse buttons, wheel,
relative motion, absolute coordinates, reconnect, and persistent bonding.
There are four independent profiles (slots 0..3), with one selected at a time.
Each profile has its own persistent static random BLE identity, bond records,
HID subscriptions and mouse mode. Clearing a profile removes only that profile's
bond. The selected profile resets to slot 0 after reboot, matching nRF52840.
The board advertises as **AnyKVM**. Both mouse report maps remain registered,
so changing mouse mode does not rebuild the BLE service.

## Profile switching

AnyKVM v1 profile commands:

- `0x20`: release held input, disconnect the old host, select the profile,
  restore its keys and mouse mode, and advertise for that host to reconnect.
- `0x21`: clear only the specified profile. Clearing an inactive profile leaves
  the active BLE connection intact; clearing the active one disconnects it and
  opens that profile for a new pairing. The local identity and mouse mode stay.
- `0x22`: report the selected slot, connection, bond and mouse mode.
- `0x23`: advertise the selected profile. A bonded profile accepts only its
  saved peer; an empty profile permits pairing.

Select `ble:DEVICE:0` through `ble:DEVICE:3` for separate targets sharing the
same USB adapter. Pair each target with its own slot, for example:

```sh
anykvm -pair ble:cu.usbmodemXXXX:0
anykvm -pair ble:cu.usbmodemXXXX:1
anykvm -pair ble:cu.usbmodemXXXX:2
anykvm -pair ble:cu.usbmodemXXXX:3
```

During each pairing action, select **AnyKVM** on the corresponding target.
Switching slots does not clear any bond. All four slots survive power loss.
The same physical host can use separate slots with separate keys and identities.

The Arduino core's default three-entry bond table is not increased. Instead,
the firmware provides per-profile NVS storage callbacks and loads only the
selected profile into the controller's resolving list. Stable local addresses
keep the profiles independent; the stack's shared local IRK is not distributed.
Store format and record layout are versioned against the pinned core below.

**Upgrading from the single-profile firmware requires pairing again.** The new
profiles use new identities and a separate `bkvm-profiles` NVS namespace. The
old default bond namespace is retained but is not used by the four-slot firmware.
Forget the old AnyKVM accessory on each target, then pair its chosen slot.
An unreadable profile bank is rejected rather than silently replaced.

## Build

Use Espressif Arduino core **3.3.2** and its bundled BLE/NimBLE library. Do not
install another keyboard or NimBLE library. The shared protocol library is
provided in this repository's `firmware` directory.

The [shared builder](../README.md#build) installs the pinned Arduino CLI and
core under `build/`, using a separate local configuration. From the repository root:

```sh
python3 tools/build_firmware.py --targets esp32s3-ble
```

Firmware images are in `build/firmware/releases/esp32s3-ble/`.

Arduino IDE users should add `firmware/AnyKVMProtocol` as a local library,
open `anykvm_ble/anykvm_ble.ino`, select **XIAO_ESP32S3**, set USB Mode to
**Hardware CDC and JTAG**, and enable **USB CDC On Boot**. In the CLI board
options for this exact core version, `CDCOnBoot=default` means enabled.

## Flash and pair

Connect the board using a USB data cable. Select its serial port in the IDE,
or upload the compiled image with Arduino CLI:

```sh
build/firmware/tools/arduino-cli-1.3.1/arduino-cli \
  --config-file build/firmware/arduino-cli.yaml upload -p /dev/cu.usbmodemXXXX \
  --fqbn esp32:esp32:XIAO_ESP32S3:USBMode=hwcdc,CDCOnBoot=default,PartitionScheme=default_8MB \
  --input-dir build/firmware/releases/esp32s3-ble firmware/esp32s3-ble/anykvm_ble
```

If needed, hold BOOT while connecting USB to enter download mode. Reset after
upload and find the runtime CDC port again. Close any serial monitor before
starting AnyKVM.

Use the updated AnyKVM application's pairing action:

```sh
anykvm -pair ble:cu.usbmodemXXXX:0
```

On the target, forget an earlier AnyKVM pairing if present, then select
**AnyKVM** in Bluetooth settings. The pairing action clears the adapter's old
bond for the chosen slot before advertising. Pair again only when replacing the target or resetting
bond state; normal startup retains the bond.

Set the target's `hid.device` to
`ble:cu.usbmodemXXXX:0` (macOS), `ble:ttyACM0:0` (Linux), or `ble:COM5:0`
(Windows). Set `hid.mouse_mode` to `relative` or `absolute`. Device discovery
reports the model as **ESP32-S3**, with four profiles.

For iPhone/iPad, start with relative mode and enable AssistiveTouch to show
the mouse pointer. Absolute report acceptance and coordinate mapping depend
on the target OS and require hardware testing. Firmware compilation and
protocol tests do not confirm Bluetooth pairing or pointer behavior on a target.

## Verification

Run the native protocol tests described in [the shared library guide](../README.md).
The profile manager can also run without Arduino or attached hardware:

```sh
c++ -std=c++11 -Wall -Wextra -Werror -fsanitize=address,undefined \
  -Ifirmware/esp32s3-ble/tests/support -Ifirmware/AnyKVMProtocol/src \
  firmware/esp32s3-ble/tests/profiles_test.cpp firmware/esp32s3-ble/anykvm_ble/profiles.cpp \
  -o /tmp/anykvm-profiles-test
/tmp/anykvm-profiles-test
```

These tests substitute NVS, the controller and the host event queue. They check
four host bonds, distinct identities, per-slot mouse modes, targeted reconnect
filters, single-slot clearing, power-cycle restoration, separate keys for the
same peer, storage/controller failures, expired requests and corrupt-bank rejection.

After flashing, test pairing, held modifiers, individual key release, dragging,
scrolling, all four absolute corners, relative motion, USB unplug/replug, target
disconnect/reconnect, repeated switching among four targets, clearing an inactive
slot, and bond persistence after power loss. The native tests and compilation
do not validate real radio connections or the target OS's pairing behavior.

## References

- [Arduino-ESP32](https://github.com/espressif/arduino-esp32)
- [Seeed Studio XIAO ESP32-S3](https://wiki.seeedstudio.com/xiao_esp32s3_getting_started/)
