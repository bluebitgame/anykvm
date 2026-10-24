#!/usr/bin/env python3
"""Package board firmware with checksums, source revision and licenses."""

import argparse
import hashlib
import json
from pathlib import Path
import shutil
import tempfile
import zipfile

from build_firmware import ROOT, TARGETS


GROUPS = {
    "pico1": ("pico", "pico-pio-usb", "pico-ps2"),
    "pico2": ("pico2", "pico2-pio-usb", "pico2-ps2"),
    "nrf52840": ("nrf52840-xiao", "nrf52840-feather", "nrf52840-dongle"),
    **{target: (target,) for target in TARGETS if not target.startswith(("pico", "nrf52840"))},
}


def stage_target(target, source, staging, revision):
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
    staging.mkdir(parents=True, exist_ok=True)
    for path in files:
        shutil.copy2(path, staging / path.name)
    shutil.copy2(source / "artifacts.json", staging / "artifacts.json")
    metadata = {"target": target, "board": board, "source_revision": revision,
                "protocol_version": 1}
    (staging / "release.json").write_text(json.dumps(metadata, indent=2) + "\n", encoding="utf-8")


def write_archive(staging, output):
    output.mkdir(parents=True, exist_ok=True)
    archive = output / (staging.name + ".zip")
    with zipfile.ZipFile(archive, "w", zipfile.ZIP_DEFLATED) as bundle:
        for path in sorted(staging.rglob("*")):
            if path.is_file():
                bundle.write(path, path.relative_to(staging.parent).as_posix())
    checksum = hashlib.sha256(archive.read_bytes()).hexdigest()
    archive.with_suffix(".zip.sha256").write_text(f"{checksum}  {archive.name}\n", encoding="utf-8")
    return archive


def package(target, source, output, revision, root=ROOT):
    with tempfile.TemporaryDirectory(prefix="firmware-") as temporary:
        staging = Path(temporary) / ("firmware-" + target)
        stage_target(target, source, staging, revision)
        shutil.copy2(root / "LICENSE", staging / "LICENSE")
        return write_archive(staging, output)


def package_group(group, source, output, revision, root=ROOT):
    targets = GROUPS[group]
    if len(targets) == 1:
        return package(targets[0], source / targets[0], output, revision, root)
    with tempfile.TemporaryDirectory(prefix="firmware-") as temporary:
        staging = Path(temporary) / ("firmware-" + group)
        for target in targets:
            # Variants can have identical image names; keep each in its own directory.
            stage_target(target, source / target, staging / target, revision)
        shutil.copy2(root / "LICENSE", staging / "LICENSE")
        return write_archive(staging, output)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    selection = parser.add_mutually_exclusive_group(required=True)
    selection.add_argument("--target", choices=TARGETS)
    selection.add_argument("--group", choices=GROUPS)
    parser.add_argument("--source", type=Path)
    parser.add_argument("--output", type=Path, default=ROOT / "build/dist")
    parser.add_argument("--revision", required=True)
    args = parser.parse_args()
    if args.group:
        source = args.source or ROOT / "build/firmware/releases"
        print(package_group(args.group, source, args.output, args.revision))
    else:
        source = args.source or ROOT / "build/firmware/releases" / args.target
        print(package(args.target, source, args.output, args.revision))


if __name__ == "__main__":
    main()
