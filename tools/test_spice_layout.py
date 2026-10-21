"""Check EGL plane-layout validation on the build host, without EGL or a GPU."""
import os
from pathlib import Path
import shlex
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent.parent

HARNESS = r'''
#include "internal/spice/native_layout.h"
#include <assert.h>
#include <stdio.h>
#include <unistd.h>

int main(void) {
    FILE *first = tmpfile(), *other = tmpfile();
    assert(first && other);
    int duplicate = dup(fileno(first));
    assert(duplicate >= 0);
    uint64_t modifiers[4] = {7, 7, 7, 7};
    int fds[4] = {fileno(first), -1, -1, -1};
    int strides[4] = {4096, 64, 64, 64};
    int offsets[4] = {0, 4096, 8192, 12288};

    // One allocation may be exported with one FD or duplicate FDs per plane.
    for (int planes = 1; planes <= 4; planes++)
        assert(bk_spice_layout_valid(planes, modifiers, fds, strides, offsets));
    fds[1] = duplicate;
    assert(bk_spice_layout_valid(2, modifiers, fds, strides, offsets));
    fds[1] = fileno(other);
    assert(!bk_spice_layout_valid(2, modifiers, fds, strides, offsets));
    fds[1] = -1;

    modifiers[1] = 8;
    assert(!bk_spice_layout_valid(2, modifiers, fds, strides, offsets));
    modifiers[1] = 7;
    modifiers[0] = UINT64_C(0x00ffffffffffffff);
    assert(!bk_spice_layout_valid(1, modifiers, fds, strides, offsets));
    modifiers[0] = 7;
    strides[1] = 0;
    assert(!bk_spice_layout_valid(2, modifiers, fds, strides, offsets));
    strides[1] = 64;
    offsets[1] = -1;
    assert(!bk_spice_layout_valid(2, modifiers, fds, strides, offsets));
    offsets[1] = 4096;
    assert(!bk_spice_layout_valid(0, modifiers, fds, strides, offsets));
    assert(!bk_spice_layout_valid(5, modifiers, fds, strides, offsets));
    fds[0] = -1;
    assert(!bk_spice_layout_valid(1, modifiers, fds, strides, offsets));
    close(duplicate);
    fclose(other);
    fclose(first);
    return 0;
}
'''


class SPICELayoutTests(unittest.TestCase):
    def test_shared_allocation_and_invalid_layouts(self):
        with tempfile.TemporaryDirectory(prefix="anykvm-spice-layout-") as directory:
            source = Path(directory) / "layout.c"
            binary = Path(directory) / "layout-test"
            source.write_text(HARNESS)
            compiler = shlex.split(os.environ.get("CC", "cc"))
            subprocess.run(compiler + ["-std=c11", "-D_POSIX_C_SOURCE=200809L", "-Wall",
                                      "-Wextra", "-Werror", "-I", str(ROOT), str(source),
                                      "-o", str(binary)], check=True)
            subprocess.run([str(binary)], check=True)


if __name__ == "__main__":
    unittest.main()
