/* AnyKVM v1 serial HID firmware: USB CDC to PIO USB HID, RP2350. */
#include <stdio.h>
#include <string.h>
#include "../AnyKVMProtocol/src/AnyKVMDevice.h"
#include "pico/stdlib.h"
#include "pico/stdio_usb.h"
#include "pico/multicore.h"
#include "hardware/clocks.h"
#include "hardware/dma.h"
#include "hardware/pio.h"
#include "hardware/resets.h"
#include "hardware/structs/watchdog.h"
#include "pio_usb.h"
#include "device/usbd.h"
#include "class/hid/hid_device.h"
#include "usb_reset.h"
#include "boot_diagnostic.h"
#include "pico/bootrom.h"

void anykvm_rp2350_reboot_sleep_fix(void);

#if !defined(TUD_OPT_RHPORT) || TUD_OPT_RHPORT != 0
#error "USB-C CDC requires TinyUSB device mode on native root port 0"
#endif

static usb_device_t *usb_device = NULL;

#define MAGIC_REQ 0x33
#define PACKET_SIZE 8
#define OUT1_KBD_USB 0x01
#define OUT1_MOUSE_USB_ABS 0x08
#define OUT1_MOUSE_USB_REL 0x10
#define MOUSE_POLL_INTERVAL_MS 1

// ── Device descriptor ───────────────────────────────────────────────────────

tusb_desc_device_t const desc_device = {.bLength = sizeof(tusb_desc_device_t),
    .bDescriptorType = TUSB_DESC_DEVICE, .bcdUSB = 0x0110,
    .bDeviceClass = 0x00, .bDeviceSubClass = 0x00, .bDeviceProtocol = 0x00,
    .bMaxPacketSize0 = 64,
    .idVendor = 0xCafe, .idProduct = 0x4001, .bcdDevice = 0x0100,
    .iManufacturer = 0x01, .iProduct = 0x02, .iSerialNumber = 0x03,
    .bNumConfigurations = 0x01};

enum { ITF_NUM_KEYBOARD, ITF_NUM_MOUSE, ITF_NUM_TOTAL };
enum { EPNUM_KEYBOARD = 0x81, EPNUM_MOUSE = 0x82 };

uint8_t const desc_hid_keyboard_report[] = { TUD_HID_REPORT_DESC_KEYBOARD() };

// ── Absolute mouse ──────────────────────────────────────────────────────────

typedef struct __attribute__((packed)) {
    uint8_t  buttons;
    uint16_t x;
    uint16_t y;
    int8_t   wheel;
} abs_mouse_report_t;

uint8_t const desc_hid_abs_mouse_report[] = {
    0x05, 0x01, 0x09, 0x02, 0xA1, 0x01, 0x09, 0x01, 0xA1, 0x00,
    0x05, 0x09, 0x19, 0x01, 0x29, 0x08, 0x15, 0x00, 0x25, 0x01,
    0x95, 0x08, 0x75, 0x01, 0x81, 0x02,
    0x05, 0x01, 0x09, 0x30,
    0x15, 0x00, 0x26, 0xFF, 0x7F, 0x75, 0x10, 0x95, 0x01, 0x81, 0x02,
    0x09, 0x31,
    0x15, 0x00, 0x26, 0xFF, 0x7F, 0x75, 0x10, 0x95, 0x01, 0x81, 0x02,
    0x09, 0x38,
    0x15, 0x81, 0x25, 0x7F, 0x75, 0x08, 0x95, 0x01, 0x81, 0x06,
    0xC0, 0xC0
};

// ── Relative mouse ──────────────────────────────────────────────────────────

typedef struct __attribute__((packed)) {
    uint8_t buttons;
    int8_t  dx;
    int8_t  dy;
    int8_t  wheel;
} rel_mouse_report_t;

uint8_t const desc_hid_rel_mouse_report[] = {
    0x05, 0x01, 0x09, 0x02, 0xA1, 0x01, 0x09, 0x01, 0xA1, 0x00,
    0x05, 0x09, 0x19, 0x01, 0x29, 0x08, 0x15, 0x00, 0x25, 0x01,
    0x95, 0x08, 0x75, 0x01, 0x81, 0x02,
    0x05, 0x01, 0x09, 0x30,
    0x15, 0x81, 0x25, 0x7F, 0x75, 0x08, 0x95, 0x01, 0x81, 0x06,
    0x09, 0x31,
    0x15, 0x81, 0x25, 0x7F, 0x75, 0x08, 0x95, 0x01, 0x81, 0x06,
    0x09, 0x38,
    0x15, 0x81, 0x25, 0x7F, 0x75, 0x08, 0x95, 0x01, 0x81, 0x06,
    0xC0, 0xC0
};

