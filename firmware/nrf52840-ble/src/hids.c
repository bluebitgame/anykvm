/*
 * AnyKVM nRF52840 firmware — BLE HID service implementation.
 *
 * Two mouse report maps are available; only one is active at a time
 * (report ID 2 in both modes).  Keyboard (report ID 1) is unchanged.
 *
 * Dynamic mode switching: call anykvm_hids_deinit(), then
 * anykvm_hids_init(new_abs_mode).  This rebuilds the GATT HID service.
 * Requires CONFIG_BT_GATT_DYNAMIC_DB=y and bt_hids_uninit()
 * (nRF Connect SDK v2.5+).  See anykvm_hids_deinit() comments.
 */
#include <zephyr/kernel.h>
#include <zephyr/logging/log.h>
#include <zephyr/bluetooth/bluetooth.h>
#include <zephyr/bluetooth/conn.h>
#include <bluetooth/services/hids.h>

#include "hids.h"

LOG_MODULE_REGISTER(anykvm_hids, LOG_LEVEL_INF);

#define REPORT_ID_KEYBOARD 1
#define REPORT_ID_MOUSE    2

#define KEYBOARD_REPORT_LEN  8
#define MOUSE_REL_REPORT_LEN 4
#define MOUSE_ABS_REPORT_LEN 6

#define INPUT_REP_IDX_KEYBOARD 0
#define INPUT_REP_IDX_MOUSE    1

static const uint8_t report_map_rel[] = {
	/* --- Keyboard, report ID 1 --- */
	0x05, 0x01,       /* Usage Page (Generic Desktop) */
	0x09, 0x06,       /* Usage (Keyboard) */
	0xA1, 0x01,       /* Collection (Application) */
	0x85, REPORT_ID_KEYBOARD,
	0x05, 0x07,       /*   Usage Page (Key Codes) */
	0x19, 0xE0,       /*   Usage Min (224: LeftControl) */
	0x29, 0xE7,       /*   Usage Max (231: Right GUI) */
	0x15, 0x00,       /*   Logical Min (0) */
	0x25, 0x01,       /*   Logical Max (1) */
	0x75, 0x01,       /*   Report Size (1) */
	0x95, 0x08,       /*   Report Count (8) */
	0x81, 0x02,       /*   Input (Data, Var, Abs): modifier bits */
	0x75, 0x08,       /*   Report Size (8) */
	0x95, 0x01,       /*   Report Count (1) */
	0x81, 0x01,       /*   Input (Const): reserved byte */
	0x05, 0x07,       /*   Usage Page (Key Codes) */
	0x19, 0x00,       /*   Usage Min (0) */
	0x29, 0xFF,       /*   Usage Max (255) */
	0x15, 0x00,       /*   Logical Min (0) */
	0x26, 0xFF, 0x00, /*   Logical Max (255) */
	0x75, 0x08,       /*   Report Size (8) */
	0x95, 0x06,       /*   Report Count (6) */
	0x81, 0x00,       /*   Input (Data, Array): 6 keycodes */
	0xC0,             /* End Collection */

	/* --- Mouse (relative), report ID 2 --- */
	0x05, 0x01,       /* Usage Page (Generic Desktop) */
	0x09, 0x02,       /* Usage (Mouse) */
	0xA1, 0x01,       /* Collection (Application) */
	0x85, REPORT_ID_MOUSE,
	0x09, 0x01,       /*   Usage (Pointer) */
	0xA1, 0x00,       /*   Collection (Physical) */
	0x05, 0x09,       /*     Usage Page (Buttons) */
	0x19, 0x01,       /*     Usage Min (1) */
	0x29, 0x03,       /*     Usage Max (3) */
	0x15, 0x00,       /*     Logical Min (0) */
	0x25, 0x01,       /*     Logical Max (1) */
	0x75, 0x01,       /*     Report Size (1) */
	0x95, 0x03,       /*     Report Count (3) */
	0x81, 0x02,       /*     Input (Data, Var, Abs): button bits */
	0x75, 0x05,       /*     Report Size (5) */
	0x95, 0x01,       /*     Report Count (1) */
	0x81, 0x01,       /*     Input (Const): padding */
	0x05, 0x01,       /*     Usage Page (Generic Desktop) */
	0x09, 0x30,       /*     Usage (X) */
	0x09, 0x31,       /*     Usage (Y) */
	0x09, 0x38,       /*     Usage (Wheel) */
	0x15, 0x81,       /*     Logical Min (-127) */
	0x25, 0x7F,       /*     Logical Max (127) */
	0x75, 0x08,       /*     Report Size (8) */
	0x95, 0x03,       /*     Report Count (3) */
	0x81, 0x06,       /*     Input (Data, Var, Rel): dx, dy, wheel */
	0xC0,             /*   End Collection */
	0xC0,             /* End Collection */
};

