/*
 * AnyKVM nRF52840 firmware — LED status indicator.
 *
 * led_set       — blue LED (BLE advertising / connected on XIAO BLE).
 * led_set_pc_host — green LED (PC USB serial + BLE both connected).
 * led_on/off    — red LED (init fault blink; reserved for other alerts).
 */

#ifndef ANYKVM_LED_H_
#define ANYKVM_LED_H_

#include <stdbool.h>

void led_init(void);
void led_set(bool connected, bool advertising, bool bonded);
void led_set_pc_host(bool connected);
void led_on(void);
void led_off(void);

#endif /* ANYKVM_LED_H_ */
