import contextlib
import io
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import build_shaders
import profile_jpeg_shader as profile


class ShaderCompileProfileTests(unittest.TestCase):
    def test_function_replacement_ignores_comment_braces_and_preserves_other_functions(self):
        source = "// void target() { broken }\nvoid target() {\n// }\nif (true) { /* } */ }\n}\nvoid other() {}\n"
        changed = profile.replace_function(source, "target", "void target() { return; }")
        self.assertEqual(changed, "// void target() { broken }\nvoid target() { return; }\nvoid other() {}\n")
        with self.assertRaisesRegex(ValueError, "not found"):
            profile.function(source, "missing")

    def test_legacy_and_current_keep_the_same_encoder_with_half_the_append_sites(self):
        source = build_shaders.shader_source(build_shaders.JPEG_SOURCE).decode()
        cases = {name: text for name, text, _, _ in profile.variants(source)}
        legacy, current = cases["legacy_o3"], cases["current_o3"]
        for name in ("encodeBlock", "encodeMCUs", "writeByte", "storeSolidDC", "loadCoefficient"):
            self.assertEqual(profile.function(current, name), profile.function(legacy, name))
        old, new = profile.expanded_writer_calls(legacy), profile.expanded_writer_calls(current)
        self.assertEqual(old["writeBits"], new["writeBits"])
        self.assertEqual(old["appendBits"], new["appendBits"] * 2)
        self.assertEqual(new["appendBits"], new["writeBits"])
        self.assertIn("writer.pendingBits << (count - remaining)", profile.function(current, "appendBits"))
        self.assertIn("if (count > 24)", profile.function(legacy, "writeBits"))

    def test_prepare_only_writes_diagnostic_sources_without_invoking_a_compiler(self):
        with tempfile.TemporaryDirectory() as temporary, \
                patch.object(profile.platform, "platform", return_value="test-host"), \
                patch.object(profile.subprocess, "run") as compile, \
                contextlib.redirect_stdout(io.StringIO()):
            output = Path(temporary)
            self.assertEqual(profile.run(output, prepare_only=True), 0)
            report = json.loads((output / "report.json").read_text())
            self.assertEqual(report["entry"], "encodeMCUs")
            self.assertEqual(len(report["cases"]), 11)
            self.assertEqual(len(list(output.glob("*.hlsl"))), 11)
            self.assertTrue(all(not case["runs"] for case in report["cases"]))
            compile.assert_not_called()

    def test_worker_measurements_and_timeouts_remain_in_the_report(self):
        with tempfile.TemporaryDirectory() as temporary, \
                patch.object(profile.platform, "system", return_value="Windows"), \
                patch.object(profile.platform, "platform", return_value="test-host"), \
                contextlib.redirect_stdout(io.StringIO()):
            def compile(command, **kwargs):
                destination = Path(command[command.index("--output") + 1])
                self.assertEqual(kwargs["timeout"], 3)
                self.assertEqual(command[command.index("--flags") + 1], str(profile.O3))
                if destination.name.startswith("no_writer"):
                    raise subprocess.TimeoutExpired(command, 3)
                destination.with_suffix(".json").write_text(json.dumps(
                    {"status": "ok", "compile_ms": 100, "instruction_slots": 10, "temporary_registers": 3}))
                return subprocess.CompletedProcess(command, 0, "", "")
            source = build_shaders.shader_source(build_shaders.JPEG_SOURCE).decode()
            cases = (("current", source, profile.O3, "current"), ("no_writer", source, profile.O3, "timeout"))
            with patch.object(profile, "variants", return_value=cases), \
                    patch.object(profile.subprocess, "run", side_effect=compile):
                self.assertEqual(profile.run(Path(temporary), repeats=2, timeout=3), 1)
            report = json.loads((Path(temporary) / "report.json").read_text())
            self.assertEqual(report["cases"][0]["median_compile_ms"], 100)
            self.assertEqual(len(report["cases"][0]["runs"]), 2)
            self.assertEqual(report["cases"][1]["runs"], [{"status": "timeout", "timeout_seconds": 3}] * 2)

    def test_disassembly_stats_count_real_instructions_not_comments(self):
        assembly = "// if_nz fake\ndcl_temps 42\nloop\n if_nz r0.x\n endif\nendloop\n// approximately 217 instruction slots used\n"
        self.assertEqual(profile.assembly_stats(assembly), {
            "instruction_slots": 217, "temporary_registers": 42, "loops": 1, "branches": 1})


if __name__ == "__main__":
    unittest.main()
