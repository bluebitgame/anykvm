#include "pico/stdlib.h"
#include "pico/stdio_usb.h"
#include "pico/usb_reset.h"
#include "pico/bootrom.h"
#include "boot/picoboot.h"
#include "tusb.h"
#include "usb_reset.h"

static volatile bool pending;
static bool to_bootsel;
static uint32_t disable_mask;
static uint8_t interface_number;

bool anykvm_usb_reset_requested(void) { return pending; }

void anykvm_usb_reset_execute(void) {
    stdio_flush();
    tud_disconnect();
    sleep_ms(300);
    if (to_bootsel) rom_reset_usb_boot_extra(-1, disable_mask, false);
    rom_reboot(REBOOT2_FLAG_REBOOT_TYPE_NORMAL | REBOOT2_FLAG_NO_RETURN_ON_SUCCESS,
               10, (uint32_t)-1, 0);
    while (true) tight_loop_contents();
}

static uint16_t reset_open(uint8_t rhport, const tusb_desc_interface_t *desc, uint16_t length) {
    uint16_t used = usb_reset_interface_open(rhport, desc, length);
    if (used) interface_number = desc->bInterfaceNumber;
    return used;
}

static bool reset_control(uint8_t rhport, uint8_t stage, const tusb_control_request_t *request) {
    if (request->wIndex != interface_number ||
        (request->bRequest != RESET_REQUEST_BOOTSEL && request->bRequest != RESET_REQUEST_FLASH)) return false;
    if (stage == CONTROL_STAGE_SETUP) return tud_control_status(rhport, request);
    if (stage == CONTROL_STAGE_ACK) {
        to_bootsel = request->bRequest == RESET_REQUEST_BOOTSEL;
        disable_mask = (request->wValue & 3) | PICO_USB_RESET_BOOTSEL_INTERFACE_DISABLE_MASK;
        pending = true;
    }
    return true;
}

usbd_class_driver_t const *usbd_app_driver_get_cb(uint8_t *count) {
    static const usbd_class_driver_t driver = {
        .init = usb_reset_interface_init,
        .reset = usb_reset_interface_reset,
        .open = reset_open,
        .control_xfer_cb = reset_control,
        .xfer_cb = usb_reset_interface_xfer_cb,
    };
    *count = 1;
    return &driver;
}

void tud_cdc_line_coding_cb(uint8_t itf, const cdc_line_coding_t *coding) {
    (void)itf;
    if (coding->bit_rate == PICO_USB_RESET_MAGIC_BAUD_RATE) {
        to_bootsel = true;
        disable_mask = PICO_USB_RESET_BOOTSEL_INTERFACE_DISABLE_MASK;
        pending = true;
    }
}
