#pragma once

#include <host/ble_hs.h>
#include <host/ble_store.h>
#include <stddef.h>
#include <string.h>

namespace anykvm {

constexpr uint8_t profileCount = 4;
constexpr uint8_t maxProfileRecords = 16;

inline bool sameAddress(const ble_addr_t &a, const ble_addr_t &b) {
    return (a.type & 1) == (b.type & 1) && memcmp(a.val, b.val, sizeof(a.val)) == 0;
}

struct ProfileRecord {
    uint8_t type;
    union ble_store_value value;
};

// Versioned, bounded blobs. Persist only the occupied part, not unused records.
// Pinning the core also pins the NimBLE security-record ABI.
struct ProfileData {
    uint32_t magic;
    uint16_t version, recordSize;
    uint8_t address[6];
    uint8_t absolute, count;
    ProfileRecord records[maxProfileRecords];

    void init(const uint8_t addr[6]) {
        memset(this, 0, sizeof(*this));
        magic = 0x424b5034;
        version = 1;
        recordSize = sizeof(ProfileRecord);
        memcpy(address, addr, sizeof(address));
    }
    size_t bytes() const {
        return offsetof(ProfileData, records) + count * sizeof(ProfileRecord);
    }
    bool valid(size_t size) const {
        return magic == 0x424b5034 && version == 1 && recordSize == sizeof(ProfileRecord) &&
               count <= maxProfileRecords && absolute <= 1 && (address[5] & 0xc0) == 0xc0 &&
               size == bytes();
    }
};

inline bool managedStoreType(int type) {
    return type == BLE_STORE_OBJ_TYPE_OUR_SEC || type == BLE_STORE_OBJ_TYPE_PEER_SEC ||
           type == BLE_STORE_OBJ_TYPE_CCCD || type == BLE_STORE_OBJ_TYPE_CSFC ||
           type == BLE_STORE_OBJ_TYPE_PEER_ADDR;
}

inline ble_addr_t recordPeer(const ProfileRecord &r) {
    switch (r.type) {
    case BLE_STORE_OBJ_TYPE_OUR_SEC:
    case BLE_STORE_OBJ_TYPE_PEER_SEC:
        return r.value.sec.peer_addr;
    case BLE_STORE_OBJ_TYPE_CCCD:
        return r.value.cccd.peer_addr;
    case BLE_STORE_OBJ_TYPE_CSFC:
        return r.value.csfc.peer_addr;
    default:
        return r.value.rpa_rec.peer_rpa_addr;
    }
}

inline void recordKey(const ProfileRecord &r, union ble_store_key &key) {
    memset(&key, 0, sizeof(key));
    switch (r.type) {
    case BLE_STORE_OBJ_TYPE_OUR_SEC:
    case BLE_STORE_OBJ_TYPE_PEER_SEC:
        key.sec.peer_addr = r.value.sec.peer_addr;
        break;
    case BLE_STORE_OBJ_TYPE_CCCD:
        key.cccd.peer_addr = r.value.cccd.peer_addr;
        key.cccd.chr_val_handle = r.value.cccd.chr_val_handle;
        break;
    case BLE_STORE_OBJ_TYPE_CSFC:
        key.csfc.peer_addr = r.value.csfc.peer_addr;
        break;
    default:
        key.rpa_rec.peer_rpa_addr = r.value.rpa_rec.peer_rpa_addr;
        break;
    }
}

inline bool matches(const ProfileRecord &r, const union ble_store_key &key, uint8_t &skip) {
    ble_addr_t address;
    switch (r.type) {
    case BLE_STORE_OBJ_TYPE_OUR_SEC:
    case BLE_STORE_OBJ_TYPE_PEER_SEC:
        address = key.sec.peer_addr;
        skip = key.sec.idx;
        break;
    case BLE_STORE_OBJ_TYPE_CCCD:
        address = key.cccd.peer_addr;
        skip = key.cccd.idx;
        if (key.cccd.chr_val_handle && key.cccd.chr_val_handle != r.value.cccd.chr_val_handle)
            return false;
        break;
    case BLE_STORE_OBJ_TYPE_CSFC:
        address = key.csfc.peer_addr;
        skip = key.csfc.idx;
        break;
    default:
        address = key.rpa_rec.peer_rpa_addr;
        skip = key.rpa_rec.idx;
        break;
    }
    const ble_addr_t any = {};
    return sameAddress(address, any) || sameAddress(address, recordPeer(r));
}

class ProfileStore {
  public:
    ProfileData data = {};
    bool (*persist)(void *, const ProfileData &) = nullptr;
    void *context = nullptr;
    bool transient = false;

