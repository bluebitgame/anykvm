#!/usr/bin/env python3
"""Build AnyKVM for macOS, Linux and Windows (amd64 and arm64).

Requires Go. Missing cross toolchains and target libraries are prepared locally.
macOS builds require the Apple SDK; Windows ARM64 SDL2 preparation requires CMake.
Run without arguments to choose platform, architecture, viewer support and FFmpeg support.
FFmpeg is excluded by default; --tags ffmpeg enables its optional media backend.
Target-specific environment overrides use ANYKVM_BUILD_<OS>_<ARCH>_<VARIABLE>,
for example ANYKVM_BUILD_WINDOWS_ARM64_CC=aarch64-w64-mingw32-gcc.
See docs/build.md for dependency setup and examples.
"""

import argparse
import json
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import sys
import tempfile

import cross_toolchain
import package_opus
import build_shaders
import prepare_sources


ROOT = Path(__file__).resolve().parent.parent
TARGETS = tuple(f"{system}/{arch}" for system in ("linux", "windows", "darwin")
                for arch in ("amd64", "arm64"))
VARIABLES = ("CC", "CXX", "CGO_CFLAGS", "CGO_CPPFLAGS", "CGO_CXXFLAGS",
             "CGO_LDFLAGS", "PKG_CONFIG", "PKG_CONFIG_PATH", "PKG_CONFIG_LIBDIR",
             "PKG_CONFIG_SYSROOT_DIR", "SDKROOT", "MACOSX_DEPLOYMENT_TARGET")


def choose(title, options):
    print(f"\n{title}", flush=True)
    for index, (label, _) in enumerate(options):
        print(f"  {index}) {label}")
    while True:
        answer = input("Enter a number (Enter for 0, q to quit): ").strip().lower() or "0"
        if answer == "q":
            raise SystemExit(0)
        for index, (_, value) in enumerate(options):
            if answer == str(index):
                return value
        print(f"Enter a number from 0 to {len(options) - 1}.", flush=True)


def interactive_selection():
    systems = choose("Select target platform", (("All platforms", ("linux", "windows", "darwin")),
                                                ("Linux", ("linux",)),
                                                ("Windows", ("windows",)),
                                                ("macOS", ("darwin",))))
    arches = choose("Select target architecture", (("All architectures", ("amd64", "arm64")),
                                                   ("amd64 (x86-64)", ("amd64",)),
                                                   ("arm64 (AArch64)", ("arm64",))))
    viewer = choose("Include viewer", (("No (default)", False),
                                      ("Yes (requires SDL2)", True)))
    tags = choose("Include FFmpeg", (("No (default)", ""),
                                     ("Yes", "ffmpeg")))
    if viewer:
        tags = ",".join(filter(None, ("viewer", tags)))
    return [f"{system}/{arch}" for system in systems for arch in arches], tags


def target_environment(target, host, native_cc):
    system, arch = target.split("/")
    cpu = "x86_64" if arch == "amd64" else "aarch64"
    env = os.environ.copy()
    # Host include/library paths must not leak into a different target's build.
    if target != host:
        for name in VARIABLES:
            env.pop(name, None)
    prefix = "ANYKVM_BUILD_" + target.replace("/", "_").upper() + "_"
    for name in VARIABLES:
        if prefix + name in os.environ:
            env[name] = os.environ[prefix + name]
    if not env.get("CC"):
        if target == host:
            env["CC"] = native_cc
        elif system == "darwin":
            env["CC"] = "clang" if host.startswith("darwin/") else f"{cpu}-apple-darwin-clang"
        else:
            triple = "linux-gnu" if system == "linux" else "w64-mingw32"
            env["CC"] = f"{cpu}-{triple}-gcc"
    env.update(GOOS=system, GOARCH=arch, CGO_ENABLED="1")
    # The native macOS implementation uses ScreenCaptureKit (macOS 12.3+).
    if system == "darwin":
        env.setdefault("MACOSX_DEPLOYMENT_TARGET", "12.3")
    return env


def build_target(target, output, env, dry_run=False, runtime_files=(), tags=""):
    destination = output / target.replace("/", "-")
    suffix = ".exe" if target.startswith("windows/") else ""
    print(f"\n[{target}] CC={env['CC']}", flush=True)
    if not dry_run:
        # Go accepts quoted compiler paths without unescaping backslashes.
        # Preserve Windows paths such as C:\\llvm-mingw\\bin\\clang.exe.
        compiler_words = shlex.shlex(env["CC"], posix=True)
        compiler_words.whitespace_split = True
        compiler_words.escape = ""
        compiler_words.commenters = ""
        compiler = next(compiler_words, "")
        if not compiler or not shutil.which(compiler):
            raise ValueError(f"C compiler not found: {env['CC']}; set "
                             f"ANYKVM_BUILD_{target.replace('/', '_').upper()}_CC")

    def command(directory):
        args = ["go", "build", "-p", "2", "-trimpath", "-buildvcs=false", "-ldflags=-s -w"]
        if tags:
            args += ["-tags", tags]
        return args + ["-o", str(directory / f"anykvm{suffix}"), "./cmd/anykvm"]

    if dry_run:
        for name in VARIABLES:
            if env.get(name) and name != "CC":
                print(f"  {name}={env[name]}")
        print(f"  GOOS={env['GOOS']} GOARCH={env['GOARCH']} CGO_ENABLED=1")
        join = subprocess.list2cmdline if os.name == "nt" else shlex.join
        print("  " + join(command(destination)))
        return

    # Publish the binary after this target builds successfully.
    # A failed rebuild leaves the previous successful binaries intact.
    with tempfile.TemporaryDirectory(prefix=".anykvm-", dir=output) as temporary:
        staging = Path(temporary)
        print("  Building anykvm...", flush=True)
        subprocess.run(command(staging), cwd=ROOT, env=env, check=True)
        for source, relative in runtime_files:
            target_file = staging / relative
            target_file.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(source, target_file)
        destination.mkdir(parents=True, exist_ok=True)
        name = f"anykvm{suffix}"
        os.replace(staging / name, destination / name)
        for _, relative in runtime_files:
            (destination / relative).parent.mkdir(parents=True, exist_ok=True)
            os.replace(staging / relative, destination / relative)
    print(f"  Built {destination}", flush=True)


