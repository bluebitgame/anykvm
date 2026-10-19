/*
 * AnyKVM nRF52840 firmware — multi-device profile manager.
 */
#include <zephyr/kernel.h>
#include <zephyr/logging/log.h>
#include <zephyr/settings/settings.h>
#include <zephyr/bluetooth/bluetooth.h>
#include <zephyr/bluetooth/conn.h>
#include <zephyr/bluetooth/hci.h>
#include <zephyr/bluetooth/uuid.h>

#include "profiles.h"
#include "hids.h"
#include "reply.h"
#include "led.h"

LOG_MODULE_REGISTER(anykvm_profiles, LOG_LEVEL_INF);

#define PROFILE_COUNT CONFIG_APP_PROFILE_COUNT

/* Maps profile slot -> Zephyr bt_id (BLE identity index). */
static uint8_t slot_id[PROFILE_COUNT];
static uint8_t active_slot;
static bool advertising;
static struct bt_conn *current_conn;

/* Per-slot mouse mode: 0 = relative, 1 = absolute. */
static uint8_t slot_mmode[PROFILE_COUNT];

/* Trace current HID mode so we can rebuild only when it actually changes. */
static bool current_hid_abs;

static const struct bt_data adv_ad[] = {
	BT_DATA_BYTES(BT_DATA_FLAGS, (BT_LE_AD_GENERAL | BT_LE_AD_NO_BREDR)),
	BT_DATA_BYTES(BT_DATA_UUID16_ALL,
		      BT_UUID_16_ENCODE(BT_UUID_HIDS_VAL),
		      BT_UUID_16_ENCODE(BT_UUID_BAS_VAL)),
	BT_DATA_BYTES(BT_DATA_GAP_APPEARANCE, 0xC1, 0x03), /* 961: keyboard */
	BT_DATA(BT_DATA_NAME_COMPLETE, CONFIG_BT_DEVICE_NAME,
		sizeof(CONFIG_BT_DEVICE_NAME) - 1),
};

/* --- settings persistence ---------------------------------------------- */

static int bkvm_set(const char *name, size_t len, settings_read_cb read_cb,
		    void *cb_arg)
{
	if (!settings_name_steq(name, "mmode", NULL)) {
		return -ENOENT;
	}

	if (len != PROFILE_COUNT) {
		return -EINVAL;
	}

	uint8_t buf[PROFILE_COUNT];

	if (read_cb(cb_arg, buf, len) != (ssize_t)len) {
		return -EINVAL;
	}
	memcpy(slot_mmode, buf, PROFILE_COUNT);
	return 0;
}

static int bkvm_commit(void)
{
	LOG_INF("settings committed");
	return 0;
}

SETTINGS_STATIC_HANDLER_DEFINE(bkvm_storage, "bkvm", NULL, bkvm_set,
			       bkvm_commit, NULL);

/* --- bond bookkeeping -------------------------------------------------- */

struct bond_scan {
	uint8_t id;
	bool found;
};

static void bond_scan_cb(const struct bt_bond_info *info, void *user_data)
{
	struct bond_scan *scan = user_data;

	(void)info;
	scan->found = true;
}

static bool slot_bonded(uint8_t slot)
{
	struct bond_scan scan = { .id = slot_id[slot], .found = false };

	bt_foreach_bond(slot_id[slot], bond_scan_cb, &scan);
	return scan.found;
}

/* --- advertising ------------------------------------------------------ */

static int adv_stop(void)
{
	if (!advertising) {
		return 0;
	}
	int err = bt_le_adv_stop();

	if (err && err != -EALREADY) {
		LOG_ERR("adv stop: %d", err);
		return err;
	}
	advertising = false;
	return 0;
}

static int adv_start_active(void)
{
	struct bt_le_adv_param param = *BT_LE_ADV_CONN;

	param.id = slot_id[active_slot];

	if (slot_bonded(active_slot)) {
		param.options |= BT_LE_ADV_OPT_FILTER_CONN |
				 BT_LE_ADV_OPT_FILTER_SCAN_REQ;
	}

	int err = bt_le_adv_start(&param, adv_ad, ARRAY_SIZE(adv_ad), NULL, 0);

	if (err == -EALREADY) {
		advertising = true;
		return 0;
	}
	if (err) {
		LOG_ERR("adv start (slot %u): %d", active_slot, err);
		return err;
	}
	advertising = true;
	led_set(false, true, slot_bonded(active_slot));
	LOG_INF("advertising slot %u (id %u, bonded=%d)", active_slot,
		slot_id[active_slot], slot_bonded(active_slot));
	return 0;
}

/*
 * Rebuild HID GATT service when the mouse mode changes.
 * The caller must have stopped advertising and disconnected first.
 */
