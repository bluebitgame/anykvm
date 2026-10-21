"""Keep the optional Linux SPICE C adapter out of unsupported builds."""
import json
import os
from pathlib import Path
import subprocess
import unittest

ROOT = Path(__file__).resolve().parent.parent


class SPICEBuildTagsTests(unittest.TestCase):
    def test_native_adapter_is_opt_in_and_linux_only(self):
        for system in ("linux", "darwin", "windows"):
            for cgo in ("0", "1"):
                for enabled in (False, True):
                    with self.subTest(system=system, cgo=cgo, spice=enabled):
                        env = dict(os.environ, GOOS=system, GOARCH="amd64", CGO_ENABLED=cgo,
                                   GOFLAGS="", GOCACHE=str(ROOT / "build/go-cache"))
                        command = ["go", "list", "-json"]
                        if enabled:
                            command += ["-tags", "spice"]
                        command += ["./internal/spice"]
                        package = json.loads(subprocess.check_output(command, cwd=ROOT, env=env, text=True))
                        native = system == "linux" and cgo == "1" and enabled
                        files = set(package.get("GoFiles", [])) | set(package.get("CgoFiles", []))
                        self.assertEqual("native_linux.go" in files, native)
                        self.assertEqual("unavailable.go" in files, not native)
                        self.assertEqual("native_linux.c" in package.get("CFiles", []), native)


if __name__ == "__main__":
    unittest.main()