def main(argv=None):
    if argv is None:
        argv = sys.argv[1:]
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--targets", nargs="+", default=["all"],
                        help="all (default), native, or targets such as darwin/amd64 linux/arm64")
    parser.add_argument("--tags", default="",
                        help="Go build tags; use viewer for SDL2 GUI, ffmpeg for media, or viewer,ffmpeg")
    parser.add_argument("--output", type=Path, default=ROOT / "build" / "releases",
                        help="output directory (default: build/releases)")
    parser.add_argument("--dry-run", action="store_true", help="print the build matrix without compiling")
    parser.add_argument("--no-bootstrap", action="store_true",
                        help="use only installed/custom toolchains; do not prepare dependencies")
    args = parser.parse_args(argv)
    if not argv:
        try:
            args.targets, args.tags = interactive_selection()
        except EOFError:
            print("\nNo selection received. Build canceled.", file=sys.stderr)
            return 1
    for target in args.targets:
        if target not in (*TARGETS, "all", "native"):
            parser.error(f"unknown target {target!r}; choose from: {', '.join(TARGETS)}")
    try:
        # Discover the execution host even if the caller has GOOS/GOARCH exported.
        host_env = os.environ.copy()
        host_env.pop("GOOS", None)
        host_env.pop("GOARCH", None)
        try:
            info = json.loads(subprocess.check_output(
                ["go", "env", "-json", "GOHOSTOS", "GOHOSTARCH", "CC"],
                cwd=ROOT, env=host_env, text=True))
        except FileNotFoundError as exc:
            if not ROOT.is_dir():
                raise
            raise ValueError(
                "Cannot start 'go'. Install Go 1.24.5 or newer and add its bin directory "
                "to PATH (usually C:\\Program Files\\Go\\bin on Windows). "
                "Reopen your terminal or IDE after installation, then verify 'go version' "
                "works in the same terminal before running build.py again.") from exc
        host = f"{info['GOHOSTOS']}/{info['GOHOSTARCH']}"
        targets = []
        for selected in args.targets:
            expanded = TARGETS if selected == "all" else (host if selected == "native" else selected,)
            for target in expanded:
                if target not in TARGETS:
                    raise ValueError(f"unsupported host target: {target}")
                if target not in targets:
                    targets.append(target)
        prepare_sources.prepare(platforms={target.split('/')[0] for target in targets},
                                ffmpeg="ffmpeg" in args.tags.replace(",", " ").split(),
                                allow_download=not args.no_bootstrap, dry_run=args.dry_run)
        if any(target.startswith("linux/") for target in targets):
            build_shaders.prepare(dry_run=args.dry_run, allow_download=not args.no_bootstrap)
        output = args.output.resolve()
        if not args.dry_run:
            output.mkdir(parents=True, exist_ok=True)
            os.environ.setdefault("GOCACHE", str(ROOT / "build" / "go-cache"))
        succeeded, failed = [], []
        for target in targets:
            try:
                env = target_environment(target, host, info["CC"])
                runtime_files = []
                if not args.no_bootstrap:
                    runtime_files = cross_toolchain.prepare(target, host, env, "viewer" in args.tags.replace(",", " ").split(), args.dry_run)
                runtime_files += package_opus.runtime_files(target, args.tags, args.dry_run,
                                                           allow_download=not args.no_bootstrap)
                if not args.dry_run:
                    runtime_files += prepare_sources.notice_files(
                        {target.split('/')[0]}, "ffmpeg" in args.tags.replace(",", " ").split())
                build_target(target, output, env, args.dry_run, runtime_files, args.tags)
                succeeded.append(target)
            except (OSError, ValueError, subprocess.CalledProcessError) as exc:
                failed.append(target)
                print(f"[{target}] FAILED: {exc}\n"
                      "  Check the target C toolchain/SDK and SDL2 development library; "
                      "Linux also needs V4L2 and ALSA development libraries.", file=sys.stderr, flush=True)
        print(f"\n{'Planned' if args.dry_run else 'Built'} {len(succeeded)}/{len(targets)} targets.", flush=True)
        if failed:
            print("Failed targets: " + ", ".join(failed), file=sys.stderr)
            return 1
        return 0
    except (OSError, ValueError, subprocess.CalledProcessError) as exc:
        print(f"Build setup failed: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        sys.exit(130)
