#include "profiles.h"
#include <BLESecurity.h>
#include <esp_random.h>
#include <memory>
#include <AnyKVMProtocol.h>

namespace anykvm {
Profiles *Profiles::instance_ = nullptr;

bool Profiles::readBank(uint8_t slot, ProfileData &out) {
    char key[] = "slot0";
    key[4] += slot;
    size_t length = preferences_.getBytesLength(key);
    memset(&out, 0, sizeof(out));
    return length >= offsetof(ProfileData, records) && length <= sizeof(out) &&
           preferences_.getBytes(key, &out, length) == length && out.valid(length);
}
bool Profiles::writeBank(uint8_t slot, const ProfileData &data) {
    char key[] = "slot0";
    key[4] += slot;
    return preferences_.putBytes(key, &data, data.bytes()) == data.bytes();
}
bool Profiles::persist(void *context, const ProfileData &data) {
    auto *self = static_cast<Profiles *>(context);
    return self->writeBank(self->slot(), data);
}
void Profiles::publish() {
    bonded_.store(store_.bonded());
    absolute_.store(store_.data.absolute != 0);
}

int Profiles::readStore(int type, const union ble_store_key *key, union ble_store_value *value) {
    auto &self = *instance_;
    // The stack's global local IRK remains private: profiles use static random
    // identities and do not distribute that IRK to peers.
    if (type == BLE_STORE_OBJ_TYPE_LOCAL_IRK)
        return self.originalRead_ ? self.originalRead_(type, key, value) : BLE_HS_ENOENT;
    return self.store_.read(type, key, value);
}
int Profiles::writeStore(int type, const union ble_store_value *value) {
    auto &self = *instance_;
    if (type == BLE_STORE_OBJ_TYPE_LOCAL_IRK)
        return self.originalWrite_ ? self.originalWrite_(type, value) : BLE_HS_ENOTSUP;
    int rc = self.store_.write(type, value);
    self.publish();
    return rc;
}
int Profiles::deleteStore(int type, const union ble_store_key *key) {
    auto &self = *instance_;
    if (type == BLE_STORE_OBJ_TYPE_LOCAL_IRK)
        return self.originalDelete_ ? self.originalDelete_(type, key) : BLE_HS_ENOTSUP;
    int rc = self.store_.erase(type, key);
    self.publish();
    return rc;
}

bool Profiles::begin(BLEServer *server, BLEAdvertising *advertising) {
    server_ = server;
    advertising_ = advertising;
    if (!preferences_.begin("bkvm-profiles", false))
        return false;
    auto data = std::unique_ptr<ProfileData>(new ProfileData{});
    uint8_t addresses[profileCount][6] = {};
    for (uint8_t slot = 0; slot < profileCount; ++slot) {
        char key[] = "slot0";
        key[4] += slot;
        if (!preferences_.isKey(key)) {
            uint8_t address[6];
            bool duplicate;
            do {
                esp_fill_random(address, sizeof(address));
                address[5] |= 0xc0;
                duplicate = false;
                for (int i = 0; i < slot; ++i)
                    duplicate |= memcmp(address, addresses[i], 6) == 0;
            } while (duplicate);
            data->init(address);
            if (!writeBank(slot, *data))
                return false;
        }
        // Never silently overwrite an unreadable or incompatible bond bank.
        if (!readBank(slot, *data))
            return false;
        memcpy(addresses[slot], data->address, 6);
    }
    ble_npl_event_init(&event_, execute, this);
    store_.context = this;
    store_.persist = persist;
    instance_ = this;
    return run(Bootstrap, 0, true) == BK_OK;
}

// Run after BLE disconnect callbacks and CCCD flushes, on the host event queue.
// A timed-out request owns its storage until completed; no stack references
// survive a timeout and a second request cannot overwrite an active operation.
uint8_t Profiles::run(Operation op, uint8_t arg, bool disconnect) {
    bool expected = false;
    if (!pending_.compare_exchange_strong(expected, true))
        return BK_BUSY;
    transitioning_.store(disconnect);
    if (disconnect) {
        if (ble_gap_adv_active() && !advertising_->stop()) {
            transitioning_.store(false);
            pending_.store(false);
            return BK_INTERNAL;
        }
        if (server_->getConnectedCount())
            server_->disconnect(server_->getConnId());
        uint32_t deadline = millis() + 1000;
        while (server_->getConnectedCount() && int32_t(millis() - deadline) < 0)
            delay(1);
        if (server_->getConnectedCount()) {
            transitioning_.store(false);
            pending_.store(false);
            return BK_BUSY;
        }
    }
    operation_ = op;
    argument_ = arg;
    uint32_t id = ++request_;
    requestDeadline_ = millis() + 1800;
    ble_npl_eventq_put(nimble_port_get_dflt_eventq(), &event_);
    uint32_t deadline = requestDeadline_;
    while (completed_.load() != id && int32_t(millis() - deadline) < 0)
        delay(1);
    return completed_.load() == id ? result_.load() : uint8_t(BK_BUSY);
}
void Profiles::execute(struct ble_npl_event *event) {
    auto &self = *static_cast<Profiles *>(ble_npl_event_get_arg(event));
    uint32_t id = self.request_;
    // A request that expired in the queue must not switch devices later.
    self.result_.store(int32_t(millis() - self.requestDeadline_) >= 0
                           ? uint8_t(BK_BUSY)
                           : self.perform(self.operation_, self.argument_));
    self.transitioning_.store(false);
    self.completed_.store(id);
    self.pending_.store(false);
}

bool Profiles::detach() {
    ble_addr_t peers[maxProfileRecords];
    int count = store_.peers(peers, maxProfileRecords);
    if (count < 0)
        return false;
    // Deletion operates on a transient copy. The slot's committed NVS keys are
    // unchanged, even if power is lost halfway through a switch.
    store_.transient = true;
    for (int i = 0; i < count; ++i)
        if (ble_gap_unpair(&peers[i]) != 0)
            return false;
    store_.data.count = 0;
    return true;
}
bool Profiles::apply(const ProfileData &data) {
    store_.transient = true;
    store_.data = data;
    if (!BLEDevice::setOwnAddr(store_.data.address) ||
        !BLEDevice::setOwnAddrType(BLE_OWN_ADDR_RANDOM))
        return false;
    // Use independent stable identities. Sharing the stack's one global IRK
    // across slots would let peers mistake separate identities for one device.
    BLESecurity::setInitEncryptionKey(ESP_BLE_ENC_KEY_MASK);
    BLESecurity::setRespEncryptionKey(ESP_BLE_ENC_KEY_MASK | ESP_BLE_ID_KEY_MASK);
    for (int i = 0; i < store_.data.count; ++i) {
        ProfileRecord r = store_.data.records[i];
        if (r.type == BLE_STORE_OBJ_TYPE_PEER_SEC && r.value.sec.irk_present &&
            ble_store_write_peer_sec(&r.value.sec) != 0)
            return false;
    }
    store_.transient = false;
    healthy_.store(true);
    publish();
    return true;
}

uint8_t Profiles::startAdvertising() {
    if (!healthy_.load())
        return BK_INTERNAL;
    if (server_->getConnectedCount() || ble_gap_adv_active())
        return BK_OK;
    ble_addr_t peers[maxProfileRecords];
    int count = store_.peers(peers, maxProfileRecords);
    if (count < 0 || count > 1 || ble_gap_wl_set(peers, count) != 0)
        return BK_INTERNAL;
    advertising_->setScanFilter(bonded(), bonded());
    return advertising_->start() ? BK_OK : BK_INTERNAL;
}
bool Profiles::accepts(const ble_addr_t &peer) const {
    if (transitioning_.load() || !healthy_.load())
        return false;
    if (!store_.bonded())
        return true;
    ble_addr_t peers[maxProfileRecords];
    int count = store_.peers(peers, maxProfileRecords);
    return count == 1 && sameAddress(peer, peers[0]);
}

uint8_t Profiles::perform(Operation op, uint8_t arg) {
    if ((op == Bootstrap || op == Select || (op == Clear && arg == slot())) &&
        server_->getConnectedCount())
        return BK_BUSY;
    if (op == Bootstrap) {
        // The old single-profile firmware used NimBLE's default store. Copy just
        // its peer keys into scratch storage to remove boot-restored controller
        // entries without deleting that old NVS namespace.
        originalRead_ = ble_hs_cfg.store_read_cb;
        originalWrite_ = ble_hs_cfg.store_write_cb;
        originalDelete_ = ble_hs_cfg.store_delete_cb;
        store_.transient = true;
        if (originalRead_) {
            for (int idx = 0; idx < maxProfileRecords; ++idx) {
                union ble_store_key key = {};
                key.sec.idx = idx;
                union ble_store_value value = {};
                int rc = originalRead_(BLE_STORE_OBJ_TYPE_PEER_SEC, &key, &value);
                if (rc == BLE_HS_ENOENT)
                    break;
                if (rc || store_.write(BLE_STORE_OBJ_TYPE_PEER_SEC, &value))
                    return BK_INTERNAL;
            }
        }
        ble_hs_cfg.store_read_cb = readStore;
        ble_hs_cfg.store_write_cb = writeStore;
        ble_hs_cfg.store_delete_cb = deleteStore;
        // Never evict another profile or the current host on a storage overflow.
        ble_hs_cfg.store_status_cb = [](struct ble_store_status_event *, void *) -> int {
            return BLE_HS_ESTORE_CAP;
        };
    }
    if (op == Advertise)
        return startAdvertising();
    if (op == Mode) {
        uint8_t previous = store_.data.absolute;
        store_.data.absolute = arg;
        if (!writeBank(slot(), store_.data)) {
            store_.data.absolute = previous;
            return BK_INTERNAL;
        }
        publish();
        return BK_OK;
    }
    auto target = std::unique_ptr<ProfileData>(new ProfileData{});
    if (!readBank(arg, *target))
        return BK_INTERNAL;
    if (op == Clear)
        target->count = 0;
    if (op == Clear && arg != slot())
        return writeBank(arg, *target) ? BK_OK : BK_INTERNAL;
    auto previous = std::unique_ptr<ProfileData>(new ProfileData(store_.data));
    healthy_.store(false);
    if (!detach() || !apply(*target) || (op == Clear && !writeBank(arg, *target))) {
        // Remove any partially restored target peer before restoring the old bank.
        detach();
        store_.transient = false;
        bool recovered = apply(*previous);
        healthy_.store(recovered);
        return BK_INTERNAL;
    }
    slot_.store(arg);
    generation_.fetch_add(1);
    publish();
    uint8_t result = startAdvertising();
    return result;
}

uint8_t Profiles::select(uint8_t slot) {
    if (slot >= profileCount)
        return BK_INVALID_ARGUMENT;
    if (slot == this->slot() && !busy())
        return advertise();
    return run(Select, slot, true);
}
uint8_t Profiles::clear(uint8_t slot) {
    if (slot >= profileCount)
        return BK_INVALID_ARGUMENT;
    return run(Clear, slot, slot == this->slot());
}
uint8_t Profiles::advertise() {
    return run(Advertise, 0, false);
}
uint8_t Profiles::mode(bool absolute) {
    return run(Mode, absolute ? 1 : 0, false);
}
} // namespace anykvm
