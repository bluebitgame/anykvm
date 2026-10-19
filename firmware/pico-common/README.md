# AnyKVM Pico 1 / Pico 2: three output families

Pico 1 (RP2040) and Pico 2 (RP2350) share the application, UART/CDC transport,
protocol, USB descriptors and PS/2 implementation. Each output family is built
separately for each chip. There are **six UF2 images covering twelve connection
combinations**. Board images are not interchangeable.

This firmware controls one target at a time. Keyboard and mouse always use the
same output family. A future combined-output firmware can use a serial command to select which
connected target receives input; no extra GPIO select pins are needed.

## Build and flash

From the repository root:

```sh
python3 tools/build_firmware.py --targets \
  pico pico2 pico-pio-usb pico2-pio-usb pico-ps2 pico2-ps2
```

Add `--offline` after dependencies have been prepared. Products are:

- Pico 1 native USB: `build/firmware/releases/pico/anykvm-gpio-hid.uf2`.
- Pico 2 native USB: `build/firmware/releases/pico2/anykvm-gpio-hid.uf2`.
- Pico 1 PIO USB: `build/firmware/releases/pico-pio-usb/anykvm-pio-usb.uf2`.
- Pico 2 PIO USB: `build/firmware/releases/pico2-pio-usb/anykvm-pio-usb.uf2`.
- Pico 1 PS/2: `build/firmware/releases/pico-ps2/anykvm-ps2.uf2`.
- Pico 2 PS/2: `build/firmware/releases/pico2-ps2/anykvm-ps2.uf2`.

Hold BOOTSEL while connecting the board to USB and copy the matching UF2 to the
BOOTSEL drive. CDC and HID descriptors/PIDs have changed from the original
UART-only native-HID firmware; let the OS enumerate the updated device and
reselect its serial port. The CDC serial number uses the board's unique ID.

## Twelve supported connection combinations

"Server" means the computer running AnyKVM. "Target" means the computer
receiving keyboard/mouse input. UART is always 3.3 V, 115200 baud, 8N1.
Each entry below is a firmware-supported topology, not a hardware acceptance
record; see Validation below.

1. **Pico 1, UART -> native USB HID**: flash `pico`; server UART connects to
   GP0/1, board USB connects to target. Select the server's UART serial port.
2. **Pico 1, native CDC -> native HID, self-control**: flash `pico`; one USB
   cable connects the board to the same computer running AnyKVM and receiving
   HID input. Select its new CDC serial port; UART wiring is unnecessary.
3. **Pico 1, UART -> PIO USB HID**: flash `pico-pio-usb`; server UART connects
   to GP0/1; target USB connects to the external GP12/13 USB circuit.
4. **Pico 1, native CDC -> PIO USB HID**: flash `pico-pio-usb`; board USB connects
   to server; target USB connects to the external GP12/13 USB circuit.
5. **Pico 1, UART -> PS/2**: flash `pico-ps2`; server UART connects to GP0/1;
   target's keyboard and mouse PS/2 ports connect through level shifters to
   GP2/3 and GP4/5. Select relative mouse mode.
6. **Pico 1, native CDC -> PS/2**: flash `pico-ps2`; board USB connects to server;
   target PS/2 connects through the same GP2..5 circuit. Select relative mode.
7. **Pico 2, UART -> native USB HID**: flash `pico2`; same wiring as combination 1.
8. **Pico 2, native CDC -> native HID, self-control**: flash `pico2`; same wiring
   and same-computer requirement as combination 2.
9. **Pico 2, UART -> PIO USB HID**: flash `pico2-pio-usb`; same wiring as combination 3.
10. **Pico 2, native CDC -> PIO USB HID**: flash `pico2-pio-usb`; same wiring as combination 4.
11. **Pico 2, UART -> PS/2**: flash `pico2-ps2`; same wiring and relative mode as combination 5.
12. **Pico 2, native CDC -> PS/2**: flash `pico2-ps2`; same wiring and relative mode as combination 6.

Native CDC and native HID share one physical USB connection and one USB host.
They cannot use the same native port to bridge two separate computers. PIO USB
is a second, independent USB device connection. The native-output firmware
always exposes CDC plus keyboard and mouse; unused CDC needs no application.

