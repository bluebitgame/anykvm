#include "pico/runtime.h"
#include "pico/bootrom.h"
#include "hardware/watchdog.h"
#include "hardware/structs/watchdog.h"
#include "pico/platform.h"
#include "boot_diagnostic.h"

volatile uint32_t __attribute__((section(".uninitialized_data.boot_trace"))) anykvm_boot_trace[2];

void anykvm_boot_diagnostic_stage(uint32_t stage) {
    anykvm_boot_trace[0] = 0xB007D1A6;
    anykvm_boot_trace[1] = stage;
}

// Diagnostic builds recover from hangs after this early startup hook,
// including hangs in clock setup or the first sleep, before CDC is ready.
static void boot_watchdog_init(void) {
    if (watchdog_enable_caused_reboot()) rom_reset_usb_boot_extra(-1, 0, false);
    anykvm_boot_diagnostic_stage(1);
    watchdog_hw->scratch[1] = 0xB0070001;
    watchdog_enable(8000, false);
}
PICO_RUNTIME_INIT_FUNC_RUNTIME(boot_watchdog_init, "00201");

void anykvm_boot_diagnostic_ready(void) {
    anykvm_boot_diagnostic_stage(12);
    watchdog_hw->scratch[1] = 0xB0070002;
    watchdog_disable();
}
