/* Bound PIO USB control-IN data to the host's SETUP wLength. */
#include "pio_usb_ll.h"

bool __real_pio_usb_ll_transfer_start(endpoint_t *ep, uint8_t *buffer,
                                    uint16_t length);

bool __wrap_pio_usb_ll_transfer_start(endpoint_t *ep, uint8_t *buffer,
                                    uint16_t length) {
    root_port_t *root = PIO_USB_ROOT_PORT(0);
    // The reference device stack always queues 18 bytes for GET_DESCRIPTOR
    // DEVICE, including macOS's initial 8-byte request. Its string replies
    // also use the full descriptor length. Apply the host's limit at EP0 IN
    // without changing the external PIO library or interrupt HID reports.
    if (length && ep == &pio_usb_ep_pool[1] &&
        root->mode == PIO_USB_MODE_DEVICE && root->setup_packet) {
        const usb_setup_packet_t *setup =
            (const usb_setup_packet_t *)root->setup_packet;
        if (setup->request_type & USB_REQ_DIR_IN) {
            uint16_t requested = (uint16_t)setup->length_lsb |
                                 ((uint16_t)setup->length_msb << 8);
            if (length > requested) length = requested;
        }
    }
    return __real_pio_usb_ll_transfer_start(ep, buffer, length);
}