## Shared pin plan and future serial target selection

The authoritative GPIO allocation is [`pins.h`](pins.h), used by all three
builds. The standard Pico 1/2 header uses the same physical pin positions:

- UART TX: GP0, physical pin 1. UART RX: GP1, physical pin 2.
- PS/2 keyboard Clock: GP2, pin 4; Data: GP3, pin 5.
- PS/2 mouse Clock: GP4, pin 6; Data: GP5, pin 7.
- PIO USB D+: GP12, pin 16; D-: GP13, pin 17.
- GND: for example physical pins 3, 8 or 18. All connected signal circuits need
  a common ground.
- GP6..11 (physical pins 9, 10, 11, 12, 14, 15) and GP14/15 (pins 19/20)
  remain unassigned. There are no GPIO target-selection signals.

UART, PS/2 and PIO USB have no pin overlap, so future combined-output firmware
can keep several target computers physically attached to independent ports.
The currently free GP6..9 could accommodate a second PS/2 keyboard/mouse group;
GP10/11 could be another PIO USB pair alongside GP12/13. Those are possible data
pin allocations, not GPIO selection signals, and are not enabled by these builds.
Two PS/2 groups require eight data/clock pins. Two PIO USB device instances need
additional stack/resource/timing validation: spare GPIO alone does not establish
that the existing PIO USB library supports two independent device controllers.
Native USB belongs to one host; when it carries server CDC plus HID, that HID
output controls the server itself. A different target can stay connected to PIO
USB or PS/2. The single native controller still cannot attach to two USB hosts.

**Serial command reservation:** `0x24` (`BK_CMD_SELECT_TARGET`), with
`data[0]` the logical target-port index (0, 1, 2, ...), and `data[1..3]=0`.
Port indices identify physical keyboard+mouse output groups, **not interface
types**. A future board/firmware configuration maps each index to its pins and
backend; for example ports 0/1 can both be PS/2, both PIO USB, or different types. It uses the normal v1 eight-byte frame/CRC and `0x84` command
result. Selection must release keys/buttons on the old target, route subsequent
input only to the selected output and reset position state. Existing physical
connections need not be switched. No additional select wires are involved.

These six builds each contain one output backend. They explicitly return
`BK_UNSUPPORTED` to `0x24` without changing held keys or output; the command
number and index semantics are reserved for later combined-output firmware. This
does not claim runtime multi-target selection is implemented. Future USB and
PS/2 backends may remain connected/enumerated while only the selected backend
receives input; this avoids relying on PS/2 hotplug behavior.

### Future USB-server capacity bounds

On standard Pico 1/2 boards, 26 GPIOs are exposed (GP0..22 and GP26..28). A full
PS/2 target consumes four, giving a **GPIO-only upper bound of six targets**.
This is not a claim that the current single-target PS/2 timing engine supports
six ports; all ports still need protocol handling even when only one gets input.

PIO USB D+/D- pairs must be adjacent with the current library. Those exposed
GPIO ranges permit twelve such pairs if UART is disabled and all GPIOs are
available. This **twelve-pair wiring bound is not a USB device capacity**: it
excludes pull-up/disconnect control and VBUS sensing, and the pinned device
stack supports one controller instance. Inactive USB targets that stay enumerated
still issue control/interrupt transactions and need timely responses. Pico 1 has
eight PIO state machines and Pico 2 has twelve; instruction memory, IRQ handling
and CPU timing also constrain any multi-device stack.

A different future design could disconnect all unselected USB ports and rebind
one engine to the selected GPIO pair, accepting re-enumeration on every switch.
That still needs proper per-port pull-up/power handling and cannot be achieved
by changing the logical target index alone. Current builds initialize both UART
and CDC; reusing GP0/1 for another target requires a future USB-only control build.

## Server UART wiring

Connect server/USB-UART TX -> GP1 (Pico RX), server RX <- GP0 (Pico TX), and
GND -> GND. Use a **3.3 V TTL UART**, not an RS-232 port or 5 V TTL adapter.
The UART's power pin is not needed when the Pico is powered through USB.

For UART -> PIO USB or UART -> PS/2, the board still needs power. The native USB
port can connect to a USB supply or server USB for power. CDC remains available
but need not be opened. External target USB VBUS/PS/2 +5 V is not connected to
Pico VBUS/VSYS in the wiring below.

