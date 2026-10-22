# Build and media runtime

## Requirements

Use Go 1.24.5 or newer, Python 3 and a target C compiler. Full media builds
require CGO; `viewer` also requires SDL2. The build script downloads missing
CMake, Ninja and supported shader tools into `build/toolchains/`.
NASM enables TurboJPEG x86 SIMD. macOS FFmpeg builds also need Make and pkg-config.

- **macOS:** Apple SDK/Command Line Tools. Native screen capture requires macOS
  12.3+; system-audio capture requires macOS 13+. Building macOS from another OS
  requires a separately supplied Apple SDK and compatible compiler; automatic
  macOS runtime packaging is supported only on a macOS host.
- **Linux:** native builds need a C toolchain, pkg-config, V4L2 and ALSA headers
  (`build-essential pkg-config libv4l-dev libasound2-dev` on Debian), plus
  `libsdl2-dev` for the viewer. Debian 12 cross-build sysroots target glibc 2.36+.
- **Windows:** the desktop build script prepares LLVM-MinGW and the CMake/Ninja
  tools used for runtime libraries and ARM64 SDL2. The separate PowerShell
  builder expects a MinGW-compatible compiler already on PATH.

The optional Linux `spice` tag requires spice-client-glib, EGL, GBM and libdrm
development packages. See [SPICE upstream](spice-source.md) for build and runtime
requirements. The build script bundles their runtime libraries; the development
packages are still needed to compile this optional feature.

## Desktop build script

Run from the repository root:

```sh
python3 tools/build.py --targets native
python3 tools/build.py --targets linux/arm64 windows/arm64
python3 tools/build.py --targets all --tags viewer,ffmpeg
python3 tools/build.py --dry-run
```

On Windows use `python` if `python3` is unavailable. With no arguments, the
script interactively selects platform, architecture, viewer and FFmpeg support.
Supported targets are Linux, Windows and macOS, each on amd64/arm64.
Outputs are `build/releases/<os>-<arch>/anykvm` (`anykvm.exe` on Windows).
`--output` changes that directory. Failed targets return a nonzero status;
other targets are still attempted and existing binaries survive failed builds.

The script caches verified sources and tools under `build/toolchains/` and
packages runtime libraries and licenses alongside the executable:

- Default builds include TurboJPEG; `ffmpeg` builds use FFmpeg 9 instead.
- Native Windows builds include Opus; `viewer` builds include SDL2.
- Linux builds include V4L2/ALSA and dependencies for selected features.
  Debian 12 runtime bundles require glibc 2.36+; GPU drivers remain system-provided.

Failed builds preserve existing outputs. Successful rebuilds remove obsolete
runtime files previously staged by the script.

`--no-bootstrap` disables downloads and automatic cross-toolchain setup. Supply
custom toolchain settings below; required dependencies must already be cached.
Use `ANYKVM_BUILD_<OS>_<ARCH>_FFMPEG_LIBDIR` or
`ANYKVM_BUILD_<OS>_<ARCH>_TURBOJPEG_LIBDIR` for custom target libraries.
Per-target overrides use
`ANYKVM_BUILD_<OS>_<ARCH>_<VARIABLE>`, for example:

```sh
export ANYKVM_BUILD_LINUX_ARM64_CC=aarch64-linux-gnu-gcc
export ANYKVM_BUILD_LINUX_ARM64_PKG_CONFIG_SYSROOT_DIR=/opt/linux-arm64
python3 tools/build.py --no-bootstrap --targets linux/arm64
```

Supported variables include `CC`, `CXX`, `CGO_CFLAGS`, `CGO_CPPFLAGS`,
`CGO_CXXFLAGS`, `CGO_LDFLAGS`, `PKG_CONFIG`, `PKG_CONFIG_PATH`,
`PKG_CONFIG_LIBDIR`, `PKG_CONFIG_SYSROOT_DIR`, `SDKROOT` and
`MACOSX_DEPLOYMENT_TARGET`. Ordinary host settings do not leak into cross targets.

