// Run the production device association policy without a DRM/Vulkan host.
#define VK_NO_PROTOTYPES
#include "../third_party/vulkan/vulkan_core.h"
#define readlink fixture_readlink
#include "../drm_match_linux.h"
#undef readlink
#include <assert.h>

static const char *source_path, *render_path, *source_driver, *render_driver;
static int truncated;

ssize_t fixture_readlink(const char *path, char *target, size_t size) {
    const char *value = NULL;
    if (!strcmp(path, source_path)) value = source_driver;
    if (!strcmp(path, render_path)) value = render_driver;
    if (!value) return -1; // Missing sysfs or access denied must fail closed.
    if (truncated) { memset(target, 'x', size); return (ssize_t)size; }
    size_t length = strlen(value);
    assert(length < size);
    memcpy(target, value, length); // readlink does not add a NUL terminator.
    return (ssize_t)length;
}

static void pi(uint32_t source_minor, uint32_t primary_minor, uint32_t render_minor) {
    char source[128], render[128];
    snprintf(source, sizeof(source), "/sys/dev/char/226:%u/device/driver", source_minor);
    snprintf(render, sizeof(render), "/sys/dev/char/226:%u/device/driver", render_minor);
    source_path = source; render_path = render;
    source_driver = "../../../bus/platform/drivers/vc4-drm";
    render_driver = "../../../bus/platform/drivers/v3d";
    VkPhysicalDeviceProperties properties = {
        .vendorID = 0x14e4, .deviceType = VK_PHYSICAL_DEVICE_TYPE_INTEGRATED_GPU
    };
    VkPhysicalDeviceDrmPropertiesEXT drm = {
        .hasPrimary = VK_TRUE, .primaryMajor = 226, .primaryMinor = primary_minor,
        .hasRender = VK_TRUE, .renderMajor = 226, .renderMinor = render_minor
    };
    assert(bk_drm_match(226, source_minor, &drm, &properties, 1) == 2);
    assert(bk_drm_match(226, source_minor, &drm, &properties, 0) == 0); // Video stays exact.
    assert(bk_drm_match(226, primary_minor, &drm, &properties, 1) == 1);
    assert(bk_drm_match(226, primary_minor, &drm, &properties, 0) == 1);
    assert(bk_drm_match(0, 0, &drm, &properties, 0) == 1); // Unpinned video query.
    drm.hasPrimary = VK_FALSE;
    assert(bk_drm_match(226, source_minor, &drm, &properties, 1) == 2); // Render-only v3d.
    assert(bk_drm_match(0, 0, &drm, &properties, 1) == 0);
    drm.hasRender = VK_FALSE;
    assert(bk_drm_match(226, source_minor, &drm, &properties, 1) == 0);
    drm.hasRender = VK_TRUE;
    properties.vendorID = 0x8086;
    assert(bk_drm_match(226, source_minor, &drm, &properties, 1) == 0);
    properties.vendorID = 0x14e4;
    properties.deviceType = VK_PHYSICAL_DEVICE_TYPE_CPU;
    assert(bk_drm_match(226, source_minor, &drm, &properties, 1) == 0);
    properties.deviceType = VK_PHYSICAL_DEVICE_TYPE_INTEGRATED_GPU;
    render_driver = "../../../bus/pci/drivers/i915";
    assert(bk_drm_match(226, source_minor, &drm, &properties, 1) == 0);
    render_driver = "../../../bus/platform/drivers/v3d-extra";
    assert(bk_drm_match(226, source_minor, &drm, &properties, 1) == 0);
    render_driver = "../../../bus/platform/drivers/v3d";
    source_driver = "../../../bus/platform/drivers/simple-framebuffer";
    assert(bk_drm_match(226, source_minor, &drm, &properties, 1) == 0);
    source_driver = NULL;
    assert(bk_drm_match(226, source_minor, &drm, &properties, 1) == 0);
    source_driver = "../../../bus/platform/drivers/vc4-drm";
    render_driver = NULL;
    assert(bk_drm_match(226, source_minor, &drm, &properties, 1) == 0);
    render_driver = "../../../bus/platform/drivers/v3d";
    // A driver exposing only the primary node still identifies the renderer.
    drm.hasPrimary = VK_TRUE;
    drm.hasRender = VK_FALSE;
    snprintf(render, sizeof(render), "/sys/dev/char/226:%u/device/driver", primary_minor);
    assert(bk_drm_match(226, source_minor, &drm, &properties, 1) == 2);
    render_driver = NULL;
    assert(bk_drm_match(226, source_minor, &drm, &properties, 1) == 0);
    render_driver = "../../../bus/platform/drivers/v3d";
    truncated = 1;
    assert(bk_drm_match(226, source_minor, &drm, &properties, 1) == 0);
    truncated = 0;
    // An unknown display never gets mapped to an arbitrary Vulkan GPU.
    assert(bk_drm_match(226, 99, &drm, &properties, 1) == 0);
}

int main(void) {
    pi(1, 0, 128); // Pi 4: vc4 and v3d are separate DRM devices.
    pi(0, 1, 129); // Pi 5: reversed probe order, non-default render minor.
    pi(7, 3, 131); // Neither path relies on fixed card/render node numbers.
    return 0;
}
