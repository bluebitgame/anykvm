"""Prepare project-local, checksummed C toolchains and target development libraries."""

import hashlib
import http.client
import io
import json
import lzma
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import time
import urllib.error
import urllib.request
import zipfile


ROOT = Path(__file__).resolve().parent.parent
CACHE = ROOT / "build" / "toolchains"
LLVM_VERSION = "20260616"
SDL_VERSION = "2.32.10"


def command_string(args):
    # Match Go's quoted.Split: quotes group words without unescaping backslashes.
    words = []
    for arg in map(str, args):
        if not any(c.isspace() or c in "\"'" for c in arg):
            words.append(arg)
        elif '"' not in arg:
            words.append('"' + arg + '"')
        elif "'" not in arg:
            words.append("'" + arg + "'")
        else:
            raise ValueError("Toolchain paths cannot contain both single and double quotes")
    return " ".join(words)


def digest(path):
    result = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            result.update(chunk)
    return result.hexdigest()


def download(url, checksum=None, name=None):
    directory = CACHE / "downloads"
    directory.mkdir(parents=True, exist_ok=True)
    destination = directory / (name or url.rsplit("/", 1)[-1])
    if destination.is_file() and (checksum is None or digest(destination) == checksum):
        return destination
    print(f"  Downloading {destination.name}...", flush=True)
    temporary = destination.with_name(destination.name + ".part")
    stalled = 0
    for attempt in range(64):
        offset = temporary.stat().st_size if temporary.is_file() else 0
        try:
            headers = {"User-Agent": "AnyKVM-build"}
            if offset:
                headers["Range"] = f"bytes={offset}-"
            request = urllib.request.Request(url, headers=headers)
            with urllib.request.urlopen(request, timeout=60) as response:
                if response.status == 206:
                    content_range = response.headers.get("Content-Range", "")
                    if not content_range.startswith(f"bytes {offset}-"):
                        temporary.unlink(missing_ok=True)
                        raise ValueError("Server returned an inconsistent download range")
                    expected_size = int(content_range.rsplit("/", 1)[-1])
                    mode, total = "ab", offset
                else:
                    length = response.headers.get("Content-Length")
                    expected_size = int(length) if length is not None else None
                    mode, total = "wb", 0
                with temporary.open(mode) as output:
                    for chunk in iter(lambda: response.read(1024 * 1024), b""):
                        output.write(chunk)
                        total += len(chunk)
                        if total % (16 * 1024 * 1024) == 0:
                            print(f"    {total // (1024 * 1024)} MiB downloaded", flush=True)
            if expected_size is not None and total != expected_size:
                raise OSError(f"Incomplete download: received {total}/{expected_size} bytes")
            if checksum is not None and digest(temporary) != checksum:
                temporary.unlink(missing_ok=True)
                raise ValueError(f"SHA-256 verification failed for {destination.name}")
            os.replace(temporary, destination)
            break
        except (OSError, ValueError, http.client.HTTPException) as exc:
            progress = temporary.stat().st_size if temporary.is_file() else 0
            stalled = 0 if progress > offset else stalled + 1
            if stalled >= 5 or attempt == 63:
                raise ValueError(f"Could not download {destination.name}: {exc}") from exc
            print(f"    {exc}; resuming download (attempt {attempt + 2})...", flush=True)
            if isinstance(exc, urllib.error.HTTPError) and exc.code in (429, 503):
                time.sleep(min(2 ** attempt, 8))
    return destination


def github_asset(repo, tag, select):
    manifest = download(f"https://api.github.com/repos/{repo}/releases/tags/{tag}",
                        name=repo.replace("/", "-") + "-" + tag + ".json")
    assets = [asset for asset in json.loads(manifest.read_text())["assets"] if select(asset["name"])]
    if not assets:
        raise ValueError(f"No supported toolchain archive in {repo} release {tag}")
    asset = assets[0]
    checksum = asset.get("digest") or ""
    if not checksum.startswith("sha256:"):
        raise ValueError(f"Missing SHA-256 digest for {asset['name']}")
    return download(asset["browser_download_url"], checksum[len("sha256:"):])


