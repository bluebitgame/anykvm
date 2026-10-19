import contextlib
import io
from pathlib import Path
import struct
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import generate_test_media as media


def box(kind, payload):
    return struct.pack(">I4s", len(payload) + 8, kind) + payload


def audio_mp4(co64=False, fixed=False, outside_media=False):
    samples = [b"a", b"b", b"c"] if fixed else [b"a", b"bc", b"def"]
    size = 1 if fixed else 0
    stsz = box(b"stsz", struct.pack(">III", 0, size, 3) +
               (b"" if fixed else struct.pack(">III", 1, 2, 3)))
    offsets = (8, 8 + len(samples[0]) + len(samples[1]))
    if outside_media:
        offsets = (0, 0)
    stco = box(b"co64" if co64 else b"stco", struct.pack(">II", 0, 2) +
               struct.pack(">QQ" if co64 else ">II", *offsets))
    stsc = box(b"stsc", struct.pack(">II", 0, 2) + struct.pack(">IIIIII", 1, 2, 1, 2, 1, 1))
    table = box(b"stbl", stsz + stco + stsc)
    for kind in (b"minf", b"mdia", b"trak", b"moov"):
        table = box(kind, table)
    return box(b"mdat", b"".join(samples)) + table, samples


class MediaGenerationTests(unittest.TestCase):
    def test_generated_files_are_ignored_but_shared_readme_is_not(self):
        paths = [name for names in media.GROUPS.values() for name in names]
        result = subprocess.run(["git", "check-ignore", "--no-index", "--stdin"],
                                input="\n".join(paths) + "\n", cwd=media.ROOT,
                                text=True, capture_output=True, check=True)
        self.assertEqual(set(result.stdout.splitlines()), set(paths))
        result = subprocess.run(["git", "check-ignore", "--no-index", "internal/testdata/README.md"],
                                cwd=media.ROOT, text=True, capture_output=True)
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)

    def test_extracts_variable_and_fixed_samples_across_chunks(self):
        for wide in (False, True):
            for fixed in (False, True):
                with self.subTest(wide=wide, fixed=fixed):
                    data, samples = audio_mp4(wide, fixed)
                    self.assertEqual(media.audio_packets(data), samples)

    def test_rejects_truncated_boxes_and_samples_outside_media(self):
        for data in (b"short", struct.pack(">I4s", 100, b"moov"),
                     struct.pack(">I4s", 1, b"moov")):
            with self.assertRaises(ValueError):
                list(media.boxes(data))
        data, _ = audio_mp4(outside_media=True)
        with self.assertRaisesRegex(ValueError, "sample range"):
            media.audio_packets(data)

    def test_supports_extended_and_to_eof_box_sizes(self):
        extended = struct.pack(">I4sQ", 1, b"free", 17) + b"x"
        final = struct.pack(">I4s", 0, b"mdat") + b"abc"
        self.assertEqual(list(media.boxes(extended + final)),
                         [(b"free", 16, 17), (b"mdat", 25, 28)])

    def test_cache_repairs_both_members_of_a_changed_pair(self):
        names = ("internal/testdata/input.mp4", "internal/testdata/expected.h264")
        generation = 0

        def encode(directory, _):
            nonlocal generation
            generation += 1
            for name in names:
                (directory / name).write_bytes(bytes([generation]))

        with tempfile.TemporaryDirectory() as temporary, \
                patch.object(media, "GROUPS", {"video": names}), \
                patch.object(media.shutil, "which", return_value="ffmpeg"), \
                patch.object(media, "run", return_value="libx264 libx265 libaom-av1 aac"), \
                patch.object(media, "generate_video", side_effect=encode), \
                contextlib.redirect_stdout(io.StringIO()):
            root = Path(temporary)
            media.prepare(root, ["video"])
            with patch.object(media.shutil, "which", side_effect=AssertionError("unexpected encoder")):
                media.prepare(root, ["video"])
                media.prepare(root, ["video"], check=True)
            (root / names[1]).write_bytes(b"corrupt")
            with self.assertRaisesRegex(ValueError, "outdated"):
                media.prepare(root, ["video"], check=True)
            media.prepare(root, ["video"])
            self.assertEqual(generation, 2)
            self.assertTrue(all((root / name).read_bytes() == b"\x02" for name in names))
            (root / "build/test-media.json").write_text("null")
            with self.assertRaisesRegex(ValueError, "outdated"):
                media.prepare(root, ["video"], check=True)
            media.prepare(root, ["video"])
            self.assertEqual(generation, 3)

    def test_failed_generation_keeps_existing_files(self):
        names = ("internal/testdata/input.mp4", "internal/testdata/expected.h264")

        def incomplete(directory, _):
            (directory / names[0]).write_bytes(b"new")

        with tempfile.TemporaryDirectory() as temporary, \
                patch.object(media, "GROUPS", {"video": names}), \
                patch.object(media.shutil, "which", return_value="ffmpeg"), \
                patch.object(media, "run", return_value="libx264 libx265 libaom-av1 aac"), \
                patch.object(media, "generate_video", side_effect=incomplete), \
                contextlib.redirect_stdout(io.StringIO()):
            root = Path(temporary)
            (root / "internal/testdata").mkdir(parents=True)
            (root / names[0]).write_bytes(b"original")
            with self.assertRaisesRegex(ValueError, "did not produce"):
                media.prepare(root, ["video"])
            self.assertEqual((root / names[0]).read_bytes(), b"original")
            self.assertFalse((root / names[1]).exists())
            self.assertFalse((root / "build/test-media.json").exists())


if __name__ == "__main__":
    unittest.main()