static const uint8_t report_map_abs[] = {
	/* --- Keyboard, report ID 1 --- */
	0x05, 0x01,
	0x09, 0x06,
	0xA1, 0x01,
	0x85, REPORT_ID_KEYBOARD,
	0x05, 0x07,
	0x19, 0xE0,
	0x29, 0xE7,
	0x15, 0x00,
	0x25, 0x01,
	0x75, 0x01,
	0x95, 0x08,
	0x81, 0x02,
	0x75, 0x08,
	0x95, 0x01,
	0x81, 0x01,
	0x05, 0x07,
	0x19, 0x00,
	0x29, 0xFF,
	0x15, 0x00,
	0x26, 0xFF, 0x00,
	0x75, 0x08,
	0x95, 0x06,
	0x81, 0x00,
	0xC0,

	/* --- Mouse (absolute), report ID 2 --- */
	0x05, 0x01,       /* Usage Page (Generic Desktop) */
	0x09, 0x02,       /* Usage (Mouse) */
	0xA1, 0x01,       /* Collection (Application) */
	0x85, REPORT_ID_MOUSE,
	0x09, 0x01,       /*   Usage (Pointer) */
	0xA1, 0x00,       /*   Collection (Physical) */
	0x05, 0x09,       /*     Usage Page (Buttons) */
	0x19, 0x01,       /*     Usage Min (1) */
	0x29, 0x03,       /*     Usage Max (3) */
	0x15, 0x00,       /*     Logical Min (0) */
	0x25, 0x01,       /*     Logical Max (1) */
	0x75, 0x01,       /*     Report Size (1) */
	0x95, 0x03,       /*     Report Count (3) */
	0x81, 0x02,       /*     Input (Data, Var, Abs): button bits */
	0x75, 0x05,       /*     Report Size (5) */
	0x95, 0x01,       /*     Report Count (1) */
	0x81, 0x01,       /*     Input (Const): padding */
	0x05, 0x01,       /*     Usage Page (Generic Desktop) */
	0x09, 0x30,       /*     Usage (X) */
	0x09, 0x31,       /*     Usage (Y) */
	0x15, 0x00,       /*     Logical Min (0) */
	0x26, 0xFF, 0x7F, /*     Logical Max (32767) */
	0x75, 0x10,       /*     Report Size (16) */
	0x95, 0x02,       /*     Report Count (2) */
	0x81, 0x02,       /*     Input (Data, Var, Abs): x, y (uint16 LE) */
	0x09, 0x38,       /*     Usage (Wheel) */
	0x15, 0x81,       /*     Logical Min (-127) */
	0x25, 0x7F,       /*     Logical Max (127) */
	0x75, 0x08,       /*     Report Size (8) */
	0x95, 0x01,       /*     Report Count (1) */
	0x81, 0x06,       /*     Input (Data, Var, Rel): wheel */
	0xC0,             /*   End Collection */
	0xC0,             /* End Collection */
};

/* HID service object.  Input report count = 2 (keyboard + one mouse). */
BT_HIDS_DEF(hids_obj, KEYBOARD_REPORT_LEN,
	    MOUSE_REL_REPORT_LEN > MOUSE_ABS_REPORT_LEN ?
	    MOUSE_REL_REPORT_LEN : MOUSE_ABS_REPORT_LEN);

static struct bt_conn *active_conn;
static bool hids_inited;
static bool mouse_abs_mode;

/* --- event handlers --------------------------------------------------- */

static void hids_pm_evt_handler(enum bt_hids_pm_evt evt, struct bt_conn *conn)
{
	switch (evt) {
	case BT_HIDS_PM_EVT_BOOT_MODE_ENTERED:
		LOG_INF("boot mode entered");
		break;
	case BT_HIDS_PM_EVT_REPORT_MODE_ENTERED:
		LOG_INF("report mode entered");
		break;
	default:
		break;
	}
}

static void hids_cp_evt_handler(enum bt_hids_cp_evt evt)
{
	switch (evt) {
	case BT_HIDS_CP_EVT_HOST_SUSP:
		LOG_INF("host suspended");
		break;
	case BT_HIDS_CP_EVT_HOST_EXIT_SUSP:
		LOG_INF("host resumed");
		break;
	default:
		break;
	}
}

/* --- public API ------------------------------------------------------- */

