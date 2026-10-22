"""Prepare target-specific shared libraries without installing host packages.

FFmpeg Windows/Linux bundles are pinned BtbN builds. macOS FFmpeg and
TurboJPEG are built from checked sources using the selected target compiler.
System loaders, libc and GPU drivers remain supplied by the destination OS.
"""

import json
import lzma
import os
import platform
from pathlib import Path
import re
import shlex
import shutil
import struct
import subprocess
import tempfile

import cross_toolchain as toolchain
import package_ffmpeg


ROOT = Path(__file__).resolve().parent.parent
# Month-end releases are retained longer than BtbN's rolling daily builds.
FFMPEG_RELEASE = "autobuild-2026-09-30-13-08"
FFMPEG_BUILD = "n9.0.2-17-g2a571b6068"
FFMPEG_BUNDLES = {
    "linux/amd64": ("linux64", "01a9764d0b5364b66cfeb4617557c64321b0e232e172ec73f0a701c5f7694326"),
    "linux/arm64": ("linuxarm64", "706ccdbc8b0537646345532713a9b6750c0a6258cc001ec1c0379754f0ae730a"),
    "windows/amd64": ("win64", "3da6c7b60bb9ccd73ec5b5e815ba804a0879eb362ba0e3beebce50174c022696"),
    "windows/arm64": ("winarm64", "92460cf103340012d41413b557669902538f040e0c953abc1d719750dcd5f8a8"),
}
TURBOJPEG = ("https://github.com/libjpeg-turbo/libjpeg-turbo/releases/download/3.1.3/"
             "libjpeg-turbo-3.1.3.tar.gz",
             "075920b826834ac4ddf97661cc73491047855859affd671d52079c6867c1c6c0",
             "turbojpeg-3.1.3.tar.gz")
OPUS = ("https://downloads.xiph.org/releases/opus/opus-1.5.2.tar.gz",
        "65c1d2f78b9f2fb20082c38cbe47c951ad5839345876e46941612ee87f9a7ce1",
        "opus-1.5.2.tar.gz")
# glibc and its loader must stay paired with the destination OS.
SYSTEM_DEBS = {"libc6", "libc-bin", "libgcc-s1", "gcc-12-base", "debconf"}


def cached_download(url, checksum, name, allow_download):
    path = toolchain.CACHE / "downloads" / name
    if path.is_file() and (checksum is None or toolchain.digest(path) == checksum):
        return path
    if not allow_download:
        raise ValueError(f"{name} is not cached (--no-bootstrap prevents downloading)")
    return toolchain.download(url, checksum, name)


def source(spec, allow_download):
    url, checksum, name = spec
    archive = cached_download(url, checksum, name, allow_download)
    directory = toolchain.CACHE / ("source-" + name.removesuffix(".tar.gz").removesuffix(".tar.xz"))
    toolchain.install_archive(archive, directory)
    return directory


def compiler_words(command):
    words = shlex.shlex(command, posix=True)
    words.whitespace_split, words.escape, words.commenters = True, "", ""
    return list(words)


