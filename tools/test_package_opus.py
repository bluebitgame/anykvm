import contextlib
import hashlib
import io
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import zipfile

import package_opus


class OpusPackagingTests(unittest.TestCase):
    def test_skips_ffmpeg_and_other_platforms_without_downloading(self):
        with patch.object(package_opus, "prepare") as prepare:
            for target, tags in (("linux/amd64", ""), ("darwin/arm64", ""),
                                 ("windows/amd64", "ffmpeg"),
                                 ("windows/arm64", "viewer,ffmpeg")):
                self.assertEqual(package_opus.runtime_files(target, tags), [])
            prepare.assert_not_called()

    def test_dry_run_does_not_prepare_or_download(self):
        with patch.object(package_opus, "prepare") as prepare, \
                contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(package_opus.runtime_files("windows/arm64", dry_run=True), [])
            prepare.assert_not_called()

    def test_optional_download_failure_keeps_build_available(self):
        output = io.StringIO()
        with patch.object(package_opus, "prepare", side_effect=ValueError("network unavailable")), \
                contextlib.redirect_stderr(output):
            self.assertEqual(package_opus.runtime_files("windows/amd64"), [])
        self.assertIn("Audio will be disabled", output.getvalue())

    def test_no_bootstrap_never_downloads(self):
        with tempfile.TemporaryDirectory() as temporary, \
                patch.object(package_opus.cross_toolchain, "CACHE", Path(temporary)), \
                patch.object(package_opus.cross_toolchain, "download") as download, \
                contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(package_opus.runtime_files("windows/amd64", allow_download=False), [])
            download.assert_not_called()

    def test_architecture_selection_cache_repair_and_license(self):
        payloads = {"amd64": b"x64 DLL", "arm64": b"ARM64 DLL"}
        license_data = b"Opus license"
        dlls = {arch: (rid, hashlib.sha256(payloads[arch]).hexdigest())
                for arch, (rid, _) in package_opus.DLLS.items()}
        with tempfile.TemporaryDirectory() as temporary:
            cache = Path(temporary)
            package = cache / "downloads" / package_opus.PACKAGE
            package.parent.mkdir()
            with zipfile.ZipFile(package, "w") as archive:
                for arch, (rid, _) in dlls.items():
                    archive.writestr(f"runtimes/{rid}/native/opus.dll", payloads[arch])
                archive.writestr("LICENSE.txt", license_data)
                archive.writestr("../escaped", b"never extract")
            with patch.object(package_opus.cross_toolchain, "CACHE", cache), \
                    patch.object(package_opus, "DLLS", dlls), \
                    patch.object(package_opus, "CHECKSUM", hashlib.sha256(package.read_bytes()).hexdigest()), \
                    patch.object(package_opus, "LICENSE_CHECKSUM", hashlib.sha256(license_data).hexdigest()), \
                    patch.object(package_opus.cross_toolchain, "download") as download:
                for arch in payloads:
                    files = package_opus.runtime_files("windows/" + arch, "viewer", allow_download=False)
                    self.assertEqual(len(files), 2)
                    self.assertEqual(files[0][0].read_bytes(), payloads[arch])
                    self.assertEqual(files[0][1], Path("lib/opus.dll"))
                    self.assertEqual(files[1][0].read_bytes(), license_data)
                    files[0][0].write_bytes(b"damaged DLL")
                    repaired, _ = package_opus.prepare(arch, allow_download=False)
                    self.assertEqual(repaired.read_bytes(), payloads[arch])
                self.assertFalse((cache / "escaped").exists())
                # A wrong-architecture payload must not be published.
                with patch.object(package_opus, "DLLS", {"amd64": dlls["arm64"][:1] + (dlls["amd64"][1],)}):
                    dll = cache / f"opus-{package_opus.VERSION}-amd64/opus.dll"
                    dll.unlink()
                    with self.assertRaisesRegex(ValueError, "checksum mismatch"):
                        package_opus.prepare("amd64", allow_download=False)
                    self.assertFalse(dll.exists())
                download.assert_not_called()


if __name__ == "__main__":
    unittest.main()
