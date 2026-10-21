#!/usr/bin/env python3
"""Stage FFmpeg 9 shared libraries into an application's lib folder.

On macOS, include all non-system dylib dependencies and rewrite their paths.
Use --dependency-dir for dependencies outside their recorded install paths
(for example, extracted Homebrew bottles). Linux dependencies need staging
separately; Windows distributions supply their dependencies alongside the DLLs.
"""
import argparse
import pathlib
import shutil
import subprocess
import sys

NAMES = ("avcodec", "avutil", "avformat", "avdevice", "avfilter", "swscale", "swresample")
MAJORS = {"avutil": 61, "swresample": 7, "swscale": 10, "avcodec": 63,
          "avformat": 63, "avdevice": 63, "avfilter": 12}


def dependencies(path):
    # The first entry of otool -L for a dylib is its own install name.
    lines = subprocess.check_output(["otool", "-L", str(path)], text=True).splitlines()[2:]
    return [line.strip().split(" (", 1)[0] for line in lines]


def macos_libraries(files, search_dirs):
    lookup = {}
    for directory in search_dirs:
        for path in sorted(directory.rglob("*.dylib")):
            if path.is_file():
                lookup.setdefault(path.name, path.resolve())
    staged, pending, visited = {}, list(files), set()
    while pending:
        path = pending.pop()
        real = path.resolve(strict=True)
        for name in (path.name, real.name):
            if name in staged and staged[name] != real:
                raise ValueError(f"conflicting libraries named {name}")
            staged[name] = real
        if real in visited:
            continue
        visited.add(real)
        linked = dependencies(real)
        # SDL2-compat opens SDL3 with dlopen; otool cannot see this dependency.
        if real.name.startswith("libSDL2") and b"sdl2-compat" in real.read_bytes():
            linked = [*linked, "@loader_path/libSDL3.dylib"]
        for dependency in linked:
            if dependency.startswith(("/usr/lib/", "/System/Library/")):
                continue
            candidate = pathlib.Path(dependency.replace("@loader_path/", str(real.parent) + "/"))
            if not candidate.is_file():
                candidate = lookup.get(pathlib.Path(dependency).name)
            if candidate is None and pathlib.Path(dependency).name == "libSDL3.dylib":
                candidate = next((p for p in (pathlib.Path("/opt/homebrew/opt/sdl3/lib/libSDL3.dylib"),
                                             pathlib.Path("/usr/local/opt/sdl3/lib/libSDL3.dylib"))
                                  if p.is_file()), None)
            if candidate is None or not candidate.is_file():
                raise ValueError(f"missing dependency {dependency} required by {real}; use --dependency-dir")
            name = pathlib.Path(dependency).name
            resolved = candidate.resolve(strict=True)
            if name in staged and staged[name] != resolved:
                raise ValueError(f"conflicting libraries named {name}")
            staged[name] = resolved
            pending.append(candidate)
    return staged


def stage(source, destination, dependency_dirs=()):
    source, destination = source.resolve(), destination.resolve()
    if source == destination:
        raise ValueError("source and destination must differ")
    files = sorted(p for p in source.iterdir()
                   if any(p.name.startswith(prefix + name) for name in NAMES for prefix in ("lib", ""))
                   and (".so" in p.name or p.suffix.lower() in (".dylib", ".dll")))
    if not files:
        raise ValueError("no FFmpeg shared libraries found")
    windows = any(p.suffix.lower() == ".dll" for p in files)
    macos = any(p.suffix == ".dylib" for p in files)
    required = [f"{name}-{major}.dll" if windows else
                f"lib{name}.{major}.dylib" if macos else f"lib{name}.so.{major}"
                for name, major in MAJORS.items()]
    available = {p.name.lower() for p in files if p.is_file()}
    missing = [name for name in required if name.lower() not in available]
    if missing:
        raise ValueError("missing FFmpeg 9 libraries: " + ", ".join(missing))
    if windows:
        files = sorted(p for p in source.iterdir() if p.is_file() and p.suffix.lower() == ".dll")
    return stage_libraries(files, destination, [source, *dependency_dirs], source / "licenses")


def stage_libraries(files, destination, search_dirs=(), licenses=None):
    """Stage a shared-library graph, including relocatable macOS dependencies."""
    destination = destination.resolve()
    macos = any(p.suffix == ".dylib" for p in files)
    if macos and sys.platform != "darwin":
        raise ValueError("macOS dylib packaging requires otool, install_name_tool and codesign on macOS")
    # Resolve the complete graph before writing anything into the destination.
    staged = macos_libraries(files, search_dirs) if macos else {p.name: p.resolve() for p in files}
    if any(path.parent == destination for path in staged.values()):
        raise ValueError("destination must not contain source dependencies")
    destination.mkdir(parents=True, exist_ok=True)
    for name, path in sorted(staged.items()):
        target = destination / name
        if target.is_symlink() or target.exists():
            target.unlink()
        if name != path.name and path.name in staged:
            target.symlink_to(path.name)
        else:
            shutil.copy2(path, target)
    if macos:
        for name in sorted(staged):
            target = destination / name
            if target.is_symlink():
                continue
            command = ["install_name_tool", "-id", "@rpath/" + name]
            for dependency in dependencies(target):
                basename = pathlib.Path(dependency).name
                if basename in staged:
                    command.extend(["-change", dependency, "@loader_path/" + basename])
            subprocess.run([*command, str(target)], check=True)
            subprocess.run(["codesign", "--force", "--sign", "-", str(target)], check=True)
    if licenses is not None and licenses.is_dir():
        shutil.copytree(licenses, destination / "licenses", dirs_exist_ok=True)
    return len(staged)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("source", type=pathlib.Path)
    parser.add_argument("destination", type=pathlib.Path)
    parser.add_argument("--dependency-dir", type=pathlib.Path, action="append", default=[],
                        help="recursively search this directory for macOS dylib dependencies (repeatable)")
    args = parser.parse_args()
    try:
        count = stage(args.source, args.destination, args.dependency_dir)
    except (ValueError, OSError, subprocess.CalledProcessError) as exc:
        parser.error(str(exc))
    print(f"Staged {count} shared libraries in {args.destination.resolve()}")


if __name__ == "__main__":
    main()
