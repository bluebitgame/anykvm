#pragma once
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>

typedef struct { uint8_t bytes[512]; unsigned read, write; } ps2_fifo;
typedef struct {
    ps2_fifo replies, events;
    bool mouse, enabled, remote, wrap, scaling;
    uint8_t pending, leds, typematic, resolution, rate, id, rates[3], last;
    uint8_t keys[8], repeat_key, buttons, sent_buttons;
    int32_t dx, dy, wheel;
    uint64_t repeat_at, sample_at;
} ps2_device;
void ps2_init(ps2_device *d, bool mouse);
void ps2_receive_error(ps2_device *d);
void ps2_command(ps2_device *d, uint8_t command);
// Peek/pop choose replies first; the bus calls pop only after a complete frame.
bool ps2_peek(ps2_device *d, uint8_t *byte, bool *reply);
void ps2_pop(ps2_device *d, bool reply);
bool ps2_keyboard(ps2_device *d, const uint8_t report[8], uint64_t now);
void ps2_mouse(ps2_device *d, const uint8_t report[4]);
void ps2_task(ps2_device *d, uint64_t now);
