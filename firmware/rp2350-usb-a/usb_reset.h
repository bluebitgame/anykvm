#pragma once

#include <stdbool.h>

bool anykvm_usb_reset_requested(void);
void anykvm_usb_reset_execute(void) __attribute__((noreturn));