def build_tool(name, allow_download):
    installed = shutil.which(name)
    if installed:
        return installed
    system = platform.system().lower()
    arch = platform.machine().lower()
    arm = arch in ("arm64", "aarch64")
    if system not in ("darwin", "linux", "windows") or arch not in ("arm64", "aarch64", "amd64", "x86_64"):
        raise ValueError(f"Install {name} on this build host")
    if name == "cmake":
        repo, version = "Kitware/CMake", "4.1.2"
        host = ("macos-universal" if system == "darwin" else
                ("windows-" + ("arm64" if arm else "x86_64")) if system == "windows" else
                "linux-" + ("aarch64" if arm else "x86_64"))
        asset_name = f"cmake-{version}-{host}" + (".zip" if system == "windows" else ".tar.gz")
    else:
        repo, version = "ninja-build/ninja", "1.13.1"
        host = "mac" if system == "darwin" else ("win" if system == "windows" else "linux")
        asset_name = f"ninja-{host}{'arm64' if arm and system != 'darwin' else ''}.zip"
        if arm and system == "linux":
            asset_name = "ninja-linux-aarch64.zip"
    manifest = cached_download(f"https://api.github.com/repos/{repo}/releases/tags/v{version}", None,
                               repo.replace("/", "-") + f"-v{version}.json", allow_download)
    asset = next((item for item in json.loads(manifest.read_text())["assets"] if item["name"] == asset_name), None)
    if asset is None or not (asset.get("digest") or "").startswith("sha256:"):
        raise ValueError(f"No checksummed {name} archive for this host; install {name} on PATH")
    archive = cached_download(asset["browser_download_url"], asset["digest"][7:], asset_name, allow_download)
    directory = toolchain.CACHE / ("runtime-tool-" + asset_name.removesuffix(".zip").removesuffix(".tar.gz"))
    toolchain.install_archive(archive, directory)
    executable_name = name + (".exe" if system == "windows" else "")
    executable = next((p for p in directory.rglob(executable_name) if p.is_file()), None)
    if executable is None:
        raise ValueError(f"Missing {executable_name} in {asset_name}")
    if system != "windows":
        executable.chmod(executable.stat().st_mode | 0o111)
    return str(executable)


def cmake_build(source_dir, prefix, target, env, options=(), allow_download=True):
    cmake = build_tool("cmake", allow_download)
    ninja = build_tool("ninja", allow_download)
    system, arch = target.split("/")
    # Cached CMakeSystem.cmake overrides later -D processor/toolchain changes.
    if (prefix / "work").exists():
        shutil.rmtree(prefix / "work")
    compiler, *flags = compiler_words(env["CC"])
    compiler = toolchain.cmake_path(compiler)
    flags += compiler_words(env.get("CGO_CFLAGS", ""))
    arguments = [cmake, "-S", toolchain.cmake_path(source_dir), "-B", toolchain.cmake_path(prefix / "work"),
                 "-DCMAKE_BUILD_TYPE=Release", "-DCMAKE_INSTALL_PREFIX=" + toolchain.cmake_path(prefix),
                 "-DCMAKE_INSTALL_LIBDIR=lib", "-DCMAKE_C_COMPILER=" + compiler,
                 "-DCMAKE_C_FLAGS=" + toolchain.command_string(flags),
                 "-DCMAKE_SHARED_LINKER_FLAGS=" + env.get("CGO_LDFLAGS", ""),
                 "-DCMAKE_TRY_COMPILE_TARGET_TYPE=STATIC_LIBRARY", *options]
    if system == "darwin":
        arguments += ["-DCMAKE_SYSTEM_NAME=Darwin",
                      "-DCMAKE_SYSTEM_PROCESSOR=" + ("x86_64" if arch == "amd64" else "aarch64"),
                      "-DCMAKE_OSX_ARCHITECTURES=" + ("x86_64" if arch == "amd64" else "arm64"),
                      "-DCMAKE_OSX_DEPLOYMENT_TARGET=" + env.get("MACOSX_DEPLOYMENT_TARGET", "12.3")]
        if env.get("SDKROOT"):
            arguments += ["-DCMAKE_OSX_SYSROOT=" + toolchain.cmake_path(env["SDKROOT"])]
    else:
        arguments += ["-DCMAKE_SYSTEM_NAME=" + ("Windows" if system == "windows" else "Linux"),
                      "-DCMAKE_SYSTEM_PROCESSOR=" + ("x86_64" if arch == "amd64" else "aarch64")]
    arguments += ["-G", "Ninja", "-DCMAKE_MAKE_PROGRAM=" + toolchain.cmake_path(ninja)]
    if system == "windows":
        cpu = "x86_64" if arch == "amd64" else "aarch64"
        windres = Path(compiler).parent / (cpu + "-w64-mingw32-windres" + (".exe" if os.name == "nt" else ""))
        if windres.is_file():
            arguments += ["-DCMAKE_RC_COMPILER=" + toolchain.cmake_path(windres)]
    # CMake must not interpret Go's compiler command string as an executable.
    build_env = env.copy()
    build_env.pop("CC", None)
    build_env.pop("CXX", None)
    subprocess.run(arguments, env=build_env, check=True)
    subprocess.run([cmake, "--build", toolchain.cmake_path(prefix / "work"), "--parallel", str(min(os.cpu_count() or 2, 8))],
                   env=build_env, check=True)
    subprocess.run([cmake, "--install", toolchain.cmake_path(prefix / "work")], env=build_env, check=True)