## External PIO USB device wiring

![Pico 1/2 single-port PIO USB wiring with GPIO and physical header pin numbers](images/pico-pio-usb-wiring.svg)

[Open the SVG wiring diagram](images/pico-pio-usb-wiring.svg). The drawing uses
an external USB-B device breakout with a standard USB-A to USB-B cable. Pico
header pin numbers are 16 (GP12), 17 (GP13), 18 (GND) and 36 (3V3 OUT).
The main symbol groups pins logically; the inset shows their actual header
positions viewed from the component side with native USB at the top. The 1.5
kohm pull-up joins D+ on the connector side of its series resistor.

This is the current fixed-pull-up, powered-target bench circuit. It does not
include target VBUS sensing or power-off isolation; keep the target USB port
powered. Target VBUS is not connected to Pico VBUS/VSYS/3V3. Native USB supplies
Pico power from the server.

Both chips use the same circuit:

- GP12 -> approximately 27 ohm series resistor -> target USB D+.
- GP13 -> approximately 27 ohm series resistor -> target USB D-.
- USB signal GND -> Pico GND.
- A 1.5 kohm pull-up identifies a full-speed USB device on D+; connect it to a
  regulated 3.3 V source, **never to 5 V**. Device-mode boards such as
  RP2350-USB-A already provide this pull-up.
- Keep the USB pair short and routed together. Do not add USB-host 15 kohm
  pulldowns or provide 5 V to the target's USB port.

Use a USB device connector/breakout with a normal cable to the target. For a
USB-C receptacle, the breakout must also provide device-side CC resistors.
Keep target USB VBUS separate from the Pico's supply. For power-off/hotplug-safe
hardware, gate the D+ pull-up from target VBUS (with regulation to 3.3 V), and
ensure the circuit does not feed an unpowered target through its data pins.
GP14/15 are currently free for a future detection/control circuit; these builds
do not monitor VBUS. A bare always-on D+ pull-up is a powered-bench
setup, not a completed power-management design.

PIO USB runs on core 1 at a 120 MHz system clock; native CDC/UART runs on core 0.
It starts after native USB configuration, or after ten seconds if no native USB
host is attached. A temporary not-ready status during startup is expected.
Keyboard is boot-protocol capable. Mouse uses report protocol (absolute and
relative report IDs), so pre-OS mouse support depends on the target firmware.

## External PS/2 device wiring

Provide two separate PS/2 connections, one for keyboard and one for mouse.
Mini-DIN-6 **pin numbers** (verify numbering against the actual connector's
mating/solder-side drawing): pin 1 Data, pin 3 GND, pin 4 +5 V, pin 5 Clock;
pins 2 and 6 are unused. Do not infer pin orientation from cable colors.

- Keyboard port pin 5 Clock -> bidirectional level shifter -> GP2.
- Keyboard port pin 1 Data -> bidirectional level shifter -> GP3.
- Mouse port pin 5 Clock -> bidirectional level shifter -> GP4.
- Mouse port pin 1 Data -> bidirectional level shifter -> GP5.
- Both port grounds -> Pico GND.
- The shifters' low sides use Pico 3V3 (physical pin 36) with pull-ups to 3.3 V;
  high sides use the corresponding target port's +5 V with pull-ups to that
  supply. An open-drain bidirectional MOSFET level shifter is appropriate.
  Typical 4.7 kohm pull-ups must be checked with the actual cable/capacitance.
- Do not connect PS/2 +5 V directly to a GPIO, Pico 3V3, VBUS or the server's
  5 V. Treat both Pico versions as 3.3 V logic in this circuit.

Firmware only pulls Clock/Data low or releases them; it never drives them high.
Both ports run concurrently on core 1 at a nominal 12.5 kHz clock. Host inhibit,
request-to-send, odd parity, electrical ACK, reset, resend and identification
are handled independently on each port. Connect PS/2 before target power-on;
PS/2 hot plugging is not assumed.