// ── Configuration descriptors (abs / rel variants) ──────────────────────────

#define CONFIG_TOTAL_LEN (TUD_CONFIG_DESC_LEN + 2*TUD_HID_DESC_LEN)

uint8_t const desc_configuration_abs[] = {
    TUD_CONFIG_DESCRIPTOR(1, ITF_NUM_TOTAL, 0, CONFIG_TOTAL_LEN,
                          0, 100), // The PIO device stack does not implement remote wakeup.
    TUD_HID_DESCRIPTOR(ITF_NUM_KEYBOARD, 0, HID_ITF_PROTOCOL_KEYBOARD,
                       sizeof(desc_hid_keyboard_report), EPNUM_KEYBOARD,
                       CFG_TUD_HID_EP_BUFSIZE, 10),
    TUD_HID_DESCRIPTOR(ITF_NUM_MOUSE, 0, HID_ITF_PROTOCOL_MOUSE,
                       sizeof(desc_hid_abs_mouse_report), EPNUM_MOUSE,
                       CFG_TUD_HID_EP_BUFSIZE, MOUSE_POLL_INTERVAL_MS),
};

uint8_t const desc_configuration_rel[] = {
    TUD_CONFIG_DESCRIPTOR(1, ITF_NUM_TOTAL, 0, CONFIG_TOTAL_LEN,
                          0, 100),
    TUD_HID_DESCRIPTOR(ITF_NUM_KEYBOARD, 0, HID_ITF_PROTOCOL_KEYBOARD,
                       sizeof(desc_hid_keyboard_report), EPNUM_KEYBOARD,
                       CFG_TUD_HID_EP_BUFSIZE, 10),
    TUD_HID_DESCRIPTOR(ITF_NUM_MOUSE, 0, HID_ITF_PROTOCOL_MOUSE,
                       sizeof(desc_hid_rel_mouse_report), EPNUM_MOUSE,
                       CFG_TUD_HID_EP_BUFSIZE, MOUSE_POLL_INTERVAL_MS),
};

const uint8_t *report_desc_abs[] = { desc_hid_keyboard_report, desc_hid_abs_mouse_report };
const uint8_t *report_desc_rel[] = { desc_hid_keyboard_report, desc_hid_rel_mouse_report };

// ── String descriptors ──────────────────────────────────────────────────────

const char *string_descriptors_base[] = {
    [0] = (const char[]){0x09, 0x04},
    [1] = "AnyKVM",
    [2] = "AnyKVM USB HID",
    [3] = "123456",
};
static string_descriptor_t str_desc[4];

static void init_string_desc(void) {
    for (int idx = 0; idx < 4; idx++) {
        uint8_t len = 0;
        uint16_t *wchar_str = (uint16_t *)&str_desc[idx];
        if (idx == 0) {
            wchar_str[1] = string_descriptors_base[0][0] | ((uint16_t)string_descriptors_base[0][1] << 8);
            len = 1;
        } else if (idx <= 3) {
            len = strnlen(string_descriptors_base[idx], 31);
            for (int i = 0; i < len; i++) wchar_str[i + 1] = string_descriptors_base[idx][i];
        }
        wchar_str[0] = (TUSB_DESC_STRING << 8) | (2 * len + 2);
    }
}

// ── Descriptor buffers (rebuilt on mode switch) ─────────────────────────────

static usb_descriptor_buffers_t desc;
static uint8_t mouse_mode = OUT1_MOUSE_USB_ABS;
static pio_usb_configuration_t pio_config = PIO_USB_DEFAULT_CONFIG;

static void rebuild_descriptors(void) {
    desc.device = (uint8_t *)&desc_device;
    desc.config = (mouse_mode == OUT1_MOUSE_USB_REL) ? desc_configuration_rel : desc_configuration_abs;
    desc.hid_report = (mouse_mode == OUT1_MOUSE_USB_REL) ? report_desc_rel : report_desc_abs;
    desc.string = str_desc;
}

// ── Core 1: PIO-USB device ──────────────────────────────────────────────────

static volatile bool core1_ready = false;

void core1_main() {
    sleep_ms(100);
    init_string_desc();
    rebuild_descriptors();
    usb_device = pio_usb_device_init(&pio_config, &desc);
    core1_ready = true;
    while (true) { pio_usb_device_task(); }
}