def shared_files(directory):
    return sorted(p for p in directory.rglob("*") if p.is_file()
                  and (p.suffix.lower() in (".dll", ".dylib") or ".so" in p.name)
                  and "work" not in p.relative_to(directory).parts)


def validate_library(path, target):
    """Reject host/target mixups before publishing PE, ELF or Mach-O files."""
    system, arch = target.split("/")
    with path.open("rb") as stream:
        header = stream.read(64)
        valid = False
        if system == "windows" and header[:2] == b"MZ" and len(header) >= 64:
            stream.seek(struct.unpack_from("<I", header, 60)[0])
            pe = stream.read(6)
            valid = len(pe) == 6 and pe[:4] == b"PE\0\0" and struct.unpack_from("<H", pe, 4)[0] == (
                0x8664 if arch == "amd64" else 0xaa64)
        elif system == "linux" and header[:6] == b"\x7fELF\x02\x01" and len(header) >= 20:
            valid = struct.unpack_from("<H", header, 18)[0] == (62 if arch == "amd64" else 183)
        elif system == "darwin":
            cpu = 0x01000007 if arch == "amd64" else 0x0100000c
            if header[:4] == b"\xcf\xfa\xed\xfe" and len(header) >= 8:
                valid = struct.unpack_from("<I", header, 4)[0] == cpu
            elif len(header) >= 8 and header[:4] in (b"\xca\xfe\xba\xbe", b"\xca\xfe\xba\xbf"):
                count = struct.unpack_from(">I", header, 4)[0]
                size = 32 if header[:4] == b"\xca\xfe\xba\xbf" else 20
                stream.seek(8)
                entries = stream.read(min(count, 64) * size)
                valid = any(struct.unpack_from(">I", entries, offset)[0] == cpu
                            for offset in range(0, len(entries) - size + 1, size))
    if not valid:
        raise ValueError(f"Library does not match {target}: {path}")


def turbojpeg(target, host, env, allow_download):
    if target.startswith("darwin/") and not host.startswith("darwin/"):
        raise ValueError("macOS runtime preparation requires macOS and the Apple SDK")
    source_dir = source(TURBOJPEG, allow_download)
    prefix = toolchain.CACHE / ("runtime-turbojpeg-3.1.3-" + target.replace("/", "-"))
    # Include compiler/SDK flags so changing toolchains invalidates the build.
    recipe = json.dumps({"revision": 1, "cc": env["CC"], "cflags": env.get("CGO_CFLAGS", ""),
                         "ldflags": env.get("CGO_LDFLAGS", ""), "sdk": env.get("SDKROOT", "")}, sort_keys=True)
    stamp = prefix / ".recipe"
    if not stamp.is_file() or stamp.read_text() != recipe:
        options = ["-DENABLE_SHARED=ON", "-DENABLE_STATIC=OFF", "-DWITH_TURBOJPEG=ON",
                   "-DWITH_JAVA=OFF", "-DWITH_TOOLS=OFF", "-DWITH_TESTS=OFF"]
        if target.endswith("/amd64") and not shutil.which("nasm"):
            print("  NASM not found; building portable TurboJPEG without x86 assembly.", flush=True)
            options += ["-DWITH_SIMD=OFF"]
        cmake_build(source_dir, prefix, target, env, options, allow_download)
        stamp.write_text(recipe)
    files = [p for p in shared_files(prefix) if "turbojpeg" in p.name.lower()]
    if not files:
        raise ValueError(f"TurboJPEG build produced no shared library for {target}")
    return files, [source_dir / "LICENSE.md", source_dir / "README.ijg"]


