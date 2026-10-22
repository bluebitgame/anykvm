#!/usr/bin/env python3
"""Package board firmware with checksums, source revision and flashing guides."""

import argparse
import hashlib
import json
from pathlib import Path
import shutil
import tempfile
import zipfile

from build_firmware import ROOT, TARGETS


def package(target, source, output, revision, root=ROOT):
    family, _, board, _ = TARGETS[target]
    manifest = json.loads((source / "artifacts.json").read_text())
    if not isinstance(manifest, dict) or not manifest:
        raise ValueError("Missing firmware artifacts")
    required = {"pico": ".uf2", "nrf": ".hex", "esp32": ".bin"}[family]
    files = []
    for name, checksum in manifest.items():
        if not isinstance(name, str) or Path(name).name != name or "/" in name or "\\" in name or name in (".", ".."):
            raise ValueError("Invalid firmware artifact name")
        path = source / name
        if path.is_symlink() or not path.is_file() or path.stat().st_size == 0:
            raise ValueError(f"Missing or empty firmware artifact: {name}")
        if hashlib.sha256(path.read_bytes()).hexdigest() != checksum:
            raise ValueError(f"Firmware checksum mismatch: {name}")
        files.append(path)
    if not any(path.suffix == required for path in files):
        raise ValueError(f"Missing {required} image for {target}")
    name = "firmware-" + target
    output.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="firmware-") as temporary:
        staging = Path(temporary) / name
        staging.mkdir()
        for path in files:
            shutil.copy2(path, staging / path.name)
        shutil.copy2(source / "artifacts.json", staging / "artifacts.json")
        shutil.copy2(root / "LICENSE", staging / "LICENSE")
        # Preserve relative links between board guides and shared wiring notes.
        for guide in (root / "firmware").rglob("*.md"):
            destination = staging / "firmware" / guide.relative_to(root / "firmware")
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(guide, destination)
        metadata = {"target": target, "board": board, "source_revision": revision,
                    "protocol_version": 1}
        (staging / "release.json").write_text(json.dumps(metadata, indent=2) + "\n", encoding="utf-8")
        (staging / "README.md").write_text(
            f"# AnyKVM firmware: {target}\n\nBoard: `{board}`\n\nSource: `{revision}`\n\n"
            "Use only with the matching board. See `firmware/README.md` and the board guide "
            "for wiring and flashing instructions. Firmware uses AnyKVM serial HID protocol v1.\n"
            "`artifacts.json` contains image SHA-256 checksums.\n", encoding="utf-8")
        archive = output / (name + ".zip")
        with zipfile.ZipFile(archive, "w", zipfile.ZIP_DEFLATED) as bundle:
            for path in sorted(staging.rglob("*")):
                if path.is_file():
                    bundle.write(path, path.relative_to(staging.parent).as_posix())
    checksum = hashlib.sha256(archive.read_bytes()).hexdigest()
    archive.with_suffix(".zip.sha256").write_text(f"{checksum}  {archive.name}\n", encoding="utf-8")
    return archive


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--target", choices=TARGETS, required=True)
    parser.add_argument("--source", type=Path)
    parser.add_argument("--output", type=Path, default=ROOT / "build/dist")
    parser.add_argument("--revision", required=True)
    args = parser.parse_args()
    source = args.source or ROOT / "build/firmware/releases" / args.target
    print(package(args.target, source, args.output, args.revision))


if __name__ == "__main__":
    main()
