import contextlib
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import build_firmware as firmware


class FirmwareBuildTests(unittest.TestCase):
    def test_cached_ncs_still_prepares_a_fresh_python_environment(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            builder = firmware.Builder(root=root, directory=root / "build/firmware")
            sdk = builder.directory / f"sdk/ncs-{firmware.NCS_VERSION}"
            sdk.mkdir(parents=True)
            (sdk / ".anykvm-ready").write_text(json.dumps([firmware.NCS_VERSION, list(firmware.NCS_MODULES)]))
            for name in ("zephyr", "nrf"):
                path = sdk / name / "scripts/requirements-base.txt"
                path.parent.mkdir(parents=True)
                path.write_text(name)
            with patch.object(builder, "run") as run:
                self.assertEqual(builder.ncs(), sdk)
                self.assertIn("pip", run.call_args.args[0])
                run.reset_mock()
                self.assertEqual(builder.ncs(), sdk)
                run.assert_not_called()
                (sdk / "nrf/scripts/requirements-base.txt").write_text("updated")
                builder.ncs()
                run.assert_called_once()

    def test_xiao_target_names_the_board_and_uses_shared_nrf_sources(self):
        self.assertNotIn("nrf52840-ble", firmware.TARGETS)
        self.assertEqual(firmware.TARGETS["nrf52840-xiao"],
                         ("nrf", "nrf52840-ble", "xiao_ble", "zephyr"))

    def test_pico_output_matrix(self):
        # Every output has distinct build/output directories for each chip.
        expected = {
            "pico": ("pico-hid", "pico", "anykvm-gpio-hid"),
            "pico2": ("pico-hid", "pico2", "anykvm-gpio-hid"),
            "pico-pio-usb": ("pico-pio-usb", "pico", "anykvm-pio-usb"),
            "pico2-pio-usb": ("pico-pio-usb", "pico2", "anykvm-pio-usb"),
            "pico-ps2": ("pico-ps2", "pico", "anykvm-ps2"),
            "pico2-ps2": ("pico-ps2", "pico2", "anykvm-ps2"),
        }
        for target, definition in expected.items():
            self.assertEqual(firmware.TARGETS[target], ("pico", *definition))
            with tempfile.TemporaryDirectory() as temporary, contextlib.redirect_stdout(io.StringIO()) as output:
                root = Path(temporary)
                builder = firmware.Builder(dry_run=True, root=root, directory=root / "build/firmware")
                builder.build(target)
                commands = output.getvalue()
                self.assertIn(f"PICO_BOARD={definition[1]}", commands)
                self.assertIn(f"work/{target}", commands)
                self.assertIn(f"releases/{target}", commands)
                self.assertEqual("-DPICO_PIO_USB_DIR=" in commands, "pio-usb" in target)

    def test_all_targets_dry_run_without_files_or_subprocesses(self):
        with tempfile.TemporaryDirectory() as temporary, \
                patch.object(firmware.cross_toolchain, "download") as download, \
                patch.object(firmware.subprocess, "run") as run, \
                contextlib.redirect_stdout(io.StringIO()) as output:
            root = Path(temporary)
            builder = firmware.Builder(dry_run=True, root=root, directory=root / "build/firmware")
            for target in firmware.TARGETS:
                builder.build(target)
            self.assertEqual(list(root.iterdir()), [])
            download.assert_not_called()
            run.assert_not_called()
            text = output.getvalue()
            for target in firmware.TARGETS:
                self.assertIn(f"[{target}]", text)
            self.assertIn("PICO_BOARD=pico", text)
            self.assertIn("PICO_BOARD=pico2", text)
            self.assertIn("xiao_ble", text)
            self.assertIn("adafruit_feather_nrf52840", text)
            self.assertIn("nrf52840dongle_nrf52840", text)
            self.assertIn(firmware.ESP32_FQBN, text)
            self.assertNotIn(" upload ", text)
            self.assertNotIn(" flash ", text)

    def test_sdk_paths_and_caches_override_host_settings(self):
        inherited = {"PATH": "host-bin", "PICO_SDK_PATH": "/host/pico", "ZEPHYR_BASE": "/host/zephyr",
                     "GNUARMEMB_TOOLCHAIN_PATH": "/host/arm", "PYTHONPATH": "/host/python",
                     "PIP_TARGET": "/host/packages", "ARDUINO_DIRECTORIES_DATA": "/host/arduino"}
        directory = Path("/project/build/firmware")
        actual = firmware.build_environment(inherited, directory)
        for name in ("PICO_SDK_PATH", "ZEPHYR_BASE", "GNUARMEMB_TOOLCHAIN_PATH", "PYTHONPATH", "PIP_TARGET"):
            self.assertNotIn(name, actual)
        for name in ("PIP_CACHE_DIR", "XDG_CACHE_HOME", "TMPDIR", "TEMP", "TMP", "CCACHE_DIR",
                     "ARDUINO_DIRECTORIES_DATA", "ARDUINO_DIRECTORIES_DOWNLOADS",
                     "ARDUINO_DIRECTORIES_USER", "ARDUINO_BUILD_CACHE_PATH", "WEST_CONFIG_GLOBAL"):
            self.assertIn(directory, Path(actual[name]).parents)
        self.assertEqual(inherited["PICO_SDK_PATH"], "/host/pico")
        self.assertEqual(actual["PATH"], "host-bin")

    def test_rejects_sdk_and_output_paths_outside_build(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            with self.assertRaisesRegex(ValueError, "inside.*build"):
                firmware.Builder(root=root, directory=root / "firmware")
            self.assertEqual(list(root.iterdir()), [])

    def test_offline_preparation_does_not_download_or_invoke_git(self):
        with tempfile.TemporaryDirectory() as temporary, \
                patch.object(firmware.cross_toolchain, "download") as download, \
                patch.object(firmware.subprocess, "run") as run:
            root = Path(temporary)
            builder = firmware.Builder(offline=True, root=root, directory=root / "build/firmware")
            with self.assertRaisesRegex(ValueError, "run without --offline"):
                builder.checkout("https://example.invalid/sdk", "pinned", builder.directory / "sdk/input")
            with self.assertRaisesRegex(ValueError, "Missing verified download"):
                builder.fetch("https://example.invalid/archive", "missing", "0" * 64)
            download.assert_not_called()
            run.assert_not_called()

    def test_failed_build_keeps_last_published_image(self):
        with tempfile.TemporaryDirectory() as temporary, contextlib.redirect_stdout(io.StringIO()):
            root = Path(temporary)
            builder = firmware.Builder(root=root, directory=root / "build/firmware")
            image = builder.directory / "work/pico/app.uf2"
            image.parent.mkdir(parents=True)
            image.write_bytes(b"working-image")
            builder.publish("pico", [image], ".uf2")
            destination = builder.directory / "releases/pico/app.uf2"
            metadata = json.loads((destination.parent / "artifacts.json").read_text())
            self.assertEqual(metadata[image.name], firmware.cross_toolchain.digest(image))
            image.unlink()
            with self.assertRaisesRegex(ValueError, "did not produce"):
                builder.publish("pico", [image], ".uf2")
            self.assertEqual(destination.read_bytes(), b"working-image")
            (destination.parent / "stale.bin").write_bytes(b"old")
            image.write_bytes(b"rebuilt-image")
            builder.publish("pico", [image], ".uf2")
            self.assertEqual(destination.read_bytes(), b"rebuilt-image")
            self.assertFalse((destination.parent / "stale.bin").exists())

    def test_cmake_source_replacement_does_not_lose_sdk_cache_marker(self):
        with tempfile.TemporaryDirectory() as temporary, \
                patch.object(firmware.subprocess, "run") as run:
            root = Path(temporary)
            builder = firmware.Builder(offline=True, root=root, directory=root / "build/firmware")
            source = builder.directory / "sdk/picotool/picotool-src"
            (source / ".git").mkdir(parents=True)
            source.with_name("picotool-src.anykvm-ready.json").write_text(
                json.dumps(["https://example.invalid/sdk", "pinned", []]))
            self.assertEqual(builder.checkout("https://example.invalid/sdk", "pinned", source), source)
            run.assert_not_called()

    def test_arduino_uses_explicit_local_configuration(self):
        with tempfile.TemporaryDirectory() as temporary, contextlib.redirect_stdout(io.StringIO()):
            root = Path(temporary)
            builder = firmware.Builder(root=root, directory=root / "build/firmware")
            executable = builder.directory / f"tools/arduino-cli-{firmware.ARDUINO_VERSION}"
            executable.mkdir(parents=True)
            (executable / ("arduino-cli.exe" if os.name == "nt" else "arduino-cli")).touch()
            with patch.object(builder, "run") as run:
                command = builder.arduino()
            config = Path(command[-1])
            self.assertEqual(command[-2], "--config-file")
            values = json.loads(config.read_text())
            self.assertEqual(values["directories"]["data"], str(builder.directory / "sdk/arduino"))
            self.assertEqual(values["directories"]["user"], str(builder.directory / "sdk/arduino-user"))
            self.assertEqual(values["directories"]["downloads"], str(builder.directory / "cache/arduino-downloads"))
            self.assertTrue(any(f"esp32:esp32@{firmware.ESP32_VERSION}" in c.args[0]
                                for c in run.call_args_list))


if __name__ == "__main__":
    unittest.main()
