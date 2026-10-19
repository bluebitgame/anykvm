cmake_minimum_required(VERSION 3.13)
if (NOT PICO_SDK_PATH)
    set(PICO_SDK_PATH $ENV{HOME}/pico-sdk)
endif()
include(${PICO_SDK_PATH}/pico_sdk_init.cmake)
set(PICO_BOARD pico2 CACHE STRING "Board type")
project(anykvm C CXX ASM)
pico_sdk_init()
if (PICO_SDK_VERSION_STRING VERSION_LESS "2.3.1")
    message(FATAL_ERROR "RP2350 USB reconnect requires Pico SDK 2.3.1 or newer")
endif()

set(PICO_PIO_USB_DIR "" CACHE PATH "Path to the Waveshare RP2350-USB-A PIO USB library")
if (NOT EXISTS "${PICO_PIO_USB_DIR}/src/pio_usb.h")
    message(FATAL_ERROR "Set PICO_PIO_USB_DIR to the Waveshare RP2350-USB-A source directory")
endif()
add_subdirectory("${PICO_PIO_USB_DIR}" pico_pio_usb)

add_executable(anykvm main.c)
# Keep SDK CDC descriptors, but complete reset requests before leaving USB
# callbacks. The main loop shuts down PIO and detaches USB before rebooting.
target_sources(anykvm PRIVATE usb_reset.c rp2350_reboot.c pio_control.c)
# Bound the reference PIO device stack's EP0 replies to SETUP wLength.
target_link_options(anykvm PRIVATE "-Wl,--wrap=pio_usb_ll_transfer_start")
# usb_reset.c supplies both callbacks and defers reset execution to main.
target_compile_definitions(anykvm PRIVATE
    PICO_USB_RESET_INCLUDE_DEFAULT_APP_DRIVER_CB=0
    PICO_ENABLE_USB_RESET_VIA_BAUD_RATE=0)
option(ANYKVM_USB_BOOT_DIAGNOSTICS "Return to BOOTSEL on native USB enumeration timeout" OFF)
if (ANYKVM_USB_BOOT_DIAGNOSTICS)
    target_compile_definitions(anykvm PRIVATE ANYKVM_USB_BOOT_DIAGNOSTICS=1)
    target_sources(anykvm PRIVATE boot_diagnostic.c)
endif()
# Native USB-C CDC uses pico_stdio_usb and its SDK-default TinyUSB config,
# exactly like the Waveshare usb_device example. PIO HID is independent.
target_link_libraries(anykvm pico_stdlib pico_multicore pico_pio_usb)
pico_enable_stdio_usb(anykvm 1)
pico_enable_stdio_uart(anykvm 0)
pico_add_extra_outputs(anykvm)
