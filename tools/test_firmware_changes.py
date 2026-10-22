import os
from pathlib import Path
import subprocess
import tempfile
import unittest

import firmware_changes


class FirmwareChangesTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.previous = Path.cwd()
        os.chdir(self.temporary.name)
        self.addCleanup(self.restore)
        self.git("init", "-q")
        self.git("config", "user.email", "test@example.com")
        self.git("config", "user.name", "Test")
        self.git("config", "commit.gpgsign", "false")
        Path("firmware").mkdir()
        self.commit("firmware/main.c", "first")
        self.git("tag", "v0.1.0-beta.1")

    def restore(self):
        os.chdir(self.previous)
        self.temporary.cleanup()

    def git(self, *args):
        return subprocess.check_output(["git", *args], stderr=subprocess.STDOUT, text=True).strip()

    def commit(self, name, content):
        path = Path(name)
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content)
        self.git("add", "-A")
        self.git("commit", "-qm", "fixture")

    def test_first_release_includes_firmware(self):
        self.assertEqual(firmware_changes.changed([]), (True, ""))

    def test_desktop_only_changes_skip_firmware(self):
        self.commit("main.go", "desktop")
        self.assertEqual(firmware_changes.changed(["v0.1.0-beta.1"]), (False, "v0.1.0-beta.1"))

    def test_firmware_and_builder_changes_include_firmware(self):
        self.commit("tools/build_firmware.py", "builder")
        self.assertTrue(firmware_changes.changed(["v0.1.0-beta.1"])[0])
        Path("firmware/main.c").unlink()
        self.git("add", "-A")
        self.git("commit", "-qm", "delete firmware")
        self.assertTrue(firmware_changes.changed(["v0.1.0-beta.1"])[0])

    def test_current_release_is_excluded_on_rerun(self):
        self.commit("firmware/main.c", "second")
        self.git("tag", "v0.1.1-beta.2")
        self.assertEqual(firmware_changes.changed(["v0.1.1-beta.2", "v0.1.0-beta.1"], "v0.1.1-beta.2"),
                         (True, "v0.1.0-beta.1"))

    def test_nearest_ancestor_wins_and_unrelated_release_is_ignored(self):
        self.git("checkout", "-qb", "unrelated")
        self.commit("firmware/main.c", "other branch")
        self.git("tag", "v9.0.0")
        self.git("checkout", "--detach", "v0.1.0-beta.1")
        self.commit("firmware/main.c", "second")
        self.git("tag", "v0.1.1-beta.1")
        self.commit("main.go", "desktop")
        self.assertEqual(firmware_changes.changed(["v9.0.0", "missing", "v0.1.0-beta.1", "v0.1.1-beta.1"]),
                         (False, "v0.1.1-beta.1"))


if __name__ == "__main__":
    unittest.main()
