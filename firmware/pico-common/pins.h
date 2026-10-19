#pragma once
// Shared Pico 1/2 pin plan. Numbers are GPIO numbers, not header pin numbers.
// Keep UART, PS/2 and PIO USB disjoint so future multi-target firmware can
// combine backends without rewiring these first ports.
enum {
    ANYKVM_UART_TX = 0, ANYKVM_UART_RX = 1,
    ANYKVM_PS2_KBD_CLOCK = 2, ANYKVM_PS2_KBD_DATA = 3,
    ANYKVM_PS2_MOUSE_CLOCK = 4, ANYKVM_PS2_MOUSE_DATA = 5,
    ANYKVM_PIO_DP = 12, ANYKVM_PIO_DM = 13,
};
_Static_assert(ANYKVM_PIO_DM == ANYKVM_PIO_DP + 1, "PIO USB requires adjacent pins");
