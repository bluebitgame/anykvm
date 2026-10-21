"""Exercise the native Pi display/render association on any C build host."""
import pathlib
import shutil
import subprocess
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parent.parent


class DRMMatchTests(unittest.TestCase):
    def test_pi_and_exact_device_association(self):
        compiler = shutil.which("cc") or shutil.which("clang")
        if not compiler:
            self.skipTest("C compiler unavailable")
        with tempfile.TemporaryDirectory() as directory:
            binary = pathlib.Path(directory) / "drm-match"
            subprocess.run([
                compiler, "-std=c11", "-D_POSIX_C_SOURCE=200809L", "-Wall", "-Wextra", "-Werror",
                "-I", str(ROOT / "internal/gpuconv/third_party"),
                str(ROOT / "internal/gpuconv/testdata/drm_match.c"), "-o", str(binary),
            ], check=True)
            subprocess.run([str(binary)], check=True)
