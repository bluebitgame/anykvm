/* PS/2 device commands and HID -> scan set 2. No Pico dependencies. */
#include <string.h>
#include "ps2_protocol.h"

static unsigned count(const ps2_fifo *q) { return q->write - q->read; }
static void put(ps2_fifo *q, uint8_t byte) { q->bytes[q->write++ % sizeof(q->bytes)] = byte; }
static uint8_t front(ps2_fifo *q) { return q->bytes[q->read % sizeof(q->bytes)]; }
static void clear(ps2_fifo *q) { q->read = q->write = 0; }
static void reply(ps2_device *d, uint8_t byte) {
    if (count(&d->replies) < sizeof(d->replies.bytes)) put(&d->replies, byte);
}
void ps2_receive_error(ps2_device *d) { reply(d, 0xfe); }
bool ps2_peek(ps2_device *d, uint8_t *byte, bool *is_reply) {
    *is_reply = count(&d->replies) != 0;
    ps2_fifo *q = *is_reply ? &d->replies : &d->events;
    if (!count(q)) return false;
    *byte = front(q); return true;
}
void ps2_pop(ps2_device *d, bool is_reply) {
    ps2_fifo *q = is_reply ? &d->replies : &d->events;
    if (count(q)) { d->last = front(q); ++q->read; }
}
static void defaults(ps2_device *d) {
    d->enabled = !d->mouse; d->remote = d->wrap = d->scaling = false;
    d->pending = d->leds = d->id = 0;
    d->typematic = 0x2b; d->resolution = 2; d->rate = 100;
    memset(d->rates, 0, sizeof(d->rates));
    memset(d->keys, 0, sizeof(d->keys));
    d->repeat_key = d->buttons = d->sent_buttons = 0;
    d->dx = d->dy = d->wheel = 0; d->sample_at = d->repeat_at = 0;
    clear(&d->events);
}
void ps2_init(ps2_device *d, bool mouse) {
    memset(d, 0, sizeof(*d)); d->mouse = mouse; defaults(d);
    reply(d, 0xaa); if (mouse) reply(d, 0x00);
}
static int clamp(int n, int lo, int hi) { return n < lo ? lo : n > hi ? hi : n; }
static int scaled(int n) {
    int a = n < 0 ? -n : n;
    static const int small[] = {0, 1, 1, 3, 6, 9};
    int result = a < 6 ? small[a] : 2 * a;
    return n < 0 ? -result : result;
}
static void packet(ps2_device *d, ps2_fifo *q) {
    int x = clamp(d->dx, -127, 127), y = clamp(d->dy, -127, 127);
    d->dx -= x; d->dy -= y;
    if (d->scaling) { x = scaled(x); y = scaled(y); }
    uint8_t flags = 8 | (d->buttons & 7) | (x < 0 ? 0x10 : 0) | (y < 0 ? 0x20 : 0);
    put(q, flags); put(q, (uint8_t)x); put(q, (uint8_t)y);
    if (d->id == 3) {
        int wheel = clamp(d->wheel, -127, 127);
        put(q, (uint8_t)wheel); d->wheel -= wheel;
    } else d->wheel = 0;
    d->sent_buttons = d->buttons;
}
void ps2_command(ps2_device *d, uint8_t cmd) {
    if (d->wrap && cmd != 0xff && cmd != 0xec) { reply(d, cmd); return; }
    if (cmd == 0xff) {
        clear(&d->replies); defaults(d);
        reply(d, 0xfa); reply(d, 0xaa); if (d->mouse) reply(d, 0);
        return;
    }
    if (cmd == 0xfe) { reply(d, d->last); return; }
    if (d->pending) {
        uint8_t pending = d->pending; d->pending = 0;
        if (!d->mouse) {
            if (pending == 0xf0 && cmd != 0 && cmd != 2) { reply(d, 0xfe); d->pending = pending; return; }
            reply(d, 0xfa);
            if (pending == 0xf0 && cmd == 0) reply(d, 2);
            if (pending == 0xed) d->leds = cmd & 7;
            if (pending == 0xf3) d->typematic = cmd & 0x7f;
        } else {
            if ((pending == 0xe8 && cmd > 3) || (pending == 0xf3 &&
                cmd != 10 && cmd != 20 && cmd != 40 && cmd != 60 && cmd != 80 && cmd != 100 && cmd != 200)) {
                reply(d, 0xfe); d->pending = pending; return;
            }
            reply(d, 0xfa);
            if (pending == 0xe8) d->resolution = cmd;
            else {
                d->rate = cmd; d->rates[0] = d->rates[1]; d->rates[1] = d->rates[2]; d->rates[2] = cmd;
                if (d->rates[0] == 200 && d->rates[1] == 100 && d->rates[2] == 80) d->id = 3;
            }
        }
        return;
    }
    switch (cmd) {
    case 0xf2:
        reply(d, 0xfa);
        if (d->mouse) reply(d, d->id);
        else { reply(d, 0xab); reply(d, 0x83); }
        break;
    case 0xf4: reply(d, 0xfa); d->enabled = true; break;
    case 0xf5:
        reply(d, 0xfa);
        if (!d->mouse) defaults(d);
        d->enabled = false; clear(&d->events); d->repeat_key = 0;
        break;
    case 0xf6: reply(d, 0xfa); defaults(d); break;
    case 0xf3: reply(d, 0xfa); d->pending = cmd; break;
    default:
        if (!d->mouse) {
            if (cmd == 0xee) reply(d, 0xee);
            else if (cmd == 0xed || cmd == 0xf0) { reply(d, 0xfa); d->pending = cmd; }
            else reply(d, 0xfe);
        } else switch (cmd) {
        case 0xe6: reply(d, 0xfa); d->scaling = false; break;
        case 0xe7: reply(d, 0xfa); d->scaling = true; break;
        case 0xe8: reply(d, 0xfa); d->pending = cmd; break;
        case 0xe9:
            reply(d, 0xfa); reply(d, (d->remote ? 0x40 : 0) | (d->enabled ? 0x20 : 0) |
                (d->scaling ? 0x10 : 0) | ((d->buttons & 1) << 2) |
                ((d->buttons & 2) >> 1) | ((d->buttons & 4) >> 1));
            reply(d, d->resolution); reply(d, d->rate); break;
        case 0xea: reply(d, 0xfa); d->remote = false; break;
        case 0xeb: reply(d, 0xfa); packet(d, &d->replies); break;
        case 0xec: reply(d, 0xfa); d->wrap = false; d->dx = d->dy = d->wheel = 0; break;
        case 0xee: reply(d, 0xfa); d->wrap = true; clear(&d->events); break;
        case 0xf0: reply(d, 0xfa); d->remote = true; clear(&d->events); break;
        default: reply(d, 0xfe); break;
        }
    }
}
// High bit marks E0-prefixed scan set 2 keys. Special keys handled below.
static const uint8_t scan2[256] = {
    [4]=0x1c,[5]=0x32,[6]=0x21,[7]=0x23,[8]=0x24,[9]=0x2b,[10]=0x34,
    [11]=0x33,[12]=0x43,[13]=0x3b,[14]=0x42,[15]=0x4b,[16]=0x3a,[17]=0x31,
    [18]=0x44,[19]=0x4d,[20]=0x15,[21]=0x2d,[22]=0x1b,[23]=0x2c,[24]=0x3c,
    [25]=0x2a,[26]=0x1d,[27]=0x22,[28]=0x35,[29]=0x1a,
    [30]=0x16,[31]=0x1e,[32]=0x26,[33]=0x25,[34]=0x2e,[35]=0x36,[36]=0x3d,
    [37]=0x3e,[38]=0x46,[39]=0x45,[40]=0x5a,[41]=0x76,[42]=0x66,[43]=0x0d,
    [44]=0x29,[45]=0x4e,[46]=0x55,[47]=0x54,[48]=0x5b,[49]=0x5d,[50]=0x5d,
    [51]=0x4c,[52]=0x52,[53]=0x0e,[54]=0x41,[55]=0x49,[56]=0x4a,[57]=0x58,
    [58]=0x05,[59]=0x06,[60]=0x04,[61]=0x0c,[62]=0x03,[63]=0x0b,[64]=0x83,
    [65]=0x0a,[66]=0x01,[67]=0x09,[68]=0x78,[69]=0x07,[71]=0x7e,
    [73]=0xf0,[74]=0xec,[75]=0xfd,[76]=0xf1,[77]=0xe9,[78]=0xfa,
    [79]=0xf4,[80]=0xeb,[81]=0xf2,[82]=0xf5,[83]=0x77,[84]=0xca,
    [85]=0x7c,[86]=0x7b,[87]=0x79,[88]=0xda,[89]=0x69,[90]=0x72,[91]=0x7a,
    [92]=0x6b,[93]=0x73,[94]=0x74,[95]=0x6c,[96]=0x75,[97]=0x7d,[98]=0x70,
    [99]=0x71,[100]=0x61,[101]=0xaf,[103]=0x0f,
    [0xe0]=0x14,[0xe1]=0x12,[0xe2]=0x11,[0xe3]=0x9f,
    [0xe4]=0x94,[0xe5]=0x59,[0xe6]=0x91,[0xe7]=0xa7,
};
static void key(ps2_device *d, uint8_t hid, bool down) {
    ps2_fifo *q = &d->events;
    if (hid == 0x48) { // Pause has a make sequence only.
        static const uint8_t pause[] = {0xe1,0x14,0x77,0xe1,0xf0,0x14,0xf0,0x77};
        if (down) for (unsigned i = 0; i < sizeof(pause); ++i) put(q, pause[i]);
        return;
    }
    if (hid == 0x46) {
        static const uint8_t make[] = {0xe0,0x12,0xe0,0x7c};
        static const uint8_t release[] = {0xe0,0xf0,0x7c,0xe0,0xf0,0x12};
        const uint8_t *seq = down ? make : release;
        for (unsigned i = 0; i < (down ? sizeof(make) : sizeof(release)); ++i) put(q, seq[i]);
        return;
    }
    uint8_t code = scan2[hid];
    if (!code) return;
    // F7's scan code 83 is not an E0 key.
    if ((code & 0x80) && hid != 64) put(q, 0xe0);
    if (!down) put(q, 0xf0);
    put(q, hid == 64 ? code : code & 0x7f);
}
static bool contains(const uint8_t *report, uint8_t hid) {
    for (int i = 2; i < 8; ++i) if (report[i] == hid) return true;
    return false;
}
bool ps2_keyboard(ps2_device *d, const uint8_t report[8], uint64_t now) {
    if (!d->enabled) return true;
    // Worst-case six releases + six presses + eight modifiers + special keys.
    if (count(&d->events) > sizeof(d->events.bytes) - 128) return false;
    for (int i = 0; i < 8; ++i) if ((d->keys[0] ^ report[0]) & (1 << i))
        key(d, 0xe0 + i, (report[0] & (1 << i)) != 0);
    for (int i = 2; i < 8; ++i) if (d->keys[i] && !contains(report, d->keys[i])) {
        key(d, d->keys[i], false);
        if (d->repeat_key == d->keys[i]) d->repeat_key = 0;
    }
    for (int i = 2; i < 8; ++i) if (report[i] && !contains(d->keys, report[i])) {
        key(d, report[i], true);
        if (report[i] != 0x48 && report[i] != 0x46 && scan2[report[i]]) {
            d->repeat_key = report[i];
            d->repeat_at = now + 250000u * (1 + ((d->typematic >> 5) & 3));
        }
    }
    memcpy(d->keys, report, 8); return true;
}
void ps2_mouse(ps2_device *d, const uint8_t report[4]) {
    if (!d->enabled && !d->remote) return;
    d->buttons = report[0] & 7;
    d->dx = clamp(d->dx + (int8_t)report[1], -32767, 32767);
    d->dy = clamp(d->dy - (int8_t)report[2], -32767, 32767); // PS/2 Y is up.
    d->wheel = clamp(d->wheel - (int8_t)report[3], -32767, 32767);
}
void ps2_task(ps2_device *d, uint64_t now) {
    if (count(&d->events) > sizeof(d->events.bytes) - 16) return;
    if (!d->mouse) {
        if (d->enabled && d->repeat_key && now >= d->repeat_at) {
            key(d, d->repeat_key, true);
            d->repeat_at = now + ((8u + (d->typematic & 7)) << ((d->typematic >> 3) & 3)) * 1000000u / 240u;
        }
    } else if (d->enabled && !d->remote && !d->wrap && now >= d->sample_at) {
        if (d->dx || d->dy || d->wheel || d->buttons != d->sent_buttons) packet(d, &d->events);
        d->sample_at = now + 1000000u / d->rate;
    }
}
