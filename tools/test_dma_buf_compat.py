"""Compile DMA-BUF compatibility declarations against old and new header layouts."""
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent.parent


class DMABufCompatibilityTests(unittest.TestCase):
    def test_old_new_and_partial_headers(self):
        compiler = shutil.which("cc") or shutil.which("clang")
        if not compiler:
            self.skipTest("C compiler unavailable")
        base = """#include <stdint.h>
#include <sys/ioctl.h>
typedef uint32_t __u32;
typedef int32_t __s32;
#define DMA_BUF_BASE 'b'
#define DMA_BUF_SYNC_READ 1
"""
        export = """struct dma_buf_export_sync_file { __u32 flags; __s32 fd; };
#define DMA_BUF_IOCTL_EXPORT_SYNC_FILE _IOWR(DMA_BUF_BASE, 2, struct dma_buf_export_sync_file)
"""
        imported = """struct dma_buf_import_sync_file { __u32 flags; __s32 fd; };
#define DMA_BUF_IOCTL_IMPORT_SYNC_FILE _IOW(DMA_BUF_BASE, 3, struct dma_buf_import_sync_file)
"""
        source = """#include <stddef.h>
#include "dma_buf_compat_linux.h"
#include "dma_buf_compat_linux.h"
_Static_assert(sizeof(struct dma_buf_export_sync_file) == 8, "export ABI size");
_Static_assert(sizeof(struct dma_buf_import_sync_file) == 8, "import ABI size");
_Static_assert(offsetof(struct dma_buf_export_sync_file, fd) == 4, "export fd offset");
_Static_assert(offsetof(struct dma_buf_import_sync_file, fd) == 4, "import fd offset");
int main(void) {
    struct dma_buf_export_sync_file out = {.flags = DMA_BUF_SYNC_READ, .fd = -1};
    struct dma_buf_import_sync_file in = {.flags = DMA_BUF_SYNC_READ, .fd = out.fd};
    return in.fd != -1 || !DMA_BUF_IOCTL_EXPORT_SYNC_FILE || !DMA_BUF_IOCTL_IMPORT_SYNC_FILE;
}
"""
        for name, declarations in (("old", ""), ("new", export + imported),
                                   ("export-only", export), ("import-only", imported)):
            with self.subTest(headers=name), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                (root / "linux").mkdir()
                (root / "linux/dma-buf.h").write_text(base + declarations)
                code = root / "test.c"
                code.write_text(source)
                binary = root / "test"
                subprocess.run([compiler, "-std=c11", "-Wall", "-Wextra", "-Werror",
                                "-I", str(root), "-I", str(ROOT / "internal/gpuconv"),
                                str(code), "-o", str(binary)], check=True)
                subprocess.run([str(binary)], check=True)


if __name__ == "__main__":
    unittest.main()