For native Windows amd64 builds, this alternative can also stage FFmpeg DLLs:

```powershell
./tools/build_windows.ps1
./tools/build_windows.ps1 -Tags ffmpeg -FFmpegBin C:\ffmpeg9\bin
```

It writes `build/windows/anykvm.exe`; `-Compiler` and `-Output` are optional.
`FFmpegBin` requires `-Tags ffmpeg`. Neither builder starts a service.

## GitHub Actions and Releases

GitHub Actions builds Linux, Windows and macOS for amd64 and arm64. Pushes to
`main`, pull requests and manual runs produce downloadable build artifacts.

After pushing `main` and verifying all six builds, tag the tested commit.
The tag run rebuilds all targets and publishes a Release only if they pass:

```sh
git tag -a v0.1.1-beta.1 -m "AnyKVM v0.1.1-beta.1"
git push origin refs/tags/v0.1.1-beta.1
```

Tags containing `-` are marked as prereleases. Packages include the executable,
runtime files, licenses and documentation: ZIP for Windows, tar.gz for Linux
and macOS, plus `SHA256SUMS` for verification.

Release builds use native media support without `ffmpeg`, `viewer` or `spice`.
Linux packages include V4L2/ALSA libraries and require glibc 2.36+.
Windows includes the MinGW-built Opus runtime.
macOS packages are not notarized. Firmware is attached separately as
`firmware-<target>.zip` only when firmware sources or build/package scripts
changed since the previous published ancestor Release containing firmware. See the
[firmware guide](../firmware/README.md#release-packages).

To package a local build:

```sh
python3 tools/package_release.py --target linux/arm64
```

Archives are written to `build/dist/`.

## Direct Go builds

Before invoking Go directly, restore sources for the target platform:

```sh
python3 tools/prepare_sources.py --platforms darwin
go build -o build/anykvm ./cmd/anykvm
```

Replace `darwin` with `linux` or `windows` for those targets. Linux CGO builds
also need `python3 tools/build_shaders.py`; macOS, Windows and CGO-disabled
portability builds do not embed SPIR-V. Add `--ffmpeg` when preparing sources
for a `go build -tags ffmpeg` or `-tags viewer,ffmpeg` build.

Use an `.exe` output name on Windows. `prepare_sources.py --offline` and
`build_shaders.py --offline` use installed or cached inputs only. Source versions
and checksums are recorded in `tools/sources.lock.json`. Generated sources and
shaders are ignored by Git; shader tools are not needed at runtime.

## Native and optional codecs

Default builds provide native hardware video encoding without FFmpeg:

- **macOS:** H.264/HEVC through VideoToolbox; native JPEG and audio codecs.
- **Windows:** H.264/HEVC/AV1 through NVIDIA NVENC, AMD AMF, Intel QSV or
  Media Foundation.
- **Linux:** H.264/HEVC/AV1 through Vulkan Video; Raspberry Pi also has V4L2 H.264.

Codec availability depends on the GPU and driver; HEVC/AV1 require hardware
encoding. Windows vendor runtimes must match the executable architecture.
See [capture setup](capture.md) and [GameStream codec settings](gamestream.md#ports-and-codecs).

`-tags ffmpeg` plus CGO adds FFmpeg decoding/conversion and media paths. It uses
libavcodec NVENC/AMF/QSV for Windows encoding and adds
NVENC/VAAPI on Linux. Software H.264 encoding uses x264 where available.
Installing libraries cannot enable these in an untagged binary. Encoded
passthrough does not require a decoder; [ARD HEVC](ard-client.md) and
[CoreDevice pixel/JPEG output](iphone-coredevice.md) do.

Default JPEG uses TurboJPEG, with Go and macOS ImageIO fallbacks. FFmpeg builds use
FFmpeg JPEG and never load TurboJPEG. GPU JPEG and restart-based lossless
cropping remain available in both builds; other MJPEG uses whole-frame
passthrough when lossless cropping is unavailable.

`build.py` packages native Windows audio as `lib/opus.dll`. Custom JPEG and Opus
library locations can be set with `ANYKVM_JPEG_LIBDIR` and `ANYKVM_OPUS_LIBDIR`.

## FFmpeg libraries and packaging

`build.py --tags ffmpeg` prepares FFmpeg 9 headers and shared libraries
automatically. FFmpeg 7/8 libraries are incompatible. The application loads
libraries in process; an `ffmpeg` executable is not a substitute.

For custom builds, stage matching OS/CPU libraries with the standalone packager.
Search order includes `ANYKVM_FFMPEG_LIBDIR`, executable `lib/`, the executable
directory and system paths; macOS also checks Homebrew FFmpeg paths. Libraries,
codec dependencies and application must match OS/CPU.

```sh
python3 tools/package_ffmpeg.py /path/to/ffmpeg9/lib /path/to/app/lib
```

On Windows, use the distribution's DLL directory as the source. On macOS, the
packager follows non-system dependencies, rewrites `@loader_path` references
and signs staged dylibs. Repeat `--dependency-dir` for extracted dependencies.
Linux dependencies must be staged separately. Replace old bundles when changing
profiles to avoid retaining unused libraries.

A smaller macOS profile can be built from FFmpeg 9 sources and x264/Opus prefixes
containing headers, pkg-config metadata and static libraries (Intel also needs
NASM):

```sh
bash tools/build_ffmpeg_macos.sh /path/to/ffmpeg9 /path/to/x264 /path/to/opus
python3 tools/package_ffmpeg.py build/ffmpeg-macos/lean/lib /path/to/app/lib
```

The profile includes H.264/HEVC, MJPEG, AAC/ALAC, Opus and platform conversion;
it is not a general codec bundle. Build/package separately for each architecture.

## Source layout

The unified entry point is in `cmd/anykvm`; application/configuration wiring is
in `internal/app`, and protocol/device modules are under `internal/`.
Comments, documentation and build-script output use English. The configuration
schema is defined in [config.go](../internal/app/config.go); usage is covered in
the [configuration guide](configuration.md).

Firmware uses a separate [shared builder](../firmware/README.md#build), with
SDKs and images kept under `build/firmware/`.

## Tests and latency measurements

After preparing build inputs, generate the
[synthetic media fixtures](../internal/testdata/README.md).
That guide covers encoder requirements, generation options and fixture formats.

The Python suite includes Go build-tag checks and also needs prepared embedded
assets. Browser unit tests use the Node.js test runner.

```sh
python3 tools/generate_test_media.py
python3 -m unittest discover -s tools
go test ./...
node --test internal/wsmedia/web_test/*.test.mjs internal/wsmedia/codec.test.mjs tools/test_jpeg_player.mjs
ANYKVM_FFMPEG_LIBDIR=/path/to/ffmpeg9/lib ANYKVM_TEST_AVMEDIA=1 \
  go test -tags ffmpeg ./internal/avmedia
```

Hardware/browser tests opt in through their documented environment variables.
`ANYKVM_TEST_FFMPEG` supplies a test-only independent decoder executable.
Linux/Windows builds with CGO disabled are portability checks, not media releases.
Native macOS builds require CGO.

For configuration changes, run the focused first-run and writing-policy checks:

```sh
go test ./internal/app -run 'TestFirstRunConfig|TestConfigWritingPolicy' -count=1
```

## References

- [FFmpeg](https://ffmpeg.org/)
- [Linux](https://github.com/torvalds/linux)
- [SDL](https://github.com/libsdl-org/SDL)
- [nv-codec-headers](https://github.com/FFmpeg/nv-codec-headers)
- [AMF](https://github.com/GPUOpen-LibrariesAndSDKs/AMF)
- [libvpl](https://github.com/intel/libvpl)
- [hls.js](https://github.com/video-dev/hls.js)