def ffmpeg_macos(target, env, allow_download):
    group = next(g for g in json.loads((ROOT / "tools/sources.lock.json").read_text())["groups"]
                 if g["name"] == "ffmpeg")
    ffmpeg_source = source((group["url"], group["sha256"], "ffmpeg-runtime-9.0.2.tar.xz"), allow_download)
    lock = json.loads((ROOT / "tools/runtime-sources.lock.json").read_text())
    x264_source = source(tuple(lock["x264"]), allow_download)
    opus_source = source(OPUS, allow_download)
    prefix = toolchain.CACHE / ("runtime-ffmpeg-9.0.2-" + target.replace("/", "-"))
    arch = "x86_64" if target.endswith("amd64") else "arm64"
    recipe = json.dumps({"revision": 1, "arch": arch, "sdk": env.get("SDKROOT", ""),
                         "deployment": env.get("MACOSX_DEPLOYMENT_TARGET", "12.3")}, sort_keys=True)
    if not (prefix / ".recipe").is_file() or (prefix / ".recipe").read_text() != recipe:
        for tool in ("make", "pkg-config", "clang"):
            if not shutil.which(tool):
                raise ValueError(f"Automatic macOS FFmpeg builds require {tool}")
        opus_prefix, x264_prefix = prefix / "opus", prefix / "x264"
        cmake_build(opus_source, opus_prefix, target, {**env, "CC": "clang"},
                    ["-DBUILD_SHARED_LIBS=OFF", "-DCMAKE_POSITION_INDEPENDENT_CODE=ON",
                     "-DOPUS_BUILD_TESTING=OFF", "-DOPUS_BUILD_PROGRAMS=OFF"], allow_download)
        x264_work = prefix / "x264-work"
        x264_work.mkdir(parents=True, exist_ok=True)
        build_env = env.copy()
        deployment = env.get("MACOSX_DEPLOYMENT_TARGET", "12.3")
        build_env.update(CC="clang", PKG_CONFIG="pkg-config", CFLAGS=f"-arch {arch} -mmacosx-version-min={deployment}",
                         LDFLAGS=f"-arch {arch} -mmacosx-version-min={deployment}")
        cpu = "x86_64" if arch == "x86_64" else "aarch64"
        subprocess.run(["sh", str(x264_source / "configure"), "--prefix=" + str(x264_prefix),
                        "--host=" + cpu + "-apple-darwin", "--enable-static", "--enable-pic",
                        "--disable-cli", "--disable-opencl", "--disable-asm"], cwd=x264_work,
                       env=build_env, check=True)
        subprocess.run(["make", "-j", str(min(os.cpu_count() or 2, 8))], cwd=x264_work, env=build_env, check=True)
        subprocess.run(["make", "install"], cwd=x264_work, env=build_env, check=True)
        build_env.update(ANYKVM_FFMPEG_ARCH=arch, ANYKVM_FFMPEG_JOBS=str(min(os.cpu_count() or 2, 8)))
        subprocess.run(["bash", str(ROOT / "tools/build_ffmpeg_macos.sh"), str(ffmpeg_source),
                        str(x264_prefix), str(opus_prefix), str(prefix)], env=build_env, check=True)
        (prefix / "lib/licenses").mkdir(exist_ok=True)
        shutil.copy2(x264_source / "COPYING", prefix / "lib/licenses/x264-COPYING")
        shutil.copy2(opus_source / "COPYING", prefix / "lib/licenses/opus-COPYING")
        (prefix / ".recipe").write_text(recipe)
    return prefix / "lib"


