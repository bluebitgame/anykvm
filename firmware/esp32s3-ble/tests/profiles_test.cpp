#include "../anykvm_ble/profiles.h"
#include <AnyKVMProtocol.h>
#include <assert.h>
#include <algorithm>
#include <memory>

using namespace anykvm;
HostConfig ble_hs_cfg = {};
namespace fake {
bool active=false, failWrite=false, failAddress=false, failRestore=false, holdEvents=false;
unsigned connections=0, disconnects=0;
uint8_t address[6]={}, keyDistribution=0;
uint32_t clock=0;
std::vector<ble_addr_t> resolving, whitelist;
Blobs blobs;
std::vector<ble_npl_event *> queued;
void pump() {
  if (queued.empty()) return;
  auto *event=queued.front(); queued.erase(queued.begin()); event->function(event);
}
}
uint32_t millis() { return fake::clock; }
void delay(unsigned ms) { fake::clock+=ms; if (!fake::holdEvents) fake::pump(); }
void ble_npl_event_init(ble_npl_event *e, void (*f)(ble_npl_event *), void *c) { e->function=f; e->context=c; }
void *ble_npl_event_get_arg(ble_npl_event *e) { return e->context; }
ble_npl_eventq *nimble_port_get_dflt_eventq() { static ble_npl_eventq q; return &q; }
void ble_npl_eventq_put(ble_npl_eventq *, ble_npl_event *e) { fake::queued.push_back(e); }
int ble_gap_adv_active() { return fake::active; }
int ble_gap_wl_set(const ble_addr_t *p, uint8_t n) { fake::whitelist.assign(p,p+n); return 0; }
void esp_fill_random(void *p, size_t n) {
  static uint8_t seed=1; auto *out=static_cast<uint8_t *>(p);
  for (size_t i=0;i<n;++i) out[i]=seed++;
}
int ble_gap_unpair(const ble_addr_t *peer) {
  assert(!fake::active && fake::connections==0);
  fake::resolving.erase(std::remove_if(fake::resolving.begin(),fake::resolving.end(),
      [&](const ble_addr_t &a){return sameAddress(a,*peer);}),fake::resolving.end());
  for (int type : {BLE_STORE_OBJ_TYPE_OUR_SEC,BLE_STORE_OBJ_TYPE_PEER_SEC,
                   BLE_STORE_OBJ_TYPE_CCCD,BLE_STORE_OBJ_TYPE_CSFC,BLE_STORE_OBJ_TYPE_PEER_ADDR}) {
    ble_store_key key;memset(&key,0,sizeof(key));
    if (type==BLE_STORE_OBJ_TYPE_CCCD) key.cccd.peer_addr=*peer;
    else if (type==BLE_STORE_OBJ_TYPE_CSFC) key.csfc.peer_addr=*peer;
    else if (type==BLE_STORE_OBJ_TYPE_PEER_ADDR) key.rpa_rec.peer_rpa_addr=*peer;
    else key.sec.peer_addr=*peer;
    int rc;
    do {rc=ble_hs_cfg.store_delete_cb(type,&key);} while (rc==0);
    assert(rc==BLE_HS_ENOENT);
  }
  return 0;
}
int ble_store_write_peer_sec(const ble_store_value_sec *sec) {
  if (fake::failRestore) { fake::failRestore=false; return BLE_HS_ESTORE_FAIL; }
  ble_store_value value={}; value.sec=*sec;
  int rc=ble_hs_cfg.store_write_cb(BLE_STORE_OBJ_TYPE_PEER_SEC,&value);
  if (rc) return rc;
  bool found=false;
  for (const auto &a:fake::resolving) found|=sameAddress(a,sec->peer_addr);
  if (!found) fake::resolving.push_back(sec->peer_addr);
  return 0;
}

