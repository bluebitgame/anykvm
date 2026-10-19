/*
 * AnyKVM nRF52840 firmware — BLE HID service.
 *
 * Supports two mouse report maps, selected by init_mode:
 *   - Relative mouse (4B: buttons, dx, dy, wheel) — for iOS and relative hosts
 *   - Absolute mouse (6B: buttons, x_le 2B, y_le 2B, wheel) — for macOS/Win/Linux
 *
 * Only ONE mouse report is exposed at a time (report ID 2 in both modes).
 * Use anykvm_hids_deinit() + anykvm_hids_init(new_mode) to switch.
 */
#ifndef ANYKVM_HIDS_H_
#define ANYKVM_HIDS_H_

#include <stdbool.h>
#include <stdint.h>

struct bt_conn;

int  anykvm_hids_init(bool abs_mode);

/*
 * De-register the HID GATT service, allowing re-initialisation with a
 * different report map.  Requires CONFIG_BT_GATT_DYNAMIC_DB=y.
 * After calling this, anykvm_hids_init() can be called again.
 */
int  anykvm_hids_deinit(void);

/* Notify a connection established/lost so HIDS can (de)register the client. */
void anykvm_hids_connected(struct bt_conn *conn);
void anykvm_hids_disconnected(struct bt_conn *conn);

/*
 * Send a mouse input report using the currently active report format.
 * For relative mode:  dx, dy are treated as int8 deltas.
 * For absolute mode: dx, dy are treated as uint16 absolute coordinates (0-32767).
 */
int  anykvm_hids_send_mouse(uint8_t buttons, int16_t dx_or_x,
			     int16_t dy_or_y, int8_t wheel);

int  anykvm_hids_send_keyboard(const uint8_t report[8]);

#endif /* ANYKVM_HIDS_H_ */
