#!/usr/bin/env python3
"""Archive a desktop build with its runtime files, documentation and licenses."""

import argparse
import hashlib
from pathlib import Path
import shutil
import tempfile


ROOT = Path(__file__).resolve().parent.parent
TARGETS = tuple(f"{system}/{arch}" for system in ("linux", "windows", "darwin")
                for arch in ("amd64", "arm64"))


def package(target, source, output, root=ROOT):
    if target not in TARGETS:
        raise ValueError(f"Unsupported target: {target}")
    executable = "anykvm.exe" if target.startswith("windows/") else "anykvm"
    if not (source / executable).is_file():
        raise ValueError(f"Missing executable: {source / executable}")
    name = "anykvm-" + target.replace("/", "-")
    output.mkdir(parents=True, exist_ok=True)
    # Stage separately so packaging never changes the original build directory.
    with tempfile.TemporaryDirectory(prefix="anykvm-release-") as temporary:
        staging = Path(temporary) / name
        shutil.copytree(source, staging, symlinks=True)
        shutil.copy2(root / "LICENSE", staging / "LICENSE")
        shutil.copy2(root / "README.md", staging / "README.md")
        shutil.copytree(root / "docs", staging / "docs", ignore=shutil.ignore_patterns("plans"))
        if not target.startswith("windows/"):
            (staging / executable).chmod(0o755)
        archive_format = "zip" if target.startswith("windows/") else "gztar"
        archive = Path(shutil.make_archive(str(output.resolve() / name), archive_format,
                                           root_dir=temporary, base_dir=name))
    checksum = hashlib.sha256()
    with archive.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            checksum.update(chunk)
    archive.with_name(archive.name + ".sha256").write_text(
        f"{checksum.hexdigest()}  {archive.name}\n", encoding="utf-8")
    return archive


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--target", choices=TARGETS, required=True)
    parser.add_argument("--source", type=Path, help="built target directory")
    parser.add_argument("--output", type=Path, default=ROOT / "build/dist")
    args = parser.parse_args()
    source = args.source or ROOT / "build/releases" / args.target.replace("/", "-")
    try:
        archive = package(args.target, source, args.output)
    except (OSError, ValueError, shutil.Error) as exc:
        parser.exit(1, f"Release packaging failed: {exc}\n")
    print(f"Packaged {archive}")


if __name__ == "__main__":
    main()
