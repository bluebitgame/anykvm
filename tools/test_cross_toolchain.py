import contextlib
import hashlib
import io
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest
from unittest.mock import patch

import cross_toolchain


class Response(io.BytesIO):
    def __init__(self, payload, length, status=200, content_range=None):
        super().__init__(payload)
        self.status = status
        self.headers = {"Content-Length": str(length)}
        if content_range is not None:
            self.headers["Content-Range"] = content_range


class CrossToolchainTests(unittest.TestCase):
    def test_progressing_download_can_resume_more_than_five_times(self):
        payload = b"0123456789abcdefghijklmn"
        calls = []

        def open_response(request, **kwargs):
            offset = int(request.get_header("Range", "bytes=0-")[6:-1])
            calls.append(offset)
            return Response(payload[offset:offset + 3], len(payload) - offset,
                            206 if offset else 200,
                            f"bytes {offset}-{len(payload) - 1}/{len(payload)}" if offset else None)

        with tempfile.TemporaryDirectory() as temporary, \
                patch.object(cross_toolchain, "CACHE", Path(temporary)), \
                patch.object(cross_toolchain.urllib.request, "urlopen", side_effect=open_response), \
                contextlib.redirect_stdout(io.StringIO()):
            result = cross_toolchain.download("https://example.invalid/archive", hashlib.sha256(payload).hexdigest())
            self.assertEqual(result.read_bytes(), payload)
            self.assertEqual(calls, list(range(0, len(payload), 3)))

    def test_download_stops_after_five_attempts_without_progress(self):
        with tempfile.TemporaryDirectory() as temporary, \
                patch.object(cross_toolchain, "CACHE", Path(temporary)), \
                patch.object(cross_toolchain.urllib.request, "urlopen", side_effect=OSError("offline")) as request, \
                contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaisesRegex(ValueError, "Could not download"):
                cross_toolchain.download("https://example.invalid/archive")
            self.assertEqual(request.call_count, 5)

    def test_incomplete_download_resumes_and_verifies_complete_archive(self):
        payload = b"verified archive bytes"
        checksum = hashlib.sha256(payload).hexdigest()
        requests = []

        def open_response(request, **kwargs):
            requests.append(request)
            if len(requests) == 1:
                return Response(payload[:8], len(payload))
            return Response(payload[8:], len(payload) - 8, 206,
                            f"bytes 8-{len(payload) - 1}/{len(payload)}")

        with tempfile.TemporaryDirectory() as temporary, \
                patch.object(cross_toolchain, "CACHE", Path(temporary)), \
                patch.object(cross_toolchain.urllib.request, "urlopen", side_effect=open_response), \
                contextlib.redirect_stdout(io.StringIO()):
            archive = cross_toolchain.download("https://example.invalid/compiler.tar.xz", checksum)
            self.assertEqual(archive.read_bytes(), payload)
            self.assertEqual(requests[1].get_header("Range"), "bytes=8-")
            self.assertFalse(archive.with_name(archive.name + ".part").exists())

    def test_wrong_checksum_never_publishes_download(self):
        with tempfile.TemporaryDirectory() as temporary, \
                patch.object(cross_toolchain, "CACHE", Path(temporary)), \
                patch.object(cross_toolchain.urllib.request, "urlopen",
                             side_effect=lambda *args, **kwargs: Response(b"wrong", 5)), \
                contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaisesRegex(ValueError, "SHA-256"):
                cross_toolchain.download("https://example.invalid/compiler.tar.xz", "0" * 64)
            self.assertFalse((Path(temporary) / "downloads/compiler.tar.xz").exists())
            self.assertFalse((Path(temporary) / "downloads/compiler.tar.xz.part").exists())

    def test_archive_cannot_write_outside_extraction_directory(self):
        for name, link in (("../outside", None), ("escape", "../outside")):
            with self.subTest(name=name), tempfile.TemporaryDirectory() as temporary:
                directory = Path(temporary) / "sdk"
                directory.mkdir()
                data = io.BytesIO()
                member = tarfile.TarInfo(name)
                if link is None:
                    member.size = 4
                else:
                    member.type, member.linkname = tarfile.SYMTYPE, link
                with tarfile.open(fileobj=data, mode="w") as archive:
                    archive.addfile(member, io.BytesIO(b"data") if link is None else None)
                data.seek(0)
                with tarfile.open(fileobj=data) as archive, self.assertRaises(ValueError):
                    cross_toolchain.extract_tar(archive, directory)
                self.assertFalse((Path(temporary) / "outside").exists())

    def test_dry_run_and_explicit_compiler_do_not_download(self):
        with patch.object(cross_toolchain, "download") as download, \
                contextlib.redirect_stdout(io.StringIO()):
            cross_toolchain.prepare("linux/arm64", "darwin/amd64", {}, False, dry_run=True)
            cross_toolchain.prepare("windows/arm64", "darwin/amd64", {}, True, dry_run=True)
            env = {"CC": "custom-cc"}
            with patch.dict("os.environ", {"ANYKVM_BUILD_LINUX_ARM64_CC": "custom-cc"}):
                cross_toolchain.prepare("linux/arm64", "darwin/amd64", env, False)
            self.assertEqual(env, {"CC": "custom-cc"})
            download.assert_not_called()

    def test_debian_absolute_link_resolves_inside_target_sysroot(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary) / "sdk"
            directory.mkdir()
            data = io.BytesIO()
            member = tarfile.TarInfo("lib64/ld-linux.so")
            member.type, member.linkname = tarfile.SYMTYPE, "/lib/target/ld-linux.so"
            with tarfile.open(fileobj=data, mode="w") as archive:
                archive.addfile(member)
            data.seek(0)
            with tarfile.open(fileobj=data) as archive:
                cross_toolchain.extract_tar(archive, directory, sysroot=True)
            self.assertEqual((directory / member.name).resolve(),
                             (directory / "lib/target/ld-linux.so").resolve())

    def test_interrupted_sdl_build_reuses_configuration_and_objects(self):
        attempts = []
        with tempfile.TemporaryDirectory() as temporary:
            cache = Path(temporary)
            sdk = cache / f"SDL2-{cross_toolchain.SDL_VERSION}-windows-arm64"

            def run_cmake(args, **kwargs):
                if "-B" in args:
                    directory = Path(args[args.index("-B") + 1])
                    configuration = directory / "CMakeCache.txt"
                    if attempts:
                        self.assertEqual(configuration.read_text(), "cached checks")
                        self.assertNotIn("-G", args)
                    configuration.write_text("cached checks")
                elif "--build" in args:
                    directory = Path(args[2])
                    attempts.append(directory)
                    object_file = directory / "completed.obj"
                    if len(attempts) == 1:
                        object_file.write_bytes(b"compiled object")
                        raise subprocess.CalledProcessError(1, args)
                    self.assertEqual(object_file.read_bytes(), b"compiled object")
                elif "--install" in args:
                    sdk.mkdir()

            with patch.object(cross_toolchain, "CACHE", cache), \
                    patch.object(cross_toolchain, "sdl_source", return_value=cache / "source"), \
                    patch.object(cross_toolchain, "llvm_mingw", return_value=cache / "compiler"), \
                    patch.object(cross_toolchain.shutil, "which", return_value="installed"), \
                    patch.object(cross_toolchain.subprocess, "run", side_effect=run_cmake):
                with self.assertRaises(subprocess.CalledProcessError):
                    cross_toolchain.windows_sdl("arm64", "darwin/amd64", False)
                self.assertFalse((sdk / ".ready").exists())
                cross_toolchain.windows_sdl("arm64", "darwin/amd64", False)
                self.assertEqual(attempts[0], attempts[1])
                self.assertTrue((sdk / ".ready").exists())


if __name__ == "__main__":
    unittest.main()
