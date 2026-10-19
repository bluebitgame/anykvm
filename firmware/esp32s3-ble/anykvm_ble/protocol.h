#pragma once
#include <AnyKVMInput.h>

namespace anykvm {
inline uint16_t crc16(const uint8_t *p, size_t n) { return bk_crc16(p, n); }
inline void frame(uint8_t out[8], uint8_t cmd, const uint8_t d[4]) { bk_frame(out, cmd, d); }
class Parser {
 public:
  template <typename Handler> void feed(uint8_t b, Handler handle) {
    uint8_t cmd, data[4];
    if (bk_parse(&parser_, b, &cmd, data)) handle(cmd, data);
  }
  void reset() { parser_.used = 0; }
 private:
  bk_parser parser_ = {};
};
}
