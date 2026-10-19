import pathlib
import tempfile
import unittest
from unittest.mock import patch

import package_ffmpeg


class PackageFFmpegTests(unittest.TestCase):
    def test_macos_transitive_dependencies_and_aliases(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory).resolve()
            codec = root / "libavcodec.63.dylib"
            codec.touch()
            dependencies = root / "bottles"
            dependencies.mkdir()
            opus = dependencies / "libopus.0.1.dylib"
            opus.touch()
            (dependencies / "libopus.0.dylib").symlink_to(opus.name)
            helper = dependencies / "libhelper.1.dylib"
            helper.touch()
            graph = {codec: ["@@HOMEBREW_PREFIX@@/opt/opus/lib/libopus.0.dylib"],
                     opus: ["@loader_path/libhelper.1.dylib"],
                     helper: [str(codec), "/usr/lib/libSystem.B.dylib"]}
            with patch.object(package_ffmpeg, "dependencies", side_effect=lambda p: graph[p]):
                staged = package_ffmpeg.macos_libraries([codec], [dependencies])
            self.assertEqual(set(staged), {codec.name, opus.name, "libopus.0.dylib", helper.name})
            self.assertEqual(staged["libopus.0.dylib"], opus)

    def test_missing_macos_dependency_leaves_destination_untouched(self):
        with tempfile.TemporaryDirectory() as directory:
            source, destination = pathlib.Path(directory) / "source", pathlib.Path(directory) / "lib"
            source.mkdir()
            for name, major in package_ffmpeg.MAJORS.items():
                (source / f"lib{name}.{major}.dylib").touch()
            with patch.object(package_ffmpeg.sys, "platform", "darwin"), \
                    patch.object(package_ffmpeg, "dependencies", return_value=["/missing/libcodec.1.dylib"]):
                with self.assertRaisesRegex(ValueError, "missing dependency"):
                    package_ffmpeg.stage(source, destination)
            self.assertFalse(destination.exists())

    def test_macos_includes_sdl3_loaded_by_sdl2_compat(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory).resolve()
            compat = root / "libSDL2-2.0.0.dylib"
            compat.write_bytes(b"sdl2-compat")
            sdl3 = root / "libSDL3.0.dylib"
            sdl3.touch()
            (root / "libSDL3.dylib").symlink_to(sdl3.name)
            with patch.object(package_ffmpeg, "dependencies", return_value=[]):
                staged = package_ffmpeg.macos_libraries([compat], [root])
            self.assertEqual(set(staged), {compat.name, sdl3.name, "libSDL3.dylib"})

    def test_windows_includes_codec_dependencies(self):
        with tempfile.TemporaryDirectory() as directory:
            source, destination = pathlib.Path(directory) / "source", pathlib.Path(directory) / "lib"
            source.mkdir()
            for name, major in package_ffmpeg.MAJORS.items():
                (source / f"{name}-{major}.dll").write_bytes(b"library")
            (source / "opus.dll").write_bytes(b"dependency")
            package_ffmpeg.stage(source, destination)
            self.assertEqual((destination / "opus.dll").read_bytes(), b"dependency")

    def test_rejects_wrong_ffmpeg_major(self):
        with tempfile.TemporaryDirectory() as directory:
            source = pathlib.Path(directory)
            (source / "avcodec-62.dll").touch()
            with self.assertRaisesRegex(ValueError, "missing FFmpeg 9 libraries"):
                package_ffmpeg.stage(source, source / "output")

    def test_linux_stages_versioned_libraries_and_symlinks(self):
        with tempfile.TemporaryDirectory() as directory:
            source, destination = pathlib.Path(directory) / "source", pathlib.Path(directory) / "lib"
            source.mkdir()
            for name, major in package_ffmpeg.MAJORS.items():
                library = source / f"lib{name}.so.{major}.1.102"
                library.write_bytes(b"library")
                (source / f"lib{name}.so.{major}").symlink_to(library.name)
            package_ffmpeg.stage(source, destination)
            self.assertEqual((destination / "libavcodec.so.63").read_bytes(), b"library")
            self.assertTrue((destination / "libavcodec.so.63").is_symlink())

    def test_rejects_complete_old_abi_before_staging(self):
        old_majors = {"avutil": 59, "swresample": 5, "swscale": 8, "avcodec": 61,
                      "avformat": 61, "avdevice": 61, "avfilter": 10}
        for platform in ("windows", "linux", "macos"):
            with self.subTest(platform=platform), tempfile.TemporaryDirectory() as directory:
                source = pathlib.Path(directory)
                destination = source / "output"
                for name, major in old_majors.items():
                    filename = (f"{name}-{major}.dll" if platform == "windows" else
                                f"lib{name}.{major}.dylib" if platform == "macos" else
                                f"lib{name}.so.{major}")
                    (source / filename).touch()
                with self.assertRaisesRegex(ValueError, "missing FFmpeg 9 libraries"):
                    package_ffmpeg.stage(source, destination)
                self.assertFalse(destination.exists())


if __name__ == "__main__":
    unittest.main()
