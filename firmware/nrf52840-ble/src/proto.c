/* AnyKVM v1 framing shared with all serial HID modules. */
#include "proto.h"
#include "../../AnyKVMProtocol/src/AnyKVMProtocol.h"
static bk_parser parser;
uint16_t proto_crc16(const uint8_t *data, size_t len) { return bk_crc16(data, len); }
void proto_reset(void) { parser.used = 0; }
void proto_rx_byte(uint8_t byte, proto_frame_handler_t handler) {
    uint8_t cmd, data[4];
    if (bk_parse(&parser, byte, &cmd, data)) handler(cmd, data);
}
