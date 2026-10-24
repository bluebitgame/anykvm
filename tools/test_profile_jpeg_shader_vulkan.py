import contextlib
import io
import json
from pathlib import Path
import struct
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import profile_jpeg_shader_vulkan as profile


def module():
    # Header + OpLabel + OpLoopMerge + OpBranchConditional.
    words = [0x07230203, 0x10300, 0, 10, 0, (2 << 16) | 248, 1,
             (4 << 16) | 246, 2, 3, 0, (4 << 16) | 250, 4, 5, 6]
    return struct.pack("<" + str(len(words)) + "I", *words)


class VulkanShaderCompileProfileTests(unittest.TestCase):
    def test_spirv_counts_and_malformed_input(self):
        stats = profile.spirv_stats(module())
        self.assertEqual((stats["instructions"], stats["blocks"], stats["loops"], stats["conditional_branches"]), (3, 1, 1, 1))
        for broken in (b"", b"DXBC" + bytes(16), module()[:-4]):
            with self.subTest(broken=broken), self.assertRaises(ValueError):
                profile.spirv_stats(broken)

    def test_compares_the_same_flags_alternates_order_and_excludes_warmup(self):
        calls = []
        def compile(command, **kwargs):
            calls.append(Path(command[-1]).stem)
            self.assertIn("-D__spirv__=1", command)
            self.assertEqual(command[command.index("-e") + 1], "encodeMCUs")
            self.assertEqual(command[command.index("--target-env") + 1], "vulkan1.1")
            self.assertIn("-O", command)
            self.assertNotIn("-Od", command)
            Path(command[command.index("-o") + 1]).write_bytes(module())
            return subprocess.CompletedProcess(command, 0, "", "")
        with tempfile.TemporaryDirectory() as temporary, \
                patch.object(profile.platform, "platform", return_value="test-host"), \
                patch.object(profile.subprocess, "check_output", return_value="glslang test"), \
                patch.object(profile.subprocess, "run", side_effect=compile), \
                patch.object(profile, "child_cpu_seconds", side_effect=[0, .002] * 6), \
                contextlib.redirect_stdout(io.StringIO()):
            report = profile.run(Path(temporary), "glslang", repeats=2, modes=("production",))
            self.assertEqual(calls, ["legacy_o3", "current_o3", "current_o3", "legacy_o3", "legacy_o3", "current_o3"])
            self.assertEqual(len(report["cases"]), 2)
            for case in report["cases"]:
                self.assertEqual(case["flags"], ["-O"])
                self.assertEqual(len(case["runs"]), 2)
                self.assertEqual(case["median_cpu_ms"], 2)
            saved = json.loads((Path(temporary) / "report.json").read_text())
            self.assertEqual(saved["compiler_version"], "glslang test")


if __name__ == "__main__":
    unittest.main()