static ble_addr_t peer(uint8_t id) { return ble_addr_t{BLE_ADDR_RANDOM,{id,2,3,4,5,0xc0}}; }
static ProfileStore legacy;
static int originalRead(int t,const ble_store_key *k,ble_store_value *v){return legacy.read(t,k,v);}
static int originalWrite(int t,const ble_store_value *v){return legacy.write(t,v);}
static int originalDelete(int t,const ble_store_key *k){return legacy.erase(t,k);}
static void rebootCore() {
  fake::active=false; fake::connections=0; fake::resolving.clear(); fake::queued.clear();
  ble_hs_cfg={originalRead,originalWrite,originalDelete,nullptr};
  ble_addr_t peers[16];int n=legacy.peers(peers,16);
  fake::resolving.assign(peers,peers+n);
}
static void pair(uint8_t id,uint8_t secret) {
  ble_store_value value={};value.sec.peer_addr=peer(id);
  value.sec.ltk_present=true;value.sec.irk_present=true;
  memset(value.sec.ltk,secret,16);memset(value.sec.irk,secret+1,16);
  assert(ble_hs_cfg.store_write_cb(BLE_STORE_OBJ_TYPE_OUR_SEC,&value)==0);
  assert(ble_store_write_peer_sec(&value.sec)==0);
  value={};value.cccd.peer_addr=peer(id);value.cccd.chr_val_handle=42;value.cccd.flags=1;
  assert(ble_hs_cfg.store_write_cb(BLE_STORE_OBJ_TYPE_CCCD,&value)==0);
}
static void assertPair(uint8_t id,uint8_t secret) {
  ble_store_key key={};key.sec.peer_addr=peer(id);
  ble_store_value value={};
  assert(ble_hs_cfg.store_read_cb(BLE_STORE_OBJ_TYPE_PEER_SEC,&key,&value)==0);
  assert(value.sec.ltk[0]==secret && value.sec.irk[0]==uint8_t(secret+1));
  assert(fake::resolving.size()==1 && sameAddress(fake::resolving[0],peer(id)));
  memset(&key,0,sizeof(key));key.cccd.peer_addr=peer(id);key.cccd.chr_val_handle=42;
  assert(ble_hs_cfg.store_read_cb(BLE_STORE_OBJ_TYPE_CCCD,&key,&value)==0 && value.cccd.flags==1);
}

