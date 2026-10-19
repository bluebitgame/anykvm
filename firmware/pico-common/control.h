#pragma once
#include <stdbool.h>
#include <stdint.h>
// Queries never acquire the keyboard. A competing writer may take over after
// two seconds without a state-changing command; held keys are released first.
typedef struct { int source; uint64_t last_write; } bk_owner;
static inline bool bk_owner_accept(bk_owner *owner, int source, uint8_t command,
                                   uint64_t now, bool *takeover) {
    *takeover = false;
    if (command != 0x04 && (command < 0x10 || command > 0x15)) return true;
    if (owner->source >= 0 && owner->source != source) {
        if (now - owner->last_write < 2000000) return false;
        *takeover = true;
    }
    owner->source = source; owner->last_write = now;
    return true;
}
