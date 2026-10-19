/*
 * Copyright (c) 2026 Raspberry Pi (Trading) Ltd.
 * SPDX-License-Identifier: BSD-3-Clause
 *
 * Adapted from runtime_init_rp2350_sleep_fix in Pico SDK pico_low_power:
 * https://github.com/raspberrypi/pico-sdk/blob/079c6f3/src/rp2_common/pico_low_power/low_power.c
 * SDK 2.1 does not include this ROM-reboot sleep-state workaround.
 */
#include "pico/stdlib.h"
#include "hardware/irq.h"
#include "hardware/structs/nvic.h"
#include "hardware/structs/watchdog.h"
#include "hardware/sync.h"

void anykvm_rp2350_reboot_sleep_fix(void) {
#if PICO_RP2350 && !defined(__riscv)
    if (!(watchdog_hw->reason & WATCHDOG_REASON_TIMER_BITS)) return;
    uint32_t flags = save_and_disable_interrupts();
    uint32_t saved[(NUM_IRQS + 31) / 32];
    for (uint i = 0; i < count_of(saved); ++i) {
        saved[i] = nvic_hw->icer[i];
        nvic_hw->icer[i] = (uint32_t)-1;
    }
    const uint32_t index = FIRST_USER_IRQ / 32;
    const uint32_t bit = 1u << (FIRST_USER_IRQ % 32);
    nvic_hw->icpr[index] = bit;
    nvic_hw->iser[index] = bit;
    // An enabled pending IRQ wakes WFI even while interrupts are masked.
    // Align the store/WFI pair as in the SDK workaround for Cortex-M33.
    pico_default_asm_volatile(".p2align 2\nstr %0, [%1]\nwfi\n"
                             : : "l"(bit), "l"(&nvic_hw->ispr[index]));
    nvic_hw->icer[index] = bit;
    nvic_hw->icpr[index] = bit;
    for (uint i = 0; i < count_of(saved); ++i) nvic_hw->iser[i] = saved[i];
    restore_interrupts(flags);
#endif
}
