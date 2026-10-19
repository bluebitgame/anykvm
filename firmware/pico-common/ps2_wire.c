#include "ps2_wire.h"
enum { IDLE, SEND, RECEIVE, RECEIVE_DONE };
static void idle(ps2_wire *w, uint64_t now) {
    w->state = IDLE; w->clock_low = w->data_low = false;
    w->next = now + 50;
}
static bool tx_bit(ps2_wire *w) {
    if (!w->bit) return false;
    if (w->bit <= 8) return (w->byte >> (w->bit - 1)) & 1;
    return w->bit == 9 ? w->parity != 0 : true;
}
void ps2_wire_tick(ps2_wire *w, uint64_t now, bool clock, bool data) {
    if (w->state == IDLE) {
        if (!clock) {
            if (!w->inhibited) w->inhibited_at = now;
            w->inhibited = true; w->next = now + 50; return;
        }
        if (w->inhibited && !data) {
            w->state = RECEIVE; w->bit = w->byte = w->parity = 0;
            w->low_phase = false; w->valid = true; w->inhibited = false;
            w->next = now + 20;
            return;
        }
        if (w->inhibited) { w->inhibited = false; w->next = now + 50; }
        if (!data || now < w->next) return;
        if (!ps2_peek(w->device, &w->byte, &w->reply)) return;
        w->state = SEND; w->bit = 0; w->parity = 1;
        for (int i = 0; i < 8; ++i) w->parity ^= (w->byte >> i) & 1;
        w->data_low = true; w->low_phase = false; w->next = now + 40;
        return;
    }
    if (now < w->next) return;
    if (w->state == RECEIVE_DONE) {
        idle(w, now);
        if (w->valid) { ps2_command(w->device, w->byte); w->host_seen = true; }
        else ps2_receive_error(w->device); // Ask host to resend a malformed frame.
        return;
    }
    if (!w->low_phase) { // Falling edge, after a high phase.
        if (!clock) { // Host inhibited our clock. Retry this byte after release.
            idle(w, now); w->inhibited = true; w->inhibited_at = now; return;
        }
        w->clock_low = true; w->low_phase = true;
        if (w->state == RECEIVE && w->bit == 10) w->data_low = w->valid;
        w->next = now + 40;
        return;
    }
    // Rising edge. Host data is sampled on the following tick, after releasing
    // the clock; device data remains unchanged throughout its falling edge.
    if (w->state == RECEIVE) {
        if (w->bit < 8) {
            w->byte |= (uint8_t)data << w->bit; w->parity ^= data;
        } else if (w->bit == 8) w->valid = (w->parity ^ data) == 1;
        else if (w->bit == 9) w->valid = w->valid && data;
        if (w->bit == 10) w->state = RECEIVE_DONE;
    }
    w->clock_low = false; w->low_phase = false;
    ++w->bit;
    if (w->state == SEND) {
        if (w->bit == 11) { ps2_pop(w->device, w->reply); idle(w, now); }
        else w->data_low = !tx_bit(w);
    }
    w->next = now + 40;
}
