/* Both PS/2 ports run concurrently on core 1; native USB remains on core 0. */
#include "pico/stdlib.h"
#include "pico/multicore.h"
#include "pico/util/queue.h"
#include "hardware/sync.h"
#include "tusb.h"
#include "output.h"
#include "pins.h"
#include "ps2_wire.h"
#include "../AnyKVMProtocol/src/AnyKVMProtocol.h"

typedef struct { uint8_t mouse, report[8]; } input_report;
static queue_t inputs;
static ps2_device keyboard, mouse;
static ps2_wire wires[2];
static volatile bool host_seen;
static const unsigned clocks[] = {ANYKVM_PS2_KBD_CLOCK, ANYKVM_PS2_MOUSE_CLOCK};
static const unsigned data_pins[] = {ANYKVM_PS2_KBD_DATA, ANYKVM_PS2_MOUSE_DATA};
static void drive(unsigned pin, bool low) {
    // Output latch always zero. Never actively drive a PS/2 line high.
    gpio_set_dir(pin, low ? GPIO_OUT : GPIO_IN);
}
static void core1(void) {
    // Timing is software-driven on a dedicated core with no interrupt handlers.
    save_and_disable_interrupts();
    ps2_init(&keyboard, false); ps2_init(&mouse, true);
    wires[0].device = &keyboard; wires[1].device = &mouse;
    input_report pending;
    bool have_pending = false;
    for (;;) {
        uint64_t now = time_us_64();
        for (unsigned i = 0; i < 2; ++i) {
            unsigned clock = clocks[i], data = data_pins[i];
            ps2_wire_tick(&wires[i], now, gpio_get(clock), gpio_get(data));
            drive(data, wires[i].data_low); drive(clock, wires[i].clock_low);
        }
        host_seen = wires[0].host_seen || wires[1].host_seen;
        // Only convert reports while both ports are idle. Large key diffs must
        // not stretch an in-flight clock pulse.
        if (wires[0].state == 0 && wires[1].state == 0) {
            if (!have_pending) have_pending = queue_try_remove(&inputs, &pending);
            if (have_pending) {
                if (pending.mouse) { ps2_mouse(&mouse, pending.report); have_pending = false; }
                else if (ps2_keyboard(&keyboard, pending.report, now)) have_pending = false;
            }
            ps2_task(&keyboard, now); ps2_task(&mouse, now);
        }
    }
}
void output_init(void) {
    queue_init(&inputs, sizeof(input_report), 64);
    for (unsigned i = 0; i < 2; ++i) {
        unsigned pins[] = {clocks[i], data_pins[i]};
        for (unsigned j = 0; j < 2; ++j) {
            unsigned pin = pins[j];
            gpio_init(pin); gpio_put(pin, 0); gpio_disable_pulls(pin); drive(pin, false);
        }
    }
    multicore_launch_core1(core1);
}
void output_task(void) {}
bool output_ready(void) { return host_seen; }
uint8_t output_capabilities(void) { return BK_KEYBOARD | BK_RELATIVE | BK_PS2; }
bool output_default_absolute(void) { return false; }
uint8_t output_mode(bool absolute) { return absolute ? BK_UNSUPPORTED : BK_OK; }
static bool enqueue(bool mouse_report, const uint8_t *bytes, size_t size) {
    input_report report = {.mouse = mouse_report};
    memcpy(report.report, bytes, size);
    absolute_time_t deadline = make_timeout_time_ms(100);
    do {
        if (queue_try_add(&inputs, &report)) return true;
        tud_task();
    } while (!time_reached(deadline));
    return false;
}
bool output_keyboard(const uint8_t report[8]) { return enqueue(false, report, 8); }
bool output_mouse(bool absolute, const uint8_t *report, size_t size) {
    if (absolute || size != 4) return false;
    return enqueue(true, report, size);
}
