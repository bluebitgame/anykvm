#include "../anykvm_ble/protocol.h"
#include "../anykvm_ble/report_map.h"
#include <assert.h>
#include <vector>

struct Capture : anykvm::Reports {
  std::vector<uint8_t> keys;
  std::vector<std::vector<uint8_t>> mice;
  bool absolute = false;
  void keyboard(const uint8_t report[8]) override { keys.assign(report, report + 8); }
  void mouse(bool abs, const uint8_t *report, size_t size) override {
    absolute = abs;
    mice.emplace_back(report, report + size);
  }
};

int main() {
  assert(anykvm::crc16(reinterpret_cast<const uint8_t *>("123456789"), 9) == 0x4b37);
  anykvm::Parser parser;
  uint8_t bytes[8], data[4] = {0x33, 1, 0, 0};
  anykvm::frame(bytes, 0x11, data);
  int received = 0;
  auto handler = [&](uint8_t cmd, const uint8_t *payload) {
    assert(cmd == 0x11 && memcmp(payload, data, 4) == 0);
    ++received;
  };
  // Junk, a truncated frame, an invalid CRC, and a valid frame containing 0x33.
  for (uint8_t b : {0xff, 0x33, 0x22, 0x00}) parser.feed(b, handler);
  for (uint8_t b : bytes) parser.feed(b, handler);
  assert(received == 1);
  bytes[7] ^= 1;
  for (uint8_t b : bytes) parser.feed(b, handler);
  assert(received == 1);
  bytes[7] ^= 1;
  for (uint8_t b : bytes) parser.feed(b, handler);
  assert(received == 2);

  Capture capture;
  anykvm::Input input(capture);
  auto command = [&](uint8_t cmd, uint8_t a = 0, uint8_t b = 0,
                     uint8_t c = 0, uint8_t d = 0) {
    uint8_t payload[] = {a, b, c, d};
    input.handle(cmd, payload);
  };
  command(0x11, 0xe1, 1);
  command(0x11, 4, 1);
  command(0x11, 4, 1); // Duplicate press must not consume another key slot.
  assert(capture.keys[0] == 2 && capture.keys[2] == 4 && capture.keys[3] == 0);
  command(0x11, 4, 0);
  assert(capture.keys[0] == 2 && capture.keys[2] == 0);
  command(0x10);
  assert(capture.keys == std::vector<uint8_t>(8, 0));
  for (uint8_t key = 4; key < 11; ++key) command(0x11, key, 1);
  for (int i = 2; i < 8; ++i) assert(capture.keys[i] == i + 2);

  command(0x13, 0x88); // Left down, then right down, then left up.
  command(0x13, 0x44);
  assert(capture.mice.back()[0] == 3);
  command(0x13, 0x80);
  assert(capture.mice.back()[0] == 2);
  command(0x15, 0x80, 127); // -128 must split without losing distance.
  auto penultimate = capture.mice[capture.mice.size() - 2];
  auto last = capture.mice.back();
  assert(int8_t(penultimate[1]) + int8_t(last[1]) == -128);
  assert(int8_t(penultimate[2]) + int8_t(last[2]) == 127);
  command(0x14, 0, 0xfb);
  assert(int8_t(capture.mice.back()[3]) == -5);

  command(0x04, 0x08);
  command(0x12, 0x40, 0, 0x7f, 0xff);
  assert(capture.absolute && input.absolute());
  assert((capture.mice.back() == std::vector<uint8_t>{0, 0, 0x40, 0xff, 0x7f, 0}));
  command(0x13, 0x88);
  command(0x14, 0, 1);
  assert((capture.mice.back() == std::vector<uint8_t>{1, 0, 0x40, 0xff, 0x7f, 1}));
  input.release(); // Release held buttons without moving the absolute pointer.
  assert((capture.mice.back() == std::vector<uint8_t>{0, 0, 0x40, 0xff, 0x7f, 0}));
  command(0x04, 0xff); // Invalid mode must leave the report format unchanged.
  assert(input.absolute());
  command(0x04, 0x10);
  capture.mice.clear();
  command(0x12, 0, 100, 0, 100); // First position is an anchor.
  assert(capture.mice.back()[1] == 0 && capture.mice.back()[2] == 0);
  capture.mice.clear();
  command(0x12, 1, 144, 0, 0); // 300, -100 across multiple reports.
  int dx = 0, dy = 0;
  for (const auto &m : capture.mice) { dx += int8_t(m[1]); dy += int8_t(m[2]); }
  assert(dx == 300 && dy == -100);
  input.reset();
  command(0x12, 0, 100, 0, 100);
  assert(capture.mice.back()[1] == 0 && capture.mice.back()[2] == 0);

  // Validate declared input report sizes against the bytes sent by Input.
  int report = 0, size = 0, count = 0, bits[4] = {};
  for (size_t i = 0; i < sizeof(reportMap);) {
    uint8_t prefix = reportMap[i++];
    int n = prefix & 3;
    if (n == 3) n = 4;
    assert(i + n <= sizeof(reportMap));
    unsigned value = 0;
    for (int j = 0; j < n; ++j) value |= unsigned(reportMap[i++]) << (8 * j);
    if (prefix == 0x85) report = value;
    if (prefix == 0x75) size = value;
    if (prefix == 0x95) count = value;
    if ((prefix & 0xfc) == 0x80) {
      assert(report > 0 && report < 4);
      bits[report] += size * count;
    }
  }
  assert(bits[1] == 64 && bits[2] == 32 && bits[3] == 48);
}
