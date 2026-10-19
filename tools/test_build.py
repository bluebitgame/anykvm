import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import build


class BuildTests(unittest.TestCase):
    def test_only_linux_builds_require_shader_compilation(self):
        for target in ("darwin/amd64", "windows/arm64", "linux/arm64"):
            with self.subTest(target=target), tempfile.TemporaryDirectory() as temporary, \
                    patch.object(build.subprocess, "check_output", return_value=json.dumps({
                        "GOHOSTOS": "darwin", "GOHOSTARCH": "amd64", "CC": "clang"})), \
                    patch.object(build.prepare_sources, "prepare"), \
                    patch.object(build.prepare_sources, "notice_files", return_value=[]) as notices, \
                    patch.object(build.build_shaders, "prepare") as shaders, \
                    patch.object(build.cross_toolchain, "prepare", return_value=[]), \
                    patch.object(build.package_opus, "runtime_files", return_value=[]), \
                    patch.object(build, "build_target"), \
                    patch.dict(build.os.environ, {}, clear=False), \
                    contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(build.main(["--targets", target, "--output", temporary]), 0)
                self.assertEqual(shaders.call_count, int(target.startswith("linux/")))
                notices.assert_called_once_with({target.split("/")[0]}, False)

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
            self.assertEqual(build.interactive_selection(), (list(build.TARGETS), ""))
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


if __name__ == "__main__":
    unittest.main()