def ffmpeg(target, host, env, allow_download):
    if target.startswith("darwin/"):
        if not host.startswith("darwin/"):
            raise ValueError("macOS FFmpeg preparation requires macOS and the Apple SDK")
        return ffmpeg_macos(target, env, allow_download)
    platform, checksum = FFMPEG_BUNDLES[target]
    suffix = ".zip" if target.startswith("windows/") else ".tar.xz"
    name = f"ffmpeg-{FFMPEG_BUILD}-{platform}-gpl-shared-9.0{suffix}"
    archive = cached_download(f"https://github.com/BtbN/FFmpeg-Builds/releases/download/{FFMPEG_RELEASE}/{name}",
                              checksum, name, allow_download)
    directory = toolchain.CACHE / ("runtime-" + name.removesuffix(suffix))
    toolchain.install_archive(archive, directory)
    return directory / ("bin" if target.startswith("windows/") else "lib")


def windows_opus(target, env, allow_download):
    # Build with the same MinGW compiler as the application, avoiding the
    # additional Visual C++ redistributable required by the optional NuGet DLL.
    source_dir = source(OPUS, allow_download)
    prefix = toolchain.CACHE / ("runtime-opus-1.5.2-" + target.replace("/", "-"))
    recipe = json.dumps({"revision": 3, "cc": env["CC"], "cflags": env.get("CGO_CFLAGS", "")}, sort_keys=True)
    stamp = prefix / ".recipe"
    if not stamp.is_file() or stamp.read_text() != recipe:
        options = ["-DBUILD_SHARED_LIBS=ON", "-DOPUS_BUILD_TESTING=OFF", "-DOPUS_BUILD_PROGRAMS=OFF"]
        if target.endswith("/arm64"):
            # AArch64 guarantees NEON. Opus 1.5's ARM runtime CPU detection
            # supports Linux/macOS, but not MinGW on Windows ARM64.
            options += ["-DOPUS_MAY_HAVE_NEON=OFF", "-DOPUS_PRESUME_NEON=ON"]
            # Opus 1.5's headers also gate NEON declarations on MAY_HAVE,
            # even when NEON is presumed and runtime dispatch is disabled.
            options += ["-DCMAKE_C_FLAGS_RELEASE=-O3 -DNDEBUG "
                        "-DOPUS_ARM_MAY_HAVE_NEON -DOPUS_ARM_MAY_HAVE_NEON_INTR"]
        cmake_build(source_dir, prefix, target, env, options, allow_download)
        stamp.write_text(recipe)
    files = [p for p in shared_files(prefix) if "opus" in p.name.lower() and p.suffix.lower() == ".dll"]
    if len(files) != 1:
        raise ValueError(f"Expected one Opus DLL for {target}, found {len(files)}")
    return files[0], source_dir / "COPYING"


def debian_index(arch, allow_download):
    name = f"debian-bookworm-{arch}-Packages.xz"
    path = cached_download(f"https://deb.debian.org/debian/dists/bookworm/main/binary-{arch}/Packages.xz",
                           None, name, allow_download)
    packages, fields = {}, {}
    with lzma.open(path, "rt", encoding="utf-8") as stream:
        for line in stream:
            if line == "\n":
                if "Package" in fields:
                    packages[fields["Package"]] = fields
                    for virtual in fields.get("Provides", "").split(","):
                        packages.setdefault(virtual.strip().split(" ")[0], fields)
                fields = {}
            elif not line.startswith((" ", "\t")) and ": " in line:
                key, value = line.rstrip("\n").split(": ", 1)
                fields[key] = value
    return packages


def debian_closure(packages, names):
    pending, selected = list(names), {}
    while pending:
        name = pending.pop()
        if name in SYSTEM_DEBS or name in selected:
            continue
        if name not in packages:
            raise ValueError(f"Missing Debian runtime package {name}")
        package = packages[name]
        if package.get("Package", name) in SYSTEM_DEBS:
            continue
        selected[name] = package
        for dependency in (package.get("Pre-Depends", "") + "," + package.get("Depends", "")).split(","):
            alternatives = [re.split(r"[\s:(]", part.strip())[0] for part in dependency.split("|")]
            alternatives = [part for part in alternatives if part]
            if not alternatives:
                continue
            chosen = next((part for part in alternatives if part in packages or part in SYSTEM_DEBS), None)
            if chosen is None:
                raise ValueError(f"Cannot resolve {dependency} required by {name}")
            pending.append(chosen)
    return selected


