import hashlib
import json
from pathlib import Path
import tempfile
import unittest
import zipfile

import package_firmware as firmware


class FirmwarePackagingTests(unittest.TestCase):
    def fixture(self, root, target):
        source = root / "images"
        source.mkdir()
        family = firmware.TARGETS[target][0]
        suffix = {"pico": ".uf2", "nrf": ".hex", "esp32": ".bin"}[family]
        image = source / ("image" + suffix)
        image.write_bytes(b"firmware")
        (source / "artifacts.json").write_text(json.dumps({image.name: hashlib.sha256(image.read_bytes()).hexdigest()}))
        (root / "LICENSE").write_text("license")
        (root / "firmware").mkdir()
        (root / "firmware/README.md").write_text("flashing guide")
        return source, image

    def test_all_boards_include_images_metadata_and_checksum_without_docs(self):
        for target in firmware.TARGETS:
            with self.subTest(target=target), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                source, image = self.fixture(root, target)
                archive = firmware.package(target, source, root / "dist", "abc123", root)
                prefix = f"firmware-{target}/"
                with zipfile.ZipFile(archive) as bundle:
                    self.assertEqual(bundle.read(prefix + image.name), b"firmware")
                    metadata = json.loads(bundle.read(prefix + "release.json"))
                    self.assertEqual(metadata["target"], target)
                    self.assertEqual(metadata["source_revision"], "abc123")
                    self.assertNotIn("validation", metadata)
                    self.assertFalse(any(name.endswith(".md") for name in bundle.namelist()))
                    self.assertIn(prefix + "LICENSE", bundle.namelist())
                self.assertEqual(archive.with_suffix(".zip.sha256").read_text(),
                                 f"{hashlib.sha256(archive.read_bytes()).hexdigest()}  {archive.name}\n")

    def test_groups_cover_each_target_once(self):
        targets = [target for group in firmware.GROUPS.values() for target in group]
        self.assertCountEqual(targets, firmware.TARGETS)
        self.assertEqual(len(targets), len(set(targets)))

    def test_groups_preserve_same_named_images_and_require_all_variants(self):
        for group in ("pico1", "pico2", "nrf52840"):
            with self.subTest(group=group), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                (root / "LICENSE").write_text("license")
                source = root / "images"
                image_name = "zephyr.hex" if group == "nrf52840" else "image.uf2"
                for target in firmware.GROUPS[group]:
                    directory = source / target
                    directory.mkdir(parents=True)
                    data = target.encode()
                    (directory / image_name).write_bytes(data)
                    (directory / "artifacts.json").write_text(json.dumps({
                        image_name: hashlib.sha256(data).hexdigest()}))
                archive = firmware.package_group(group, source, root / "dist", "abc123", root)
                self.assertEqual(archive.name, f"firmware-{group}.zip")
                with zipfile.ZipFile(archive) as bundle:
                    for target in firmware.GROUPS[group]:
                        prefix = f"firmware-{group}/{target}/"
                        self.assertEqual(bundle.read(prefix + image_name), target.encode())
                        self.assertEqual(json.loads(bundle.read(prefix + "release.json"))["target"], target)
                    self.assertFalse(any(name.endswith(".md") for name in bundle.namelist()))
                original = archive.read_bytes()
                (source / firmware.GROUPS[group][-1] / image_name).unlink()
                with self.assertRaises(ValueError):
                    firmware.package_group(group, source, root / "dist", "abc123", root)
                self.assertEqual(archive.read_bytes(), original)

    def test_rejects_corrupt_missing_and_unsafe_images(self):
        for failure in ("checksum", "missing", "empty", "wrong-format", "traversal"):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                source, image = self.fixture(root, "pico")
                if failure == "checksum":
                    image.write_bytes(b"corrupted")
                elif failure == "missing":
                    image.unlink()
                elif failure == "empty":
                    image.write_bytes(b"")
                elif failure == "wrong-format":
                    image.rename(source / "image.bin")
                    (source / "artifacts.json").write_text(json.dumps({"image.bin": hashlib.sha256(b"firmware").hexdigest()}))
                else:
                    (source / "artifacts.json").write_text(json.dumps({"../image.uf2": "invalid"}))
                with self.assertRaises(ValueError):
                    firmware.package("pico", source, root / "dist", "abc123", root)
                self.assertFalse((root / "dist").exists())


if __name__ == "__main__":
    unittest.main()