// ── HID state ───────────────────────────────────────────────────────────────

static uint8_t kbd_report[8] = {0};

static abs_mouse_report_t abs_mouse = {0};
static rel_mouse_report_t rel_mouse = {0};

// The PIO device API indexes by USB endpoint number, including EP0.
// EP 0x81 is index 1 and EP 0x82 is index 2.
static void send_report(uint8_t index, const void *report, uint8_t size) {
    if (!usb_device) return;
    endpoint_t *ep = pio_usb_get_endpoint(usb_device, index);
    if (!ep) return;
    absolute_time_t deadline = make_timeout_time_ms(100);
    do {
        if (pio_usb_set_out_data(ep, report, size) == 0) return;
        sleep_us(100);
    } while (!time_reached(deadline));
}

static void send_kbd(void) {
    send_report(EPNUM_KEYBOARD & 0x0F, kbd_report, sizeof(kbd_report));
}

static void send_mouse(void) {
    if (!mouse_mode) return;
    if (mouse_mode == OUT1_MOUSE_USB_REL)
        send_report(EPNUM_MOUSE & 0x0F, &rel_mouse, sizeof(rel_mouse));
    else
        send_report(EPNUM_MOUSE & 0x0F, &abs_mouse, sizeof(abs_mouse));
}

static void save_outputs(void) {
    uint8_t state[2] = {MAGIC_REQ, OUT1_KBD_USB | mouse_mode};
    watchdog_hw->scratch[0] = ((uint32_t)state[0] << 24)
        | ((uint32_t)state[1] << 16) | bk_crc16(state, 2);
}

static void stop_target_usb(void) {
    multicore_reset_core1();
    core1_ready = false;
    usb_device = NULL;
    dma_channel_abort(pio_config.tx_ch);
    PIO tx = pio_config.pio_tx_num == 0 ? pio0 : pio1;
    PIO rx = pio_config.pio_rx_num == 0 ? pio0 : pio1;
    pio_sm_set_enabled(tx, pio_config.sm_tx, false);
    pio_sm_set_enabled(rx, pio_config.sm_rx, false);
    pio_sm_set_enabled(rx, pio_config.sm_eop, false);
}

static void reinit_target_usb(void) {
    // Only this firmware uses PIO0/PIO1. Core reset alone does not release
    // the SDK's DMA/SM claims or the loaded PIO programs.
    stop_target_usb();
    PIO tx = pio_config.pio_tx_num == 0 ? pio0 : pio1;
    PIO rx = pio_config.pio_rx_num == 0 ? pio0 : pio1;
    dma_channel_unclaim(pio_config.tx_ch);
    pio_sm_unclaim(tx, pio_config.sm_tx);
    pio_sm_unclaim(rx, pio_config.sm_rx);
    pio_sm_unclaim(rx, pio_config.sm_eop);
    pio_clear_instruction_memory(tx);
    if (rx != tx) pio_clear_instruction_memory(rx);
    gpio_init(pio_config.pin_dp);
    gpio_put(pio_config.pin_dp, 0);
    gpio_set_dir(pio_config.pin_dp, GPIO_OUT);
    sleep_ms(100);
    memset(kbd_report, 0, sizeof(kbd_report));
    memset(&abs_mouse, 0, sizeof(abs_mouse));
    memset(&rel_mouse, 0, sizeof(rel_mouse));
    multicore_launch_core1(core1_main);
    for (int i = 0; i < 100 && !core1_ready; i++) sleep_ms(10);
}

static bk_device control;
static bk_parser parser;
static void control_reply(void *ctx, uint8_t cmd, const uint8_t *data) {
    (void)ctx;
    uint8_t bytes[8]; bk_frame(bytes, cmd, data);
    for (int i = 0; i < 8; ++i) putchar_raw(bytes[i]);
    stdio_flush();
}
static uint8_t control_status(void *ctx) {
    (void)ctx;
    endpoint_t *ep = usb_device ? pio_usb_get_endpoint(usb_device, EPNUM_KEYBOARD & 0x0f) : NULL;
    return ep && ep->is_tx ? 0x04 : 0;
}
static void control_keyboard(void *ctx, const uint8_t *report) {
    (void)ctx;
    memcpy(kbd_report, report, 8); send_kbd();
}
static void control_mouse(void *ctx, bool absolute, const uint8_t *report, size_t size) {
    (void)ctx;
    if (absolute) memcpy(&abs_mouse, report, size);
    else memcpy(&rel_mouse, report, size);
    send_mouse();
}
static uint8_t control_mode(void *ctx, bool absolute) {
    (void)ctx;
    mouse_mode = absolute ? OUT1_MOUSE_USB_ABS : OUT1_MOUSE_USB_REL;
    save_outputs();
    reinit_target_usb();
    return BK_OK;
}
static void control_init(void) {
    control.capabilities = BK_KEYBOARD | BK_RELATIVE | BK_ABSOLUTE | BK_USB;
    control.model = BK_MODEL_PICO;
    control.reply = control_reply; control.status = control_status;
    control.mode = control_mode;
    control.input.absolute = mouse_mode == OUT1_MOUSE_USB_ABS;
    control.input.keyboard = control_keyboard; control.input.mouse = control_mouse;
}

