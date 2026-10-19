# AnyKVM PS/2 keyboard and mouse — Pico 1 / Pico 2

Native USB CDC or GP0/1 UART receives AnyKVM commands. GP2/3 emulate a PS/2
keyboard (Clock/Data); GP4/5 emulate a PS/2 mouse. Use external bidirectional
3.3 V/5 V open-drain level shifters. The mouse is **relative only**.

Build with `python3 tools/build_firmware.py --targets pico-ps2 pico2-ps2`.
Flash the corresponding `build/firmware/releases/<target>/anykvm-ps2.uf2`.

See the [shared wiring and usage guide](../pico-common/README.md), including
[PS/2 circuitry and limitations](../pico-common/README.md#external-ps2-device-wiring)
and [validation scope](../pico-common/README.md#validation). Both images build;
physical PS/2 acceptance remains pending. Protocol, wire timing and key/mouse
translation have platform-independent host tests.
