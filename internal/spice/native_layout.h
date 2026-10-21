#ifndef ANYKVM_SPICE_NATIVE_LAYOUT_H
#define ANYKVM_SPICE_NATIVE_LAYOUT_H

#include <stdint.h>
#include <sys/stat.h>

// EGL_MESA_image_dma_buf_export returns one modifier per plane. Secondary FD
// slots may be -1 when planes share the first allocation. We support only one
// allocation, so every supplied FD must identify the same object as fds[0].
// This helper borrows all FDs and leaves cleanup to the caller.
static int bk_spice_layout_valid(int planes, const uint64_t modifiers[4],
                                 const int fds[4], const int strides[4],
                                 const int offsets[4]) {
  struct stat first, current;
  if (planes < 1 || planes > 4 || fds[0] < 0 ||
      modifiers[0] == UINT64_C(0x00ffffffffffffff) || fstat(fds[0], &first) < 0)
    return 0;
  for (int i = 0; i < planes; i++) {
    if (modifiers[i] != modifiers[0] || strides[i] <= 0 || offsets[i] < 0)
      return 0;
    if (fds[i] < -1)
      return 0;
    if (fds[i] >= 0 &&
        (fstat(fds[i], &current) < 0 || current.st_dev != first.st_dev ||
         current.st_ino != first.st_ino))
      return 0;
  }
  return 1;
}

#endif
