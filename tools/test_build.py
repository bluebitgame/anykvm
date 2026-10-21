import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest
import subprocess
from unittest.mock import patch

import build


class BuildTests(unittest.TestCase):
    def setUp(self):
        for name, value in (("system", "Linux"), ("machine", "x86_64")):
            patcher = patch.object(build.platform, name, return_value=value)
            patcher.start()
            self.addCleanup(patcher.stop)

    def test_only_linux_builds_require_shader_compilation(self):
        for target in ("darwin/amd64", "windows/arm64", "linux/arm64"):
            with self.subTest(target=target), tempfile.TemporaryDirectory() as temporary, \
                    patch.object(build.subprocess, "check_output", return_value=json.dumps({
                        "GOHOSTOS": "darwin", "GOHOSTARCH": "amd64", "CC": "clang"})), \
                    patch.object(build.prepare_sources, "prepare"), \
                    patch.object(build, "prepare_windows_cross_headers"), \
                    patch.object(build.prepare_sources, "notice_files", return_value=[]) as notices, \
                    patch.object(build.build_shaders, "prepare") as shaders, \
                    patch.object(build.cross_toolchain, "prepare", return_value=[]), \
                    patch.object(build.prepare_runtime, "prepare", return_value=[]) as runtimes, \
                    patch.object(build, "build_target"), \
                    patch.dict(build.os.environ, {}, clear=False), \
                    contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(build.main(["--targets", target, "--output", temporary]), 0)
                self.assertEqual(shaders.call_count, int(target.startswith("linux/")))
                notices.assert_called_once_with({target.split("/")[0]}, False)
                self.assertEqual(runtimes.call_args.args[0:2], (target, "darwin/amd64"))
                self.assertEqual(runtimes.call_args.args[3:5], ("", False))
                self.assertTrue(runtimes.call_args.kwargs["allow_download"])

    def test_windows_header_adjustment_only_for_cross_builds(self):
        original = b"#include <Ws2tcpip.h>\n"
        for host, targets, dry_run, changed in (
                ("windows/amd64", ["windows/arm64"], False, False),
                ("linux/amd64", ["linux/arm64"], False, False),
                ("linux/amd64", ["windows/amd64"], True, False),
                ("linux/amd64", ["windows/arm64"], False, True),
                ("darwin/arm64", ["linux/arm64", "windows/amd64"], False, True)):
            with self.subTest(host=host, targets=targets, dry_run=dry_run), \
                    tempfile.TemporaryDirectory() as temporary, \
                    patch.object(build, "ROOT", Path(temporary)):
                header = build.ROOT / "internal/gamestream/native/third_party/enet/include/enet/win32.h"
                header.parent.mkdir(parents=True)
                header.write_bytes(original)
                build.prepare_windows_cross_headers(host, targets, dry_run)
                expected = original.replace(b"Ws2tcpip", b"ws2tcpip") if changed else original
                self.assertEqual(header.read_bytes(), expected)
                build.prepare_windows_cross_headers(host, targets, dry_run)
                self.assertEqual(header.read_bytes(), expected)

    def test_missing_required_runtime_fails_target_before_publishing(self):
        with tempfile.TemporaryDirectory() as temporary, \
                patch.object(build.subprocess, "check_output", return_value=json.dumps({
                    "GOHOSTOS": "darwin", "GOHOSTARCH": "amd64", "CC": "clang"})), \
                patch.object(build.prepare_sources, "prepare"), \
                patch.object(build.prepare_runtime, "prepare", side_effect=ValueError("runtime is not cached")) as runtime, \
                patch.object(build, "build_target") as compile, \
                patch.dict(build.os.environ, {}, clear=False), \
                contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            result = build.main(["--targets", "darwin/amd64", "--tags", "ffmpeg",
                                 "--no-bootstrap", "--output", temporary])
            self.assertEqual(result, 1)
            self.assertEqual(runtime.call_args.args[3:5], ("ffmpeg", False))
            self.assertFalse(runtime.call_args.kwargs["allow_download"])
            compile.assert_not_called()

    def test_interactive_media_choice_follows_existing_menus(self):
        for choice, tags in (("0", ""), ("1", "ffmpeg")):
            with self.subTest(choice=choice), patch("builtins.input", side_effect=["1", "2", "0", choice]), \
                    contextlib.redirect_stdout(io.StringIO()):
                targets, actual = build.interactive_selection()
                self.assertEqual((targets, actual), (["linux/arm64"], tags))

    def test_interactive_viewer_combines_with_ffmpeg(self):
        with patch("builtins.input", side_effect=["1", "2", "1", "1"]), \
                contextlib.redirect_stdout(io.StringIO()):
            targets, tags = build.interactive_selection()
        self.assertEqual(targets, ["linux/arm64"])
        self.assertEqual(tags, "viewer,ffmpeg")

    def test_optional_feature_numbers_and_labels(self):
        for viewer, media, tags in (("0", "0", ""), ("0", "1", "ffmpeg"),
                                    ("1", "0", "viewer"), ("1", "1", "viewer,ffmpeg"),
                                    ("", "", ""), (" ", "1", "ffmpeg"),
                                    ("1", "", "viewer")):
            output = io.StringIO()
            with self.subTest(viewer=viewer, media=media), \
                    patch("builtins.input", side_effect=["1", "2", viewer, media]), \
                    contextlib.redirect_stdout(output):
                targets, actual = build.interactive_selection()
            self.assertEqual((targets, actual), (["linux/arm64"], tags))
            self.assertEqual(output.getvalue().count("0) No (default)"), 2)
            self.assertEqual(output.getvalue().count("1) Yes"), 2)
        output = io.StringIO()
        with patch("builtins.input", side_effect=["", "", "", ""]), \
                contextlib.redirect_stdout(output):
            self.assertEqual(build.interactive_selection(), (["linux/amd64"], ""))
        titles = ("Select target platform", "Select target architecture", "Include viewer", "Include FFmpeg")
        positions = [output.getvalue().index(title) for title in titles]
        self.assertEqual(positions, sorted(positions))

    def test_default_and_ffmpeg_commands(self):
        for tags in ("", "ffmpeg", "viewer", "viewer,ffmpeg"):
            with self.subTest(tags=tags), tempfile.TemporaryDirectory() as temporary:
                commands = []
                notices = []
                for dependency in ("go-ios", "rootshell-vnc"):
                    notice = Path(temporary) / f"{dependency}-LICENSE"
                    notice.write_bytes(f"{dependency} license\n".encode())
                    notices.append((notice, Path(f"licenses/{dependency}/LICENSE")))

                def compile(args, **kwargs):
                    commands.append(args)
                    Path(args[args.index("-o") + 1]).write_bytes(b"compiled")

                with patch.object(build.shutil, "which", return_value="cc"), \
                        patch.object(build.subprocess, "run", side_effect=compile), \
                        contextlib.redirect_stdout(io.StringIO()):
                    build.build_target("linux/arm64", Path(temporary),
                                       {"CC": "cc"}, tags=tags,
                                       runtime_files=notices)
                for args in commands:
                    if tags:
                        self.assertEqual(args[args.index("-tags") + 1], tags)
                    else:
                        self.assertNotIn("-tags", args)
                self.assertEqual(len(commands), 1)
                self.assertEqual(commands[0][-1], "./cmd/anykvm")
                self.assertTrue((Path(temporary) / "linux-arm64" / "anykvm").is_file())
                for notice, relative in notices:
                    self.assertEqual((Path(temporary) / "linux-arm64" / relative).read_bytes(), notice.read_bytes())

    def test_rebuild_removes_only_previous_managed_files_and_failure_preserves_bundle(self):
        with tempfile.TemporaryDirectory() as temporary, \
                patch.object(build.shutil, "which", return_value="cc"), \
                contextlib.redirect_stdout(io.StringIO()):
            root = Path(temporary)
            output = root / "output"
            output.mkdir()
            destination = output / "linux-arm64"
            old_lib, jpeg = root / "libavcodec.so.63", root / "libturbojpeg.so.0"
            old_lib.write_bytes(b"ffmpeg")
            jpeg.write_bytes(b"jpeg")

            def compile(args, **kwargs):
                Path(args[args.index("-o") + 1]).write_bytes(b"compiled")

            with patch.object(build.subprocess, "run", side_effect=compile):
                build.build_target("linux/arm64", output, {"CC": "cc"},
                                   runtime_files=[(old_lib, Path("lib") / old_lib.name)])
                custom = destination / "lib/custom.so"
                custom.write_bytes(b"user library")
                build.build_target("linux/arm64", output, {"CC": "cc"},
                                   runtime_files=[(jpeg, Path("lib") / jpeg.name)])
            self.assertFalse((destination / "lib" / old_lib.name).exists())
            self.assertEqual(custom.read_bytes(), b"user library")
            manifest = (destination / ".runtime-files.json").read_bytes()
            with patch.object(build.subprocess, "run", side_effect=subprocess.CalledProcessError(1, "go")):
                with self.assertRaises(subprocess.CalledProcessError):
                    build.build_target("linux/arm64", output, {"CC": "cc"}, runtime_files=[])
            self.assertEqual((destination / "anykvm").read_bytes(), b"compiled")
            self.assertEqual((destination / "lib" / jpeg.name).read_bytes(), b"jpeg")
            self.assertEqual((destination / ".runtime-files.json").read_bytes(), manifest)

    @unittest.skipIf(build.os.name == "nt", "Unix library symlinks")
    def test_runtime_aliases_are_preserved_and_duplicate_entries_are_deduplicated(self):
        with tempfile.TemporaryDirectory() as temporary, \
                patch.object(build.shutil, "which", return_value="cc"), \
                contextlib.redirect_stdout(io.StringIO()):
            root = Path(temporary)
            library = root / "libcodec.so.1.0"
            library.write_bytes(b"shared library")
            alias = root / "libcodec.so.1"
            alias.symlink_to(library.name)

            def compile(args, **kwargs):
                Path(args[args.index("-o") + 1]).touch()

            files = [(library, Path("lib") / library.name), (alias, Path("lib") / alias.name)]
            with patch.object(build.subprocess, "run", side_effect=compile):
                build.build_target("linux/arm64", root, {"CC": "cc"}, runtime_files=files * 2)
            packaged = root / "linux-arm64/lib" / alias.name
            self.assertTrue(packaged.is_symlink())
            self.assertEqual(packaged.read_bytes(), b"shared library")


if __name__ == "__main__":
    unittest.main()
