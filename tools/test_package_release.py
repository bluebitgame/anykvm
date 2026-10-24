import hashlib
import os
from pathlib import Path
import tarfile
import tempfile
import unittest
import zipfile
from unittest.mock import patch

import package_release


class ReleasePackagingTests(unittest.TestCase):
    def test_archives_preserve_runtime_licenses_and_exclude_docs(self):
        for target in package_release.TARGETS:
            with self.subTest(target=target), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                source = root / "binary"
                source.mkdir()
                windows = target.startswith("windows/")
                executable = "anykvm.exe" if windows else "anykvm"
                (source / executable).write_bytes(b"executable")
                for relative in ("lib/opus.dll", "licenses/dependency/LICENSE"):
                    path = source / relative
                    path.parent.mkdir(parents=True, exist_ok=True)
                    path.write_text(relative)
                (root / "LICENSE").write_text("project license")
                (root / "README.md").write_text("readme")
                (root / "docs/plans").mkdir(parents=True)
                (root / "docs/build.md").write_text("build guide")
                (root / "docs/plans/private.md").write_text("local plan")
                (source / "docs").mkdir()
                (source / "docs/stale.md").write_text("old build documentation")
                (source / "README.md").write_text("old readme")
                archive = package_release.package(target, source, root / "dist", root)
                prefix = "anykvm-" + target.replace("/", "-") + "/"
                expected = {prefix + name for name in (
                    executable, "lib/opus.dll", "licenses/dependency/LICENSE",
                    "LICENSE")}
                if windows:
                    with zipfile.ZipFile(archive) as bundle:
                        self.assertEqual({n for n in bundle.namelist() if not n.endswith("/")}, expected)
                        self.assertEqual(bundle.read(prefix + executable), b"executable")
                else:
                    with tarfile.open(archive) as bundle:
                        self.assertEqual({m.name for m in bundle.getmembers() if m.isfile()}, expected)
                        self.assertEqual(bundle.getmember(prefix + executable).mode & 0o777, 0o755)
                self.assertEqual(archive.with_name(archive.name + ".sha256").read_text(),
                                 f"{hashlib.sha256(archive.read_bytes()).hexdigest()}  {archive.name}\n")
                self.assertEqual((source / "README.md").read_text(), "old readme")

    def test_unix_archive_permissions_do_not_depend_on_host(self):
        original_gettarinfo = tarfile.TarFile.gettarinfo

        def windows_permissions(bundle, *args, **kwargs):
            member = original_gettarinfo(bundle, *args, **kwargs)
            member.mode = 0o666
            return member

        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / "binary"
            source.mkdir()
            executable = source / "anykvm"
            executable.write_bytes(b"executable")
            original_mode = executable.stat().st_mode
            (root / "LICENSE").touch()
            (root / "README.md").touch()
            (root / "docs").mkdir()
            with patch.object(tarfile.TarFile, "gettarinfo", windows_permissions):
                archive = package_release.package("linux/amd64", source, root / "dist", root)
            with tarfile.open(archive) as bundle:
                self.assertEqual(bundle.getmember("anykvm-linux-amd64/anykvm").mode, 0o755)
                for member in bundle.getmembers():
                    if member.isdir():
                        self.assertEqual(member.mode, 0o755)
            self.assertEqual(executable.stat().st_mode, original_mode)

    def test_missing_executable_does_not_produce_release(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            with self.assertRaisesRegex(ValueError, "Missing executable"):
                package_release.package("windows/amd64", root, root / "dist")
            self.assertFalse((root / "dist").exists())

    @unittest.skipIf(os.name == "nt", "Unix library symlinks")
    def test_unix_archives_preserve_shared_library_aliases(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / "binary"
            (source / "lib").mkdir(parents=True)
            (source / "anykvm").write_bytes(b"executable")
            (source / "lib/libcodec.so.1.0").write_bytes(b"shared library")
            (source / "lib/libcodec.so.1").symlink_to("libcodec.so.1.0")
            (root / "LICENSE").touch()
            (root / "README.md").touch()
            (root / "docs").mkdir()
            archive = package_release.package("linux/amd64", source, root / "dist", root)
            with tarfile.open(archive) as bundle:
                alias = bundle.getmember("anykvm-linux-amd64/lib/libcodec.so.1")
                self.assertTrue(alias.issym())
                self.assertEqual(alias.linkname, "libcodec.so.1.0")


if __name__ == "__main__":
    unittest.main()
