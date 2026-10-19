#ifndef ANYKVM_CAPTURE_DEVICE_TYPES_DARWIN_H
#define ANYKVM_CAPTURE_DEVICE_TYPES_DARWIN_H

#import <AVFoundation/AVFoundation.h>

// Use the renamed device types only where the runtime and SDK support them.
static inline AVCaptureDeviceType bk_external_device_type(void) {
#if __MAC_OS_X_VERSION_MAX_ALLOWED >= 140000
    if (@available(macOS 14.0, *)) {
        return AVCaptureDeviceTypeExternal;
    }
#endif
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"
    return AVCaptureDeviceTypeExternalUnknown;
#pragma clang diagnostic pop
}

static inline AVCaptureDeviceType bk_microphone_device_type(void) {
#if __MAC_OS_X_VERSION_MAX_ALLOWED >= 140000
    if (@available(macOS 14.0, *)) {
        return AVCaptureDeviceTypeMicrophone;
    }
#endif
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"
    return AVCaptureDeviceTypeBuiltInMicrophone;
#pragma clang diagnostic pop
}

#endif