int anykvm_hids_init(bool abs_mode)
{
	struct bt_hids_init_param param = { 0 };

	if (hids_inited) {
		LOG_ERR("hids already initialised; call deinit first");
		return -EALREADY;
	}

	param.info.bcd_hid = 0x0111; /* HID 1.11 */
	param.info.b_country_code = 0x00;
	param.info.flags = BT_HIDS_REMOTE_WAKE | BT_HIDS_NORMALLY_CONNECTABLE;

	if (abs_mode) {
		param.rep_map.data = report_map_abs;
		param.rep_map.size = sizeof(report_map_abs);
		param.inp_rep_group_init.reports[INPUT_REP_IDX_MOUSE].size =
			MOUSE_ABS_REPORT_LEN;
	} else {
		param.rep_map.data = report_map_rel;
		param.rep_map.size = sizeof(report_map_rel);
		param.inp_rep_group_init.reports[INPUT_REP_IDX_MOUSE].size =
			MOUSE_REL_REPORT_LEN;
	}

	param.inp_rep_group_init.reports[INPUT_REP_IDX_KEYBOARD].size =
		KEYBOARD_REPORT_LEN;
	param.inp_rep_group_init.reports[INPUT_REP_IDX_KEYBOARD].id =
		REPORT_ID_KEYBOARD;
	param.inp_rep_group_init.reports[INPUT_REP_IDX_MOUSE].id =
		REPORT_ID_MOUSE;
	param.inp_rep_group_init.cnt = 2;

	param.pm_evt_handler = hids_pm_evt_handler;
	param.cp_evt_handler = hids_cp_evt_handler;

	int err = bt_hids_init(&hids_obj, &param);

	if (!err) {
		hids_inited = true;
		mouse_abs_mode = abs_mode;
	}
	return err;
}

/*
 * De-register the HID GATT service so the report map can be changed.
 *
 * Uses bt_hids_uninit() which is available in nRF Connect SDK v2.5+.
 * If your SDK version does not provide this function, replace the body
 * with bt_gatt_service_unregister() calls targeting the HID, BAS and DIS
 * services that bt_hids_init() registered.  See the HIDS module source
 * (subsys/bluetooth/services/hids.c) for the service pointer names.
 */
int anykvm_hids_deinit(void)
{
	if (!hids_inited) {
		return 0;
	}

	if (active_conn) {
		bt_hids_disconnected(&hids_obj, active_conn);
		active_conn = NULL;
	}

	bt_hids_uninit(&hids_obj);
	hids_inited = false;
	return 0;
}

void anykvm_hids_connected(struct bt_conn *conn)
{
	int err = bt_hids_connected(&hids_obj, conn);

	if (err) {
		LOG_ERR("hids connected: %d", err);
		return;
	}
	active_conn = conn;
}

void anykvm_hids_disconnected(struct bt_conn *conn)
{
	if (conn == active_conn) {
		active_conn = NULL;
	}
	bt_hids_disconnected(&hids_obj, conn);
}

int anykvm_hids_send_keyboard(const uint8_t report[8])
{
	if (active_conn == NULL) {
		return 0;
	}
	return bt_hids_inp_rep_send(&hids_obj, active_conn,
				    INPUT_REP_IDX_KEYBOARD, report,
				    KEYBOARD_REPORT_LEN, NULL);
}

int anykvm_hids_send_mouse(uint8_t buttons, int16_t dx_or_x,
			    int16_t dy_or_y, int8_t wheel)
{
	if (active_conn == NULL) {
		return 0;
	}

	if (mouse_abs_mode) {
		uint8_t report[MOUSE_ABS_REPORT_LEN] = {
			buttons,
			(uint8_t)((uint16_t)dx_or_x & 0xFF),
			(uint8_t)(((uint16_t)dx_or_x >> 8) & 0xFF),
			(uint8_t)((uint16_t)dy_or_y & 0xFF),
			(uint8_t)(((uint16_t)dy_or_y >> 8) & 0xFF),
			(uint8_t)(int8_t)wheel,
		};

		return bt_hids_inp_rep_send(&hids_obj, active_conn,
					    INPUT_REP_IDX_MOUSE, report,
					    MOUSE_ABS_REPORT_LEN, NULL);
	}

	/* relative mode */
	uint8_t report[MOUSE_REL_REPORT_LEN] = {
		buttons,
		(uint8_t)(int8_t)dx_or_x,
		(uint8_t)(int8_t)dy_or_y,
		(uint8_t)(int8_t)wheel,
	};

	return bt_hids_inp_rep_send(&hids_obj, active_conn,
				    INPUT_REP_IDX_MOUSE, report,
				    MOUSE_REL_REPORT_LEN, NULL);
}
