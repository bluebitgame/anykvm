"""Exercise remote SPICE pixel conversion and primary lifecycle without a VM or SPICE libraries."""
import os
from pathlib import Path
import shlex
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent.parent


class SPICEBitmapTests(unittest.TestCase):
    def test_formats_padding_and_orientation(self):
        self.compile_and_run("bitmap_pixels.c")

    def test_primary_lifecycle_and_owned_snapshots(self):
        self.compile_and_run("bitmap_lifecycle.c")

    def compile_and_run(self, fixture):
        with tempfile.TemporaryDirectory(prefix="anykvm-spice-bitmap-") as directory:
            source = ROOT / "internal/spice/testdata" / fixture
            binary = Path(directory) / "bitmap-test"
            compiler = shlex.split(os.environ.get("CC", "cc"))
            subprocess.run(compiler + ["-std=c11", "-Wall", "-Wextra", "-Werror", "-I",
                                      str(ROOT), str(source), "-o", str(binary)], check=True)
            subprocess.run([str(binary)], check=True)


if __name__ == "__main__":
    unittest.main()
