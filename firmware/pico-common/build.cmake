# Included after project() and pico_sdk_init() by each output target.
set(common ${CMAKE_CURRENT_LIST_DIR})
add_executable(${ANYKVM_APP} ${common}/main.c ${common}/usb.c
    ${common}/../rp2350-usb-a/rp2350_reboot.c)
target_include_directories(${ANYKVM_APP} PRIVATE ${common})
target_link_libraries(${ANYKVM_APP} pico_stdlib pico_unique_id hardware_uart tinyusb_device)
target_compile_options(${ANYKVM_APP} PRIVATE -Wall -Wextra)
if (ANYKVM_OUTPUT STREQUAL "native")
    target_compile_definitions(${ANYKVM_APP} PRIVATE ANYKVM_OUTPUT_NATIVE=1
        ANYKVM_USB_PID=0x4010 ANYKVM_USB_PRODUCT="AnyKVM Native HID")
elseif (ANYKVM_OUTPUT STREQUAL "pio")
    if (NOT EXISTS "${PICO_PIO_USB_DIR}/src/pio_usb.h")
        message(FATAL_ERROR "Set PICO_PIO_USB_DIR to the prepared PIO USB library")
    endif()
    add_subdirectory(${PICO_PIO_USB_DIR} pico_pio_usb)
    target_sources(${ANYKVM_APP} PRIVATE ${common}/pio_output.c
        ${common}/../rp2350-usb-a/pio_control.c)
    target_link_libraries(${ANYKVM_APP} pico_pio_usb)
    target_link_options(${ANYKVM_APP} PRIVATE "-Wl,--wrap=pio_usb_ll_transfer_start")
    target_compile_definitions(${ANYKVM_APP} PRIVATE ANYKVM_OUTPUT_PIO=1
        ANYKVM_USB_PID=0x4011 ANYKVM_USB_PRODUCT="AnyKVM PIO USB")
elseif (ANYKVM_OUTPUT STREQUAL "ps2")
    target_sources(${ANYKVM_APP} PRIVATE ${common}/ps2_output.c ${common}/ps2_protocol.c ${common}/ps2_wire.c)
    target_link_libraries(${ANYKVM_APP} pico_multicore pico_util)
    target_compile_definitions(${ANYKVM_APP} PRIVATE ANYKVM_USB_PID=0x4012
        ANYKVM_USB_PRODUCT="AnyKVM PS2")
else()
    message(FATAL_ERROR "Unknown output backend: ${ANYKVM_OUTPUT}")
endif()
pico_enable_stdio_usb(${ANYKVM_APP} 0)
pico_enable_stdio_uart(${ANYKVM_APP} 0)
pico_add_extra_outputs(${ANYKVM_APP})
