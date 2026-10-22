import contextlib
import hashlib
import io
import json
import os
from pathlib import Path
import struct
import tempfile
import unittest
from unittest.mock import patch

import package_ffmpeg
import prepare_runtime as runtime


def library_bytes(system, arch):
    if system == "linux":
        data = bytearray(64)
        data[:6] = b"\x7fELF\x02\x01"
        struct.pack_into("<H", data, 18, 62 if arch == "amd64" else 183)
    elif system == "windows":
        data = bytearray(70)
        data[:2] = b"MZ"
        struct.pack_into("<I", data, 60, 64)
        data[64:68] = b"PE\0\0"
        struct.pack_into("<H", data, 68, 0x8664 if arch == "amd64" else 0xaa64)
    else:
        data = bytearray(64)
        data[:4] = b"\xcf\xfa\xed\xfe"
        struct.pack_into("<I", data, 4, 0x01000007 if arch == "amd64" else 0x0100000c)
    return bytes(data)


class RuntimeTests(unittest.TestCase):
    def test_rejects_wrong_os_architecture_and_corrupt_libraries(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "library"
            targets = [f"{os}/{arch}" for os in ("linux", "windows", "darwin")
                       for arch in ("amd64", "arm64")]
            for target in targets:
                path.write_bytes(library_bytes(*target.split("/")))
                for requested in targets:
                    with self.subTest(target=target, requested=requested):
                        if target == requested:
                            runtime.validate_library(path, requested)
                        else:
                            with self.assertRaisesRegex(ValueError, "does not match"):
                                runtime.validate_library(path, requested)
            for content in (b"", b"not a binary", b"MZ" + b"\0" * 62 + b"PE\0\0"):
                path.write_bytes(content)
                with self.assertRaises(ValueError):
                    runtime.validate_library(path, "windows/amd64")

    def test_universal_macos_library_accepts_both_slices(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "universal.dylib"
            path.write_bytes(struct.pack(">II", 0xcafebabe, 2) +
                             struct.pack(">IIIII", 0x01000007, 0, 0, 0, 0) +
                             struct.pack(">IIIII", 0x0100000c, 0, 0, 0, 0))
            runtime.validate_library(path, "darwin/amd64")
            runtime.validate_library(path, "darwin/arm64")

    def test_offline_cache_is_verified_and_never_downloads(self):
        with tempfile.TemporaryDirectory() as temporary, \
                patch.object(runtime.toolchain, "CACHE", Path(temporary)), \
                patch.object(runtime.toolchain, "download") as download:
            directory = Path(temporary) / "downloads"
            directory.mkdir()
            path = directory / "source.tar.gz"
            path.write_bytes(b"correct")
            checksum = hashlib.sha256(b"correct").hexdigest()
            self.assertEqual(runtime.cached_download("url", checksum, path.name, False), path)
            path.write_bytes(b"corrupt")
            with self.assertRaisesRegex(ValueError, "not cached"):
                runtime.cached_download("url", checksum, path.name, False)
            download.assert_not_called()

    def test_dry_run_does_not_prepare_or_download(self):
        with patch.object(runtime, "turbojpeg") as jpeg, patch.object(runtime, "ffmpeg") as ffmpeg, \
                patch.object(runtime, "windows_opus") as opus, \
                patch.object(runtime, "linux_libraries") as linux, \
                patch.object(runtime.toolchain, "download") as download, \
                contextlib.redirect_stdout(io.StringIO()):
            for target in (*runtime.FFMPEG_BUNDLES, "darwin/amd64", "darwin/arm64"):
                for tags in ("", "viewer", "ffmpeg", "viewer,ffmpeg", "spice"):
                    self.assertEqual(runtime.prepare(target, "darwin/amd64", {}, tags, True), [])
            for tool in (jpeg, ffmpeg, opus, linux, download):
                tool.assert_not_called()

    def test_debian_dependency_closure_handles_alternatives_virtuals_and_cycles(self):
        packages = {"root": {"Depends": "helper (>= 1), missing | virtual:any, libc6"},
                    "helper": {"Depends": "root", "Pre-Depends": "libgcc-s1"},
                    "virtual": {"Package": "provider", "Depends": "helper"}}
        self.assertEqual(set(runtime.debian_closure(packages, ["root"])), {"root", "helper", "virtual"})
        with self.assertRaisesRegex(ValueError, "Cannot resolve"):
            runtime.debian_closure({"root": {"Depends": "unavailable"}}, ["root"])

    def test_prebuilt_ffmpeg_download_is_pinned_for_each_target(self):
        with patch.object(runtime, "cached_download", return_value=Path("archive")) as download, \
                patch.object(runtime.toolchain, "install_archive"):
            for target, (platform, checksum) in runtime.FFMPEG_BUNDLES.items():
                directory = runtime.ffmpeg(target, "darwin/amd64", {}, False)
                url, actual_checksum, name, online = download.call_args.args
                self.assertIn(runtime.FFMPEG_RELEASE, url)
                self.assertIn(platform + "-gpl-shared-9.0", name)
                self.assertEqual(actual_checksum, checksum)
                self.assertFalse(online)
                self.assertEqual(directory.name, "bin" if target.startswith("windows/") else "lib")

    def test_linux_native_and_ffmpeg_bundles_follow_selected_features(self):
        with tempfile.TemporaryDirectory() as temporary, \
                patch.object(runtime.toolchain, "CACHE", Path(temporary) / "cache"), \
                patch.object(runtime, "linux_libraries", return_value=[]) as linux, \
                contextlib.redirect_stdout(io.StringIO()):
            root = Path(temporary)
            jpeg = root / "libturbojpeg.so.0"
            jpeg.write_bytes(library_bytes("linux", "arm64"))
            notice = root / "LICENSE.md"
            notice.write_text("license")
            ffmpeg_dir = root / "ffmpeg"
            ffmpeg_dir.mkdir()
            for name, major in package_ffmpeg.MAJORS.items():
                (ffmpeg_dir / f"lib{name}.so.{major}").write_bytes(library_bytes("linux", "arm64"))
            with patch.object(runtime, "turbojpeg", return_value=([jpeg], [notice])) as prepare_jpeg, \
                    patch.object(runtime, "ffmpeg", return_value=ffmpeg_dir) as ffmpeg:
                for tags in ("viewer", "viewer,ffmpeg"):
                    env = {}
                    files = runtime.prepare("linux/arm64", "darwin/amd64", env, tags)
                    names = {relative.as_posix() for _, relative in files}
                    self.assertEqual("lib/libturbojpeg.so.0" in names, "ffmpeg" not in tags)
                    self.assertEqual("licenses/turbojpeg/LICENSE.md" in names, "ffmpeg" not in tags)
                    self.assertEqual("lib/libavcodec.so.63" in names, "ffmpeg" in tags)
                    self.assertIn("--disable-new-dtags,-rpath,$ORIGIN/lib", env["CGO_LDFLAGS"])
                    linux.assert_called_with("arm64", set(tags.split(",")), True)
                ffmpeg.assert_called_once()
                prepare_jpeg.assert_called_once()

    def test_target_override_rejects_host_library(self):
        with tempfile.TemporaryDirectory() as temporary, \
                patch.object(runtime.toolchain, "CACHE", Path(temporary) / "cache"), \
                contextlib.redirect_stdout(io.StringIO()):
            path = Path(temporary) / "libturbojpeg.so.0"
            path.write_bytes(library_bytes("linux", "amd64"))
            with self.assertRaisesRegex(ValueError, "does not match linux/arm64"):
                runtime.prepare("linux/arm64", "linux/amd64",
                                {"ANYKVM_BUILD_LINUX_ARM64_TURBOJPEG_LIBDIR": temporary})

    def test_native_windows_bundles_compiled_opus_with_loader_filename(self):
        with tempfile.TemporaryDirectory() as temporary, \
                patch.object(runtime.toolchain, "CACHE", Path(temporary) / "cache"), \
                contextlib.redirect_stdout(io.StringIO()):
            root = Path(temporary)
            jpeg, opus, notice = root / "libturbojpeg.dll", root / "libopus-0.dll", root / "COPYING"
            for path in (jpeg, opus):
                path.write_bytes(library_bytes("windows", "arm64"))
            notice.write_text("license")
            with patch.object(runtime, "turbojpeg", return_value=([jpeg], [])), \
                    patch.object(runtime, "windows_opus", return_value=(opus, notice)) as prepare_opus, \
                    patch.object(runtime, "ffmpeg") as prepare_ffmpeg:
                files = runtime.prepare("windows/arm64", "darwin/amd64", {}, allow_download=False)
                names = {str(relative) for _, relative in files}
                self.assertIn("lib/opus.dll", names)
                self.assertIn("lib/opus-LICENSE.txt", names)
                prepare_opus.assert_called_once_with("windows/arm64", {}, False)
                prepare_ffmpeg.assert_not_called()

    def test_offline_viewer_requires_cached_sdl_and_packages_it(self):
        with tempfile.TemporaryDirectory() as temporary, \
                patch.object(runtime.toolchain, "CACHE", Path(temporary)), \
                patch.object(runtime.toolchain, "windows_sdl") as prepare_sdl:
            with self.assertRaisesRegex(ValueError, "SDL2.*not cached"):
                runtime.viewer_libraries("windows/amd64", "windows/amd64", False)
            directory = Path(temporary) / f"SDL2-{runtime.toolchain.SDL_VERSION}-windows"
            (directory / "lib/x64").mkdir(parents=True)
            (directory / ".ready").touch()
            (directory / "LICENSE.txt").write_text("license")
            (directory / "lib/x64/SDL2.dll").write_bytes(library_bytes("windows", "amd64"))
            files = runtime.viewer_libraries("windows/amd64", "windows/amd64", False)
            self.assertEqual({str(relative) for _, relative in files}, {"SDL2.dll", "SDL2-LICENSE.txt"})
            prepare_sdl.assert_not_called()

    def test_windows_arm_opus_uses_mandatory_neon_without_runtime_cpu_probe(self):
        with tempfile.TemporaryDirectory() as temporary, \
                patch.object(runtime.toolchain, "CACHE", Path(temporary)), \
                patch.object(runtime, "source", return_value=Path(temporary)), \
                patch.object(runtime, "cmake_build") as cmake:
            prefix = Path(temporary) / "runtime-opus-1.5.2-windows-arm64"
            (prefix / "bin").mkdir(parents=True)
            (prefix / "bin/opus.dll").write_bytes(library_bytes("windows", "arm64"))
            runtime.windows_opus("windows/arm64", {"CC": "clang"}, False)
            options = cmake.call_args.args[4]
            self.assertIn("-DOPUS_MAY_HAVE_NEON=OFF", options)
            self.assertIn("-DOPUS_PRESUME_NEON=ON", options)
            self.assertFalse(cmake.call_args.args[5])

    def test_cmake_receives_target_compiler_and_flags_separately(self):
        with tempfile.TemporaryDirectory() as temporary, \
                patch.object(runtime.shutil, "which", return_value="tool"), \
                patch.object(runtime.subprocess, "run") as run:
            env = {"CC": '"/compiler path/clang" --target=aarch64-linux-gnu --sysroot=/sdk',
                   "CGO_CFLAGS": "-I/sdk/include", "CGO_LDFLAGS": "-L/sdk/lib"}
            runtime.cmake_build(Path(temporary), Path(temporary) / "prefix", "linux/arm64", env)
            arguments = run.call_args_list[0].args[0]
            self.assertIn("-DCMAKE_C_COMPILER=/compiler path/clang", arguments)
            self.assertIn("-DCMAKE_SYSTEM_PROCESSOR=aarch64", arguments)
            self.assertIn("-DCMAKE_C_FLAGS=--target=aarch64-linux-gnu --sysroot=/sdk -I/sdk/include", arguments)
            self.assertNotIn("CC", run.call_args_list[0].kwargs["env"])

    def test_cmake_normalizes_windows_tool_paths(self):
        for arch, cpu in (("amd64", "x86_64"), ("arm64", "aarch64")):
            with self.subTest(arch=arch), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary) / "compiler path"
                root.mkdir()
                suffix = ".exe" if os.name == "nt" else ""
                compiler = root / (cpu + "-w64-mingw32-clang" + suffix)
                windres = root / (cpu + "-w64-mingw32-windres" + suffix)
                windres.touch()
                env = {"CC": '"' + str(compiler).replace("/", "\\") + '"'}
                with patch.object(runtime, "build_tool", return_value=r"C:\Program Files\tools\ninja.exe"), \
                        patch.object(runtime.subprocess, "run") as run:
                    runtime.cmake_build(root, root / "prefix", "windows/" + arch, env)
                arguments = run.call_args_list[0].args[0]
                self.assertIn("-DCMAKE_C_COMPILER=" + compiler.as_posix(), arguments)
                self.assertIn("-DCMAKE_RC_COMPILER=" + windres.as_posix(), arguments)
                self.assertIn("-DCMAKE_MAKE_PROGRAM=C:/Program Files/tools/ninja.exe", arguments)
                for argument in arguments[1:]:
                    self.assertNotIn("\\", argument)

    def test_bootstrap_tools_use_host_architecture_and_verified_assets(self):
        for system, arch, name, asset_name in (
                ("Windows", "ARM64", "ninja", "ninja-winarm64.zip"),
                ("Linux", "aarch64", "ninja", "ninja-linux-aarch64.zip"),
                ("Darwin", "x86_64", "cmake", "cmake-4.1.2-macos-universal.tar.gz")):
            with self.subTest(system=system, arch=arch), tempfile.TemporaryDirectory() as temporary, \
                    patch.object(runtime.toolchain, "CACHE", Path(temporary)), \
                    patch.object(runtime.shutil, "which", return_value=None), \
                    patch.object(runtime.platform, "system", return_value=system), \
                    patch.object(runtime.platform, "machine", return_value=arch), \
                    patch.object(runtime.toolchain, "install_archive") as extract:
                root = Path(temporary)
                manifest = root / "manifest.json"
                manifest.write_text(json.dumps({"assets": [{"name": asset_name,
                    "browser_download_url": "https://example.invalid/" + asset_name,
                    "digest": "sha256:" + "a" * 64}]}))

                def unpack(archive, directory):
                    directory.mkdir()
                    (directory / (name + (".exe" if system == "Windows" else ""))).touch()

                extract.side_effect = unpack
                with patch.object(runtime, "cached_download", side_effect=[manifest, root / asset_name]) as download:
                    result = runtime.build_tool(name, False)
                    self.assertTrue(Path(result).is_file())
                    self.assertEqual(download.call_args.args[1:4], ("a" * 64, asset_name, False))


if __name__ == "__main__":
    unittest.main()
