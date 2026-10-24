#!/usr/bin/env python3
"""Build all supported HID adapters with SDKs and outputs under build/firmware."""

import argparse
import json
import os
from pathlib import Path
import platform
import re
import shlex
import shutil
import subprocess
import sys
import tempfile

import cross_toolchain


ROOT = Path(__file__).resolve().parent.parent
BUILD = ROOT / "build/firmware"
PICO_VERSION = "2.3.1"
PICO_REVISION = "079c6f39023649b154152db30f1d781e884879bc"
ARM_VERSION = "13.3.rel1"
NCS_VERSION = "v2.5.3"
ARDUINO_VERSION = "1.3.1"
ESP32_VERSION = "3.3.2"
ESP32_INDEX = "https://espressif.github.io/arduino-esp32/package_esp32_index.json"
ESP32_FQBN = "esp32:esp32:XIAO_ESP32S3:USBMode=hwcdc,CDCOnBoot=default,PartitionScheme=default_8MB"
PIO_URL = "https://files.waveshare.com/wiki/RP2350-USB-A/RP2350-USB-A.zip"
PIO_CHECKSUM = "e709329b4ec4735ba1895578edac8fd0fba1f575656df213b55e6997fda1dec5"
NCS_MODULES = ("zephyr", "nrfxlib", "cmsis", "hal_nordic", "mbedtls", "tinycrypt", "zcbor", "segger")
HOST_PACKAGES = ("cmake==3.27.9", "ninja==1.11.1.1", "west==1.2.0", "setuptools<81")
# family, application directory, SDK board, artifact basename
TARGETS = {
    "pico": ("pico", "pico-hid", "pico", "anykvm-gpio-hid"),
    "pico2": ("pico", "pico-hid", "pico2", "anykvm-gpio-hid"),
    "pico-pio-usb": ("pico", "pico-pio-usb", "pico", "anykvm-pio-usb"),
    "pico2-pio-usb": ("pico", "pico-pio-usb", "pico2", "anykvm-pio-usb"),
    "pico-ps2": ("pico", "pico-ps2", "pico", "anykvm-ps2"),
    "pico2-ps2": ("pico", "pico-ps2", "pico2", "anykvm-ps2"),
    "rp2350-usb-a": ("pico", "rp2350-usb-a", "pico2", "anykvm"),
    "nrf52840-xiao": ("nrf", "nrf52840-ble", "xiao_ble", "zephyr"),
    "nrf52840-feather": ("nrf", "nrf52840-ble", "adafruit_feather_nrf52840", "zephyr"),
    "nrf52840-dongle": ("nrf", "nrf52840-ble", "nrf52840dongle_nrf52840", "zephyr"),
    "esp32s3-ble": ("esp32", "esp32s3-ble/anykvm_ble", ESP32_FQBN, "anykvm_ble.ino"),
}


def host_name():
    system = platform.system().lower()
    arch = {"amd64": "x86_64", "arm64": "aarch64"}.get(platform.machine().lower(),
                                                          platform.machine().lower())
    if (system, arch) not in {("darwin", "x86_64"), ("darwin", "aarch64"),
                              ("linux", "x86_64"), ("linux", "aarch64"), ("windows", "x86_64")}:
        raise ValueError(f"Unsupported firmware build host: {system}/{arch}")
    return system, arch


def build_environment(base, directory):
    env = base.copy()
    for name in ("PICO_SDK_PATH", "PICO_TOOLCHAIN_PATH", "PICO_PLATFORM", "PICO_BOARD", "PICO_COMPILER",
                 "PICOTOOL_FORCE_FETCH_FROM_GIT", "PICOTOOL_FETCH_FROM_GIT_PATH",
                 "ZEPHYR_BASE", "ZEPHYR_SDK_INSTALL_DIR",
                 "ZEPHYR_TOOLCHAIN_VARIANT", "GNUARMEMB_TOOLCHAIN_PATH", "PYTHONHOME", "PYTHONPATH",
                 "VIRTUAL_ENV", "CMAKE_PREFIX_PATH", "CMAKE_GENERATOR", "CC", "CXX", "CFLAGS",
                 "CXXFLAGS", "CPPFLAGS", "LDFLAGS", "PIP_TARGET", "PIP_PREFIX", "PIP_USER"):
        env.pop(name, None)
    cache = directory / "cache"
    env.update(PYTHONNOUSERSITE="1", PIP_CONFIG_FILE=os.devnull, PIP_DISABLE_PIP_VERSION_CHECK="1",
               PIP_CACHE_DIR=str(cache / "pip"), XDG_CACHE_HOME=str(cache),
               CCACHE_DIR=str(cache / "ccache"), TMPDIR=str(cache / "tmp"),
               TEMP=str(cache / "tmp"), TMP=str(cache / "tmp"),
               WEST_CONFIG_GLOBAL=str(cache / "west-global"), WEST_CONFIG_SYSTEM=os.devnull,
               ARDUINO_DIRECTORIES_DATA=str(directory / "sdk/arduino"),
               ARDUINO_DIRECTORIES_DOWNLOADS=str(cache / "arduino-downloads"),
               ARDUINO_DIRECTORIES_USER=str(directory / "sdk/arduino-user"),
               ARDUINO_BUILD_CACHE_PATH=str(cache / "arduino-build"))
    return env


