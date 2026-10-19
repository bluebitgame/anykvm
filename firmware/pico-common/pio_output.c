/* Second USB device controller implemented on GP12/13, independent of CDC. */
#include "pico/stdlib.h"
#include "pico/multicore.h"
#include "pio_usb.h"
#include "output.h"
#include "pins.h"
#include "reports.h"
#include "../AnyKVMProtocol/src/AnyKVMProtocol.h"

static usb_device_t *volatile target;
static pio_usb_configuration_t config = PIO_USB_DEFAULT_CONFIG;
static const tusb_desc_device_t device = {
    .bLength = sizeof(tusb_desc_device_t), .bDescriptorType = TUSB_DESC_DEVICE,
    .bcdUSB = 0x0110, .bMaxPacketSize0 = 64, .idVendor = 0xCafe,
    .idProduct = 0x4013, .bcdDevice = 0x0200,
    .iManufacturer = 1, .iProduct = 2, .iSerialNumber = 3, .bNumConfigurations = 1,
};
static const uint8_t configuration[] = {
    TUD_CONFIG_DESCRIPTOR(1, 2, 0, TUD_CONFIG_DESC_LEN + 2 * TUD_HID_DESC_LEN, 0, 100),
    TUD_HID_DESCRIPTOR(0, 0, HID_ITF_PROTOCOL_KEYBOARD, sizeof(keyboard_descriptor), 0x81, 64, 1),
    TUD_HID_DESCRIPTOR(1, 0, HID_ITF_PROTOCOL_NONE, sizeof(mouse_descriptor), 0x82, 64, 1),
};
static const uint8_t *reports[] = {keyboard_descriptor, mouse_descriptor};
static string_descriptor_t strings[4];
static usb_descriptor_buffers_t descriptors;
static bool started;
static absolute_time_t start_deadline;
static void core1(void) {
    target = pio_usb_device_init(&config, &descriptors);
    while (true) pio_usb_device_task();
}
void output_init(void) {
    config.pin_dp = ANYKVM_PIO_DP;
    const char *text[] = {"", "AnyKVM", "AnyKVM PIO HID", "PIO-HID"};
    for (unsigned i = 0; i < 4; ++i) {
        uint16_t *s = (uint16_t *)&strings[i];
        size_t n = i ? strlen(text[i]) : 1;
        s[0] = (TUSB_DESC_STRING << 8) | (2 * n + 2);
        if (!i) s[1] = 0x0409;
        else for (size_t j = 0; j < n; ++j) s[j + 1] = text[i][j];
    }
    descriptors.device = (const uint8_t *)&device;
    descriptors.config = configuration;
    descriptors.hid_report = reports;
    descriptors.string = strings;
    // Force target-side detach even across a watchdog reboot.
    gpio_init(ANYKVM_PIO_DP); gpio_put(ANYKVM_PIO_DP, 0); gpio_set_dir(ANYKVM_PIO_DP, GPIO_OUT);
    start_deadline = make_timeout_time_ms(10000);
}
void output_task(void) {
    if (!started && (tud_mounted() || time_reached(start_deadline))) {
        // Native USB is configured first, as on the validated USB-A board.
        sleep_ms(100);
        multicore_launch_core1(core1);
        started = true;
    }
}
bool output_ready(void) {
    endpoint_t *ep = target ? pio_usb_get_endpoint(target, 1) : NULL;
    return ep && ep->is_tx;
}
static bool send_report(unsigned endpoint, const uint8_t *report, size_t size) {
    if (!output_ready()) return false;
    endpoint_t *ep = pio_usb_get_endpoint(target, endpoint);
    if (!ep) return false;
    absolute_time_t deadline = make_timeout_time_ms(100);
    do {
        if (pio_usb_set_out_data(ep, report, size) == 0) return true;
        tud_task(); sleep_us(100);
    } while (!time_reached(deadline));
    return false;
}
uint8_t output_capabilities(void) { return BK_KEYBOARD | BK_RELATIVE | BK_ABSOLUTE | BK_USB; }
bool output_default_absolute(void) { return true; }
uint8_t output_mode(bool absolute) { (void)absolute; return BK_OK; }
bool output_keyboard(const uint8_t report[8]) { return send_report(1, report, 8); }
bool output_mouse(bool absolute, const uint8_t *report, size_t size) {
    uint8_t packet[7];
    packet[0] = absolute ? 1 : 2;
    memcpy(packet + 1, report, size);
    return send_report(2, packet, size + 1);
}
