#pragma once
#include "AnyKVMDevice.h"
namespace anykvm {
class Reports {
 public:
  virtual ~Reports() = default;
  virtual void keyboard(const uint8_t report[8]) = 0;
  virtual void mouse(bool absolute, const uint8_t *report, size_t size) = 0;
};
class Input {
 public:
  explicit Input(Reports &r) {
    state_.context = &r;
    state_.keyboard = [](void *c, const uint8_t *p) { static_cast<Reports *>(c)->keyboard(p); };
    state_.mouse = [](void *c, bool a, const uint8_t *p, size_t n) { static_cast<Reports *>(c)->mouse(a, p, n); };
  }
  bool absolute() const { return state_.absolute; }
  void reset() { bk_reset(&state_); }
  void release() { bk_release(&state_); }
  void handle(uint8_t cmd, const uint8_t *d) {
    if (cmd == 4) {
      if (d[0] != 8 && d[0] != 16) return;
      if (state_.absolute != (d[0] == 8)) { release(); state_.absolute = d[0] == 8; }
    } else bk_input_command(&state_, cmd, d);
  }
 private:
  bk_input state_ = {};
};
}
