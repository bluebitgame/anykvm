#pragma once
#include <stdint.h>

#ifdef ANYKVM_USB_BOOT_DIAGNOSTICS
void anykvm_boot_diagnostic_stage(uint32_t stage);
void anykvm_boot_diagnostic_ready(void);
#else
static inline void anykvm_boot_diagnostic_stage(uint32_t stage) { (void)stage; }
static inline void anykvm_boot_diagnostic_ready(void) {}
#endif
