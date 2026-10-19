#pragma once
#include "AnyKVMProtocol.h"

typedef struct {
    void *context;
    void (*keyboard)(void *, const uint8_t *);
    void (*mouse)(void *, bool, const uint8_t *, size_t);
    uint8_t keys[8], buttons;
    int32_t x, y;
    bool absolute, have_position;
} bk_input;

static inline int32_t bk_limit(int32_t n, int32_t lo, int32_t hi) {
    return n < lo ? lo : (n > hi ? hi : n);
}

static inline void bk_mouse(bk_input *s, int32_t dx, int32_t dy, int wheel) {
    if (s->absolute) {
        uint8_t r[6] = {s->buttons,    (uint8_t)s->x,        (uint8_t)(s->x >> 8),
                        (uint8_t)s->y, (uint8_t)(s->y >> 8), (uint8_t)wheel};
        s->mouse(s->context, true, r, 6);
    } else {
        do {
            int sx = bk_limit(dx, -127, 127), sy = bk_limit(dy, -127, 127);
            uint8_t r[4] = {s->buttons, (uint8_t)sx, (uint8_t)sy, (uint8_t)wheel};
            s->mouse(s->context, false, r, 4);
            dx -= sx;
            dy -= sy;
            wheel = 0;
        } while (dx || dy);
    }
}

static inline void bk_reset(bk_input *s) {
    memset(s->keys, 0, 8);
    s->buttons = 0;
    s->x = s->y = 0;
    s->have_position = false;
}

static inline void bk_release(bk_input *s) {
    memset(s->keys, 0, 8);
    s->buttons = 0;
    s->have_position = false;
    s->keyboard(s->context, s->keys);
    bk_mouse(s, 0, 0, 0);
}

static inline uint8_t bk_input_command(bk_input *s, uint8_t cmd, const uint8_t *d) {
    switch (cmd) {
    case 0x10:
        bk_release(s);
        break;
    case 0x11: {
        uint8_t code = d[0];
        if (d[1] > 1 || code < 4)
            return BK_INVALID_ARGUMENT;
        if (code >= 0xe0 && code <= 0xe7) {
            if (d[1])
                s->keys[0] |= 1 << (code - 0xe0);
            else
                s->keys[0] &= ~(1 << (code - 0xe0));
        } else {
            int found = -1, empty = -1;
            for (int i = 2; i < 8; ++i) {
                if (s->keys[i] == code)
                    found = i;
                if (s->keys[i] == 0 && empty < 0)
                    empty = i;
            }
            if (!d[1] && found >= 0)
                s->keys[found] = 0;
            if (d[1] && found < 0 && empty >= 0)
                s->keys[empty] = code;
        }
        s->keyboard(s->context, s->keys);
        break;
    }
    case 0x12: {
        int32_t x = (uint16_t)d[0] << 8 | d[1], y = (uint16_t)d[2] << 8 | d[3];
        if (x > 32767 || y > 32767)
            return BK_INVALID_ARGUMENT;
        int32_t dx = s->have_position ? x - s->x : 0;
        int32_t dy = s->have_position ? y - s->y : 0;
        s->x = x;
        s->y = y;
        s->have_position = true;
        bk_mouse(s, dx, dy, 0);
        break;
    }
    case 0x13:
        for (int i = 0; i < 3; ++i) {
            if (d[0] & (0x80 >> i)) {
                if (d[0] & (0x08 >> i))
                    s->buttons |= 1 << i;
                else
                    s->buttons &= ~(1 << i);
            }
        }
        bk_mouse(s, 0, 0, 0);
        break;
    case 0x14:
        bk_mouse(s, 0, 0, bk_limit((int8_t)d[1], -127, 127));
        break;
    case 0x15: {
        int dx = (int8_t)d[0], dy = (int8_t)d[1];
        s->x = bk_limit(s->x + dx, 0, 32767);
        s->y = bk_limit(s->y + dy, 0, 32767);
        s->have_position = true;
        bk_mouse(s, dx, dy, 0);
        break;
    }
    default:
        return BK_INVALID_COMMAND;
    }
    return BK_OK;
}

typedef struct {
    bk_input input;
    uint8_t capabilities, slots, model;
    void *context;
    void (*reply)(void *, uint8_t, const uint8_t *);
    uint8_t (*status)(void *);
    uint8_t (*mode)(void *, bool);
    uint8_t (*profile)(void *, uint8_t, uint8_t);
} bk_device;

static inline void bk_status(bk_device *dev) {
    uint8_t caps[4] = {BK_VERSION, dev->capabilities, dev->slots, dev->model};
    uint8_t ready[4] = {dev->slots, 0, 0, 0};
    uint8_t status[4] = {dev->status(dev->context), 0, 0, 0};
    if (dev->input.absolute)
        status[0] |= 0x20;
    dev->reply(dev->context, 0x83, caps);
    dev->reply(dev->context, 0x80, ready);
    dev->reply(dev->context, 0x81, status);
}

static inline void bk_command(bk_device *dev, uint8_t cmd, const uint8_t *d) {
    uint8_t result = BK_OK;
    switch (cmd) {
    case 0x01:
    case 0x05:
    case 0x22:
        bk_status(dev);
        break;
    case 0x04:
        if (d[0] != 0x08 && d[0] != 0x10)
            result = BK_INVALID_ARGUMENT;
        else if (!(dev->capabilities & (d[0] == 0x08 ? BK_ABSOLUTE : BK_RELATIVE)))
            result = BK_UNSUPPORTED;
        else if (dev->input.absolute != (d[0] == 0x08)) {
            bk_release(&dev->input);
            result = dev->mode(dev->context, d[0] == 0x08);
            if (!result)
                dev->input.absolute = d[0] == 0x08;
        }
        bk_status(dev);
        break;
    case BK_CMD_SELECT_TARGET:
        // Wire command reserved now; switching requires a combined-output
        // firmware and must release the old target before routing new input.
        result = BK_UNSUPPORTED;
        break;
    case 0x20:
    case 0x21:
    case 0x23:
        if (!(dev->capabilities & BK_PROFILES))
            result = BK_UNSUPPORTED;
        else if (cmd != 0x23 && d[0] >= dev->slots)
            result = BK_INVALID_ARGUMENT;
        else {
            bk_release(&dev->input);
            result = dev->profile(dev->context, cmd, d[0]);
        }
        bk_status(dev);
        break;
    default:
        result = bk_input_command(&dev->input, cmd, d);
        break;
    }
    uint8_t ack[4] = {cmd, result, 0, 0};
    dev->reply(dev->context, 0x84, ack);
}
