#pragma once
#include <Arduino.h>
#include <BLEDevice.h>
#include <Preferences.h>
#include <atomic>
#include <nimble/nimble_port.h>
#include "profile_store.h"

namespace anykvm {

// Only the selected bank is visible to NimBLE. Inactive keys never occupy its
// three-entry default store or controller resolving list. All store mutations
// and transitions run on the NimBLE host task; main reads atomic snapshots.
class Profiles {
 public:
  bool begin(BLEServer *server, BLEAdvertising *advertising);
  uint8_t select(uint8_t slot);
  uint8_t clear(uint8_t slot);
  uint8_t advertise();
  uint8_t mode(bool absolute);
  uint8_t slot() const { return slot_.load(); }
  uint32_t generation() const { return generation_.load(); }
  bool absolute() const { return absolute_.load(); }
  bool bonded() const { return bonded_.load(); }
  bool busy() const { return pending_.load() || !healthy_.load(); }
  bool advertising() const { return ble_gap_adv_active() != 0; }
  bool accepts(const ble_addr_t &peer) const;

 private:
  enum Operation { Bootstrap, Select, Clear, Advertise, Mode };
  static Profiles *instance_;
  static int readStore(int, const union ble_store_key *, union ble_store_value *);
  static int writeStore(int, const union ble_store_value *);
  static int deleteStore(int, const union ble_store_key *);
  static void execute(struct ble_npl_event *);
  static bool persist(void *, const ProfileData &);
  bool readBank(uint8_t, ProfileData &);
  bool writeBank(uint8_t, const ProfileData &);
  bool detach();
  bool apply(const ProfileData &);
  uint8_t startAdvertising();
  uint8_t perform(Operation, uint8_t);
  uint8_t run(Operation, uint8_t, bool disconnect);
  void publish();

  Preferences preferences_;
  BLEServer *server_ = nullptr;
  BLEAdvertising *advertising_ = nullptr;
  ProfileStore store_;
  ble_store_read_fn *originalRead_ = nullptr;
  ble_store_write_fn *originalWrite_ = nullptr;
  ble_store_delete_fn *originalDelete_ = nullptr;
  struct ble_npl_event event_ = {};
  Operation operation_ = Bootstrap;
  uint8_t argument_ = 0;
  uint32_t request_ = 0, requestDeadline_ = 0;
  std::atomic<uint32_t> completed_{0}, generation_{0};
  std::atomic<uint8_t> slot_{0}, result_{0};
  std::atomic<bool> pending_{false}, transitioning_{false}, healthy_{false}, bonded_{false}, absolute_{false};
};

}  // namespace anykvm
