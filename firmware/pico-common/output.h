#pragma once
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
void output_init(void);
void output_task(void);
bool output_ready(void);
uint8_t output_capabilities(void);
bool output_default_absolute(void);
uint8_t output_mode(bool absolute);
// Return false on backpressure; the common adapter reports BK_BUSY.
bool output_keyboard(const uint8_t report[8]);
bool output_mouse(bool absolute, const uint8_t *report, size_t size);
