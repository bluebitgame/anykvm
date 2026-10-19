/* Native USB: CDC on every build, plus HID on the native-output build. */
#include <string.h>
#include "pico/stdlib.h"
#include "pico/unique_id.h"
#include "tusb.h"
#if ANYKVM_OUTPUT_NATIVE
#include "reports.h"
#endif
#include "output.h"
#include "../AnyKVMProtocol/src/AnyKVMProtocol.h"

enum { CDC_CONTROL, CDC_DATA,
#if ANYKVM_OUTPUT_NATIVE
    KEYBOARD, MOUSE,
#endif
    INTERFACE_COUNT };
#define TOTAL_LEN (TUD_CONFIG_DESC_LEN + TUD_CDC_DESC_LEN + CFG_TUD_HID * TUD_HID_DESC_LEN)
static const uint8_t configuration[] = {
    TUD_CONFIG_DESCRIPTOR(1, INTERFACE_COUNT, 0, TOTAL_LEN, 0, 100),
    TUD_CDC_DESCRIPTOR(CDC_CONTROL, 4, 0x83, 8, 0x04, 0x84, 64),
#if ANYKVM_OUTPUT_NATIVE
    TUD_HID_DESCRIPTOR(KEYBOARD, 0, HID_ITF_PROTOCOL_KEYBOARD,
                       sizeof(keyboard_descriptor), 0x81, 64, 1),
    TUD_HID_DESCRIPTOR(MOUSE, 0, HID_ITF_PROTOCOL_NONE,
                       sizeof(mouse_descriptor), 0x82, 64, 1),
#endif
};
static const tusb_desc_device_t device = {
    .bLength = sizeof(tusb_desc_device_t), .bDescriptorType = TUSB_DESC_DEVICE,
    .bcdUSB = 0x0200, .bDeviceClass = TUSB_CLASS_MISC,
    .bDeviceSubClass = MISC_SUBCLASS_COMMON, .bDeviceProtocol = MISC_PROTOCOL_IAD,
    .bMaxPacketSize0 = 64, .idVendor = 0xCafe,
    .idProduct = ANYKVM_USB_PID, .bcdDevice = 0x0200,
    .iManufacturer = 1, .iProduct = 2, .iSerialNumber = 3, .bNumConfigurations = 1,
};
const uint8_t *tud_descriptor_device_cb(void) { return (const uint8_t *)&device; }
const uint8_t *tud_descriptor_configuration_cb(uint8_t index) {
    (void)index; return configuration;
}
const uint16_t *tud_descriptor_string_cb(uint8_t index, uint16_t langid) {
    (void)langid;
    static uint16_t value[64];
    static char serial[PICO_UNIQUE_BOARD_ID_SIZE_BYTES * 2 + 1];
    if (!index) { value[0] = 0x0304; value[1] = 0x0409; return value; }
    pico_get_unique_board_id_string(serial, sizeof(serial));
    const char *strings[] = {NULL, "AnyKVM", ANYKVM_USB_PRODUCT, serial, "AnyKVM Control"};
    if (index >= sizeof(strings) / sizeof(strings[0])) return NULL;
    size_t length = strlen(strings[index]);
    if (length > 63) length = 63;
    for (size_t i = 0; i < length; ++i) value[i + 1] = strings[index][i];
    value[0] = (TUSB_DESC_STRING << 8) | (2 * length + 2);
    return value;
}
#if ANYKVM_OUTPUT_NATIVE
const uint8_t *tud_hid_descriptor_report_cb(uint8_t instance) {
    return instance == 0 ? keyboard_descriptor : mouse_descriptor;
}
uint16_t tud_hid_get_report_cb(uint8_t instance, uint8_t id, hid_report_type_t type,
                             uint8_t *buffer, uint16_t length) {
    (void)instance; (void)id; (void)type; (void)buffer; (void)length; return 0;
}
void tud_hid_set_report_cb(uint8_t instance, uint8_t id, hid_report_type_t type,
                          const uint8_t *buffer, uint16_t length) {
    (void)instance; (void)id; (void)type; (void)buffer; (void)length;
}
static bool send_report(uint8_t instance, uint8_t id, const void *report, size_t size) {
    if (!tud_ready()) return false;
    absolute_time_t deadline = make_timeout_time_ms(100);
    while (!tud_hid_n_ready(instance) && !time_reached(deadline)) tud_task();
    return tud_hid_n_ready(instance) && tud_hid_n_report(instance, id, report, size);
}
void output_init(void) {}
void output_task(void) {}
bool output_ready(void) { return tud_ready(); }
uint8_t output_capabilities(void) { return BK_KEYBOARD | BK_RELATIVE | BK_ABSOLUTE | BK_USB; }
bool output_default_absolute(void) { return true; }
uint8_t output_mode(bool absolute) { (void)absolute; return BK_OK; }
bool output_keyboard(const uint8_t report[8]) { return send_report(0, 0, report, 8); }
bool output_mouse(bool absolute, const uint8_t *report, size_t size) {
    return send_report(1, absolute ? 1 : 2, report, size);
}
#endif