def extract_tar(archive, directory, sysroot=False):
    directory = directory.resolve()
    for member in archive:
        target = (directory / member.name).resolve()
        if directory != target and directory not in target.parents:
            raise ValueError(f"Unsafe archive path: {member.name}")
        if member.issym() or member.islnk():
            # Debian absolute links refer to the target sysroot, never the host.
            if sysroot and member.linkname.startswith("/"):
                mapped = directory / member.linkname.lstrip("/")
                member.linkname = (os.path.relpath(mapped, target.parent) if member.issym()
                                   else str(mapped.relative_to(directory)))
            base = target.parent if member.issym() else directory
            link = (base / member.linkname).resolve()
            if directory != link and directory not in link.parents:
                raise ValueError(f"Unsafe archive link: {member.name}")
        elif not (member.isfile() or member.isdir()):
            raise ValueError(f"Unsupported archive entry: {member.name}")
        archive.extract(member, directory)


def install_archive(archive, destination):
    if (destination / ".ready").is_file():
        return
    destination.parent.mkdir(parents=True, exist_ok=True)
    print(f"  Extracting {archive.name}...", flush=True)
    with tempfile.TemporaryDirectory(prefix=".extract-", dir=destination.parent) as temporary:
        unpacked = Path(temporary) / "unpacked"
        unpacked.mkdir()
        if archive.suffix == ".zip":
            with zipfile.ZipFile(archive) as source:
                for member in source.infolist():
                    target = (unpacked / member.filename).resolve()
                    if unpacked.resolve() != target and unpacked.resolve() not in target.parents:
                        raise ValueError(f"Unsafe archive path: {member.filename}")
                source.extractall(unpacked)
        else:
            with tarfile.open(archive) as source:
                extract_tar(source, unpacked)
        children = list(unpacked.iterdir())
        content = children[0] if len(children) == 1 and children[0].is_dir() else unpacked
        (content / ".ready").write_text(archive.name + "\n")
        if destination.exists():
            shutil.rmtree(destination)
        content.rename(destination)


def llvm_mingw(host, dry_run):
    system, arch = host.split("/")
    directory = CACHE / f"llvm-mingw-{LLVM_VERSION}-{system}-{arch}"
    if not dry_run and not (directory / ".ready").is_file():
        if system == "darwin":
            name = f"llvm-mingw-{LLVM_VERSION}-ucrt-macos-universal.tar.xz"
            archive = download(f"https://github.com/mstorsjo/llvm-mingw/releases/download/{LLVM_VERSION}/{name}",
                               "2cab02a2e964bd4aae981150a45985d07c657cfa8d244959eb9e2dcc5eedd7b1")
        else:
            cpu = "x86_64" if arch == "amd64" else "aarch64"
            ending = f"-ucrt-{cpu}.zip" if system == "windows" else f"-{cpu}.tar.xz"
            archive = github_asset("mstorsjo/llvm-mingw", LLVM_VERSION,
                                   lambda name: name.endswith(ending) and
                                   (system == "windows" or "-ucrt-ubuntu-" in name))
        install_archive(archive, directory)
    return directory


def debian_packages(arch, names):
    index = download(f"https://deb.debian.org/debian/dists/bookworm/main/binary-{arch}/Packages.xz",
                     name=f"debian-bookworm-{arch}-Packages.xz")
    wanted, found, fields = set(names), {}, {}
    with lzma.open(index, "rt", encoding="utf-8") as source:
        for line in source:
            if line == "\n":
                if fields.get("Package") in wanted:
                    found[fields["Package"]] = fields
                fields = {}
                if wanted == found.keys():
                    break
            elif not line.startswith((" ", "\t")) and ": " in line:
                key, value = line.rstrip("\n").split(": ", 1)
                fields[key] = value
    if wanted != found.keys():
        raise ValueError("Missing Debian packages: " + ", ".join(sorted(wanted - found.keys())))
    return found


