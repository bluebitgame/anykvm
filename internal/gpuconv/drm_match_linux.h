#ifndef BK_DRM_MATCH_LINUX_H
#define BK_DRM_MATCH_LINUX_H

#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>

// Resolve by dev_t, not card index: vc4 and v3d card numbering depends on
// probe order. Sysfs driver names are platform driver names (vc4-drm/v3d),
// rather than the DRM version ioctl's vc4/v3d names.
static int bk_drm_driver(uint32_t major, uint32_t minor, const char *driver) {
    char path[128], target[512];
    snprintf(path, sizeof(path), "/sys/dev/char/%u:%u/device/driver", major, minor);
    ssize_t length = readlink(path, target, sizeof(target) - 1);
    if (length <= 0 || (size_t)length >= sizeof(target) - 1)
        return 0;
    target[length] = '\0';
    const char *name = strrchr(target, '/');
    return name && !strcmp(name + 1, driver);
}

// Rank 1 preserves exact primary-device matching; rank 2 is the compute-only
// vc4 display -> v3d renderer pairing on Pi 4/5. Never pick an arbitrary GPU
// merely because it can import DMA-BUFs. The actual format/modifier, memory
// import and fences are still validated by the normal GPU processing path.
static int bk_drm_match(uint32_t major, uint32_t minor,
                       const VkPhysicalDeviceDrmPropertiesEXT *drm,
                       const VkPhysicalDeviceProperties *properties, int compute_only) {
    if (drm->hasPrimary && (!major ||
        (drm->primaryMajor == major && drm->primaryMinor == minor)))
        return 1;
    if (!major || !compute_only || properties->vendorID != 0x14e4 ||
        properties->deviceType != VK_PHYSICAL_DEVICE_TYPE_INTEGRATED_GPU ||
        !bk_drm_driver(major, minor, "vc4-drm"))
        return 0;
    // Prefer the render node so this also works if v3d has no primary node.
    if (drm->hasRender && bk_drm_driver(drm->renderMajor, drm->renderMinor, "v3d"))
        return 2;
    return drm->hasPrimary && bk_drm_driver(drm->primaryMajor, drm->primaryMinor, "v3d") ? 2 : 0;
}
#endif