Keyboard supports scan set 2, standard alphanumeric/navigation/keypad keys,
F1..F12, modifiers, Print Screen and Pause, LED commands and typematic. Other
scan sets are rejected. Unsupported HID usages (for example consumer/media
keys) have no PS/2 mapping. Mouse supports three buttons, relative movement,
stream/remote/wrap modes, sample rate/resolution/scaling commands and IntelliMouse
ID 3 wheel negotiation (200/100/80 samples/s). Without wheel negotiation it
uses standard three-byte packets. Five-button Explorer extensions are not
advertised. PS/2 cannot provide USB-style absolute pointing.

The connected status means a valid command has been received on at least one
PS/2 port since boot. PS/2 provides no reliable unplug indication here, so this
bit is not continuous cable detection. USB outputs report USB readiness.

## AnyKVM configuration and input ownership

Select the serial port **on the server**, with the existing `hid:` selector.
For native CDC it is typically `/dev/ttyACM0`, `/dev/cu.usbmodem...` or `COM5`;
for a USB-UART adapter it is typically `/dev/ttyUSB0` or `/dev/cu.usbserial-...`.
Example target HID configuration:

```yaml
hid:
  device: hid:/dev/ttyACM0
  mouse_mode: relative
```

Use the actual path; depending on platform the application also accepts the
short serial name (for example `hid:ttyACM0`). USB HID families accept
`absolute` or `relative`; PS/2 requires `relative` and rejects absolute-mode
selection with `BK_UNSUPPORTED`. Both USB mouse report layouts remain enumerated,
so mode changes keep CDC online. Update the AnyKVM host as well as the firmware
for PS/2 discovery (new capability bit 6); protocol framing/version stays v1.

CDC and UART have separate parsers and replies return only to the requesting
transport. Read-only queries can use either transport without acquiring control.
The first state-changing command owns input. A competing writer receives
`BK_BUSY` until two seconds without a state-changing command; takeover releases
held input before accepting the new writer. Inactivity alone does not release
held keys. CDC disconnect/DTR closure releases input if CDC owns it. A target
USB unplug resets the common tracked state. Use one active controller normally.
Bounded output queues/timeouts return `BK_BUSY` when the target cannot keep up;
as with other adapters, streamed input ACKs are drained by the host and are not
automatically retried. A success ACK means queued/sent, not consumed by the OS.

## Validation

The six Pico 1/2 targets have build verification, plus host-side tests for PS/2
commands, key sequences, movement/wheel packets, transport ownership, and a
simulated open-drain bus exercising frame parity, command ACK and host inhibit.
The Go serial driver tests cover PS/2 discovery and relative-only mode selection.
Builds do not establish physical compatibility: all twelve new/updated Pico
connection paths still require board-level acceptance. The separately maintained
RP2350-USB-A board's previous hardware acceptance does not cover these images.

Useful checks from the repository root:

```sh
python3 -m unittest discover -s tools -p test_build_firmware.py
cc -std=c11 -Wall -Wextra -Werror firmware/tests/protocol_test.c -o build/protocol-test
build/protocol-test
cc -std=c11 -Wall -Wextra -Werror firmware/tests/pico_test.c \
  firmware/pico-common/ps2_protocol.c firmware/pico-common/ps2_wire.c -o build/pico-test
build/pico-test
go test ./internal/hid
```

Before hardware acceptance, test both chips for enumeration/reconnect, CDC and
UART ownership/replies, key release, absolute/relative USB pointing, PS/2 BIOS
reset/LED/typematic behavior, mouse wheel negotiation, host inhibit, cable lengths
and supply sequencing. No connected device is flashed by the build script.

## Source layout

- `main.c`, `control.h`: protocol adapter and UART/CDC ownership.
- `usb.c`, `reports.h`, `tusb_config.h`: native CDC and USB HID descriptors.
- `pio_output.c`: external GPIO USB output, sharing HID report descriptors.
- `ps2_output.c`: core/queue/GPIO adapter for the two PS/2 ports.
- `ps2_protocol.c`, `ps2_wire.c`: platform-independent PS/2 behavior and timing.
- `pins.h`, `build.cmake`: common pin plan and build definitions.
- `../AnyKVMProtocol/src/`: protocol framing, commands and common input state.

PIO USB uses the existing checksum-pinned Waveshare library and the shared EP0
length correction from `../rp2350-usb-a/pio_control.c`. Its independent historical
board firmware remains a separate target.
