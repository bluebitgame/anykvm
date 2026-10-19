#pragma once
// Deterministic host-task, controller and NVS substitutes for native tests.
// Arduino compilation separately verifies the real core's ABI and interfaces.
#include <stdint.h>
#include <stddef.h>
#include <string.h>
#include <map>
#include <string>
#include <vector>

enum { BLE_HS_ENOENT=5, BLE_HS_ENOTSUP=8, BLE_HS_ESTORE_CAP=27, BLE_HS_ESTORE_FAIL=28,
       BLE_ADDR_PUBLIC=0, BLE_ADDR_RANDOM=1, BLE_OWN_ADDR_RANDOM=1,
       BLE_STORE_OBJ_TYPE_OUR_SEC=1, BLE_STORE_OBJ_TYPE_PEER_SEC=2,
       BLE_STORE_OBJ_TYPE_CCCD=3, BLE_STORE_OBJ_TYPE_PEER_ADDR=6,
       BLE_STORE_OBJ_TYPE_LOCAL_IRK=7, BLE_STORE_OBJ_TYPE_CSFC=8,
       ESP_BLE_ENC_KEY_MASK=1, ESP_BLE_ID_KEY_MASK=2 };
struct ble_addr_t { uint8_t type; uint8_t val[6]; };
struct ble_store_key_sec { ble_addr_t peer_addr; uint8_t idx; };
struct ble_store_key_cccd { ble_addr_t peer_addr; uint16_t chr_val_handle; uint8_t idx; };
struct ble_store_key_csfc { ble_addr_t peer_addr; uint8_t idx; };
struct ble_store_key_rpa_rec { ble_addr_t peer_rpa_addr; uint8_t idx; };
struct ble_store_value_sec {
  ble_addr_t peer_addr; uint8_t ltk[16], irk[16]; bool ltk_present, irk_present;
};
struct ble_store_value_cccd { ble_addr_t peer_addr; uint16_t chr_val_handle, flags; };
struct ble_store_value_csfc { ble_addr_t peer_addr; uint8_t csfc[1]; };
struct ble_store_value_rpa_rec { ble_addr_t peer_rpa_addr, peer_addr; };
union ble_store_key {
  ble_store_key_sec sec; ble_store_key_cccd cccd; ble_store_key_csfc csfc;
  ble_store_key_rpa_rec rpa_rec;
};
union ble_store_value {
  ble_store_value_sec sec; ble_store_value_cccd cccd; ble_store_value_csfc csfc;
  ble_store_value_rpa_rec rpa_rec;
};
using ble_store_read_fn = int(int, const ble_store_key *, ble_store_value *);
using ble_store_write_fn = int(int, const ble_store_value *);
using ble_store_delete_fn = int(int, const ble_store_key *);
struct ble_store_status_event {};
struct HostConfig {
  ble_store_read_fn *store_read_cb;
  ble_store_write_fn *store_write_cb;
  ble_store_delete_fn *store_delete_cb;
  int (*store_status_cb)(ble_store_status_event *, void *);
};
extern HostConfig ble_hs_cfg;
struct ble_npl_event { void (*function)(ble_npl_event *); void *context; };
struct ble_npl_eventq {};
void ble_npl_event_init(ble_npl_event *, void (*)(ble_npl_event *), void *);
void *ble_npl_event_get_arg(ble_npl_event *);
ble_npl_eventq *nimble_port_get_dflt_eventq();
void ble_npl_eventq_put(ble_npl_eventq *, ble_npl_event *);
uint32_t millis();
void delay(unsigned);
int ble_gap_adv_active();
int ble_gap_wl_set(const ble_addr_t *, uint8_t);
int ble_gap_unpair(const ble_addr_t *);
int ble_store_write_peer_sec(const ble_store_value_sec *);
void esp_fill_random(void *, size_t);

namespace fake {
extern bool active, failWrite, failAddress, failRestore, holdEvents;
extern unsigned connections, disconnects;
extern uint8_t address[6], keyDistribution;
extern std::vector<ble_addr_t> resolving, whitelist;
using Blobs = std::map<std::string, std::vector<uint8_t>>;
extern Blobs blobs;
void pump();
}
class Preferences {
 public:
  bool begin(const char *name, bool) { return strlen(name) <= 15; }
  bool isKey(const char *key) { return fake::blobs.count(key); }
  size_t getBytesLength(const char *key) { return fake::blobs[key].size(); }
  size_t getBytes(const char *key, void *data, size_t size) {
    if (size != fake::blobs[key].size()) return 0;
    memcpy(data, fake::blobs[key].data(), size); return size;
  }
  size_t putBytes(const char *key, const void *data, size_t size) {
    if (fake::failWrite) return 0;
    const auto *p = static_cast<const uint8_t *>(data);
    fake::blobs[key] = std::vector<uint8_t>(p, p+size); return size;
  }
};
class BLEServer {
 public:
  unsigned getConnectedCount() { return fake::connections; }
  unsigned getConnId() { return 1; }
  void disconnect(unsigned) { ++fake::disconnects; fake::connections = 0; }
};
class BLEAdvertising {
 public:
  bool stop() { fake::active=false; return true; }
  bool start() { fake::active=true; return true; }
  void setScanFilter(bool, bool) {}
};
class BLEDevice {
 public:
  static bool setOwnAddr(uint8_t *p) {
    if (fake::failAddress) { fake::failAddress=false; return false; }
    if (fake::active || fake::connections) return false;
    memcpy(fake::address,p,6); return true;
  }
  static bool setOwnAddrType(uint8_t) { return true; }
};
class BLESecurity {
 public:
  static void setInitEncryptionKey(uint8_t key) { fake::keyDistribution=key; }
  static void setRespEncryptionKey(uint8_t) {}
};