def extract_deb(package, directory):
    # Read the ar container directly; no dpkg installation or package scripts run.
    with package.open("rb") as source:
        if source.read(8) != b"!<arch>\n":
            raise ValueError(f"Invalid Debian archive: {package.name}")
        while True:
            header = source.read(60)
            if len(header) != 60:
                break
            name = header[:16].decode("ascii").strip().rstrip("/")
            size = int(header[48:58])
            if name.startswith("data.tar"):
                with tarfile.open(fileobj=io.BytesIO(source.read(size))) as archive:
                    extract_tar(archive, directory, sysroot=True)
                return
            source.seek(size + size % 2, 1)
    raise ValueError(f"No data archive in {package.name}")


def linux_sdk(arch, viewer, dry_run):
    directory = CACHE / f"linux-{arch}"
    if dry_run:
        return directory
    names = ["libc6-dev", "libc6", "libgcc-12-dev", "libgcc-s1", "linux-libc-dev", "libasound2-dev", "libasound2", "libv4l-dev", "libv4l-0", "libv4lconvert0"]
    if viewer:
        names += ["libsdl2-dev", "libsdl2-2.0-0", "libx11-dev", "x11proto-dev"]
    ready = directory / ".packages.json"
    installed = json.loads(ready.read_text()) if ready.is_file() else {}
    missing = [name for name in names if name not in installed]
    if missing:
        packages = debian_packages(arch, missing)
        directory.mkdir(parents=True, exist_ok=True)
        for name in missing:
            package = packages[name]
            archive = download("https://deb.debian.org/debian/" + package["Filename"], package["SHA256"])
            print(f"  Preparing {name} ({arch})...", flush=True)
            extract_deb(archive, directory)
            installed[name] = package["SHA256"]
        ready.write_text(json.dumps(installed, indent=2) + "\n")
    return directory


def sdl_source():
    source = CACHE / f"SDL2-{SDL_VERSION}-source"
    if not (source / ".ready").is_file():
        archive = github_asset("libsdl-org/SDL", "release-" + SDL_VERSION,
                               lambda name: name == f"SDL2-{SDL_VERSION}.tar.gz")
        install_archive(archive, source)
    return source


def windows_sdl(arch, host, dry_run):
    if arch == "arm64":
        directory = CACHE / f"SDL2-{SDL_VERSION}-windows-arm64"
        if not dry_run and not (directory / ".ready").is_file():
            if not shutil.which("cmake"):
                raise ValueError("Preparing Windows ARM64 SDL2 requires CMake")
            source = sdl_source()
            compiler = llvm_mingw(host, dry_run)
            suffix = ".exe" if host.startswith("windows/") else ""
            generator = ["-G", "Ninja"] if shutil.which("ninja") else []
            if host.startswith("windows/") and not generator:
                raise ValueError("Preparing Windows ARM64 SDL2 on Windows requires Ninja and CMake")
            # Reuse CMake checks and object files after an interrupted build.
            build_dir = CACHE / f"SDL2-{SDL_VERSION}-windows-arm64-build-{host.replace('/', '-')}"
            build_dir.mkdir(parents=True, exist_ok=True)
            if (build_dir / "CMakeCache.txt").is_file():
                generator = []
            subprocess.run(["cmake", *generator, "-S", str(source), "-B", str(build_dir),
                            "-DCMAKE_SYSTEM_NAME=Windows", "-DCMAKE_SYSTEM_PROCESSOR=ARM64",
                            "-DCMAKE_C_COMPILER=" + str(compiler / "bin" / ("aarch64-w64-mingw32-clang" + suffix)),
                            "-DCMAKE_RC_COMPILER=" + str(compiler / "bin" / ("aarch64-w64-mingw32-windres" + suffix)),
                            "-DCMAKE_BUILD_TYPE=Release", "-DCMAKE_INSTALL_PREFIX=" + str(directory),
                            "-DSDL_SHARED=ON", "-DSDL_STATIC=OFF", "-DSDL_TEST=OFF"], check=True)
            subprocess.run(["cmake", "--build", str(build_dir), "--parallel", "2"], check=True)
            subprocess.run(["cmake", "--install", str(build_dir)], check=True)
            (directory / ".ready").write_text(SDL_VERSION + "\n")
        return directory / "include/SDL2", directory / "lib", directory / "bin/SDL2.dll"
    directory = CACHE / f"SDL2-{SDL_VERSION}-windows"
    if not dry_run and not (directory / ".ready").is_file():
        name = f"SDL2-devel-{SDL_VERSION}-VC.zip"
        archive = download(f"https://github.com/libsdl-org/SDL/releases/download/release-{SDL_VERSION}/{name}",
                           "af347939395a58b365846aaea27391e69f9ec9d4dd650d6ac40802159b418a6e")
        install_archive(archive, directory)
    if not dry_run:
        # MinGW's -lSDL2 can use the official COFF import library from the VC SDK.
        source = directory / "lib/x64/SDL2.lib"
        if not source.is_file():
            raise ValueError("SDL2 SDK is missing the x64 import library")
        shutil.copy2(source, source.with_name("libSDL2.dll.a"))
    return directory / "include", directory / "lib/x64", directory / "lib/x64/SDL2.dll"


