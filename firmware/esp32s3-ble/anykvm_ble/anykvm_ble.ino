// XIAO ESP32-S3: USB CDC -> bonded BLE HID keyboard and mouse.
// Built with Espressif Arduino core 3.3.2, NimBLE, USB CDC on boot.
#include <Arduino.h>
#include <BLEDevice.h>
#include <BLEHIDDevice.h>
#include <BLESecurity.h>
#include <host/ble_store.h>
#include <atomic>
#include "protocol.h"
#include "report_map.h"
#include "profiles.h"

#if !defined(CONFIG_IDF_TARGET_ESP32S3) || !ARDUINO_USB_CDC_ON_BOOT
#error Select XIAO_ESP32S3 and enable USB CDC On Boot.
#endif
#if !defined(CONFIG_NIMBLE_ENABLED)
#error This sketch requires the bundled NimBLE BLE library.
#endif

static BLEServer *server;
static BLEAdvertising *advertising;
static BLECharacteristic *keyboardReport, *relativeReport, *absoluteReport;
static std::atomic<bool> connected{false}, authenticated{false};
struct ConnectionEvent { uint8_t kind, slot; uint32_t generation; };
static QueueHandle_t events;
static anykvm::Profiles profiles;
static uint32_t advertiseAfter = 0;
static bool wasSerialOpen = false;
static anykvm::Parser parser;

static bk_device device;
static void sendKeyboard(void *, const uint8_t *report) {
  if (!authenticated.load()) return;
  keyboardReport->setValue(const_cast<uint8_t *>(report), 8);
  keyboardReport->notify();
}
static void sendMouse(void *, bool absolute, const uint8_t *report, size_t size) {
  if (!authenticated.load()) return;
  BLECharacteristic *c = absolute ? absoluteReport : relativeReport;
  c->setValue(const_cast<uint8_t *>(report), size);
  c->notify();
  delay(8);
}

static void reply(uint8_t cmd, uint8_t a = 0, uint8_t b = 0) {
  if (!Serial) return;
  uint8_t data[4] = {a, b, 0, 0}, bytes[8];
  anykvm::frame(bytes, cmd, data);
  Serial.write(bytes, sizeof(bytes));
}

static void sendReply(void *, uint8_t cmd, const uint8_t *data) {
  if (!Serial) return;
  uint8_t bytes[8];
  bk_frame(bytes, cmd, data);
  Serial.write(bytes, sizeof(bytes));
}
static uint8_t deviceStatus(void *) {
  return profiles.slot() | (connected.load() ? 0x04 : 0) |
         (profiles.bonded() ? 0x08 : 0) | (profiles.advertising() ? 0x10 : 0);
}
static void status() { bk_status(&device); }
static void queueEvent(uint8_t kind) {
  ConnectionEvent event{kind, profiles.slot(), profiles.generation()};
  xQueueSend(events, &event, portMAX_DELAY);
}

// BLE callbacks run on another task. Keep serial and input state in loop().
class ServerCallbacks : public BLEServerCallbacks {
  void onConnect(BLEServer *, ble_gap_conn_desc *desc) override {
    if (!profiles.accepts(desc->peer_id_addr)) {
      server->disconnect(desc->conn_handle);
      return;
    }
    authenticated.store(false);
    connected.store(true);
    queueEvent(1);
    BLESecurity::startSecurity(desc->conn_handle);
  }
  void onDisconnect(BLEServer *) override {
    authenticated.store(false);
    connected.store(false);
    queueEvent(2);
  }
};

class SecurityCallbacks : public BLESecurityCallbacks {
  bool onSecurityRequest() override { return true; }
  void onAuthenticationComplete(ble_gap_conn_desc *desc) override {
    // Bond keys may finish persisting after the encryption callback.
    bool success = desc->sec_state.encrypted && connected.load() && profiles.accepts(desc->peer_id_addr);
    authenticated.store(success);
    queueEvent(success ? 3 : 4);
  }
};

