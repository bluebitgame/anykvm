"""Keep FFmpeg JPEG independent of TurboJPEG across supported native builds."""
import json
import os
from pathlib import Path
import subprocess
import unittest


ROOT = Path(__file__).resolve().parent.parent


class JPEGBuildTagsTests(unittest.TestCase):
    def test_backend_selection(self):
        for platform in ("darwin", "linux", "windows"):
            for arch in ("amd64", "arm64"):
                for cgo in ("0", "1"):
                    for ffmpeg in (False, True):
                        with self.subTest(platform=platform, arch=arch, cgo=cgo, ffmpeg=ffmpeg):
                            env = dict(os.environ, GOOS=platform, GOARCH=arch,
                                       CGO_ENABLED=cgo, GOFLAGS="",
                                       GOCACHE=str(ROOT / "build/go-cache"))
                            command = ["go", "list", "-json"]
                            if ffmpeg:
                                command += ["-tags", "ffmpeg"]
                            command += ["./internal/jpegcodec"]
                            package = json.loads(subprocess.check_output(
                                command, cwd=ROOT, env=env, text=True))
                            sources = set(package.get("GoFiles", [])) | set(package.get("CgoFiles", []))
                            native = set(package.get("CFiles", []))
                            imageio = cgo == "1" and platform == "darwin"
                            turbo = cgo == "1" and not ffmpeg
                            self.assertEqual("jpeg_ffmpeg.go" in sources, cgo == "1" and ffmpeg)
                            self.assertEqual("jpeg_turbo.go" in sources, turbo)
                            self.assertEqual("native_turbo.c" in native, turbo)
                            self.assertEqual("transform_turbo.go" in sources, turbo)
                            self.assertEqual("transform_turbo.c" in native, turbo)
                            self.assertEqual("transform_stub.go" in sources, not turbo)
                            self.assertEqual("jpeg_darwin.go" in sources, imageio)
                            self.assertEqual("jpeg_go.go" in sources, cgo == "0")
                            self.assertNotIn("jpeg_simd.go", sources)
                            self.assertNotIn("native_simd.c", native)


if __name__ == "__main__":
    unittest.main()
