import contextlib
import io
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import build_shaders


class ShaderPreparationTests(unittest.TestCase):
    def test_missing_offline_compiler_never_downloads(self):
        with tempfile.TemporaryDirectory() as temporary, \
                patch.object(build_shaders.cross_toolchain, "CACHE", Path(temporary)), \
                patch.object(build_shaders.platform, "system", return_value="Darwin"), \
                patch.object(build_shaders.cross_toolchain, "download") as download:
            with self.assertRaisesRegex(ValueError, "not cached"):
                build_shaders.pinned_compiler(False)
            download.assert_not_called()

    def test_pinned_compiler_is_verified_and_reused_offline(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            compiler = root / f"glslang-{build_shaders.GLSLANG_VERSION}-darwin/bin/glslang"
            def install(*args):
                compiler.parent.mkdir(parents=True)
                compiler.write_bytes(b"compiler")
                compiler.parent.parent.joinpath(".ready").write_text("verified")
            with patch.object(build_shaders.cross_toolchain, "CACHE", root), \
                    patch.object(build_shaders.platform, "system", return_value="Darwin"), \
                    patch.object(build_shaders.cross_toolchain, "download", return_value=root / "archive") as download, \
                    patch.object(build_shaders.cross_toolchain, "install_archive", side_effect=install):
                self.assertEqual(build_shaders.pinned_compiler(True), str(compiler))
                self.assertEqual(download.call_args.args[1], build_shaders.GLSLANG_PACKAGES["darwin"][1])
                self.assertEqual(build_shaders.pinned_compiler(False), str(compiler))
                self.assertEqual(download.call_count, 1)

    def test_missing_changed_and_corrupt_assets_are_rebuilt(self):
        with tempfile.TemporaryDirectory() as temporary:
            source = Path(temporary) / "desktop.hlsl"
            source.write_text("first source")
            output = source.with_suffix(".spv")
            def compile_shader(*args, **kwargs):
                output.write_bytes(b"compiled " + source.read_bytes())
            with patch.object(build_shaders, "SOURCES", (source,)), \
                    patch.object(build_shaders.shutil, "which", return_value="glslang"), \
                    patch.object(build_shaders.subprocess, "run", side_effect=compile_shader) as compile:
                build_shaders.prepare()
                build_shaders.prepare()
                self.assertEqual(compile.call_count, 1)
                source.write_text("changed source")
                build_shaders.prepare()
                self.assertEqual(output.read_bytes(), b"compiled changed source")
                output.write_bytes(b"corrupt output")
                build_shaders.prepare()
                self.assertTrue(build_shaders.current(source))
                self.assertEqual(compile.call_count, 3)

    def test_dry_run_needs_no_compiler_and_writes_nothing(self):
        with tempfile.TemporaryDirectory() as temporary:
            source = Path(temporary) / "desktop.hlsl"
            source.write_text("source")
            with patch.object(build_shaders, "SOURCES", (source,)), \
                    patch.object(build_shaders.shutil, "which", return_value=None), \
                    patch.object(build_shaders.subprocess, "run") as compile, \
                    contextlib.redirect_stdout(io.StringIO()):
                build_shaders.prepare(dry_run=True)
                compile.assert_not_called()
                self.assertEqual(list(source.parent.iterdir()), [source])

    def test_each_shader_is_cached_and_rebuilt_independently(self):
        with tempfile.TemporaryDirectory() as temporary:
            sources = [Path(temporary) / name for name in ("desktop.hlsl", "jpeg420.hlsl")]
            for source in sources:
                source.write_text(source.stem)

            def compile_shader(command, **kwargs):
                output = Path(command[command.index("-o") + 1])
                output.write_bytes(Path(command[-1]).read_bytes())

            with patch.object(build_shaders, "SOURCES", tuple(sources)), \
                    patch.object(build_shaders.shutil, "which", return_value="glslang"), \
                    patch.object(build_shaders.subprocess, "run", side_effect=compile_shader) as compile:
                build_shaders.prepare()
                self.assertEqual(compile.call_count, 2)
                build_shaders.prepare()
                self.assertEqual(compile.call_count, 2)
                sources[1].write_text("changed JPEG shader")
                build_shaders.prepare()
                self.assertEqual(compile.call_count, 3)
                self.assertEqual(Path(compile.call_args.args[0][-1]), sources[1])

    def test_all_jpeg_stages_are_built_and_cached(self):
        with tempfile.TemporaryDirectory() as temporary:
            source = Path(temporary) / "jpeg420.hlsl"
            source.write_text("shared JPEG source")

            def compile_shader(command, **kwargs):
                entry = command[command.index("-e") + 1]
                Path(command[command.index("-o") + 1]).write_bytes(entry.encode())

            with patch.object(build_shaders, "SOURCES", (source,)), \
                    patch.object(build_shaders, "JPEG_SOURCE", source), \
                    patch.object(build_shaders.shutil, "which", return_value="glslang"), \
                    patch.object(build_shaders.subprocess, "run", side_effect=compile_shader) as compile:
                build_shaders.prepare()
                self.assertEqual(compile.call_count, 10)
                build_shaders.prepare()
                self.assertEqual(compile.call_count, 10)
                build_shaders.shader_output(source, "encodeMCUs").unlink()
                build_shaders.prepare()
                self.assertEqual(compile.call_count, 11)
                command = compile.call_args.args[0]
                self.assertEqual(command[command.index("-e") + 1], "encodeMCUs")
                for entry in ("compareTiles", "prepareWork", "main", "selectEntropy", "prepareEntropy", "encodeMCUs", "indexSegments", "scanSegmentGroups", "addSegmentOffsets", "packJPEG"):
                    self.assertTrue(build_shaders.current(source, entry))


    def test_shared_tile_kernel_invalidates_both_consumers(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            common = root / "internal/gpuconv/shaders/tile_compare.hlsl"
            common.parent.mkdir(parents=True)
            common.write_text("void compareTile() {}")
            jpeg = root / "jpeg420.hlsl"
            surface = root / "progressive_surface.hlsl"
            for source in (jpeg, surface):
                source.write_text("// ANYKVM_TILE_COMPARE")

            def compile_shader(command, **kwargs):
                entry = command[command.index("-e") + 1]
                Path(command[command.index("-o") + 1]).write_bytes(entry.encode())

            with patch.object(build_shaders, "ROOT", root), \
                    patch.object(build_shaders, "SOURCES", (jpeg, surface)), \
                    patch.object(build_shaders, "JPEG_SOURCE", jpeg), \
                    patch.object(build_shaders.shutil, "which", return_value="glslang"), \
                    patch.object(build_shaders.subprocess, "run", side_effect=compile_shader) as compile:
                build_shaders.prepare()
                self.assertEqual(compile.call_count, 11)
                build_shaders.prepare()
                self.assertEqual(compile.call_count, 11)
                common.write_text("void compareTile() { /* changed */ }")
                build_shaders.prepare()
                self.assertEqual(compile.call_count, 22)

    def test_all_progressive_stages_are_built_and_cached(self):
        with tempfile.TemporaryDirectory() as temporary:
            source = Path(temporary) / "progressive.hlsl"
            source.write_text("shared Progressive source")

            def compile_shader(command, **kwargs):
                entry = command[command.index("-e") + 1]
                Path(command[command.index("-o") + 1]).write_bytes(entry.encode())

            with patch.object(build_shaders, "SOURCES", (source,)), \
                    patch.object(build_shaders, "PROGRESSIVE_SOURCE", source), \
                    patch.object(build_shaders.shutil, "which", return_value="glslang"), \
                    patch.object(build_shaders.subprocess, "run", side_effect=compile_shader) as compile:
                build_shaders.prepare()
                self.assertEqual(compile.call_count, 4)
                build_shaders.prepare()
                self.assertEqual(compile.call_count, 4)
                build_shaders.shader_output(source, "encodeTiles").unlink()
                build_shaders.prepare()
                self.assertEqual(compile.call_count, 5)
                for entry in ("main", "encodeTiles", "indexSegments", "packSegments"):
                    self.assertTrue(build_shaders.current(source, entry))


if __name__ == "__main__":
    unittest.main()