static int hids_apply_mode(bool abs_mode)
{
	if (abs_mode == current_hid_abs) {
		return 0;
	}

	int err = anykvm_hids_deinit();

	if (err) {
		LOG_ERR("hids deinit: %d", err);
		return err;
	}

	err = anykvm_hids_init(abs_mode);
	if (err) {
		LOG_ERR("hids init abs=%d: %d", abs_mode, err);
		return err;
	}

	current_hid_abs = abs_mode;
	LOG_INF("HIDS rebuilt (mouse mode now %s)",
		abs_mode ? "absolute" : "relative");
	return 0;
}

/* --- connection callbacks --------------------------------------------- */

/* 7.5 ms interval (6 × 1.25 ms). Matches CONFIG_BT_PERIPHERAL_PREF_* in prj.conf. */
#define FAST_CONN_INTERVAL 6
#define FAST_CONN_TIMEOUT  400

static void request_fast_conn(struct bt_conn *conn)
{
	struct bt_le_conn_param param = {
		.interval_min = FAST_CONN_INTERVAL,
		.interval_max = FAST_CONN_INTERVAL,
		.latency = 0,
		.timeout = FAST_CONN_TIMEOUT,
	};

	int err = bt_conn_le_param_update(conn, &param);

	if (err && err != -EALREADY) {
		LOG_WRN("conn param update: %d", err);
	}
}

static void on_connected(struct bt_conn *conn, uint8_t err)
{
	if (err) {
		LOG_WRN("connect failed: %u", err);
		adv_start_active();
		return;
	}
	advertising = false;
	current_conn = bt_conn_ref(conn);

	/* iOS HID requires an encrypted bond; request L2 on connect. */
	int sec_err = bt_conn_set_security(conn, BT_SECURITY_L2);

	if (sec_err && sec_err != -EALREADY) {
		LOG_WRN("security request: %d", sec_err);
	}

	anykvm_hids_connected(conn);
	reply_send_event(REPLY_EVT_CONNECTED, active_slot);
	led_set(true, false, slot_bonded(active_slot));
	request_fast_conn(conn);
}

static void on_disconnected(struct bt_conn *conn, uint8_t reason)
{
	LOG_INF("disconnected (reason %u)", reason);
	anykvm_hids_disconnected(conn);
	if (current_conn) {
		bt_conn_unref(current_conn);
		current_conn = NULL;
	}
	reply_send_event(REPLY_EVT_DISCONNECTED, active_slot);
	adv_start_active();
}

static void on_security_changed(struct bt_conn *conn, bt_security_t level,
				enum bt_security_err err)
{
	if (err) {
		LOG_WRN("security failed (level %d, err %d)", level, err);
	} else {
		LOG_INF("security level %d", level);
		if (level >= BT_SECURITY_L2 && conn == current_conn) {
			request_fast_conn(conn);
		}
	}
}

BT_CONN_CB_DEFINE(conn_callbacks) = {
	.connected = on_connected,
	.disconnected = on_disconnected,
	.security_changed = on_security_changed,
};

/* --- SMP auth (headless dongle: auto-confirm if passkey pairing) ----- */

static void auth_passkey_display(struct bt_conn *conn, unsigned int passkey)
{
	char addr[BT_ADDR_LE_STR_LEN];

	bt_addr_le_to_str(bt_conn_get_dst(conn), addr, sizeof(addr));
	LOG_INF("pairing passkey %06u for %s", passkey, addr);
	bt_conn_auth_passkey_confirm(conn);
}

static void auth_cancel(struct bt_conn *conn)
{
	char addr[BT_ADDR_LE_STR_LEN];

	bt_addr_le_to_str(bt_conn_get_dst(conn), addr, sizeof(addr));
	LOG_WRN("pairing cancelled for %s", addr);
}

static struct bt_conn_auth_cb auth_cb = {
	.passkey_display = auth_passkey_display,
	.cancel = auth_cancel,
};

static void notify_status(void)
{
	struct profile_status st;

	anykvm_profiles_status(&st);
	reply_send_status(st.slot, st.connected, st.bonded, st.advertising,
			   st.abs_mouse);
	led_set(st.connected, st.advertising, st.bonded);
}

static void pairing_complete(struct bt_conn *conn, bool bonded)
{
	char addr[BT_ADDR_LE_STR_LEN];

	bt_addr_le_to_str(bt_conn_get_dst(conn), addr, sizeof(addr));
	LOG_INF("pairing complete %s bonded=%d", addr, bonded);
	if (bonded) {
		notify_status();
	}
}

static void pairing_failed(struct bt_conn *conn, enum bt_security_err reason)
{
	char addr[BT_ADDR_LE_STR_LEN];

	bt_addr_le_to_str(bt_conn_get_dst(conn), addr, sizeof(addr));
	LOG_WRN("pairing failed %s reason=%d", addr, reason);
}

