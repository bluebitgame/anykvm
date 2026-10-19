#!/usr/bin/env python3
"""Stage prebuilt Opus DLLs for Windows builds without FFmpeg; no C build is needed."""

import argparse
import hashlib
from pathlib import Path
import shutil
import sys
import zipfile

import cross_toolchain


VERSION = "1.6.1.4"
PACKAGE = f"opussharp.natives.{VERSION}.nupkg"
URL = f"https://api.nuget.org/v3-flatcontainer/opussharp.natives/{VERSION}/{PACKAGE}"
CHECKSUM = "6fc12e30910dfe87a0df0c62c7f1d76a869301c8692ec8d5331841c5f2d2eeaa"
LICENSE_CHECKSUM = "01e1167d54a096d123cf6dfbbeb19587278845c6481d2d66d545669846079551"
DLLS = {
    "amd64": ("win-x64", "9d251c8f00377cc0a5f3532be29874b09d7daa07cf0a1850b212e403f5d768f1"),
    "arm64": ("win-arm64", "c85a7769804d1ab273c9688294088f10fe2a1d93f0fbd0cfeb25da220802e454"),
}


def verified(path, checksum):
    return path.is_file() and cross_toolchain.digest(path) == checksum


def prepare(arch, allow_download=True):
    rid, checksum = DLLS[arch]
    directory = cross_toolchain.CACHE / f"opus-{VERSION}-{arch}"
    dll, license_file = directory / "opus.dll", directory / "opus-LICENSE.txt"
    if verified(dll, checksum) and verified(license_file, LICENSE_CHECKSUM):
        return dll, license_file
    package = cross_toolchain.CACHE / "downloads" / PACKAGE
    if not verified(package, CHECKSUM):
        if not allow_download:
            raise ValueError("prebuilt Opus is not cached (--no-bootstrap prevents downloading)")
        package = cross_toolchain.download(URL, CHECKSUM, PACKAGE)
    with zipfile.ZipFile(package) as archive:
        # Read only the two known members; never extract arbitrary archive paths.
        files = ((dll, archive.read(f"runtimes/{rid}/native/opus.dll"), checksum),
                 (license_file, archive.read("LICENSE.txt"), LICENSE_CHECKSUM))
    for path, data, expected in files:
        if hashlib.sha256(data).hexdigest() != expected:
            raise ValueError(f"prebuilt Opus checksum mismatch: {path.name}")
    directory.mkdir(parents=True, exist_ok=True)
    for path, data, _ in files:
        path.write_bytes(data)
    return dll, license_file


def runtime_files(target, tags="", dry_run=False, allow_download=True):
    if not target.startswith("windows/") or "ffmpeg" in tags.replace(",", " ").split():
        return []
    if dry_run:
        print(f"  Optional Opus DLL: {target}, OpusSharp.Natives {VERSION}, staged under lib/")
        return []
    try:
        dll, license_file = prepare(target.split("/")[1], allow_download)
    except (OSError, ValueError, zipfile.BadZipFile, KeyError) as exc:
        print(f"[{target}] WARNING: Opus DLL was not packaged: {exc}. "
              "Audio will be disabled unless a matching opus.dll is supplied.", file=sys.stderr, flush=True)
        return []
    return [(dll, Path("lib/opus.dll")), (license_file, Path("lib/opus-LICENSE.txt"))]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--arch", choices=DLLS, required=True)
    parser.add_argument("--output", type=Path, required=True, help="destination library directory")
    args = parser.parse_args()
    try:
        files = prepare(args.arch)
        args.output.mkdir(parents=True, exist_ok=True)
        for source in files:
            shutil.copy2(source, args.output / source.name)
    except (OSError, ValueError, zipfile.BadZipFile, KeyError) as exc:
        print(f"Opus packaging failed: {exc}", file=sys.stderr)
        return 1
    print(f"Staged Opus DLL and license under {args.output}.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
