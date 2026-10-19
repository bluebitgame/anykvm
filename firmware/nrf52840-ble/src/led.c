/*
 * AnyKVM nRF52840 firmware - LED status indicator.
 *
 * XIAO BLE: led0 = red, led1 = green, led2 = blue.
 * Blue  — BLE connection / advertising status (led_set).
 * Green — PC USB serial + BLE both connected (led_set_pc_host + led_set).
 * Red   — fault / init error (led_on / led_off); reserved for other uses later.
 */
#include <zephyr/kernel.h>
#include <zephyr/drivers/gpio.h>

#include "led.h"

#ifndef ANYKVM_BLE_LED_NODE
#define ANYKVM_BLE_LED_NODE DT_ALIAS(led2)
#endif

#ifndef ANYKVM_HOST_LED_NODE
#define ANYKVM_HOST_LED_NODE DT_ALIAS(led1)
#endif

#ifndef ANYKVM_FAULT_LED_NODE
#define ANYKVM_FAULT_LED_NODE DT_ALIAS(led0)
#endif

#if DT_NODE_EXISTS(ANYKVM_BLE_LED_NODE)
static const struct gpio_dt_spec ble_led =
	GPIO_DT_SPEC_GET(ANYKVM_BLE_LED_NODE, gpios);
#else
static const struct gpio_dt_spec ble_led;
#endif

#if DT_NODE_EXISTS(ANYKVM_HOST_LED_NODE)
static const struct gpio_dt_spec host_led =
	GPIO_DT_SPEC_GET(ANYKVM_HOST_LED_NODE, gpios);
#else
static const struct gpio_dt_spec host_led;
#endif

#if DT_NODE_EXISTS(ANYKVM_FAULT_LED_NODE)
static const struct gpio_dt_spec fault_led =
	GPIO_DT_SPEC_GET(ANYKVM_FAULT_LED_NODE, gpios);
#else
static const struct gpio_dt_spec fault_led;
#endif

static bool ble_led_has_hw;
static bool host_led_has_hw;
static bool fault_led_has_hw;

static bool pc_host_connected;
static bool ble_connected;

static void led_blink_work_fn(struct k_work *work);
static K_WORK_DELAYABLE_DEFINE(led_blink_work, led_blink_work_fn);
static uint32_t blink_period;
static bool blink_running;

static void led_update_ready(void)
{
	if (!host_led_has_hw) {
		return;
	}
	gpio_pin_set_dt(&host_led, pc_host_connected && ble_connected);
}

static void schedule_blink(uint32_t period_ms)
{
	blink_period = period_ms;
	blink_running = true;
	k_work_reschedule(&led_blink_work, K_MSEC(period_ms));
}

static void led_blink_work_fn(struct k_work *work)
{
	if (!blink_running || !ble_led_has_hw) {
		return;
	}
	gpio_pin_toggle_dt(&ble_led);
	k_work_reschedule(&led_blink_work, K_MSEC(blink_period));
}

void led_init(void)
{
#if DT_NODE_EXISTS(ANYKVM_BLE_LED_NODE)
	if (gpio_is_ready_dt(&ble_led)) {
		gpio_pin_configure_dt(&ble_led, GPIO_OUTPUT_INACTIVE);
		ble_led_has_hw = true;
	}
#endif
#if DT_NODE_EXISTS(ANYKVM_HOST_LED_NODE)
	if (gpio_is_ready_dt(&host_led)) {
		gpio_pin_configure_dt(&host_led, GPIO_OUTPUT_INACTIVE);
		host_led_has_hw = true;
	}
#endif
#if DT_NODE_EXISTS(ANYKVM_FAULT_LED_NODE)
	if (gpio_is_ready_dt(&fault_led)) {
		gpio_pin_configure_dt(&fault_led, GPIO_OUTPUT_INACTIVE);
		fault_led_has_hw = true;
	}
#endif
}

void led_set(bool connected, bool advertising, bool bonded)
{
	ble_connected = connected;
	led_update_ready();

	if (!ble_led_has_hw) {
		return;
	}
	blink_running = false;
	k_work_cancel_delayable(&led_blink_work);

	if (connected) {
		gpio_pin_set_dt(&ble_led, 1);
	} else if (advertising) {
		schedule_blink(bonded ? 200 : 500);
	} else {
		gpio_pin_set_dt(&ble_led, 0);
	}
}

void led_set_pc_host(bool connected)
{
	if (pc_host_connected == connected) {
		return;
	}
	pc_host_connected = connected;
	led_update_ready();
}

void led_on(void)
{
	if (!fault_led_has_hw) {
		return;
	}
	gpio_pin_set_dt(&fault_led, 1);
}

void led_off(void)
{
	if (!fault_led_has_hw) {
		return;
	}
	gpio_pin_set_dt(&fault_led, 0);
}
