cmake_minimum_required(VERSION 3.20.0)

# AnyKVM nRF52840 BLE HID firmware (Zephyr / nRF Connect SDK).
find_package(Zephyr REQUIRED HINTS $ENV{ZEPHYR_BASE})
project(anykvm_ble_hid)

# Keep checkout paths out of firmware assertion strings.
# Derive the prefix in CMake so it also matches paths reached through symlinks.
get_filename_component(ANYKVM_SOURCE_ROOT "${CMAKE_CURRENT_SOURCE_DIR}/../.." ABSOLUTE)
zephyr_compile_options("-ffile-prefix-map=${ANYKVM_SOURCE_ROOT}=.")

target_sources(app PRIVATE
    src/main.c
    src/hids.c
    src/profiles.c
    src/proto.c
    src/reply.c
    src/led.c
)

target_include_directories(app PRIVATE src)