    int read(int type, const union ble_store_key *key, union ble_store_value *value) const {
        if (!managedStoreType(type))
            return BLE_HS_ENOTSUP;
        int seen = 0;
        for (int i = 0; i < data.count; ++i) {
            uint8_t skip = 0;
            if (data.records[i].type == type && matches(data.records[i], *key, skip) &&
                seen++ == skip) {
                // The caller may pass a pointer to a smaller struct cast to this union.
                copyValue(type, *value, data.records[i].value);
                return 0;
            }
        }
        return BLE_HS_ENOENT;
    }
    int write(int type, const union ble_store_value *value) {
        if (!managedStoreType(type))
            return BLE_HS_ENOTSUP;
        ProfileRecord r = {};
        r.type = type;
        copyValue(type, r.value, *value);
        union ble_store_key key;
        recordKey(r, key);
        int index = find(type, key);
        bool append = index < 0;
        if (append && data.count == maxProfileRecords)
            return BLE_HS_ESTORE_CAP;
        if (append)
            index = data.count++;
        ProfileRecord old = data.records[index];
        if (!append && memcmp(&old, &r, sizeof(r)) == 0)
            return 0;
        data.records[index] = r;
        if (save())
            return 0;
        data.records[index] = old;
        if (append)
            --data.count;
        return BLE_HS_ESTORE_FAIL;
    }
    int erase(int type, const union ble_store_key *key) {
        if (!managedStoreType(type))
            return BLE_HS_ENOTSUP;
        int index = find(type, *key);
        if (index < 0)
            return BLE_HS_ENOENT;
        ProfileRecord old = data.records[index];
        memmove(data.records + index, data.records + index + 1,
                (--data.count - index) * sizeof(ProfileRecord));
        if (save())
            return 0;
        memmove(data.records + index + 1, data.records + index,
                (data.count - index) * sizeof(ProfileRecord));
        data.records[index] = old;
        ++data.count;
        return BLE_HS_ESTORE_FAIL;
    }
    bool bonded() const {
        for (int i = 0; i < data.count; ++i)
            if ((data.records[i].type == BLE_STORE_OBJ_TYPE_OUR_SEC ||
                 data.records[i].type == BLE_STORE_OBJ_TYPE_PEER_SEC) &&
                data.records[i].value.sec.ltk_present)
                return true;
        return false;
    }
    int peers(ble_addr_t *out, int capacity) const {
        int count = 0;
        for (int i = 0; i < data.count; ++i) {
            const auto &r = data.records[i];
            if (r.type != BLE_STORE_OBJ_TYPE_OUR_SEC && r.type != BLE_STORE_OBJ_TYPE_PEER_SEC)
                continue;
            bool duplicate = false;
            for (int j = 0; j < count; ++j)
                duplicate |= sameAddress(out[j], r.value.sec.peer_addr);
            if (!duplicate) {
                if (count == capacity)
                    return -1;
                out[count++] = r.value.sec.peer_addr;
            }
        }
        return count;
    }

  private:
    bool save() {
        return transient || (persist && persist(context, data));
    }
    int find(int type, const union ble_store_key &key) const {
        int seen = 0;
        for (int i = 0; i < data.count; ++i) {
            uint8_t skip = 0;
            if (data.records[i].type == type && matches(data.records[i], key, skip) &&
                seen++ == skip)
                return i;
        }
        return -1;
    }
    static void copyValue(int type, union ble_store_value &dst, const union ble_store_value &src) {
        switch (type) {
        case BLE_STORE_OBJ_TYPE_OUR_SEC:
        case BLE_STORE_OBJ_TYPE_PEER_SEC:
            dst.sec = src.sec;
            break;
        case BLE_STORE_OBJ_TYPE_CCCD:
            dst.cccd = src.cccd;
            break;
        case BLE_STORE_OBJ_TYPE_CSFC:
            dst.csfc = src.csfc;
            break;
        default:
            dst.rpa_rec = src.rpa_rec;
            break;
        }
    }
};

} // namespace anykvm
