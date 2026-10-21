import hashlib
from pathlib import Path
import tarfile
import tempfile
import unittest
import zipfile

import package_release


class ReleasePackagingTests(unittest.TestCase):
    def test_archives_preserve_runtime_licenses_docs_and_executable(self):
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
                archive = package_release.package(target, source, root / "dist", root)
                prefix = "anykvm-" + target.replace("/", "-") + "/"
                expected = {prefix + name for name in (
                    executable, "lib/opus.dll", "licenses/dependency/LICENSE",
                    "LICENSE", "README.md", "docs/build.md")}
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
                self.assertFalse((source / "README.md").exists())

    def test_missing_executable_does_not_produce_release(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            with self.assertRaisesRegex(ValueError, "Missing executable"):
                package_release.package("windows/amd64", root, root / "dist")
            self.assertFalse((root / "dist").exists())


if __name__ == "__main__":
    unittest.main()
