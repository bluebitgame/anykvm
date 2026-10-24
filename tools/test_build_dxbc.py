import contextlib
import io
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import build_dxbc
import build_shaders


class DXBCPreparationTests(unittest.TestCase):
    def setUp(self):
        self.real_jobs = build_dxbc.shader_jobs()
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.source = self.root / "internal/jpegcodec/shaders/jpeg420.hlsl"
        self.source.parent.mkdir(parents=True)
        self.source.write_bytes(b"// ANYKVM_TILE_COMPARE\r\nvoid encodeMCUs() {}\r\n")
        self.kernel = self.root / "internal/gpuconv/shaders/tile_compare.hlsl"
        self.kernel.parent.mkdir(parents=True)
        self.kernel.write_bytes(b"void compareTile() { /* first */ }\r\n")
        self.output = self.source.parent.parent / "jpeg420_dxbc.h"
        self.jobs = [("JPEG", self.source, self.output, build_dxbc.ENTRIES, "bk_jpeg")]
        self.commands = []
        for module, name, value in ((build_dxbc, "SOURCE", self.source),
                                    (build_dxbc, "OUTPUT", self.output),
                                    (build_shaders, "ROOT", self.root),
                                    (build_shaders, "JPEG_SOURCE", self.source)):
            patcher = patch.object(module, name, value)
            patcher.start()
            self.addCleanup(patcher.stop)
        patcher = patch.object(build_dxbc, "shader_jobs", side_effect=lambda: self.jobs)
        patcher.start()
        self.addCleanup(patcher.stop)

    def compile(self, command, **kwargs):
        self.commands.append(command)
        self.assertTrue(kwargs["check"])
        self.assertEqual(command[command.index("/T") + 1], "cs_5_0")
        self.assertIn("/O3", command)
        self.assertIn("/Ges", command)
        source = Path(command[-1]).read_bytes()
        original = next(job[1] for job in self.jobs if job[1].name == Path(command[-1]).name)
        self.assertEqual(source, build_shaders.shader_source(original))
        if b"ANYKVM_TILE_COMPARE" in original.read_bytes():
            entry = b"compareTiles" if original == self.source else b"main"
            self.assertIn(b"void " + entry + b"()", source)
        self.assertNotIn(b"ANYKVM_TILE_COMPARE", source)
        entry = command[command.index("/E") + 1]
        Path(command[command.index("/Fo") + 1]).write_bytes(b"DXBC" + bytes(28) + entry.encode())

    def test_source_kernel_flags_entries_and_artifact_changes_rebuild(self):
        with patch.object(build_dxbc, "find_fxc", return_value="fxc.exe"), \
                patch.object(build_dxbc.subprocess, "run", side_effect=self.compile), \
                contextlib.redirect_stdout(io.StringIO()):
            build_dxbc.prepare()
            build_dxbc.prepare()
            self.assertEqual(len(self.commands), 10)
            self.source.write_bytes(self.source.read_bytes() + b"// modified\r\n")
            build_dxbc.prepare()
            self.assertEqual(len(self.commands), 20)
            self.kernel.write_bytes(self.kernel.read_bytes().replace(b"first", b"second"))
            build_dxbc.prepare()
            self.assertEqual(len(self.commands), 30)
            self.output.write_bytes(b"corrupt header")
            build_dxbc.prepare()
            self.assertEqual(len(self.commands), 40)
            manifest_path = self.output.with_suffix(".json")
            for key, wrong in (("profile", "cs_6_0"), ("flags", ["/Od"]),
                               ("entries", list(reversed(build_dxbc.ENTRIES))), ("version", 0)):
                manifest = json.loads(manifest_path.read_text())
                manifest[key] = wrong
                manifest_path.write_text(json.dumps(manifest))
                self.assertFalse(build_dxbc.current(build_shaders.shader_source(self.source)))
                build_dxbc.prepare()
            self.assertEqual(len(self.commands), 80)
            self.assertTrue(build_dxbc.current(build_shaders.shader_source(self.source)))
            entries = [command[command.index("/E") + 1] for command in self.commands[:10]]
            self.assertEqual(entries, list(build_dxbc.ENTRIES))
            self.assertIn(b'"encodeMCUs", bk_jpeg_dxbc_5', self.output.read_bytes())

    def test_jpeg_and_remotefx_assets_are_cached_independently(self):
        for label, original, _, entries, prefix in self.real_jobs[1:]:
            source = self.root / "internal/rdp/shaders" / original.name
            source.parent.mkdir(parents=True, exist_ok=True)
            source.write_bytes(b"void main() {}\r\n" if prefix == "bk_progressive" else b"// ANYKVM_TILE_COMPARE\r\n")
            output = self.root / "internal/gpuconv" / (source.stem + "_dxbc.h")
            self.jobs.append((label, source, output, entries, prefix))
        with patch.object(build_dxbc, "find_fxc", return_value="fxc.exe"), \
                patch.object(build_dxbc.subprocess, "run", side_effect=self.compile), \
                contextlib.redirect_stdout(io.StringIO()):
            build_dxbc.prepare()
            build_dxbc.prepare()
            self.assertEqual(len(self.commands), 15)
            self.kernel.write_bytes(self.kernel.read_bytes().replace(b"first", b"changed"))
            build_dxbc.prepare()
            self.assertEqual(len(self.commands), 26)  # JPEG + surface share the tile kernel.
            self.assertEqual([Path(command[-1]).name for command in self.commands[15:]],
                             ["jpeg420.hlsl"] * 10 + ["progressive_surface.hlsl"])
            progressive, surface = self.jobs[1], self.jobs[2]
            progressive[1].write_bytes(progressive[1].read_bytes() + b"// changed\n")
            build_dxbc.prepare()
            self.assertEqual(len(self.commands), 30)
            surface[2].write_bytes(b"corrupt surface header")
            build_dxbc.prepare()
            self.assertEqual(len(self.commands), 31)
            for _, source, output, entries, prefix in self.jobs:
                self.assertTrue(build_dxbc.current(build_shaders.shader_source(source), output, entries, prefix))
                self.assertIn((prefix + "_dxbc_stages").encode(), output.read_bytes())

    def test_failure_never_publishes_partial_or_stale_bundle(self):
        with patch.object(build_dxbc, "find_fxc", return_value="fxc.exe"), \
                patch.object(build_dxbc.subprocess, "run", side_effect=self.compile), \
                contextlib.redirect_stdout(io.StringIO()):
            build_dxbc.prepare()
            previous_header = self.output.read_bytes()
            previous_manifest = self.output.with_suffix(".json").read_bytes()
            self.source.write_bytes(self.source.read_bytes() + b"// changed\n")

            def fail(command, **kwargs):
                if command[command.index("/E") + 1] == "encodeMCUs":
                    raise subprocess.CalledProcessError(1, command)
                self.compile(command, **kwargs)

            with patch.object(build_dxbc.subprocess, "run", side_effect=fail):
                with self.assertRaises(subprocess.CalledProcessError):
                    build_dxbc.prepare()
            self.assertEqual(self.output.read_bytes(), previous_header)
            self.assertEqual(self.output.with_suffix(".json").read_bytes(), previous_manifest)
            self.assertFalse(build_dxbc.current(build_shaders.shader_source(self.source)))
            self.assertFalse(list(self.output.parent.glob(".dxbc-*")))

    def test_invalid_fxc_output_is_rejected(self):
        def invalid(command, **kwargs):
            Path(command[command.index("/Fo") + 1]).write_bytes(b"DXIL" + bytes(28))
        with patch.object(build_dxbc, "find_fxc", return_value="fxc.exe"), \
                patch.object(build_dxbc.subprocess, "run", side_effect=invalid), \
                contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaisesRegex(ValueError, "did not produce DXBC"):
                build_dxbc.prepare()
            self.assertFalse(self.output.exists())
            self.assertFalse(self.output.with_suffix(".json").exists())

    def test_valid_bundle_needs_no_compiler_and_dry_run_writes_nothing(self):
        with patch.object(build_dxbc, "find_fxc", side_effect=ValueError("no SDK")) as find, \
                patch.object(build_dxbc.subprocess, "run", side_effect=self.compile) as compile, \
                contextlib.redirect_stdout(io.StringIO()):
            build_dxbc.prepare(dry_run=True)
            find.assert_not_called()
            compile.assert_not_called()
            self.assertFalse(self.output.exists())
            build_dxbc.prepare(compiler="explicit-fxc.exe")
            compile.reset_mock()
            build_dxbc.prepare()
            find.assert_not_called()
            compile.assert_not_called()
            self.output.unlink()
            with self.assertRaisesRegex(ValueError, "no SDK"):
                build_dxbc.prepare()

    def test_sdk_search_uses_latest_version_and_host_architecture(self):
        sdk = self.root / "Windows Kits/10/bin"
        for version in ("10.0.9000.0", "10.0.26100.0"):
            for arch in ("x64", "arm64"):
                compiler = sdk / version / arch / "fxc.exe"
                compiler.parent.mkdir(parents=True)
                compiler.touch()
        for machine, arch in (("AMD64", "x64"), ("ARM64", "arm64")):
            with self.subTest(machine=machine), \
                    patch.dict(os.environ, {"ProgramFiles(x86)": str(self.root)}, clear=True), \
                    patch.object(build_dxbc.platform, "machine", return_value=machine), \
                    patch.object(build_dxbc.shutil, "which", return_value=None):
                self.assertEqual(Path(build_dxbc.find_fxc()), sdk / "10.0.26100.0" / arch / "fxc.exe")

    def test_explicit_compiler_override_does_not_silently_use_another_sdk(self):
        with patch.dict(os.environ, {"FXC": "missing-fxc.exe"}, clear=True), \
                patch.object(build_dxbc.shutil, "which", return_value=None):
            with self.assertRaisesRegex(ValueError, "FXC compiler not found"):
                build_dxbc.find_fxc()

    def test_offline_entry_list_matches_native_encoder(self):
        native = (Path(__file__).resolve().parent.parent / "internal/jpegcodec/d3d11_windows.c").read_text()
        entries = re.findall(r'load_encoding_stage\(e, compile, source, size, "([^"]+)"', native)
        self.assertEqual(entries, list(build_dxbc.ENTRIES))
        self.assertIn('entry, "cs_5_0",', native)
        progressive = (Path(__file__).resolve().parent.parent / "internal/gpuconv/progressive_windows.c").read_text()
        entries = re.search(r'const char \*entries\[\]=\{([^}]+)\}', progressive).group(1)
        self.assertEqual(re.findall(r'"([^"]+)"', entries), list(self.real_jobs[1][3]))
        self.assertEqual(self.real_jobs[2][3], ("main",))


if __name__ == "__main__":
    unittest.main()
