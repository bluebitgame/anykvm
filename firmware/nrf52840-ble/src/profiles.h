/*
 * AnyKVM nRF52840 firmware — multi-device profile manager.
 *
 * Each profile is an independent BLE identity (own address + own bond) and
 * has its own mouse mode (absolute / relative) persisted in Zephyr settings.
 * Switching profiles rebuilds the HID GATT service when the mouse mode
 * changes.
 *
 * Uses the binary reply frame protocol (reply.h) instead of printk for
 * host-bound status and event messages.
 */
#ifndef ANYKVM_PROFILES_H_
#define ANYKVM_PROFILES_H_

#include <stdbool.h>
#include <stdint.h>

struct profile_status {
	uint8_t slot;
	bool connected;
	bool bonded;
	bool advertising;
	bool abs_mouse;
};

int anykvm_profiles_init(void);
int anykvm_profiles_start(bool abs_mode);

int anykvm_profiles_select(uint8_t slot);
int anykvm_profiles_clear(uint8_t slot);
int anykvm_profiles_advertise(void);

void anykvm_profiles_status(struct profile_status *out);

/* Current active slot's mouse mode (true = absolute, false = relative). */
bool anykvm_profiles_mouse_mode(void);

/*
 * Set the mouse mode for the currently active slot, persist it in settings,
 * and rebuild the HID GATT service if the mode changed.
 */
int anykvm_profiles_set_mouse_mode(bool abs_mode);

#endif /* ANYKVM_PROFILES_H_ */