static struct bt_conn_auth_info_cb auth_info_cb = {
	.pairing_complete = pairing_complete,
	.pairing_failed = pairing_failed,
};

/* --- public API ------------------------------------------------------- */

int anykvm_profiles_init(void)
{
	int err = bt_conn_auth_cb_register(&auth_cb);

	if (err) {
		LOG_ERR("auth cb register: %d", err);
		return err;
	}

	err = bt_conn_auth_info_cb_register(&auth_info_cb);
	if (err) {
		LOG_ERR("auth info cb register: %d", err);
		return err;
	}

	return 0;
}

int anykvm_profiles_start(bool abs_mode)
{
	int err;

	ARG_UNUSED(abs_mode);

	slot_id[0] = BT_ID_DEFAULT;
	for (uint8_t s = 1; s < PROFILE_COUNT; s++) {
		int id = bt_id_create(NULL, NULL);

		if (id < 0) {
			LOG_ERR("bt_id_create slot %u: %d", s, id);
			return id;
		}
		slot_id[s] = (uint8_t)id;
	}

	memset(slot_mmode, 0, sizeof(slot_mmode));

	if (IS_ENABLED(CONFIG_BT_SETTINGS)) {
		settings_load();
	}

	active_slot = 0;

	err = hids_apply_mode(slot_mmode[active_slot] != 0);
	if (err) {
		LOG_ERR("hids apply boot mode: %d", err);
		return err;
	}

	reply_send_ready(PROFILE_COUNT);
	LOG_INF("AnyKVM BLE HID ready (%d profiles)", PROFILE_COUNT);

	return adv_start_active();
}

int anykvm_profiles_select(uint8_t slot)
{
	if (slot >= PROFILE_COUNT) {
		return -EINVAL;
	}
	if (slot == active_slot && (current_conn || advertising)) {
		return 0;
	}

	adv_stop();
	if (current_conn) {
		bt_conn_disconnect(current_conn,
				   BT_HCI_ERR_REMOTE_USER_TERM_CONN);
		for (int i = 0; i < 50 && current_conn; i++) {
			k_msleep(10);
		}
	}

	if (current_conn) {
		return -EBUSY;
	}
	/* The disconnect callback may have restarted the previous identity. */
	adv_stop();
	active_slot = slot;
	LOG_INF("selected profile slot %u", slot);

	bool want_abs = slot_mmode[slot] != 0;

	if (want_abs != current_hid_abs) {
		int err = hids_apply_mode(want_abs);

		if (err) {
			LOG_ERR("hids apply mode: %d", err);
			return err;
		}
	}

	return adv_start_active();
}

int anykvm_profiles_clear(uint8_t slot)
{
	if (slot >= PROFILE_COUNT) {
		return -EINVAL;
	}
	if (slot == active_slot && current_conn) {
		adv_stop();
		bt_conn_disconnect(current_conn, BT_HCI_ERR_REMOTE_USER_TERM_CONN);
		for (int i = 0; i < 50 && current_conn; i++) {
			k_msleep(10);
		}
		if (current_conn) {
			return -EBUSY;
		}
	}

	int err = bt_unpair(slot_id[slot], BT_ADDR_LE_ANY);

	if (err) {
		LOG_ERR("unpair slot %u: %d", slot, err);
		return err;
	}
	LOG_INF("cleared bond for slot %u", slot);

	if (slot == active_slot) {
		adv_stop();
		return adv_start_active();
	}
	return 0;
}

int anykvm_profiles_advertise(void)
{
	return adv_start_active();
}

bool anykvm_profiles_mouse_mode(void)
{
	return current_hid_abs;
}

int anykvm_profiles_set_mouse_mode(bool abs_mode)
{
	if (abs_mode == current_hid_abs) {
		return 0;
	}

	slot_mmode[active_slot] = abs_mode ? 1 : 0;

	if (IS_ENABLED(CONFIG_BT_SETTINGS)) {
		settings_runtime_set("bkvm/mmode", slot_mmode, PROFILE_COUNT);
	}

	adv_stop();
	if (current_conn) {
		bt_conn_disconnect(current_conn,
				   BT_HCI_ERR_REMOTE_USER_TERM_CONN);
		for (int i = 0; i < 50 && current_conn; i++) {
			k_msleep(10);
		}
	}

	if (current_conn) {
		return -EBUSY;
	}
	adv_stop();
	int err = hids_apply_mode(abs_mode);

	if (err) {
		return err;
	}

	return adv_start_active();
}

void anykvm_profiles_status(struct profile_status *out)
{
	out->slot = active_slot;
	out->connected = current_conn != NULL;
	out->bonded = slot_bonded(active_slot);
	out->advertising = advertising;
	out->abs_mouse = current_hid_abs;
}
