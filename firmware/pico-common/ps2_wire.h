#pragma once
#include "ps2_protocol.h"
// Open-drain outputs: true means pull low, false means release to pull-up.
// Calling at <= 5 us intervals gives nominal 12.5 kHz PS/2 clock pulses.
typedef struct {
    ps2_device *device;
    bool clock_low, data_low, inhibited, reply, low_phase, valid;
    uint8_t state, bit, byte, parity;
    uint64_t next, inhibited_at;
    bool host_seen;
} ps2_wire;
void ps2_wire_tick(ps2_wire *w, uint64_t now, bool clock, bool data);
