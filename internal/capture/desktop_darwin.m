#import <CoreGraphics/CoreGraphics.h>
#include "desktop_darwin.h"
#include <stdio.h>
#include <stdlib.h>

typedef struct {
    CGDirectDisplayID display;
    CGDisplayModeRef original;
    CFArrayRef modes;
    int changed;
} bk_display;

static void mode_info(CGDisplayModeRef mode, bk_display_mode *out) {
    out->width = (int)CGDisplayModeGetWidth(mode);
    out->height = (int)CGDisplayModeGetHeight(mode);
    out->pixel_width = (int)CGDisplayModeGetPixelWidth(mode);
    out->pixel_height = (int)CGDisplayModeGetPixelHeight(mode);
    out->refresh_rate = CGDisplayModeGetRefreshRate(mode);
    out->usable = CGDisplayModeIsUsableForDesktopGUI(mode);
}

void *bk_display_open(int index, char *error, int error_size) {
    CGDirectDisplayID ids[32]; uint32_t count = 0;
    CGError status = CGGetActiveDisplayList(32, ids, &count);
    if (status || index < 0 || (uint32_t)index >= count) {
        snprintf(error, error_size, "display index %d unavailable (CGError=%d)", index, status);
        return NULL;
    }
    bk_display *d = calloc(1, sizeof(*d));
    if (!d) { snprintf(error, error_size, "out of memory"); return NULL; }
    d->display = ids[index];
    d->original = CGDisplayCopyDisplayMode(d->display);
    const void *keys[] = {kCGDisplayShowDuplicateLowResolutionModes};
    const void *values[] = {kCFBooleanTrue};
    CFDictionaryRef options = CFDictionaryCreate(NULL, keys, values, 1, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
    d->modes = CGDisplayCopyAllDisplayModes(d->display, options);
    if (options) CFRelease(options);
    if (!d->original || !d->modes) {
        snprintf(error, error_size, "cannot read display modes");
        bk_display_free(d); return NULL;
    }
    return d;
}

uint32_t bk_display_id(void *handle) { return ((bk_display *)handle)->display; }
uint32_t bk_display_index_id(int index) {
    CGDirectDisplayID ids[32]; uint32_t count = 0;
    if (CGGetActiveDisplayList(32, ids, &count) || index < 0 || (uint32_t)index >= count) return 0;
    return ids[index];
}
int bk_display_modes(void *handle) { return (int)CFArrayGetCount(((bk_display *)handle)->modes); }
int bk_display_mode_at(void *handle, int index, bk_display_mode *out) {
    bk_display *d = handle;
    if (index < 0 || index >= CFArrayGetCount(d->modes)) return 0;
    mode_info((CGDisplayModeRef)CFArrayGetValueAtIndex(d->modes, index), out);
    return 1;
}
int bk_display_current(void *handle, bk_display_mode *out) {
    CGDisplayModeRef mode = CGDisplayCopyDisplayMode(((bk_display *)handle)->display);
    if (!mode) return 0;
    mode_info(mode, out); CGDisplayModeRelease(mode); return 1;
}
int bk_display_set(void *handle, int index, char *error, int error_size) {
    bk_display *d = handle;
    if (index < 0 || index >= CFArrayGetCount(d->modes)) return 0;
    CGDisplayModeRef mode = (CGDisplayModeRef)CFArrayGetValueAtIndex(d->modes, index);
    CGDisplayModeRef current = CGDisplayCopyDisplayMode(d->display);
    // HiDPI and low-resolution variants can share an IODisplayModeID.
    // Logical dimensions must also match before treating a DPI switch as a no-op.
    int same = current && CGDisplayModeGetIODisplayModeID(current) == CGDisplayModeGetIODisplayModeID(mode)
        && CGDisplayModeGetWidth(current) == CGDisplayModeGetWidth(mode)
        && CGDisplayModeGetHeight(current) == CGDisplayModeGetHeight(mode)
        && CGDisplayModeGetPixelWidth(current) == CGDisplayModeGetPixelWidth(mode)
        && CGDisplayModeGetPixelHeight(current) == CGDisplayModeGetPixelHeight(mode)
        && CGDisplayModeGetRefreshRate(current) == CGDisplayModeGetRefreshRate(mode);
    if (current) CGDisplayModeRelease(current);
    if (same) return 1;
    CGError status = CGDisplaySetDisplayMode(d->display, mode, NULL);
    if (status) { snprintf(error, error_size, "mode switch failed (CGError=%d)", status); return 0; }
    d->changed = 1;
    return 1;
}
int bk_display_restore(void *handle, char *error, int error_size) {
    bk_display *d = handle;
    if (!d->changed) return 1;
    CGError status = CGDisplaySetDisplayMode(d->display, d->original, NULL);
    if (status) { snprintf(error, error_size, "mode restore failed (CGError=%d)", status); return 0; }
    d->changed = 0;
    return 1;
}
void bk_display_free(void *handle) {
    bk_display *d = handle;
    if (!d) return;
    if (d->original) CGDisplayModeRelease(d->original);
    if (d->modes) CFRelease(d->modes);
    free(d);
}
