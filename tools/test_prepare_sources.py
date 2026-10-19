import hashlib
import io
import json
from pathlib import Path
import re
import tarfile
import tempfile
import unittest
from unittest.mock import patch

import prepare_sources


class SourcePreparationTests(unittest.TestCase):
    def test_one_archive_member_restores_header_and_its_license(self):
        data = b"/*\n * Copyright Example\n * Permission notice.\n */\nint api;\n"
        notice = b"Copyright Example\nPermission notice.\n"
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            archive = root / "source.tar"
            with tarfile.open(archive, "w") as output:
                info = tarfile.TarInfo("release/header.h")
                info.size = len(data)
                output.addfile(info, io.BytesIO(data))
            source_hash = hashlib.sha256(data).hexdigest()
            group = {"name": "example", "url": "https://example.invalid/source.tar",
                     "sha256": hashlib.sha256(archive.read_bytes()).hexdigest(),
                     "destination": "third_party", "files": {
                         "header.h": {"member": "release/header.h", "size": len(data), "sha256": source_hash},
                         "LICENSE": {"member": "release/header.h", "size": len(data),
                                     "source_sha256": source_hash, "extract": "c-license",
                                     "sha256": hashlib.sha256(notice).hexdigest()}}}
            with patch.object(prepare_sources, "ROOT", root), \
                    patch.object(prepare_sources, "fetch", return_value=archive):
                prepare_sources.prepare_group(group)
            self.assertEqual((root / "third_party/header.h").read_bytes(), data)
            self.assertEqual((root / "third_party/LICENSE").read_bytes(), notice)

    def test_release_notices_are_verified_and_packaged(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            paths = ("LICENSE", "vendor/LICENSE", "vendor/hls-LICENSE")
            for name in paths:
                path = root / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_bytes(name.encode())
            group = {"name": "vendor", "destination": "vendor", "files": {
                "LICENSE": {"sha256": hashlib.sha256(b"vendor/LICENSE").hexdigest()},
                "hls-LICENSE": {"sha256": hashlib.sha256(b"vendor/hls-LICENSE").hexdigest()}}}
            with patch.object(prepare_sources, "ROOT", root), \
                    patch.object(prepare_sources, "selected_groups", return_value=[group]):
                prepare_sources.stage_notices(root / "release")
                self.assertEqual((root / "release/licenses/vendor/LICENSE").read_bytes(), b"vendor/LICENSE")
                self.assertEqual((root / "release/licenses/vendor/hls-LICENSE").read_bytes(), b"vendor/hls-LICENSE")
                self.assertEqual((root / "release/LICENSE").read_bytes(), b"LICENSE")
                (root / "vendor/LICENSE").write_bytes(b"changed")
                with self.assertRaisesRegex(ValueError, "missing or changed"):
                    prepare_sources.notice_files()

    def test_pinned_license_restore_repair_and_packaging(self):
        groups = {group["name"]: group for group in prepare_sources.selected_groups()
                  if group["name"] in ("go-ios", "rootshell-vnc")}
        self.assertEqual(set(groups), {"go-ios", "rootshell-vnc"})
        version = re.search(r"github\.com/danielpaulus/go-ios\s+(\S+)",
                            (prepare_sources.ROOT / "go.mod").read_text()).group(1)
        self.assertEqual(groups["go-ios"]["files"]["LICENSE"]["url"],
                         f"https://raw.githubusercontent.com/danielpaulus/go-ios/{version}/LICENSE")
        for name, pinned in groups.items():
            with self.subTest(dependency=name), tempfile.TemporaryDirectory() as temporary:
                url = pinned["files"]["LICENSE"]["url"]
                data = f"{name} test license notice\n".encode()
                checksum = hashlib.sha256(data).hexdigest()
                group = {**pinned, "files": {"LICENSE": {"url": url, "size": len(data), "sha256": checksum}}}
                root = Path(temporary)
                cached = root / f"cache/downloads/source-{name}-{checksum[:16]}-LICENSE"
                cached.parent.mkdir(parents=True)
                cached.write_bytes(data)
                (root / "LICENSE").write_bytes(b"project license\n")
                with patch.object(prepare_sources, "ROOT", root), \
                        patch.object(prepare_sources.cross_toolchain, "CACHE", root / "cache"), \
                        patch.object(prepare_sources.cross_toolchain, "download", return_value=cached) as download, \
                        patch.object(prepare_sources, "selected_groups", return_value=[group]):
                    prepare_sources.prepare()
                    download.assert_called_once_with(url, checksum, cached.name)
                    restored = root / group["destination"] / "LICENSE"
                    self.assertEqual(restored.read_bytes(), data)
                    restored.write_bytes(b"damaged")
                    prepare_sources.prepare(allow_download=False)
                    download.assert_called_once()
                    self.assertEqual(restored.read_bytes(), data)
                    prepare_sources.stage_notices(root / "release")
                    self.assertEqual((root / f"release/licenses/{name}/LICENSE").read_bytes(), data)
                    self.assertFalse((root / "release/licenses/internal").exists())

    def test_manifest_outputs_are_excluded_from_source_submission(self):
        import subprocess
        groups = list(prepare_sources.selected_groups())
        paths = [str(Path(group["destination"]) / name) for group in groups
                 for name in (*group["files"], *group.get("generated", {}))]
        result = subprocess.run(["git", "check-ignore", "--no-index", "--stdin"],
                                input="\n".join(paths) + "\n", cwd=prepare_sources.ROOT,
                                text=True, capture_output=True, check=True)
        self.assertEqual(set(result.stdout.splitlines()), set(paths))

    def test_mirror_uses_the_same_pinned_checksum(self):
        with patch.object(prepare_sources.cross_toolchain, "download",
                          side_effect=[ValueError("upstream unavailable"), Path("verified-cache")]) as download:
            result = prepare_sources.fetch("https://upstream.example/a", "0" * 64, "input", True,
                                           ["https://mirror.example/a"])
            self.assertEqual(result, Path("verified-cache"))
            self.assertEqual([call.args[0] for call in download.call_args_list],
                             ["https://upstream.example/a", "https://mirror.example/a"])
            self.assertTrue(all(call.args[1] == "0" * 64 for call in download.call_args_list))

    def test_offline_archive_restore_and_repair(self):
        data = b"pinned upstream input\n"
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            archive = root / "cache/downloads/source-example-pinned.tar"
            archive.parent.mkdir(parents=True)
            with tarfile.open(archive, "w") as output:
                info = tarfile.TarInfo("release/header.h")
                info.size = len(data)
                output.addfile(info, io.BytesIO(data))
            checksum = hashlib.sha256(archive.read_bytes()).hexdigest()
            archive.rename(archive.with_name(f"source-example-{checksum[:16]}.tar"))
            group = {"name": "example", "url": "https://example.invalid/source.tar",
                     "sha256": checksum, "destination": "third_party",
                     "files": {"header.h": {"member": "release/header.h", "size": len(data),
                                                "sha256": hashlib.sha256(data).hexdigest()}}}
            with patch.object(prepare_sources, "ROOT", root), \
                    patch.object(prepare_sources.cross_toolchain, "CACHE", root / "cache"), \
                    patch.object(prepare_sources.cross_toolchain, "download") as download:
                prepare_sources.prepare_group(group, allow_download=False)
                self.assertEqual((root / "third_party/header.h").read_bytes(), data)
                (root / "third_party/header.h").write_bytes(b"damaged")
                prepare_sources.prepare_group(group, allow_download=False)
                self.assertEqual((root / "third_party/header.h").read_bytes(), data)
                download.assert_not_called()

    def test_corrupt_input_is_not_installed(self):
        with tempfile.TemporaryDirectory() as temporary:
            destination = Path(temporary) / "header.h"
            with self.assertRaisesRegex(ValueError, "verification"):
                prepare_sources.install(destination, b"wrong", {"size": 5, "sha256": "0" * 64})
            self.assertFalse(destination.exists())

    def test_patch_requires_exact_input_and_output(self):
        data, expected = b"old\n", b"new\n"
        with tempfile.TemporaryDirectory() as temporary:
            changes = {"source_sha256": hashlib.sha256(data).hexdigest(),
                       "result_sha256": hashlib.sha256(expected).hexdigest(),
                       "edits": [{"line": 0, "before": ["old\n"], "after": ["new\n"]}]}
            patch_file = Path(temporary) / "change.json"
            patch_file.write_text(json.dumps(changes))
            self.assertEqual(prepare_sources.patch(data, str(patch_file)), expected)
            with self.assertRaisesRegex(ValueError, "input"):
                prepare_sources.patch(b"changed upstream\n", str(patch_file))
            changes["result_sha256"] = "0" * 64
            patch_file.write_text(json.dumps(changes))
            with self.assertRaisesRegex(ValueError, "output"):
                prepare_sources.patch(data, str(patch_file))

    def test_dry_run_does_not_download_or_create_files(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            group = {"name": "example", "destination": "missing",
                     "files": {"header.h": {"sha256": "0" * 64}}}
            with patch.object(prepare_sources, "ROOT", root), \
                    patch.object(prepare_sources, "fetch") as fetch:
                prepare_sources.prepare_group(group, dry_run=True)
                fetch.assert_not_called()
                self.assertEqual(list(root.iterdir()), [])

    def test_archive_links_are_rejected(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            archive = root / "source.tar"
            with tarfile.open(archive, "w") as output:
                info = tarfile.TarInfo("release/header.h")
                info.type = tarfile.SYMTYPE
                info.linkname = "/outside"
                output.addfile(info)
            group = {"name": "example", "url": "https://example.invalid/source.tar",
                     "sha256": hashlib.sha256(archive.read_bytes()).hexdigest(),
                     "destination": "third_party",
                     "files": {"header.h": {"member": "release/header.h", "size": 0, "sha256": "0" * 64}}}
            with patch.object(prepare_sources, "ROOT", root), \
                    patch.object(prepare_sources, "fetch", return_value=archive):
                with self.assertRaisesRegex(ValueError, "archive entry"):
                    prepare_sources.prepare_group(group)
            self.assertFalse((root / "third_party/header.h").exists())


if __name__ == "__main__":
    unittest.main()
