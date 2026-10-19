/*
 * AnyKVM nRF52840 firmware — binary reply frame transmitter.
 */
#include <zephyr/kernel.h>
#include <zephyr/device.h>
#include <zephyr/drivers/uart.h>

#include "reply.h"
#include "proto.h"

static const struct device *cdc_dev;
K_MUTEX_DEFINE(reply_lock);

void reply_set_device(const struct device *cdc)
{
	cdc_dev = cdc;
}

void reply_send(uint8_t cmd, const uint8_t data[4])
{
	uint8_t frame[8];

	frame[0] = 0x33;
	frame[1] = cmd;
	frame[2] = data[0];
	frame[3] = data[1];
	frame[4] = data[2];
	frame[5] = data[3];

	uint16_t crc = proto_crc16(frame, 6);

	frame[6] = (uint8_t)((crc >> 8) & 0xFF);
	frame[7] = (uint8_t)(crc & 0xFF);

	if (cdc_dev) {
		k_mutex_lock(&reply_lock, K_FOREVER);
		for (int i = 0; i < 8; i++) {
			uart_poll_out(cdc_dev, frame[i]);
		}
		k_mutex_unlock(&reply_lock);
	}
}

void reply_send_ready(uint8_t slot_count)
{
	uint8_t data[4] = { slot_count, 0, 0, 0 };

	reply_send(0x80, data);
}

void reply_send_status(uint8_t slot, bool conn, bool bonded, bool adv,
		       bool abs_mouse)
{
	uint8_t flags = slot & 0x03;

	if (conn) {
		flags |= 0x04;
	}
	if (bonded) {
		flags |= 0x08;
	}
	if (adv) {
		flags |= 0x10;
	}
	if (abs_mouse) {
		flags |= 0x20;
	}

	uint8_t data[4] = { flags, 0, 0, 0 };

	reply_send(0x81, data);
}

void reply_send_event(uint8_t event_type, uint8_t slot)
{
	uint8_t data[4] = { event_type, slot, 0, 0 };

	reply_send(0x82, data);
}
