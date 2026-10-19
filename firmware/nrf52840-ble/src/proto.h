/*
 * AnyKVM nRF52840 firmware — protocol layer.
 *
 * AnyKVM v1 8-byte framed protocol:
 *   [0x33][cmd 1B][data 4B][CRC16 BE 2B] = 8 bytes
 * CRC16 MODBUS: poly=0xA001, init=0xFFFF, reflected.
 */
#ifndef ANYKVM_PROTO_H_
#define ANYKVM_PROTO_H_

#include <stddef.h>
#include <stdint.h>

uint16_t proto_crc16(const uint8_t *data, size_t len);

typedef void (*proto_frame_handler_t)(uint8_t cmd, const uint8_t data[4]);

void proto_reset(void);
void proto_rx_byte(uint8_t byte, proto_frame_handler_t handler);

#endif /* ANYKVM_PROTO_H_ */
