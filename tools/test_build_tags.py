"""Check Windows encoder and Opus selection across the same build-tag matrix."""
import json
import os
from pathlib import Path
import subprocess
import unittest


ROOT = Path(__file__).resolve().parent.parent
VENDOR_C = {"vendor_windows.c", "nvenc_windows.c", "amf_windows.c", "qsv_windows.c"}


class WindowsBuildTagsTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.packages = {}
        decoder = json.JSONDecoder()
        for arch in ("amd64", "arm64"):
            for cgo in ("0", "1"):
                for ffmpeg in (False, True):
                    env = dict(os.environ, GOOS="windows", GOARCH=arch, CGO_ENABLED=cgo,
                               GOFLAGS="", GOCACHE=str(ROOT / "build/go-cache"))
                    command = ["go", "list", "-json"]
                    if ffmpeg:
                        command += ["-tags", "ffmpeg"]
                    command += ["./internal/gamestream", "./internal/videoenc"]
                    output = subprocess.check_output(command, cwd=ROOT, env=env, text=True)
                    packages = {}
                    while output.strip():
                        package, end = decoder.raw_decode(output.lstrip())
                        packages[package["ImportPath"].rsplit("/", 1)[-1]] = package
                        output = output.lstrip()[end:]
                    cls.packages[arch, cgo, ffmpeg] = packages

    def test_opus_loader_selection(self):
        for (arch, cgo, ffmpeg), packages in self.packages.items():
            with self.subTest(arch=arch, cgo=cgo, ffmpeg=ffmpeg):
                files = set(packages["gamestream"].get("GoFiles", []))
                self.assertEqual("opus_native_windows.go" in files, not ffmpeg)
                self.assertEqual("opus_dll.go" in files, not ffmpeg)
                self.assertEqual("opus_native_other.go" in files, ffmpeg)

    def test_vendor_backends_follow_ffmpeg_tag(self):
        for (arch, cgo, ffmpeg), packages in self.packages.items():
            with self.subTest(arch=arch, cgo=cgo, ffmpeg=ffmpeg):
                package = packages["videoenc"]
                go_files = set(package.get("GoFiles", [])) | set(package.get("CgoFiles", []))
                c_files = set(package.get("CFiles", []))
                tests = set(package.get("TestGoFiles", []))
                flags = package.get("CgoLDFLAGS", [])
                native, library = cgo == "1" and not ffmpeg, cgo == "1" and ffmpeg
                self.assertEqual(VENDOR_C & c_files, VENDOR_C if native else set())
                self.assertEqual("encoder_vendor_windows.go" in go_files, native)
                self.assertEqual("vendor_windows_test.go" in tests, native)
                self.assertEqual("encoder_vendor_ffmpeg_windows.go" in go_files, library)
                self.assertEqual("-ld3d11" in flags, native)
                self.assertEqual("mediafoundation_windows.c" in c_files, cgo == "1")
                self.assertEqual("encoder_av.go" in go_files, library)


if __name__ == "__main__":
    unittest.main()
