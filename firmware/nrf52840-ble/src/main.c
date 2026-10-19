/*
 * AnyKVM nRF52840 firmware - main: USB CDC + AnyKVM framed protocol.
 */
#include <zephyr/kernel.h>
#include <zephyr/logging/log.h>
#include <zephyr/device.h>
#include <zephyr/drivers/uart.h>
#include <zephyr/usb/usb_device.h>
#include <zephyr/bluetooth/bluetooth.h>

#include "hids.h"
#include "profiles.h"
#include "proto.h"
#include "reply.h"
#include "led.h"
#include "../../AnyKVMProtocol/src/AnyKVMDevice.h"

LOG_MODULE_REGISTER(anykvm_main, LOG_LEVEL_INF);

static bk_device control;
static void control_reply(void *ctx, uint8_t cmd, const uint8_t *data) {
    ARG_UNUSED(ctx); reply_send(cmd, data);
}
static uint8_t control_status(void *ctx) {
    ARG_UNUSED(ctx);
    struct profile_status st;
    anykvm_profiles_status(&st);
    led_set(st.connected, st.advertising, st.bonded);
    return (st.slot & 3) | (st.connected ? 4 : 0) |
           (st.bonded ? 8 : 0) | (st.advertising ? 16 : 0);
}
static void control_keyboard(void *ctx, const uint8_t *report) {
    ARG_UNUSED(ctx); anykvm_hids_send_keyboard(report);
}
static void control_mouse(void *ctx, bool absolute, const uint8_t *r, size_t size) {
    ARG_UNUSED(ctx); ARG_UNUSED(size);
    if (absolute)
        anykvm_hids_send_mouse(r[0], (uint16_t)r[1] | (uint16_t)r[2] << 8,
                               (uint16_t)r[3] | (uint16_t)r[4] << 8, (int8_t)r[5]);
    else anykvm_hids_send_mouse(r[0], (int8_t)r[1], (int8_t)r[2], (int8_t)r[3]);
}
static uint8_t control_mode(void *ctx, bool absolute) {
    ARG_UNUSED(ctx);
    return anykvm_profiles_set_mouse_mode(absolute) ? BK_INTERNAL : BK_OK;
}
static uint8_t control_profile(void *ctx, uint8_t cmd, uint8_t slot) {
    ARG_UNUSED(ctx);
    int result = cmd == 0x20 ? anykvm_profiles_select(slot) :
                 cmd == 0x21 ? anykvm_profiles_clear(slot) : anykvm_profiles_advertise();
    control.input.absolute = anykvm_profiles_mouse_mode();
    return result ? BK_INTERNAL : BK_OK;
}
static void control_init(void) {
    control.capabilities = BK_KEYBOARD | BK_RELATIVE | BK_ABSOLUTE | BK_BLE | BK_PROFILES;
    control.slots = CONFIG_APP_PROFILE_COUNT; control.model = BK_MODEL_NRF52840;
    control.reply = control_reply; control.status = control_status;
    control.mode = control_mode; control.profile = control_profile;
    control.input.absolute = anykvm_profiles_mouse_mode();
    control.input.keyboard = control_keyboard; control.input.mouse = control_mouse;
}
static void frame_handler(uint8_t cmd, const uint8_t data[4]) {
    bk_command(&control, cmd, data);
}

/* --- USB CDC RX ------------------------------------------------------- */

static const struct device *cdc_dev;

static void host_dtr_poll(void);

K_MSGQ_DEFINE(rx_msgq, 1, 256, 4);

static void cdc_irq_handler(const struct device *dev, void *user_data)
{
	ARG_UNUSED(user_data);

	while (uart_irq_update(dev) && uart_irq_is_pending(dev)) {
		if (!uart_irq_rx_ready(dev)) {
			break;
		}
		uint8_t byte;

		while (uart_fifo_read(dev, &byte, 1) == 1) {
			k_msgq_put(&rx_msgq, &byte, K_NO_WAIT);
		}
	}
}

static int cdc_init(void)
{
	int err;

	if (IS_ENABLED(CONFIG_USB_DEVICE_STACK)) {
		err = usb_enable(NULL);
		if (err && err != -EALREADY) {
			LOG_ERR("usb_enable: %d", err);
			return err;
		}
	}

	cdc_dev = DEVICE_DT_GET_ONE(zephyr_cdc_acm_uart);
	if (!device_is_ready(cdc_dev)) {
		LOG_ERR("CDC UART not ready");
		return -ENODEV;
	}

	reply_set_device(cdc_dev);

	if (IS_ENABLED(CONFIG_UART_LINE_CTRL)) {
		for (int i = 0; i < 50; i++) {
			uint32_t dtr = 0;

			uart_line_ctrl_get(cdc_dev, UART_LINE_CTRL_DTR, &dtr);
			if (dtr) {
				break;
			}
			k_msleep(20);
		}
		(void)uart_line_ctrl_set(cdc_dev, UART_LINE_CTRL_DCD, 1);
		(void)uart_line_ctrl_set(cdc_dev, UART_LINE_CTRL_DSR, 1);
	}

	err = uart_irq_callback_user_data_set(cdc_dev, cdc_irq_handler, NULL);
	if (err) {
		LOG_ERR("uart irq callback: %d", err);
		return err;
	}

	uart_irq_rx_enable(cdc_dev);
	host_dtr_poll();
	return 0;
}

static void host_dtr_poll(void)
{
	if (!IS_ENABLED(CONFIG_UART_LINE_CTRL) || cdc_dev == NULL) {
		return;
	}

	uint32_t dtr = 0;

	if (uart_line_ctrl_get(cdc_dev, UART_LINE_CTRL_DTR, &dtr) != 0) {
		return;
	}
	led_set_pc_host(dtr != 0);
}

static void fatal_blink(void)
{
	while (1) {
		led_on();
		k_msleep(100);
		led_off();
		k_msleep(100);
	}
}

/* --- main ------------------------------------------------------------- */

int main(void)
{
	int err;
	uint8_t byte;

	led_init();

	err = cdc_init();
	if (err) {
		fatal_blink();
	}

	err = anykvm_profiles_init();
	if (err) {
		LOG_ERR("profiles init: %d", err);
		fatal_blink();
	}

	err = anykvm_hids_init(false);
	if (err) {
		LOG_ERR("hids init: %d", err);
		fatal_blink();
	}

	err = bt_enable(NULL);
	if (err) {
		LOG_ERR("bt_enable: %d", err);
		fatal_blink();
	}

	err = anykvm_profiles_start(false);
	if (err) {
		LOG_ERR("profiles start: %d", err);
		fatal_blink();
	}

	control_init();
	bool was_connected = false;
	proto_reset();
	LOG_INF("AnyKVM nRF52840 ready");

	while (1) {
        host_dtr_poll();
        bool connected = (control_status(NULL) & 4) != 0;
        if (was_connected != connected) bk_reset(&control.input);
        was_connected = connected;

		/* Drain pending USB CDC commands through the shared protocol. */
		if (k_msgq_get(&rx_msgq, &byte, K_MSEC(100)) == 0) {
			do {
				proto_rx_byte(byte, frame_handler);
			} while (k_msgq_get(&rx_msgq, &byte, K_NO_WAIT) == 0);

		}
	}
}
