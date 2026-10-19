# RP2350-USB-A AnyKVM HID firmware

The maintained C firmware for the Waveshare RP2350-USB-A board uses the
AnyKVM v1 serial HID protocol. USB-C carries CDC serial to AnyKVM;
USB-A carries PIO USB keyboard and mouse reports to the controlled computer
(D+ GP12, D- GP13). Keep the board's device-mode pull-up installed.

## Source layout

Application sources:

- `main.c`: USB descriptors, keyboard/mouse reports, and the control loop.
- `usb_reset.c` and `usb_reset.h`: deferred software BOOTSEL and reboot requests.
- `rp2350_reboot.c`: RP2350 ROM reboot sleep-state workaround.
- `pio_control.c`: PIO USB control transfer length correction.
- `boot_diagnostic.c` and `boot_diagnostic.h`: optional boot diagnostics, disabled
  in release builds.
- `CMakeLists.txt`: the only application build definition.
- `verify_reconnect.py`: hardware reconnect verification.

The binary protocol and shared input state live in
[`firmware/AnyKVMProtocol/src/`](../AnyKVMProtocol/src/).
The builder restores the Waveshare PIO USB library
from its official source archive and verifies its SHA-256 checksum.

`pio_control.c` bounds the reference PIO stack's control-IN replies to the
host's SETUP `wLength`, including macOS's initial 8-byte device descriptor
and 2-byte string descriptor probes. The USB-A configuration does not advertise
remote wakeup, which the reference device stack does not implement.

## Build

From the repository root, use the [shared builder](../README.md#build):

```sh
python3 tools/build_firmware.py --targets rp2350-usb-a
```

It prepares Pico SDK, the ARM compiler, PIO USB and the matching picotool under
`build/`. Use `--fresh` when reconfiguring after SDK or source-path changes.

Hold BOOTSEL while plugging in USB-C, then copy
`build/firmware/releases/rp2350-usb-a/anykvm.uf2` to the board's BOOTSEL drive.
For a write-and-verify workflow with a USB-enabled picotool, use BOOTSEL mode:

```sh
picotool load -v build/firmware/releases/rp2350-usb-a/anykvm.uf2
picotool reboot -a
```

Native USB-C uses the SDK CDC descriptors. Normal AnyKVM communication uses
115200. Reset requests complete their USB control ACK before the main loop
stops core 1 and PIO/DMA, detaches native USB for 300 ms, and enters the ROM.
Application startup also holds USBCTRL in reset for 500 ms before starting CDC,
so the host sees the BOOTSEL-to-CDC device change. A ROM-reboot sleep-state
workaround from the SDK is applied before the first application sleep.
While native USB is responsive, enter BOOTSEL without pressing the button:

```sh
picotool reboot -u -f
```

Wait for the BOOTSEL device to enumerate before issuing `load` or `reboot`.
Setting CDC to 1200 baud also requests BOOTSEL through the same main-loop path.
If USB does not enumerate or the firmware hangs, use the physical BOOTSEL button.
The PIO USB-A core starts once native USB is configured, with a
10-second fallback when no control host is attached. It does not wait for a
serial application to open CDC. AnyKVM status reports USB-A enumeration.

Reconnect checks cover ROM access, CDC re-enumeration and complete CRC-valid
AnyKVM discovery responses. With both ports attached, they also check that USB-A
returns as a keyboard and mouse.

To repeat the hardware check on macOS or Linux (the board will reboot):

```sh
python3 firmware/rp2350-usb-a/verify_reconnect.py \
  --picotool /path/to/picotool --serial /dev/cu.usbmodemXXXX \
  --cycles 20 --output build/firmware/reconnect-results.json
```

Use `--reset-method baud1200` to test the CDC baud-rate reset path. The optional
`ANYKVM_USB_BOOT_DIAGNOSTICS=ON` build records startup stages and uses an early
watchdog to recover to BOOTSEL after a startup hang; it is off in release builds.
On macOS, add `--require-usb-a` when USB-A is also connected to this Mac to check
both HID interfaces after every reboot.

## AnyKVM

Set the target's `hid.device` to `hid:cu.usbmodemXXXX` on macOS,
`hid:ttyACM0` on Linux, or `hid:COM5` on Windows. Set `hid.mouse_mode` to
`absolute` or `relative`. No additional protocol selector is needed.

## Protocol

All maintained boards use the [AnyKVM v1 protocol](../README.md).
Update the host application and firmware together. Requests and replies have the
same eight-byte envelope and CRC; keys are native USB HID usages and absolute
coordinates are unsigned 0..32767. Device discovery queries version and capabilities.
The USB-C control channel stays online while changing the PIO USB-A mouse mode.
Mouse mode is saved in watchdog scratch state; the keyboard stays enabled.

Both mouse descriptors request a 1 ms USB polling interval. Local testing on an
RP2350 A2 board with a Mac/USB hub measured about 996 mouse reports per second.
This measures firmware throughput; visual cursor latency requires a separate test.
The same setup passed software BOOTSEL/CDC reset checks, mouse-mode switching,
protocol checks, pointer positioning, relative motion and modifier release.

Maintainer hardware acceptance for RP2350-USB-A is complete, including flashing,
USB-C/USB-A reconnect, keyboard/mouse input, mouse modes and host integration.
This is separate from the untested
[Pico 1/2 GPIO UART path](../pico-hid/README.md#validation).

After firmware changes, repeat the target-side checks: typing with held modifiers,
positioning at all four corners, scrolling, clearing held buttons, and switching
mouse modes.

## References

- [Waveshare RP2350-USB-A](https://www.waveshare.com/wiki/RP2350-USB-A)