// ── main ────────────────────────────────────────────────────────────────────

int main() {
    anykvm_boot_diagnostic_stage(2);
    anykvm_rp2350_reboot_sleep_fix();
    uint32_t saved = watchdog_hw->scratch[0];
    uint8_t state[2] = {saved >> 24, saved >> 16};
    if (state[0] == MAGIC_REQ && bk_crc16(state, 2) == (saved & 0xFFFF)) {
        uint8_t saved_mode = state[1] & 0x38;
        if (saved_mode == OUT1_MOUSE_USB_ABS || saved_mode == OUT1_MOUSE_USB_REL)
            mouse_mode = saved_mode;
    }
    pio_config.pin_dp = 12; // Waveshare USB-A: D+ GP12, D- GP13
    anykvm_boot_diagnostic_stage(3);
    set_sys_clock_khz(120000, true);
    anykvm_boot_diagnostic_stage(4);
    // Make the BOOTSEL -> CDC change a real USB detach, including on hubs.
    // The SDK preserves USBCTRL during early resets for USB-to-SWD, but
    // this firmware owns native USB exclusively and can reset it here.
    reset_block_mask(1u << RESET_USBCTRL);
    anykvm_boot_diagnostic_stage(5);
    sleep_ms(500);
    anykvm_boot_diagnostic_stage(6);
    unreset_block_mask_wait_blocking(1u << RESET_USBCTRL);
    anykvm_boot_diagnostic_stage(7);
    stdio_init_all();
    anykvm_boot_diagnostic_stage(8);
    stdio_set_translate_crlf(&stdio_usb, false);
    // The board has an external D+ pull-up. Force a target-side detach
    // across watchdog reboot so the host requests the new descriptors.
    gpio_init(pio_config.pin_dp);
    gpio_put(pio_config.pin_dp, 0);
    gpio_set_dir(pio_config.pin_dp, GPIO_OUT);
    sleep_ms(100);

    // Start PIO only after native USB is configured, without waiting for
    // a serial application to open CDC/DTR. Keep a bounded fallback so
    // USB-A also starts when no control host is connected to USB-C.
    absolute_time_t native_usb_deadline = make_timeout_time_ms(10000);
    anykvm_boot_diagnostic_stage(9);
    while (!tud_mounted() && !time_reached(native_usb_deadline)) sleep_ms(1);
#ifdef ANYKVM_USB_BOOT_DIAGNOSTICS
    // During hardware diagnosis, return to ROM if native USB never configures.
    // This distinguishes a failed CDC enumeration from an earlier boot hang.
    if (!tud_mounted()) {
        tud_disconnect();
        sleep_ms(300);
        rom_reset_usb_boot_extra(-1, 0, false);
    }
#endif
    anykvm_boot_diagnostic_stage(10);
    sleep_ms(50);
    anykvm_boot_diagnostic_stage(11);
    multicore_reset_core1();
    multicore_launch_core1(core1_main);

    // Wait for PIO-USB to come up
    for (int i = 0; i < 100 && !core1_ready; i++) sleep_ms(10);
    anykvm_boot_diagnostic_ready();

    control_init();
    bool was_connected = false;
    while (true) {
        if (anykvm_usb_reset_requested()) {
            stop_target_usb();
            anykvm_usb_reset_execute();
        }
        bool target_connected = (control_status(NULL) & 0x04) != 0;
        if (was_connected != target_connected) bk_reset(&control.input);
        was_connected = target_connected;
        uint8_t cmd, data[4];
        int byte = getchar_timeout_us(1000);
        if (byte >= 0 && bk_parse(&parser, (uint8_t)byte, &cmd, data))
            bk_command(&control, cmd, data);
    }
}