static uint8_t setMode(void *, bool absolute) { return profiles.mode(absolute); }
static uint8_t profile(void *, uint8_t cmd, uint8_t slot) {
  uint8_t result = cmd == 0x20 ? profiles.select(slot) :
                   cmd == 0x21 ? profiles.clear(slot) : profiles.advertise();
  device.input.absolute = profiles.absolute();
  return result;
}
static void handle(uint8_t cmd, const uint8_t *data) {
  if (cmd >= 0x11 && cmd <= 0x15 && (profiles.busy() || !authenticated.load())) {
    uint8_t ack[4] = {cmd, BK_BUSY, 0, 0};
    sendReply(nullptr, 0x84, ack);
    return;
  }
  bk_command(&device, cmd, data);
}

void setup() {
  device.capabilities = BK_KEYBOARD | BK_RELATIVE | BK_ABSOLUTE | BK_BLE | BK_PROFILES;
  device.slots = anykvm::profileCount;
  device.model = BK_MODEL_ESP32S3;
  device.reply = sendReply;
  device.status = deviceStatus;
  device.mode = setMode;
  device.profile = profile;
  device.input.keyboard = sendKeyboard;
  device.input.mouse = sendMouse;
  Serial.begin(115200);
  Serial.setDebugOutput(false);
  events = xQueueCreate(16, sizeof(ConnectionEvent));
  if (!events) abort();
  BLEDevice::init("AnyKVM");
  BLEDevice::setSecurityCallbacks(new SecurityCallbacks());
  server = BLEDevice::createServer();
  server->setCallbacks(new ServerCallbacks());
  server->advertiseOnDisconnect(false);
  auto *hid = new BLEHIDDevice(server);
  keyboardReport = hid->inputReport(1);
  hid->outputReport(1); // Accept keyboard LED output from the host.
  relativeReport = hid->inputReport(2);
  absoluteReport = hid->inputReport(3);
  hid->manufacturer()->setValue("AnyKVM");
  hid->hidInfo(0, 0x02);
  hid->reportMap(reportMap, sizeof(reportMap));
  auto *security = new BLESecurity();
  security->setAuthenticationMode(ESP_LE_AUTH_REQ_SC_BOND);
  security->setCapability(ESP_IO_CAP_NONE);
  security->setInitEncryptionKey(ESP_BLE_ENC_KEY_MASK | ESP_BLE_ID_KEY_MASK);
  security->setRespEncryptionKey(ESP_BLE_ENC_KEY_MASK | ESP_BLE_ID_KEY_MASK);
  hid->startServices();
  hid->setBatteryLevel(100);
  advertising = server->getAdvertising();
  advertising->setAppearance(HID_KEYBOARD);
  advertising->addServiceUUID(hid->hidService()->getUUID());
  advertising->setScanResponse(true);
  if (!profiles.begin(server, advertising)) abort();
  device.input.absolute = profiles.absolute();
}

void loop() {
  ConnectionEvent event;
  while (xQueueReceive(events, &event, 0) == pdTRUE) {
    // Old-session events retain their source slot, and cannot reset or
    // disconnect a newer session after a profile transition.
    if (event.kind == 2) reply(0x82, 2, event.slot);
    if (event.generation != profiles.generation() || event.slot != profiles.slot()) continue;
    bk_reset(&device.input);
    if (event.kind == 2) advertiseAfter = millis() + 100;
    if (event.kind == 3) {
      bk_release(&device.input);
      reply(0x82, 1, event.slot);
    }
    if (event.kind == 4 && connected.load()) server->disconnect(server->getConnId());
    status();
  }
  static uint32_t generation = 0;
  if (generation != profiles.generation() || device.input.absolute != profiles.absolute()) {
    generation = profiles.generation();
    bk_reset(&device.input);
    device.input.absolute = profiles.absolute();
    status();
  }
  if (!connected.load() && !profiles.busy() && !profiles.advertising() &&
      int32_t(millis() - advertiseAfter) >= 0) {
    profiles.advertise();
    advertiseAfter = millis() + 500;
  }

  bool serialOpen = bool(Serial);
  if (serialOpen != wasSerialOpen) {
    if (wasSerialOpen) bk_release(&device.input);
    parser.reset();
    if (serialOpen) status();
    wasSerialOpen = serialOpen;
  }
  // A budget keeps connection events and USB close handling responsive.
  for (int budget = 0; serialOpen && Serial.available() && budget < 256; ++budget)
    parser.feed(uint8_t(Serial.read()), handle);
  delay(1);
}
