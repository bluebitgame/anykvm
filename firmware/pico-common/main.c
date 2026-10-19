/* Shared UART/CDC control path for the three Pico output backends. */
#include "pico/stdlib.h"
#include "hardware/uart.h"
#include "hardware/clocks.h"
#include "tusb.h"
#include "output.h"
#include "../AnyKVMProtocol/src/AnyKVMDevice.h"
#include "control.h"
#include "pins.h"

static bk_device control;
static bk_parser parsers[2];
static bk_owner owner;
static int replying;
static bool report_failed, release_pending;
void anykvm_rp2350_reboot_sleep_fix(void);

static void reply(void *ctx, uint8_t command, const uint8_t *data) {
    (void)ctx;
    uint8_t frame[8], result[4];
    memcpy(result, data, 4);
    if (command == 0x84 && result[1] == BK_OK && report_failed) result[1] = BK_BUSY;
    bk_frame(frame, command, result);
    if (replying == 0) uart_write_blocking(uart0, frame, sizeof(frame));
    else {
        absolute_time_t deadline = make_timeout_time_ms(100);
        while (tud_mounted() && tud_cdc_write_available() < sizeof(frame) && !time_reached(deadline)) {
            tud_task(); tud_cdc_write_flush();
        }
        if (tud_mounted() && tud_cdc_write_available() >= sizeof(frame)) {
            tud_cdc_write(frame, sizeof(frame)); tud_cdc_write_flush();
        }
    }
}
static uint8_t status(void *ctx) { (void)ctx; return output_ready() ? 0x04 : 0; }
static uint8_t mode(void *ctx, bool absolute) { (void)ctx; return output_mode(absolute); }
static void keyboard(void *ctx, const uint8_t *report) {
    (void)ctx; if (!output_keyboard(report)) report_failed = true;
}
static void mouse(void *ctx, bool absolute, const uint8_t *report, size_t size) {
    (void)ctx; if (!output_mouse(absolute, report, size)) report_failed = true;
}
static void release_inputs(void) {
    report_failed = false;
    bk_release(&control.input);
    release_pending = report_failed;
}
static void poll_input(int source) {
    // Bound batches so a busy CDC writer cannot starve UART or output tasks.
    for (int i = 0; i < 32; ++i) {
        int byte = -1;
        if (source == 0) {
            if (uart_is_readable(uart0)) byte = uart_getc(uart0);
        } else if (tud_cdc_available()) byte = tud_cdc_read_char();
        if (byte < 0) break;
        uint8_t cmd, data[4];
        if (!bk_parse(&parsers[source], (uint8_t)byte, &cmd, data)) continue;
        replying = source;
        report_failed = false;
        bool takeover = false;
        if (!bk_owner_accept(&owner, source, cmd, time_us_64(), &takeover)) {
            const uint8_t busy[4] = {cmd, BK_BUSY, 0, 0};
            reply(NULL, 0x84, busy);
            continue;
        }
        if (takeover || release_pending) {
            release_inputs();
            if (release_pending) {
                const uint8_t busy[4] = {cmd, BK_BUSY, 0, 0};
                reply(NULL, 0x84, busy);
                continue;
            }
        }
        bk_command(&control, cmd, data);
    }
}
int main(void) {
    anykvm_rp2350_reboot_sleep_fix();
#if ANYKVM_OUTPUT_PIO
    // PIO USB programs are timed for a multiple of 12 MHz on both chips.
    set_sys_clock_khz(120000, true);
#endif
    uart_init(uart0, 115200);
    gpio_set_function(ANYKVM_UART_TX, GPIO_FUNC_UART);
    gpio_set_function(ANYKVM_UART_RX, GPIO_FUNC_UART);
    uart_set_fifo_enabled(uart0, true);
    tusb_init();
    output_init();
    control.capabilities = output_capabilities();
    control.model = BK_MODEL_PICO;
    control.reply = reply; control.status = status; control.mode = mode;
    control.input.absolute = output_default_absolute();
    control.input.keyboard = keyboard; control.input.mouse = mouse;
    owner.source = -1;
    bool cdc_open = false, target_ready = false;
    while (true) {
        tud_task(); output_task();
        bool connected = tud_cdc_connected();
        if (cdc_open && !connected) {
            memset(&parsers[1], 0, sizeof(parsers[1]));
            if (owner.source == 1) { release_inputs(); owner.source = -1; }
        }
        cdc_open = connected;
        bool ready = output_ready();
        if (ready != target_ready) bk_reset(&control.input);
        target_ready = ready;
        if (release_pending && ready) release_inputs();
        poll_input(0); poll_input(1);
    }
}
