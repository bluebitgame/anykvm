#!/usr/bin/env python3
"""Restore pinned third-party build inputs without checking them into Git."""

import argparse
import concurrent.futures
import hashlib
import json
from pathlib import Path
import os
import re
import shutil
import tarfile
import tempfile

import cross_toolchain

ROOT = Path(__file__).resolve().parent.parent
LOCK = Path(__file__).with_name("sources.lock.json")


def digest(data):
    return hashlib.sha256(data).hexdigest()


def matches(path, checksum):
    return path.is_file() and cross_toolchain.digest(path) == checksum


def target_path(directory, relative):
    path = Path(relative)
    if path.is_absolute() or ".." in path.parts:
        raise ValueError(f"Invalid dependency path: {relative}")
    return directory / path


def fetch(url, checksum, name, allow_download, mirrors=()):
    cached = cross_toolchain.CACHE / "downloads" / name
    if not allow_download:
        if not matches(cached, checksum):
            raise ValueError(f"Dependency is not cached: {name}; run without --no-bootstrap first")
        return cached
    failure = None
    for candidate in (url, *mirrors):
        try:
            return cross_toolchain.download(candidate, checksum, name)
        except ValueError as exc:
            failure = exc
    raise failure


def patch(data, relative):
    changes = json.loads((Path(__file__).parent / relative).read_text())
    if digest(data) != changes["source_sha256"]:
        raise ValueError(f"Patch input does not match: {relative}")
    lines = data.decode("utf8").splitlines(True)
    for edit in reversed(changes["edits"]):
        first = edit["line"]
        last = first + len(edit["before"])
        if lines[first:last] != edit["before"]:
            raise ValueError(f"Patch context does not match: {relative}")
        lines[first:last] = edit["after"]
    result = "".join(lines).encode("utf8")
    if digest(result) != changes["result_sha256"]:
        raise ValueError(f"Patch output does not match: {relative}")
    return result


def install(path, data, entry):
    expected = entry.get("source_sha256", entry["sha256"])
    if len(data) != entry["size"] or digest(data) != expected:
        raise ValueError(f"Dependency input failed verification: {path.name}")
    if entry.get("patch"):
        data = patch(data, entry["patch"])
    if entry.get("extract") == "c-license":
        if not data.startswith(b"/*") or b"*/" not in data:
            raise ValueError(f"Missing C license header: {path.name}")
        header = data[2:data.index(b"*/")].decode("utf8")
        data = ("\n".join(re.sub(r"^\s*\* ?", "", line) for line in header.splitlines()).strip() + "\n").encode("utf8")
    elif entry.get("extract"):
        raise ValueError(f"Unknown dependency extraction: {entry['extract']}")
    if digest(data) != entry["sha256"]:
        raise ValueError(f"Dependency output failed verification: {path.name}")
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary = tempfile.mkstemp(prefix=".prepare-", dir=path.parent)
    try:
        with os.fdopen(descriptor, "wb") as output:
            output.write(data)
        os.chmod(temporary, 0o644)
        os.replace(temporary, path)
    finally:
        Path(temporary).unlink(missing_ok=True)


def prepare_group(group, allow_download=True, dry_run=False):
    directory = target_path(ROOT, group["destination"])
    missing = {name: entry for name, entry in group["files"].items()
               if not matches(target_path(directory, name), entry["sha256"])}
    generated = {name: value.encode("utf8") for name, value in group.get("generated", {}).items()
                 if not matches(target_path(directory, name), digest(value.encode("utf8")))}
    if not missing and not generated:
        return
    print(f"  {'Would prepare' if dry_run else 'Preparing'} {group['name']} sources...", flush=True)
    if dry_run:
        return
    if missing and group.get("url"):
        archive = fetch(group["url"], group["sha256"],
                        f"source-{group['name']}-{group['sha256'][:16]}.tar", allow_download)
        members = {}
        for name, entry in missing.items():
            members.setdefault(entry["member"], []).append((name, entry))
        # Read selected regular files only; never extract paths or archive links.
        with tarfile.open(archive, "r|*") as source:
            for member in source:
                if member.name not in members:
                    continue
                outputs = members.pop(member.name)
                size = outputs[0][1]["size"]
                if not member.isfile() or member.size != size:
                    raise ValueError(f"Invalid archive entry: {member.name}")
                data = source.extractfile(member).read(size + 1)
                for name, entry in outputs:
                    install(target_path(directory, name), data, entry)
        if members:
            raise ValueError(f"Dependency archive is incomplete: {group['name']}")
    elif missing:
        def restore(item):
            name, entry = item
            checksum = entry.get("source_sha256", entry["sha256"])
            cached = fetch(entry["url"], checksum,
                           f"source-{group['name']}-{checksum[:16]}-{Path(name).name}", allow_download,
                           entry.get("mirrors", ()))
            install(target_path(directory, name), cached.read_bytes(), entry)

        with concurrent.futures.ThreadPoolExecutor(max_workers=3) as pool:
            list(pool.map(restore, missing.items()))
    for name, data in generated.items():
        install(target_path(directory, name), data, {"size": len(data), "sha256": digest(data)})


def selected_groups(platforms=None, ffmpeg=False):
    for group in json.loads(LOCK.read_text())["groups"]:
        if platforms is not None:
            if group.get("platforms") and not set(group["platforms"]).intersection(platforms):
                continue
            if group.get("feature") == "ffmpeg" and not ffmpeg:
                continue
        yield group


def prepare(platforms=None, ffmpeg=False, allow_download=True, dry_run=False):
    for group in selected_groups(platforms, ffmpeg):
        prepare_group(group, allow_download=allow_download, dry_run=dry_run)


def notice_files(platforms=None, ffmpeg=False):
    """Return prepared notices for release packaging, preserving upstream text."""
    result = [(ROOT / "LICENSE", Path("LICENSE"))]
    for group in selected_groups(platforms, ffmpeg):
        for name, entry in group["files"].items():
            if not re.search(r"(^|[-_.])(LICENSE|LICENCE|COPYING|NOTICE)([-_.]|$)", Path(name).name.upper()):
                continue
            source = target_path(ROOT / group["destination"], name)
            if not matches(source, entry["sha256"]):
                raise ValueError(f"License notice is missing or changed: {source}")
            result.append((source, Path("licenses") / group["name"] / name))
    return result


def stage_notices(directory, platforms=None, ffmpeg=False):
    for source, relative in notice_files(platforms, ffmpeg):
        destination = directory / relative
        destination.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(source, destination)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--platforms", nargs="+", choices=("linux", "windows", "darwin"),
                        help="prepare selected platforms; default restores all sources")
    parser.add_argument("--ffmpeg", action="store_true", help="include FFmpeg headers with --platforms")
    parser.add_argument("--offline", action="store_true", help="restore from verified local cache only")
    parser.add_argument("--dry-run", action="store_true", help="show missing inputs without changing files")
    parser.add_argument("--stage-notices", type=Path, help="copy prepared license notices into a release directory")
    args = parser.parse_args()
    try:
        prepare(args.platforms, args.ffmpeg, not args.offline, args.dry_run)
        if args.stage_notices and not args.dry_run:
            stage_notices(args.stage_notices, args.platforms, args.ffmpeg)
    except (OSError, ValueError, tarfile.TarError) as exc:
        parser.exit(1, f"Source preparation failed: {exc}\n")


if __name__ == "__main__":
    main()
