# AnyKVM native USB HID — Pico 1 / Pico 2

Native USB exposes a composite CDC control interface, keyboard and mouse. Control
comes from GP0/1 UART, or from CDC on the **same computer** receiving HID input.
Pico 1 and Pico 2 share the source in [`../pico-common/`](../pico-common/README.md).

Build with `python3 tools/build_firmware.py --targets pico pico2` and flash
`build/firmware/releases/pico/anykvm-gpio-hid.uf2` or
`build/firmware/releases/pico2/anykvm-gpio-hid.uf2` respectively.

See the [shared guide](../pico-common/README.md) for all twelve connection
combinations, six images, wiring, mode selection, transport ownership and
validation scope. The USB CDC interface/PID and mouse report IDs changed from
the original UART-only firmware. Absolute/relative mode changes no longer
re-enumerate USB, so self-control retains its CDC connection.

## Validation

Both chip targets build; physical acceptance of these updated images remains
pending. See the [shared validation notes](../pico-common/README.md#validation).
