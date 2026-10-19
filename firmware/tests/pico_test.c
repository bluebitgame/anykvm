#include <assert.h>
#include <stdio.h>
#include <string.h>
#include "../pico-common/control.h"
#include "../pico-common/pins.h"
#include "../pico-common/ps2_wire.h"

static void expect(ps2_device *d, const uint8_t *bytes, size_t size) {
    for (size_t i = 0; i < size; ++i) {
        uint8_t byte; bool reply;
        assert(ps2_peek(d, &byte, &reply)); assert(byte == bytes[i]);
        ps2_pop(d, reply);
    }
    uint8_t byte; bool reply; assert(!ps2_peek(d, &byte, &reply));
}
#define EXPECT(d, ...) do { const uint8_t expected[] = {__VA_ARGS__}; expect(d, expected, sizeof(expected)); } while (0)
static void keyboard_test(void) {
    ps2_device d; ps2_init(&d, false); EXPECT(&d, 0xaa);
    ps2_command(&d, 0xff); EXPECT(&d, 0xfa, 0xaa);
    ps2_command(&d, 0xf2); EXPECT(&d, 0xfa, 0xab, 0x83);
    ps2_command(&d, 0xed); ps2_command(&d, 5); EXPECT(&d, 0xfa, 0xfa); assert(d.leds == 5);
    ps2_command(&d, 0xf0); ps2_command(&d, 0); EXPECT(&d, 0xfa, 0xfa, 2);
    ps2_command(&d, 0xf0); ps2_command(&d, 1); EXPECT(&d, 0xfa, 0xfe);
    ps2_command(&d, 2); EXPECT(&d, 0xfa);
    ps2_command(&d, 0xee); EXPECT(&d, 0xee);
    uint8_t r[8] = {0,0,4}; assert(ps2_keyboard(&d, r, 0)); EXPECT(&d, 0x1c);
    ps2_task(&d, 499999); EXPECT(&d);
    ps2_task(&d, 500000); EXPECT(&d, 0x1c);
    memset(r, 0, 8); assert(ps2_keyboard(&d, r, 500001)); EXPECT(&d, 0xf0, 0x1c);
    r[0] = 0x10; r[2] = 79; r[3] = 64; // Right Ctrl, Right arrow, F7.
    assert(ps2_keyboard(&d, r, 600000)); EXPECT(&d, 0xe0,0x14, 0xe0,0x74, 0x83);
    memset(r, 0, 8); assert(ps2_keyboard(&d, r, 600001));
    EXPECT(&d, 0xe0,0xf0,0x14, 0xe0,0xf0,0x74, 0xf0,0x83);
    r[2] = 0x46; assert(ps2_keyboard(&d, r, 700000)); EXPECT(&d, 0xe0,0x12,0xe0,0x7c);
    r[2] = 0; assert(ps2_keyboard(&d, r, 700001)); EXPECT(&d, 0xe0,0xf0,0x7c,0xe0,0xf0,0x12);
    r[2] = 0x48; assert(ps2_keyboard(&d, r, 800000)); EXPECT(&d, 0xe1,0x14,0x77,0xe1,0xf0,0x14,0xf0,0x77);
    r[2] = 0; assert(ps2_keyboard(&d, r, 800001)); EXPECT(&d);
    ps2_command(&d, 0xf5); EXPECT(&d, 0xfa);
    r[2] = 4; assert(ps2_keyboard(&d, r, 900000)); EXPECT(&d);
    ps2_command(&d, 0xf4); EXPECT(&d, 0xfa);
    assert(ps2_keyboard(&d, r, 1000000)); EXPECT(&d, 0x1c);
    // Queued break sequences must survive a host ID query and reply first.
    r[2] = 0; assert(ps2_keyboard(&d, r, 1000001));
    ps2_command(&d, 0xf2); EXPECT(&d, 0xfa,0xab,0x83, 0xf0,0x1c);
    ps2_command(&d, 0xfe); EXPECT(&d, 0x1c);
    ps2_receive_error(&d); EXPECT(&d, 0xfe);
    // Slow/inhibited hosts cannot overrun the byte ring.
    int accepted = 0;
    for (int i = 0; i < 1000; ++i) {
        r[2] = (i & 1) ? 4 : 0;
        if (!ps2_keyboard(&d, r, 1100000 + i)) break;
        ++accepted;
    }
    assert(accepted > 100 && accepted < 1000);
    assert(d.events.write - d.events.read <= sizeof(d.events.bytes));
}
static void mouse_test(void) {
    ps2_device d; ps2_init(&d, true); EXPECT(&d, 0xaa,0);
    ps2_command(&d, 0xff); EXPECT(&d, 0xfa,0xaa,0);
    ps2_command(&d, 0xf4); EXPECT(&d, 0xfa);
    uint8_t r[] = {1, 5, 3, 0}; ps2_mouse(&d, r); ps2_task(&d, 10000);
    EXPECT(&d, 0x29,5,0xfd);
    const uint8_t rates[] = {200,100,80};
    for (unsigned i = 0; i < 3; ++i) { ps2_command(&d, 0xf3); ps2_command(&d, rates[i]); EXPECT(&d, 0xfa,0xfa); }
    ps2_command(&d, 0xf2); EXPECT(&d, 0xfa,3);
    ps2_command(&d, 0xf0); EXPECT(&d, 0xfa);
    r[1] = 0xff; r[2] = 0xfe; r[3] = 1;
    ps2_mouse(&d, r); ps2_task(&d, 50000); EXPECT(&d);
    ps2_command(&d, 0xeb); EXPECT(&d, 0xfa,0x19,0xff,2,0xff);
    ps2_command(&d, 0xe9); EXPECT(&d, 0xfa,0x64,2,80);
    ps2_command(&d, 0xee); EXPECT(&d, 0xfa);
    ps2_command(&d, 0x55); EXPECT(&d, 0x55);
    ps2_command(&d, 0xec); EXPECT(&d, 0xfa);
    ps2_command(&d, 0xe8); ps2_command(&d, 7); EXPECT(&d, 0xfa,0xfe);
    ps2_command(&d, 1); EXPECT(&d, 0xfa); assert(d.resolution == 1);
    ps2_command(&d, 0xf6); EXPECT(&d, 0xfa); assert(!d.enabled && !d.remote && d.id == 0);
    ps2_command(&d, 0xf4); ps2_command(&d, 0xe7); EXPECT(&d, 0xfa,0xfa);
    uint8_t large[] = {0,127,127,0};
    ps2_mouse(&d, large); ps2_mouse(&d, large); ps2_task(&d, 100000);
    EXPECT(&d, 0x28,254,2); // 2:1 scaling still fits signed 9-bit movement.
    ps2_task(&d, 110000); EXPECT(&d, 0x28,254,2);
}
static void pin_test(void) {
    const unsigned pins[] = {
        ANYKVM_UART_TX, ANYKVM_UART_RX,
        ANYKVM_PS2_KBD_CLOCK, ANYKVM_PS2_KBD_DATA,
        ANYKVM_PS2_MOUSE_CLOCK, ANYKVM_PS2_MOUSE_DATA,
        ANYKVM_PIO_DP, ANYKVM_PIO_DM,
    };
    uint32_t used = 0;
    for (unsigned i = 0; i < sizeof(pins) / sizeof(pins[0]); ++i) {
        assert(pins[i] < 29); assert(!(used & (1u << pins[i]))); used |= 1u << pins[i];
    }
}
static void owner_test(void) {
    bk_owner owner = {.source = -1}; bool takeover;
    assert(bk_owner_accept(&owner, 1, 0x22, 0, &takeover)); assert(owner.source == -1);
    assert(bk_owner_accept(&owner, 0, 0x11, 100, &takeover)); assert(!takeover);
    assert(bk_owner_accept(&owner, 1, 0x22, 200, &takeover)); assert(owner.source == 0);
    assert(!bk_owner_accept(&owner, 1, 0x11, 200, &takeover));
    assert(bk_owner_accept(&owner, 1, 0x24, 2000100, &takeover));
    assert(!takeover && owner.source == 0); // Unsupported reservation is read-only.
    assert(bk_owner_accept(&owner, 1, 0x11, 2000100, &takeover)); assert(takeover);
    assert(bk_owner_accept(&owner, 1, 0x11, 2000200, &takeover)); assert(!takeover);
}
// Clock the actual wire state machine with a fake open-drain host.
static uint64_t now;
static void tick(ps2_wire *w, bool host_clock_low, bool host_data_low) {
    ps2_wire_tick(w, now, !w->clock_low && !host_clock_low, !w->data_low && !host_data_low);
    now += 2;
}
static uint8_t receive(ps2_wire *w) {
    uint16_t bits = 0; unsigned n = 0;
    for (unsigned i = 0; i < 2000 && n < 11; ++i) {
        bool old_clock = w->clock_low;
        tick(w, false, false);
        if (!old_clock && w->clock_low) { if (!w->data_low) bits |= 1u << n; ++n; }
    }
    assert(n == 11); assert(!(bits & 1)); assert(bits & (1u << 10));
    unsigned parity = 0; for (int i = 1; i <= 9; ++i) parity ^= (bits >> i) & 1;
    assert(parity == 1);
    // Finish stop bit so the backend commits the byte.
    while (w->state != 0) tick(w, false, false);
    return (bits >> 1) & 0xff;
}
static void send(ps2_wire *w, uint8_t byte, bool corrupt) {
    for (int i = 0; i < 80; ++i) tick(w, true, false);
    for (int i = 0; i < 10; ++i) tick(w, true, true);
    tick(w, false, true); // Host request-to-send, start bit remains low.
    unsigned n = 0; bool host_data = true;
    unsigned parity = 1; for (int i = 0; i < 8; ++i) parity ^= (byte >> i) & 1;
    if (corrupt) parity ^= 1;
    for (unsigned i = 0; i < 2000 && n < 11; ++i) {
        bool old_clock = w->clock_low;
        tick(w, false, host_data);
        if (!old_clock && w->clock_low) {
            host_data = n < 8 ? !((byte >> n) & 1) : n == 8 ? !parity : false;
            if (n == 10) assert(w->data_low == !corrupt); // Electrical ACK.
            ++n;
        }
    }
    assert(n == 11);
    while (w->state != 0) tick(w, false, false);
}
static void wire_test(void) {
    ps2_device d; ps2_init(&d, false);
    ps2_wire w = {.device = &d}; now = 0;
    assert(receive(&w) == 0xaa);
    send(&w, 0xf2, false); assert(w.host_seen);
    assert(receive(&w) == 0xfa); assert(receive(&w) == 0xab); assert(receive(&w) == 0x83);
    send(&w, 0xf4, true); assert(receive(&w) == 0xfe);
    send(&w, 0xf4, false); assert(receive(&w) == 0xfa);
    // Inhibit a partially sent scan code; it must restart from its start bit.
    uint8_t keys[8] = {0,0,4}; assert(ps2_keyboard(&d, keys, now));
    for (int i = 0; i < 120; ++i) tick(&w, false, false);
    for (int i = 0; i < 100; ++i) tick(&w, true, false);
    assert(w.state == 0); assert(!w.data_low);
    assert(receive(&w) == 0x1c);
}
int main(void) {
    keyboard_test(); mouse_test(); owner_test(); wire_test(); pin_test();
    puts("Pico PS/2 protocol, wire timing and transport ownership tests passed");
}