def linux_libraries(arch, features, allow_download):
    names = ["libv4l-0", "libv4lconvert0", "libasound2"]
    if "viewer" in features:
        names += ["libsdl2-2.0-0"]
    if "spice" in features:
        names += ["libspice-client-glib-2.0-8", "libegl1", "libgbm1", "libdrm2"]
    directory = toolchain.CACHE / f"runtime-linux-system-{arch}"
    index = debian_index(arch, allow_download)
    packages = debian_closure(index, names)
    files = []
    for name, package in sorted(packages.items()):
        archive = cached_download("https://deb.debian.org/debian/" + package["Filename"],
                                   package["SHA256"], Path(package["Filename"]).name, allow_download)
        extracted = directory / (package["Package"] + "-" + package["SHA256"][:12])
        if not (extracted / ".ready").is_file():
            extracted.mkdir(parents=True, exist_ok=True)
            toolchain.extract_deb(archive, extracted)
            (extracted / ".ready").touch()
        for path in shared_files(extracted):
            # Keep plugin subdirectories; flatten only the linker search directory.
            relative = path.relative_to(extracted)
            parts = relative.parts
            triple = "x86_64-linux-gnu" if arch == "amd64" else "aarch64-linux-gnu"
            if triple in parts:
                relative = Path(*parts[parts.index(triple) + 1:])
            else:
                relative = Path(path.name)
            files.append((path, Path("lib") / relative))
        copyright_file = extracted / "usr/share/doc" / package["Package"] / "copyright"
        if copyright_file.is_file():
            files.append((copyright_file, Path("licenses/runtime") / (package["Package"] + "-copyright")))
    return files


def override(env, target, name):
    prefix = "ANYKVM_BUILD_" + target.replace("/", "_").upper() + "_"
    return env.get(prefix + name)


def viewer_libraries(target, host, allow_download):
    system, arch = target.split("/")
    if system == "linux":
        return []  # Included in the Debian runtime closure.
    suffix = "darwin" if system == "darwin" else "windows-arm64" if arch == "arm64" else "windows"
    directory = toolchain.CACHE / f"SDL2-{toolchain.SDL_VERSION}-{suffix}"
    if not (directory / ".ready").is_file():
        if not allow_download:
            raise ValueError(f"SDL2 for {target} is not cached (--no-bootstrap prevents downloading)")
        if system == "darwin":
            if not host.startswith("darwin/"):
                raise ValueError("Automatic macOS SDL2 preparation requires macOS")
            toolchain.macos_sdl(False)
        else:
            toolchain.windows_sdl(arch, host, False)
    if system == "darwin":
        files = [(p, Path("lib") / p.name) for p in sorted((directory / "lib").glob("libSDL2*.dylib"))]
        notice = directory / "LICENSE.txt"
    else:
        dll = directory / ("bin/SDL2.dll" if arch == "arm64" else "lib/x64/SDL2.dll")
        files = [(dll, Path("SDL2.dll"))]
        notice = (toolchain.CACHE / f"SDL2-{toolchain.SDL_VERSION}-source" / "LICENSE.txt"
                  if arch == "arm64" else directory / "LICENSE.txt")
    if not files or not notice.is_file() or any(not p.is_file() for p, _ in files):
        raise ValueError(f"Incomplete SDL2 runtime cache for {target}")
    return [*files, (notice, Path("SDL2-LICENSE.txt"))]


