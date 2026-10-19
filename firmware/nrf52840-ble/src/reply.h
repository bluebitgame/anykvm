/*
 * AnyKVM nRF52840 firmware — binary reply frames.
 *
 * Sends 8-byte AnyKVM v1 reply frames over CDC ACM UART.
 * Frame: [0x33][cmd 1B][data 4B][CRC16 BE 2B]
 *
 * Reply commands:
 *   0x80  ready       data: [slot_count][0][0][0]
 *   0x81  status      data: [slot|conn|bonded|adv|abs_mouse packed][0][0][0]
 *   0x82  event       data: [event_type][slot][0][0]
 *   0x83  capabilities and 0x84 command results use the shared device layer.
 */
#ifndef ANYKVM_REPLY_H_
#define ANYKVM_REPLY_H_

#include <stdint.h>
#include <stdbool.h>

struct device;

#define REPLY_EVT_CONNECTED    0x01
#define REPLY_EVT_DISCONNECTED 0x02

/* Status packed bits in data[0]:
 *   bits 0-1: slot (0-3)
 *   bit 2:   connected
 *   bit 3:   bonded
 *   bit 4:   advertising
 *   bit 5:   abs_mouse (0=rel, 1=abs)
 */

void reply_send(uint8_t cmd, const uint8_t data[4]);
void reply_set_device(const struct device *cdc);

void reply_send_ready(uint8_t slot_count);
void reply_send_status(uint8_t slot, bool conn, bool bonded, bool adv,
		       bool abs_mouse);
void reply_send_event(uint8_t event_type, uint8_t slot);

#endif /* ANYKVM_REPLY_H_ */