int main() {
  BLEServer server;BLEAdvertising advertising;
  // A previous single-profile installation must not leak keys into the new
  // slots, or lose its old on-disk records during bootstrap.
  legacy.transient=true;
  ble_store_value old={};old.sec.peer_addr=peer(99);old.sec.ltk_present=true;old.sec.irk_present=true;
  assert(legacy.write(BLE_STORE_OBJ_TYPE_PEER_SEC,&old)==0);
  rebootCore();
  Profiles profiles;
  assert(profiles.begin(&server,&advertising));
  assert(!profiles.bonded() && profiles.slot()==0 && fake::resolving.empty());
  assert(legacy.data.count==1);
  assert(fake::keyDistribution==ESP_BLE_ENC_KEY_MASK);
  std::vector<std::vector<uint8_t>> identities;
  for(uint8_t slot=0;slot<4;++slot) {
    assert(profiles.select(slot)==BK_OK);
    identities.emplace_back(fake::address,fake::address+6);
    assert(!profiles.bonded());
    pair(slot+1,10+slot);
    assert(profiles.bonded() && profiles.accepts(peer(slot+1)) && !profiles.accepts(peer(90)));
    assert(profiles.mode(slot%2)==BK_OK && profiles.absolute()==bool(slot%2));
  }
  for(size_t i=0;i<4;++i) for(size_t j=i+1;j<4;++j) assert(identities[i]!=identities[j]);
  // More than three hosts remain independently bonded, without increasing the
  // prebuilt stack's limit or loading inactive keys into the controller.
  for(uint8_t slot=0;slot<4;++slot) {
    fake::connections=1;
    assert(profiles.select(slot)==BK_OK);
    assert(fake::connections==0 && profiles.slot()==slot && profiles.absolute()==bool(slot%2));
    assertPair(slot+1,10+slot);
    assert(fake::whitelist.size()==1 && sameAddress(fake::whitelist[0],peer(slot+1)));
  }
  assert(profiles.select(4)==BK_INVALID_ARGUMENT && profiles.clear(4)==BK_INVALID_ARGUMENT);
  // Clearing an inactive bank must preserve the current connection and mode.
  fake::connections=1;unsigned before=fake::disconnects;
  assert(profiles.clear(1)==BK_OK && fake::connections==1 && fake::disconnects==before);
  assert(profiles.slot()==3 && profiles.absolute());assertPair(4,13);
  assert(profiles.select(1)==BK_OK && !profiles.bonded());
  assert(profiles.absolute()); // Clear retains the slot's mouse mode and identity.
  assert(std::vector<uint8_t>(fake::address,fake::address+6)==identities[1]);
  // The same physical peer in two slots must retain different keys.
  pair(1,91);
  assert(profiles.select(0)==BK_OK);assertPair(1,10);
  assert(profiles.select(1)==BK_OK);assertPair(1,91);
  auto committed=fake::blobs;
  fake::failWrite=true;
  // NimBLE passes smaller record structs cast as unions. Exercise the exact
  // pointer shape under ASan, and ensure a failed subscription write rolls back.
  ble_store_value_cccd subscription={peer(1),42,2};
  assert(ble_hs_cfg.store_write_cb(BLE_STORE_OBJ_TYPE_CCCD,
      reinterpret_cast<const ble_store_value *>(&subscription))==BLE_HS_ESTORE_FAIL);
  ble_store_key key;memset(&key,0,sizeof(key));key.cccd.peer_addr=peer(1);key.cccd.chr_val_handle=42;
  assert(ble_hs_cfg.store_read_cb(BLE_STORE_OBJ_TYPE_CCCD,&key,
      reinterpret_cast<ble_store_value *>(&subscription))==0 && subscription.flags==1);
  assert(profiles.mode(false)==BK_INTERNAL && profiles.absolute());
  assert(profiles.clear(0)==BK_INTERNAL);
  assert(profiles.clear(1)==BK_INTERNAL && profiles.bonded());assertPair(1,91);
  assert(fake::blobs==committed);fake::failWrite=false;
  fake::failAddress=true;
  assert(profiles.select(2)==BK_INTERNAL && profiles.slot()==1 && !profiles.busy());assertPair(1,91);
  fake::failRestore=true;
  assert(profiles.select(2)==BK_INTERNAL && profiles.slot()==1);assertPair(1,91);
  assert(fake::blobs==committed);
  // A timed-out host operation cannot borrow request storage from the next one.
  fake::holdEvents=true;
  assert(profiles.select(2)==BK_BUSY && profiles.busy());
  assert(profiles.select(3)==BK_BUSY);
  fake::holdEvents=false;fake::pump();
  assert(profiles.slot()==1 && !profiles.busy());assertPair(1,91);
  assert(profiles.select(2)==BK_OK);assertPair(3,12);
  // Simulate a power cycle: bank identities, keys, subscriptions and modes
  // survive, while active slot resets to zero as on nRF52840.
  rebootCore();
  Profiles restarted;assert(restarted.begin(&server,&advertising));
  assert(restarted.slot()==0 && restarted.bonded() && !restarted.absolute());
  for(uint8_t slot=0;slot<4;++slot) {
    assert(restarted.select(slot)==BK_OK);
    assert(std::vector<uint8_t>(fake::address,fake::address+6)==identities[slot]);
    assertPair(slot==1?1:slot+1,slot==1?91:10+slot);
  }
  assert(restarted.clear(3)==BK_OK && !restarted.bonded());
  assert(fake::resolving.empty() && fake::whitelist.empty());
  assert(restarted.select(2)==BK_OK);assertPair(3,12);
  // Corrupted banks are rejected without replacement or key deletion.
  auto corrupt=fake::blobs;fake::blobs["slot1"][0]^=1;auto expected=fake::blobs;
  rebootCore();Profiles broken;assert(!broken.begin(&server,&advertising));
  assert(fake::blobs==expected);fake::blobs=corrupt;
}
