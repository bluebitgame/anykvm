"""Compile shared compute HLSL to local SPIR-V build assets."""
import argparse
import hashlib
import json
import platform
from pathlib import Path
import shutil
import subprocess
import tempfile

import cross_toolchain

ROOT = Path(__file__).resolve().parent.parent
JPEG_SOURCE = ROOT / "internal/jpegcodec/shaders/jpeg420.hlsl"
JPEG_DECODE_SOURCE = ROOT / "internal/jpegcodec/shaders/jpeg_decode.hlsl"
PROGRESSIVE_SOURCE = ROOT / "internal/rdp/shaders/progressive.hlsl"
PROGRESSIVE_SURFACE_SOURCE = ROOT / "internal/rdp/shaders/progressive_surface.hlsl"
SOURCES = (
    ROOT / "internal/gpuconv/shaders/desktop.hlsl",
    JPEG_SOURCE,
    JPEG_DECODE_SOURCE,
    PROGRESSIVE_SOURCE,
    PROGRESSIVE_SURFACE_SOURCE,
)
GLSLANG_VERSION = "16.6.0"
GLSLANG_PACKAGES = {
    "darwin": ("macos-universal-release.tar.gz", "4f8c05f2888a73ed0b7b274bc524af9130e09ae94b43c0f3f9ed83cfae852834"),
    "linux-amd64": ("linux-x86_64-release.tar.gz", "2c34071f56ecf39d16233294d3739cf43f981fd39080f2fd9dccb782101191ce"),
    "windows-amd64": ("windows-x86_64-release.zip", "82bf434e69b9bb4829de7e2b4bc2c5e7a7861e53d66cf75e5cc70f5f694a8d9b"),
}


def pinned_compiler(allow_download):
    system = platform.system().lower()
    cpu = {"x86_64": "amd64", "AMD64": "amd64"}.get(platform.machine(), platform.machine())
    host = system if system == "darwin" else f"{system}-{cpu}"
    if host not in GLSLANG_PACKAGES:
        raise ValueError(f"install DXC or glslang on PATH for shader build host {host}")
    ending, checksum = GLSLANG_PACKAGES[host]
    name = f"glslang-{GLSLANG_VERSION}-{ending}"
    directory = cross_toolchain.CACHE / f"glslang-{GLSLANG_VERSION}-{host}"
    compiler = directory / "bin" / ("glslang.exe" if system == "windows" else "glslang")
    if not compiler.is_file() or not (directory / ".ready").is_file():
        archive = cross_toolchain.CACHE / "downloads" / name
        if not allow_download:
            if not archive.is_file() or cross_toolchain.digest(archive) != checksum:
                raise ValueError("Pinned glslang is not cached; install DXC/glslang or run without --offline/--no-bootstrap")
        else:
            archive = cross_toolchain.download(
                f"https://github.com/KhronosGroup/glslang/releases/download/{GLSLANG_VERSION}/{name}", checksum)
        cross_toolchain.install_archive(archive, directory)
    if not compiler.is_file():
        raise ValueError(f"Shader compiler is missing from {directory}")
    return str(compiler)


def shader_output(source, entry):
    suffix = "" if entry == "main" else "_" + entry
    return source.with_name(source.stem + suffix + ".spv")


def shader_source(source):
    text = source.read_bytes().decode("utf-8")
    if "// ANYKVM_TILE_COMPARE" in text:
        kernel = (ROOT / "internal/gpuconv/shaders/tile_compare.hlsl").read_bytes().decode("utf-8")
        entry = "compareTiles" if source == JPEG_SOURCE else "main"
        kernel = kernel.replace("void compareTile(", "void " + entry + "(", 1)
        text = text.replace("// ANYKVM_TILE_COMPARE", kernel)
    return text.encode()


def current(source, entry="main"):
    output = shader_output(source, entry)
    try:
        manifest = json.loads(output.with_suffix(".json").read_text())
        return (manifest.get("entry_point") == entry
                and manifest["source_sha256"] == hashlib.sha256(shader_source(source)).hexdigest()
                and manifest["spirv_sha256"] == hashlib.sha256(output.read_bytes()).hexdigest())
    except (OSError, ValueError, KeyError, TypeError):
        return False


def prepare(compiler=None, dry_run=False, allow_download=True):
    jobs = [(source, entry) for source in SOURCES
            for entry in (("compareTiles", "prepareWork", "main", "selectEntropy", "prepareEntropy", "encodeMCUs", "indexSegments", "scanSegmentGroups", "addSegmentOffsets", "packJPEG") if source == JPEG_SOURCE else ("main", "inverseDCT", "packNV12") if source == JPEG_DECODE_SOURCE else ("main", "encodeTiles", "indexSegments", "packSegments") if source == PROGRESSIVE_SOURCE else ("main",))]
    pending = [(source, entry) for source, entry in jobs if compiler is not None or not current(source, entry)]
    if not pending:
        return
    if dry_run:
        print("  Would generate compute SPIR-V (DXC/glslang, or a pinned glslang download).")
        return
    compiler = compiler or shutil.which("dxc") or shutil.which("glslangValidator") or shutil.which("glslang")
    if not compiler:
        compiler = pinned_compiler(allow_download)
    for source, entry in pending:
        compile_shader(source, compiler, entry)


def compile_shader(source, compiler, entry="main"):
    output = shader_output(source, entry)
    if Path(compiler).stem.lower() == "dxc":
        command = [compiler, "-spirv", "-T", "cs_6_0", "-E", entry, "-D__spirv__=1",
                   "-fspv-target-env=vulkan1.1", "-fvk-use-dx-layout", "-Fo", str(output), str(source)]
    else:
        command = [compiler, "-D", "-D__spirv__=1", "-V", "--target-env", "vulkan1.1",
                   "-S", "comp", "-e", entry, "-o", str(output), str(source)]
    # Runtime D3D and SPIR-V must see identical expanded shared source.
    expanded_source = shader_source(source)
    if expanded_source == source.read_bytes():
        subprocess.run(command, check=True)
    else:
        with tempfile.TemporaryDirectory() as temporary:
            expanded = Path(temporary) / source.name
            expanded.write_bytes(expanded_source)
            command[-1] = str(expanded)
            subprocess.run(command, check=True)

    manifest = {"entry_point": entry, "source_sha256": hashlib.sha256(shader_source(source)).hexdigest(),
                "spirv_sha256": hashlib.sha256(output.read_bytes()).hexdigest()}
    output.with_suffix(".json").write_text(json.dumps(manifest, indent=2) + "\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--compiler", help="DXC or glslang executable; forces recompilation when supplied")
    parser.add_argument("--offline", action="store_true", help="use installed or cached shader compilers only")
    args = parser.parse_args()
    try:
        prepare(args.compiler, allow_download=not args.offline)
    except (OSError, ValueError, subprocess.CalledProcessError) as exc:
        parser.error(str(exc))


if __name__ == "__main__":
    main()