def prepare(target, host, env, tags="", dry_run=False, allow_download=True):
    features = set(tags.replace(",", " ").split())
    linker_flags = []
    description = [] if "ffmpeg" in features else ["TurboJPEG"]
    if "ffmpeg" in features:
        description += ["FFmpeg 9 (with x264/Opus)"]
    elif target.startswith("windows/"):
        description += ["Opus"]
    if "viewer" in features and not target.startswith("linux/"):
        description += ["SDL2"]
    if target.startswith("linux/"):
        description += ["V4L2/ALSA" + ("/SDL2" if "viewer" in features else "")]
        # DT_RPATH also covers transitive dependencies of dlopened libraries.
        linker_flags = ["-Wl,--disable-new-dtags,-rpath,$ORIGIN/lib"]
    elif target.startswith("darwin/") and "viewer" in features:
        if "-Wl,-rpath,@executable_path/lib" not in env.get("CGO_LDFLAGS", ""):
            linker_flags = ["-Wl,-rpath,@executable_path/lib"]
    print(f"[{target}] Preparing runtime libraries: {', '.join(description)}.", flush=True)
    if dry_run:
        if linker_flags:
            toolchain.append_flags(env, "CGO_LDFLAGS", linker_flags)
        return []
    output = []
    toolchain.CACHE.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=".runtime-", dir=toolchain.CACHE) as temporary:
        staging = Path(temporary)
        if "ffmpeg" not in features:
            jpeg_override = override(env, target, "TURBOJPEG_LIBDIR")
            if jpeg_override:
                jpeg_files = [p for p in shared_files(Path(jpeg_override)) if "turbojpeg" in p.name.lower()]
                jpeg_notices = [p for p in (Path(jpeg_override) / "LICENSE.md", Path(jpeg_override) / "README.ijg")
                                if p.is_file()]
                if not jpeg_files:
                    raise ValueError(f"No TurboJPEG library in {jpeg_override}")
            else:
                jpeg_files, jpeg_notices = turbojpeg(target, host, env, allow_download)
            for path in jpeg_files:
                validate_library(path, target)
            package_ffmpeg.stage_libraries(jpeg_files, staging / "lib", [p.parent for p in jpeg_files])
            for path in jpeg_notices:
                (staging / "licenses/turbojpeg").mkdir(parents=True, exist_ok=True)
                shutil.copy2(path, staging / "licenses/turbojpeg" / path.name)
        if "ffmpeg" in features:
            custom = override(env, target, "FFMPEG_LIBDIR")
            ffmpeg_dir = Path(custom) if custom else ffmpeg(target, host, env, allow_download)
            package_ffmpeg.stage(ffmpeg_dir, staging / "lib")
            # Prebuilt distributions carry notices beside bin/lib rather than inside it.
            for name in ("LICENSE.txt", "LICENSE", "COPYING.GPLv2", "COPYING.LGPLv2.1"):
                path = ffmpeg_dir.parent / name
                if path.is_file():
                    (staging / "licenses/ffmpeg").mkdir(parents=True, exist_ok=True)
                    shutil.copy2(path, staging / "licenses/ffmpeg" / name)
        elif target.startswith("windows/"):
            dll, notice = windows_opus(target, env, allow_download)
            validate_library(dll, target)
            shutil.copy2(dll, staging / "lib/opus.dll")
            shutil.copy2(notice, staging / "lib/opus-LICENSE.txt")
        for path in shared_files(staging):
            validate_library(path, target)
        # Keep only complete prepared bundles in the cache; failed preparation
        # cannot change an existing successful release.
        bundle = toolchain.CACHE / ("runtime-bundle-" + target.replace("/", "-") +
                                    ("-ffmpeg" if "ffmpeg" in features else "-native"))
        if bundle.exists():
            shutil.rmtree(bundle)
        shutil.copytree(staging, bundle, symlinks=True)
        output += [(p, p.relative_to(bundle)) for p in sorted(bundle.rglob("*")) if p.is_file()]
    if target.startswith("linux/"):
        output += linux_libraries(target.split("/")[1], features, allow_download)
    elif "viewer" in features:
        output += viewer_libraries(target, host, allow_download)
    for path, relative in output:
        if path.suffix.lower() in (".dll", ".dylib") or ".so" in path.name:
            validate_library(path, target)
    # Executable search paths must not become build flags for its dependencies.
    if linker_flags:
        toolchain.append_flags(env, "CGO_LDFLAGS", linker_flags)
    return output
