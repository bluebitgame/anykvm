#pragma once
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include <string.h>

enum {
  BK_VERSION = 1,
  // Reserved for future firmware exposing several physical target outputs.
  // Single-output firmware replies BK_UNSUPPORTED without changing input.
  BK_CMD_SELECT_TARGET = 0x24, // data[0] = logical physical-port index, not interface type
  BK_KEYBOARD = 1, BK_RELATIVE = 2, BK_ABSOLUTE = 4,
  BK_BLE = 8, BK_PROFILES = 16, BK_USB = 32, BK_PS2 = 64,
  BK_MODEL_PICO = 1, BK_MODEL_NRF52840 = 2, BK_MODEL_ESP32S3 = 3,
  BK_OK = 0, BK_INVALID_COMMAND = 1, BK_INVALID_ARGUMENT = 2,
  BK_UNSUPPORTED = 3, BK_BUSY = 4, BK_INTERNAL = 5
};

static inline uint16_t bk_crc16(const uint8_t *data, size_t size) {
  uint16_t crc = 0xffff;
  while (size--) {
    crc ^= *data++;
    for (int bit = 0; bit < 8; ++bit)
      crc = (crc >> 1) ^ ((crc & 1) ? 0xa001 : 0);
  }
  return crc;
}

static inline void bk_frame(uint8_t out[8], uint8_t cmd, const uint8_t data[4]) {
  out[0] = 0x33; out[1] = cmd;
  memcpy(out + 2, data, 4);
  uint16_t crc = bk_crc16(out, 6);
  out[6] = crc >> 8; out[7] = crc;
}

typedef struct { uint8_t bytes[8]; uint8_t used; } bk_parser;

// Sliding window: skip noise, bad CRCs and partial frames without losing sync.
static inline bool bk_parse(bk_parser *p, uint8_t byte, uint8_t *cmd, uint8_t data[4]) {
  p->bytes[p->used++] = byte;
  if (p->used != 8) return false;
  if (p->bytes[0] == 0x33 && bk_crc16(p->bytes, 6) ==
      ((uint16_t)p->bytes[6] << 8 | p->bytes[7])) {
    *cmd = p->bytes[1]; memcpy(data, p->bytes + 2, 4); p->used = 0;
    return true;
  }
  memmove(p->bytes, p->bytes + 1, 7); p->used = 7;
  return false;
}