def macos_sdl(dry_run):
    directory = CACHE / f"SDL2-{SDL_VERSION}-darwin"
    if not dry_run and not (directory / ".ready").is_file():
        archive = github_asset("libsdl-org/SDL", "release-" + SDL_VERSION,
                               lambda name: name == f"SDL2-{SDL_VERSION}.dmg")
        with tempfile.TemporaryDirectory(prefix=".sdl-sdk-", dir=CACHE) as temporary:
            mount = Path(temporary) / "mount"
            subprocess.run(["hdiutil", "attach", "-readonly", "-nobrowse", "-mountpoint", str(mount),
                            str(archive)], check=True, stdout=subprocess.DEVNULL)
            try:
                (directory / "include").mkdir(parents=True, exist_ok=True)
                (directory / "lib").mkdir(exist_ok=True)
                shutil.copytree(mount / "SDL2.framework/Headers", directory / "include/SDL2", dirs_exist_ok=True)
                library = directory / "lib/libSDL2.dylib"
                shutil.copy2(mount / "SDL2.framework/Versions/A/SDL2", library)
                shutil.copy2(mount / "License.txt", directory / "LICENSE.txt")
                subprocess.run(["install_name_tool", "-id", "@rpath/libSDL2.dylib", str(library)], check=True,
                               stderr=subprocess.PIPE)
                # Updating the install name invalidates the original ARM64 code signature.
                subprocess.run(["codesign", "--force", "--sign", "-", str(library)], check=True,
                               stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
            finally:
                subprocess.run(["hdiutil", "detach", str(mount)], check=True, stdout=subprocess.DEVNULL)
        (directory / ".ready").write_text(SDL_VERSION + "\n")
    return directory


def append_flags(env, name, args):
    # Setting CGO_CFLAGS replaces Go's defaults; keep release code optimized.
    default = "-O2 -g" if name in ("CGO_CFLAGS", "CGO_CXXFLAGS") else ""
    env[name] = " ".join(filter(None, (env.get(name, default), command_string(args))))


def pkg_config(host, dry_run):
    # Go's PKG_CONFIG setting accepts an executable, not an interpreter command.
    suffix = ".exe" if host.startswith("windows/") else ""
    executable = CACHE / "bin" / ("target-pkg-config" + suffix)
    source = ROOT / "tools" / "target_pkg_config.go"
    stamp = executable.with_suffix(".sha256")
    checksum = digest(source)
    if not dry_run and (not executable.is_file() or not stamp.is_file() or stamp.read_text() != checksum):
        executable.parent.mkdir(parents=True, exist_ok=True)
        system, arch = host.split("/")
        env = os.environ.copy()
        env.update(GOOS=system, GOARCH=arch, CGO_ENABLED="0")
        subprocess.run(["go", "build", "-trimpath", "-o", str(executable), str(source)],
                       cwd=ROOT, env=env, check=True)
        stamp.write_text(checksum)
    return command_string([executable])


def prepare(target, host, env, viewer, dry_run=False):
    system, arch = target.split("/")
    prefix = "ANYKVM_BUILD_" + target.replace("/", "_").upper() + "_"
    # Explicit toolchain/SDK choices remain authoritative.
    if any(prefix + name in os.environ for name in
           ("CC", "CGO_CFLAGS", "CGO_LDFLAGS", "PKG_CONFIG", "PKG_CONFIG_LIBDIR", "SDKROOT")):
        return []
    if system == "linux" and target != host:
        print(f"[{target}] Preparing LLVM and Debian target libraries (cached locally).", flush=True)
        compiler = llvm_mingw(host, dry_run)
        sdk = linux_sdk(arch, viewer, dry_run)
        cpu = "x86_64" if arch == "amd64" else "aarch64"
        triple = cpu + "-linux-gnu"
        suffix = ".exe" if host.startswith("windows/") else ""
        env["CC"] = command_string([
            compiler / "bin" / ("clang" + suffix), "-Qunused-arguments", "--target=" + triple,
            "--sysroot=" + str(sdk), "--gcc-install-dir=" + str(sdk / "usr/lib/gcc" / triple / "12"),
            "-rtlib=libgcc", "-fuse-ld=" + str(compiler / "bin" / ("ld.lld" + suffix))])
        append_flags(env, "CGO_CFLAGS", ["-I" + str(sdk / "usr/include" / triple)])
        # Go's default executable mode uses absolute relocations, while Clang defaults to PIE.
        append_flags(env, "CGO_LDFLAGS", ["-no-pie", "-L" + str(sdk / "usr/lib" / triple),
                                        "-Wl,--allow-shlib-undefined"])
        if viewer:
            env["PKG_CONFIG"] = pkg_config(host, dry_run)
            env["ANYKVM_BUILD_SDL_PREFIX"] = str(sdk / "usr")
            env["ANYKVM_BUILD_SDL_TRIPLE"] = triple
    elif system == "windows":
        print(f"[{target}] Preparing LLVM-MinGW (cached locally).", flush=True)
        compiler = llvm_mingw(host, dry_run)
        cpu = "x86_64" if arch == "amd64" else "aarch64"
        suffix = ".exe" if host.startswith("windows/") else ""
        env["CC"] = command_string([compiler / "bin" / f"{cpu}-w64-mingw32-clang{suffix}"])
        if viewer:
            include, lib, dll = windows_sdl(arch, host, dry_run)
            append_flags(env, "CGO_CFLAGS", ["-I" + str(include)])
            append_flags(env, "CGO_LDFLAGS", ["-L" + str(lib)])
            license_dir = CACHE / f"SDL2-{SDL_VERSION}-{'source' if arch == 'arm64' else 'windows'}"
            return [(dll, Path("SDL2.dll")), (license_dir / "LICENSE.txt", Path("SDL2-LICENSE.txt"))]
    elif system == "darwin" and host.startswith("darwin/") and viewer:
        print(f"[{target}] Preparing universal SDL2 (cached locally).", flush=True)
        sdk = macos_sdl(dry_run)
        env["PKG_CONFIG"] = pkg_config(host, dry_run)
        env["ANYKVM_BUILD_SDL_PREFIX"] = str(sdk)
        env["ANYKVM_BUILD_SDL_TRIPLE"] = ""
        append_flags(env, "CGO_LDFLAGS", ["-Wl,-rpath,@executable_path/lib"])
        if not dry_run:
            files = [(path, Path("lib") / path.name) for path in (sdk / "lib").glob("libSDL2*.dylib")]
            files.append((sdk / "LICENSE.txt", Path("SDL2-LICENSE.txt")))
            return files
    return []