class Builder:
    def __init__(self, jobs=4, dry_run=False, offline=False, fresh=False, root=ROOT, directory=BUILD):
        self.root, self.directory = root.resolve(), directory.resolve()
        if self.root / "build" not in self.directory.parents:
            raise ValueError("Firmware SDKs and outputs must be inside the repository's build directory")
        self.jobs, self.dry_run, self.offline, self.fresh = jobs, dry_run, offline, fresh
        self.system, self.arch = host_name()
        self.env = build_environment(os.environ, self.directory)
        self.tools = self.directory / "python" / ("Scripts" if self.system == "windows" else "bin")
        self.python = self.tools / ("python.exe" if self.system == "windows" else "python")
        self.env["PATH"] = str(self.tools) + os.pathsep + self.env.get("PATH", "")
        if not dry_run:
            (self.directory / "cache/tmp").mkdir(parents=True, exist_ok=True)

    def tool(self, name):
        return self.tools / (name + ".exe" if self.system == "windows" else name)

    def run(self, command, cwd=None, capture=False, env=None):
        command = [str(arg) for arg in command]
        join = subprocess.list2cmdline if self.system == "windows" else shlex.join
        print("  " + join(command), flush=True)
        if self.dry_run:
            return ""
        return subprocess.run(command, cwd=cwd or self.root, env=env or self.env,
                              check=True, text=True, stdout=subprocess.PIPE if capture else None).stdout

    def write_json(self, path, data):
        if not self.dry_run:
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(json.dumps(data, indent=2) + "\n", encoding="utf8")

    def fetch(self, url, name, checksum=None):
        cached = cross_toolchain.CACHE / "downloads" / name
        if self.dry_run:
            print(f"  Prepare {url}", flush=True)
            return cached
        if self.offline:
            if not cached.is_file() or (checksum and cross_toolchain.digest(cached) != checksum):
                raise ValueError(f"Missing verified download: {name}; prepare once without --offline")
            return cached
        return cross_toolchain.download(url, checksum, name)

    def checkout(self, url, revision, destination, submodules=()):
        # Keep the preparation marker outside the source tree.
        ready = destination.with_name(destination.name + ".anykvm-ready.json")
        expected = json.dumps([url, revision, list(submodules)])
        if (destination / ".git").exists():
            for marker in (ready, destination / ".anykvm-ready"):
                if marker.is_file() and marker.read_text() == expected:
                    if not self.dry_run and marker != ready:
                        ready.write_text(expected)
                    return destination
        if self.offline and not self.dry_run:
            raise ValueError(f"SDK is not prepared: {destination.name}; run without --offline")
        if not self.dry_run:
            destination.parent.mkdir(parents=True, exist_ok=True)
        if not (destination / ".git").exists():
            self.run(["git", "init", destination])
            self.run(["git", "-C", destination, "remote", "add", "origin", url])
        self.run(["git", "-C", destination, "fetch", "--depth=1", "origin", revision])
        self.run(["git", "-C", destination, "checkout", "--detach", "FETCH_HEAD"])
        if submodules:
            self.run(["git", "-C", destination, "submodule", "update", "--init", "--depth=1", *submodules])
        if not self.dry_run:
            ready.write_text(expected)
        return destination

    def host_tools(self):
        ready = self.directory / "python/.host-packages.json"
        expected = list(HOST_PACKAGES)
        if ready.is_file() and json.loads(ready.read_text()) == expected and self.python.is_file():
            return
        if self.offline and not self.dry_run:
            raise ValueError("Local Python build tools are missing; run without --offline first")
        if not self.python.is_file():
            self.run([sys.executable, "-m", "venv", self.directory / "python"])
        self.run([self.python, "-m", "pip", "install", *HOST_PACKAGES])
        self.write_json(ready, expected)

    def arm_toolchain(self):
        directory = self.directory / f"toolchains/arm-gnu-{ARM_VERSION}-{self.system}-{self.arch}"
        if (directory / ".ready").is_file():
            return directory
        platform_name = {"darwin": "darwin-" + ("arm64" if self.arch == "aarch64" else self.arch),
                         "linux": self.arch, "windows": "mingw-w64-i686"}[self.system]
        name = f"arm-gnu-toolchain-{ARM_VERSION}-{platform_name}-arm-none-eabi"
        name += ".zip" if self.system == "windows" else ".tar.xz"
        url = f"https://developer.arm.com/-/media/Files/downloads/gnu/{ARM_VERSION}/binrel/{name}"
        checksum_file = self.fetch(url + ".sha256asc", name + ".sha256asc")
        if self.dry_run:
            self.fetch(url, name)
        else:
            match = re.search(r"\b[0-9a-fA-F]{64}\b", checksum_file.read_text())
            if not match:
                raise ValueError("Invalid ARM toolchain SHA-256 file")
            archive = self.fetch(url, name, match[0].lower())
            cross_toolchain.install_archive(archive, directory)
        return directory

    def pico_sdk(self):
        return self.checkout("https://github.com/raspberrypi/pico-sdk.git", PICO_REVISION,
                             self.directory / f"sdk/pico-sdk-{PICO_VERSION}", ("lib/tinyusb",))

    def picotool(self, sdk):
        directory = self.directory / f"sdk/picotool-{PICO_VERSION}"
        source = self.checkout("https://github.com/raspberrypi/picotool.git", PICO_VERSION,
                               directory / "picotool-src", (".",))
        package = directory / "picotool"
        executable = package / ("picotool.exe" if self.system == "windows" else "picotool")
        if not executable.is_file() or not (package / "picotoolConfig.cmake").is_file():
            # Prebuild locally so Pico SDK's FetchContent cannot fetch Git during --offline.
            self.run([self.tool("cmake"), "-S", source, "-B", directory / "picotool-build", "-G", "Ninja",
                      f"-DPICO_SDK_PATH={sdk}", f"-DCMAKE_INSTALL_PREFIX={directory}",
                      "-DPICOTOOL_NO_LIBUSB=ON", "-DPICOTOOL_FLAT_INSTALL=ON",
                      "-DCMAKE_BUILD_TYPE=Release", "-DCMAKE_FIND_USE_PACKAGE_REGISTRY=OFF",
                      "-DCMAKE_EXPORT_PACKAGE_REGISTRY=OFF"])
            self.run([self.tool("cmake"), "--build", directory / "picotool-build", "--parallel", self.jobs])
            self.run([self.tool("cmake"), "--install", directory / "picotool-build"])
        return package

    def pio_usb(self):
        directory = self.directory / "sdk/waveshare-pio-usb"
        if not (directory / ".ready").is_file():
            archive = self.fetch(PIO_URL, "waveshare-rp2350-usb-a.zip", PIO_CHECKSUM)
            if not self.dry_run:
                cross_toolchain.install_archive(archive, directory)
        if not self.dry_run:
            matches = [p.parent.parent for p in directory.rglob("src/pio_usb.h")]
            if len(matches) != 1:
                raise ValueError("Waveshare source archive must contain one PIO USB library")
            return matches[0]
        return directory

    def arduino(self):
        directory = self.directory / f"tools/arduino-cli-{ARDUINO_VERSION}"
        executable = directory / ("arduino-cli.exe" if self.system == "windows" else "arduino-cli")
        if not executable.is_file():
            system = {"darwin": "macOS", "linux": "Linux", "windows": "Windows"}[self.system]
            cpu = "ARM64" if self.arch == "aarch64" else "64bit"
            name = f"arduino-cli_{ARDUINO_VERSION}_{system}_{cpu}"
            name += ".zip" if self.system == "windows" else ".tar.gz"
            url = f"https://api.github.com/repos/arduino/arduino-cli/releases/tags/v{ARDUINO_VERSION}"
            manifest = self.fetch(url, f"arduino-cli-{ARDUINO_VERSION}-release.json")
            if self.dry_run:
                print(f"  Install {name} below {directory}")
            else:
                asset = next((a for a in json.loads(manifest.read_text())["assets"] if a["name"] == name), None)
                if not asset or not (asset.get("digest") or "").startswith("sha256:"):
                    raise ValueError("Arduino CLI release is missing a SHA-256 digest")
                archive = self.fetch(asset["browser_download_url"], name, asset["digest"][7:])
                cross_toolchain.install_archive(archive, directory)
        config = self.directory / "arduino-cli.yaml"
        # JSON is also valid YAML; quoted Windows paths need no custom escaping.
        self.write_json(config, {"board_manager": {"additional_urls": [ESP32_INDEX]},
                                "directories": {"data": self.env["ARDUINO_DIRECTORIES_DATA"],
                                                "downloads": self.env["ARDUINO_DIRECTORIES_DOWNLOADS"],
                                                "user": self.env["ARDUINO_DIRECTORIES_USER"]},
                                "build_cache": {"path": self.env["ARDUINO_BUILD_CACHE_PATH"]},
                                "network": {"connection_timeout": "600s"},
                                "updater": {"enable_notification": False}})
        command = [executable, "--config-file", config]
        core = self.directory / f"sdk/arduino/packages/esp32/hardware/esp32/{ESP32_VERSION}/platform.txt"
        if not core.is_file():
            if self.offline and not self.dry_run:
                raise ValueError("ESP32 Arduino Core is not installed; run without --offline")
            self.run([*command, "core", "update-index"])
            self.run([*command, "core", "install", f"esp32:esp32@{ESP32_VERSION}"])
        return command

    def ncs(self):
        directory = self.directory / f"sdk/ncs-{NCS_VERSION}"
        ready = directory / ".anykvm-ready"
        expected = [NCS_VERSION, list(NCS_MODULES)]
        if not ready.is_file() or json.loads(ready.read_text()) != expected:
            if self.offline and not self.dry_run:
                raise ValueError("nRF Connect SDK is not prepared; run without --offline")
            if not (directory / ".west/config").is_file():
                self.checkout("https://github.com/nrfconnect/sdk-nrf.git", NCS_VERSION, directory / "nrf")
                self.run([self.tool("west"), "init", "-l", directory / "nrf"], cwd=directory)
            # Imported Zephyr projects are only known after west resolves its manifest.
            project_filter = "-.*," + ",".join("+" + name for name in NCS_MODULES)
            self.run([self.tool("west"), "config", "--local", "--", "manifest.project-filter", project_filter], cwd=directory)
            self.run([self.tool("west"), "update", "--narrow", "--fetch-opt=--depth=1"], cwd=directory)
            self.write_json(ready, expected)
        requirements = [directory / "zephyr/scripts/requirements-base.txt",
                        directory / "nrf/scripts/requirements-base.txt"]
        packages_ready = self.directory / "python/.ncs-requirements.json"
        packages_expected = [NCS_VERSION, [cross_toolchain.digest(p) for p in requirements]] if not self.dry_run else []
        if self.dry_run or not packages_ready.is_file() or json.loads(packages_ready.read_text()) != packages_expected:
            if self.offline and not self.dry_run:
                raise ValueError("nRF Python requirements are not prepared; run without --offline")
            self.run([self.python, "-m", "pip", "install", "-r", requirements[0], "-r", requirements[1]])
            self.write_json(packages_ready, packages_expected)
        return directory

    def publish(self, target, files, required):
        if self.dry_run:
            print(f"  Publish {required} to {self.directory / 'releases' / target}")
            return
        files = [p for p in files if p.is_file()]
        if not any(p.suffix == required for p in files):
            raise ValueError(f"Build did not produce a {required} image for {target}")
        destination = self.directory / "releases" / target
        destination.parent.mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(prefix=".firmware-", dir=destination.parent) as temporary:
            staging = Path(temporary) / "new"
            staging.mkdir()
            for source in files:
                shutil.copy2(source, staging / source.name)
            self.write_json(staging / "artifacts.json", {p.name: cross_toolchain.digest(p) for p in files})
            previous = Path(temporary) / "previous"
            if destination.exists():
                os.replace(destination, previous)
            try:
                os.replace(staging, destination)
            except OSError:
                if previous.exists():
                    os.replace(previous, destination)
                raise
        print(f"  Built {destination}", flush=True)

    def build(self, target, prepare_only=False):
        family, application, board, basename = TARGETS[target]
        print(f"\n[{target}]", flush=True)
        source = self.root / "firmware" / application
        work = self.directory / "work" / target
        if family in ("pico", "nrf"):
            self.host_tools()
            arm = self.arm_toolchain()
        if family == "pico":
            sdk = self.pico_sdk()
            pio = self.pio_usb() if application in ("rp2350-usb-a", "pico-pio-usb") else None
            picotool = self.picotool(sdk)
            if prepare_only:
                return
            command = [self.tool("cmake"), "-S", source, "-B", work, "-G", "Ninja"]
            if self.fresh:
                command.append("--fresh")
            command += [f"-DPICO_SDK_PATH={sdk}", f"-DPICO_TOOLCHAIN_PATH={arm / 'bin'}",
                        f"-DPICO_BOARD={board}", "-DCMAKE_BUILD_TYPE=Release",
                        f"-DPython3_EXECUTABLE={self.python}", "-DCMAKE_FIND_USE_PACKAGE_REGISTRY=OFF",
                        "-DCMAKE_EXPORT_PACKAGE_REGISTRY=OFF", "-DPICOTOOL_FORCE_FETCH_FROM_GIT=OFF",
                        f"-Dpicotool_DIR={picotool}"]
            if pio:
                command.append(f"-DPICO_PIO_USB_DIR={pio}")
            if self.offline:
                command.append("-DFETCHCONTENT_FULLY_DISCONNECTED=ON")
            self.run(command)
            self.run([self.tool("cmake"), "--build", work, "--parallel", self.jobs])
            self.publish(target, [work / (basename + suffix) for suffix in (".uf2", ".elf", ".bin", ".hex")], ".uf2")
        elif family == "nrf":
            sdk = self.ncs()
            if prepare_only:
                return
            env = self.env.copy()
            env.update(ZEPHYR_BASE=str(sdk / "zephyr"), ZEPHYR_TOOLCHAIN_VARIANT="gnuarmemb",
                       GNUARMEMB_TOOLCHAIN_PATH=str(arm), CMAKE_BUILD_PARALLEL_LEVEL=str(self.jobs))
            self.run([self.tool("west"), "build", "-s", source, "-d", work, "-b", board,
                      "-p", "always" if self.fresh else "auto", "--", f"-DPYTHON_EXECUTABLE={self.python}",
                      f"-DUSER_CACHE_DIR={self.directory / 'cache/zephyr'}",
                      "-DCMAKE_FIND_USE_PACKAGE_REGISTRY=OFF"],
                     cwd=sdk, env=env)
            self.publish(target, [work / "zephyr" / (basename + suffix)
                                  for suffix in (".hex", ".elf", ".bin", ".uf2")], ".hex")
        else:
            command = self.arduino()
            if prepare_only:
                return
            images = work / "images"
            if not self.dry_run and images.exists():
                shutil.rmtree(images)
            compile_args = [*command, "compile", "--fqbn", board, "--libraries", self.root / "firmware",
                            "--build-path", work / "compile", "--output-dir", images,
                            "--jobs", self.jobs]
            if self.fresh:
                compile_args.append("--clean")
            self.run([*compile_args, source])
            files = list(images.iterdir()) if not self.dry_run else []
            self.publish(target, [p for p in files if p.suffix in (".bin", ".elf", ".hex", ".json")], ".bin")


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--targets", nargs="+", choices=(*TARGETS, "all"), default=["all"])
    parser.add_argument("--list-targets", action="store_true", help="list boards without preparing SDKs")
    parser.add_argument("--jobs", type=int, default=min(os.cpu_count() or 4, 8))
    parser.add_argument("--dry-run", action="store_true", help="show commands without creating files or downloading")
    parser.add_argument("--offline", action="store_true", help="use already prepared SDKs and tools only")
    parser.add_argument("--prepare-only", action="store_true", help="prepare SDKs/tools without compiling firmware")
    parser.add_argument("--fresh", action="store_true", help="reconfigure the selected build directories")
    args = parser.parse_args(argv)
    if args.jobs < 1:
        parser.error("--jobs must be positive")
    if args.list_targets:
        for name, (_, application, board, _) in TARGETS.items():
            print(f"{name}: {application} ({board})")
        return 0
    targets = list(dict.fromkeys(name for target in args.targets
                                for name in (TARGETS if target == "all" else (target,))))
    try:
        builder = Builder(args.jobs, args.dry_run, args.offline, args.fresh)
        failed = []
        for target in targets:
            try:
                builder.build(target, args.prepare_only)
            except (OSError, ValueError, subprocess.CalledProcessError) as exc:
                print(f"[{target}] FAILED: {exc}", file=sys.stderr, flush=True)
                failed.append(target)
        print(f"\n{'Planned' if args.dry_run else 'Prepared' if args.prepare_only else 'Built'} "
              f"{len(targets) - len(failed)}/{len(targets)} targets.")
        return int(bool(failed))
    except (OSError, ValueError) as exc:
        print(f"Firmware setup failed: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        sys.exit(130)
