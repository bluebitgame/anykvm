#!/usr/bin/env python3
"""Decide whether firmware changed since the nearest published ancestor release."""

import os
import json
from pathlib import Path
import subprocess


INPUTS = ("firmware", "tools/build_firmware.py", "tools/cross_toolchain.py",
          "tools/package_firmware.py", "tools/firmware_changes.py")


def git(*args):
    return subprocess.run(["git", *args], check=True, capture_output=True, text=True).stdout.strip()


def changed(tags, current_tag=""):
    candidates = []
    for tag in tags:
        if tag == current_tag:
            continue
        ref = "refs/tags/" + tag
        # Published tags can be on another branch or refer to deleted refs.
        resolved = subprocess.run(["git", "rev-parse", "--verify", ref + "^{commit}"],
                                  capture_output=True, text=True)
        if resolved.returncode:
            continue
        commit = resolved.stdout.strip()
        ancestor = subprocess.run(["git", "merge-base", "--is-ancestor", commit, "HEAD"])
        if ancestor.returncode == 1:
            continue
        ancestor.check_returncode()
        candidates.append((int(git("rev-list", "--count", commit + "..HEAD")), tag, commit))
    if not candidates:
        return True, ""
    _, tag, commit = min(candidates)
    return bool(git("diff", "--name-only", commit, "HEAD", "--", *INPUTS)), tag


def published_firmware_tags(releases):
    return [release["tag_name"] for release in releases
            if not release.get("draft", False) and any(
                asset.get("name", "").startswith("firmware-") and asset["name"].endswith(".zip")
                for asset in release.get("assets", []))]


def main():
    repository = os.environ["GITHUB_REPOSITORY"]
    # Desktop-only releases must not hide firmware that has not been published.
    result = subprocess.run(["gh", "api", f"repos/{repository}/releases", "--paginate", "--slurp"],
                            check=True, capture_output=True, text=True)
    current = os.environ.get("GITHUB_REF_NAME", "") if os.environ.get("GITHUB_REF_TYPE") == "tag" else ""
    releases = [release for page in json.loads(result.stdout) for release in page]
    include, baseline = changed(published_firmware_tags(releases), current)
    print(f"Firmware changed: {str(include).lower()}; baseline: {baseline or 'first release'}")
    with Path(os.environ["GITHUB_OUTPUT"]).open("a", encoding="utf-8") as output:
        output.write(f"include={str(include).lower()}\n")


if __name__ == "__main__":
    main()
