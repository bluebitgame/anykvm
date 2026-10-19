# Build and media runtime

## Requirements

Use Go 1.24.5 or newer, Python 3 and a target C compiler.
Linux CGO builds compile Vulkan shaders using DXC/glslang on PATH, or a pinned,
checksum-verified glslang downloaded into `build/toolchains/`. Automatic shader
tool preparation supports macOS and amd64 Linux/Windows hosts; other build
hosts need an installed DXC/glslang.
Full media builds require CGO. The optional `viewer` tag also requires SDL2.

- **macOS:** Apple SDK/Command Line Tools. Native screen capture requires macOS
  12.3+; system-audio capture requires macOS 13+. Building macOS from another OS
  requires a separately supplied Apple SDK and compatible compiler.
- **Linux:** native builds need a C toolchain, pkg-config, V4L2 and ALSA headers
  (`build-essential pkg-config libv4l-dev libasound2-dev` on Debian), plus
  `libsdl2-dev` for the viewer. Debian 12 cross-build sysroots target glibc 2.36+.
- **Windows:** the desktop build script prepares LLVM-MinGW. Windows ARM64
  viewer builds also need CMake, and Ninja when built on Windows. The separate
  PowerShell builder expects a MinGW-compatible compiler already on PATH.

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

The script restores pinned sources, compiles Linux Vulkan shaders when needed,
and prepares supported cross toolchains under
`build/toolchains/`. Initial setup needs network access;
verified downloads are cached. macOS/Windows viewer builds bundle matching SDL2
libraries; Linux viewers use the target system's SDL2 runtime.
Linux releases also need V4L2/ALSA runtime libraries, such as `libv4l-0`,
`libv4lconvert0` and `libasound2` on Debian 12.

`--no-bootstrap` uses installed/custom toolchains and cached dependencies without
automatic downloads. It also skips automatic cross compiler/sysroot configuration;
set the overrides below to reuse a toolchain under `build/toolchains/` explicitly.
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

Use an `.exe` output name on Windows. `prepare_sources.py --offline` restores
from verified cache only. `--platforms linux --ffmpeg` restricts restoration to
one platform including FFmpeg headers; without `--platforms`, all sources are
restored. `build_shaders.py --compiler /path/to/dxc` selects a compiler outside
PATH. `build_shaders.py --offline` uses only installed or cached compilers.
Neither DXC nor glslang is needed on the runtime host.

Versions/checksums are in `tools/sources.lock.json`. Downloaded sources, generated
SPIR-V and its hash manifest are ignored by Git.

Linux builds use restored DRM UAPI headers for direct `ioctl` access and
Vulkan/vk_video headers for GPU conversion and encoding. The Vulkan loader is
resolved at runtime; target systems need no Vulkan SDK or shader compiler.
Source preparation also restores the hls.js UMD browser bundle with its embedded
transmuxer worker.

## Native and optional codecs

Default builds exclude the FFmpeg bridge and loader. Native capabilities depend
on OS, hardware and drivers:

- **macOS:** ScreenCaptureKit/AVFoundation capture, VideoToolbox H.264/HEVC,
  ImageIO JPEG and AudioToolbox audio/Opus.
- **Linux:** V4L2/ALSA capture, DRM/KMS with Vulkan conversion and native Vulkan
  H.264, or Raspberry Pi V4L2 H.264. Tagged builds use libavcodec NVENC/VAAPI
  and software x264 where supported. Native Vulkan HEVC/AV1 is absent.
- **Windows:** DXGI desktop and Media Foundation device capture. Native builds
  use vendor NVENC/AMF/QSV APIs, then Media Foundation; H.264 and available
  hardware AV1 are supported. Tagged builds replace vendor bridges with
  libavcodec and retain native Media Foundation. Windows HEVC encoding is absent.

GPU candidates are probed before use. A successful build does not establish
hardware/driver support. See [capture setup and hardware checks](capture.md).

Native Windows builds compile the pinned NVIDIA Video Codec SDK 12.0 interface,
AMD AMF and Intel VPL headers. AMF requests the stable 1.4.28 base interfaces.
AMF is pinned to upstream commit `a4c8f39ae1959c9ca9d56ade35d6a23c52fc77c5`
to include the C-interface `AMFContext2` GUID fix.
VPL calls the installed runtime through `MFXInitialize`, with `MFXInitEx` for
H.264-only Media SDK drivers; no VPL dispatcher is linked. Vendor DLLs must
match the process architecture. Windows arm64 uses vendor APIs only when a
compatible native driver runtime exists, otherwise it tries Media Foundation.

`-tags ffmpeg` plus CGO enables software decoding/conversion, library encoders
and other FFmpeg media paths. Installing libraries cannot enable them in an
untagged binary. Encoded passthrough does not require a decoder; [ARD HEVC](ard-client.md)
and [CoreDevice pixel/JPEG output](iphone-coredevice.md) do.

Linux/Windows JPEG optionally loads TurboJPEG 2 from the executable's `lib/`,
`ANYKVM_JPEG_LIBDIR` or system paths (`libturbojpeg.so.0` / `turbojpeg.dll`).
Without it or CGO, JPEG uses Go. This is independent of FFmpeg, and quality
numbers need not produce identical files across JPEG backends.

Native Windows GameStream audio loads `opus.dll` from `ANYKVM_OPUS_LIBDIR`,
then executable `lib/` and the executable directory. Build scripts package the
pinned DLL; its matching Visual C++ runtime is also needed. Missing
or failed audio encoders leave video/input available. Tagged builds use
libavcodec/libopus instead. To stage a DLL separately:

```sh
python3 tools/package_opus.py --arch amd64 --output build/windows/lib
```

Use `--arch arm64` for ARM64. A PCM source is still required.

## FFmpeg libraries and packaging

Source preparation restores the FFmpeg 9 public API headers and generates
`avconfig.h` and `ffversion.h` for little-endian amd64/arm64 targets. The headers
are used only by builds with CGO and the `ffmpeg` tag.

Tagged builds load **FFmpeg 9** shared libraries in process; production does not
run an `ffmpeg` executable. Required ABI majors are libavcodec/libavformat/
libavdevice 63, libavutil 61, libswscale 10 and libswresample 7. The packager also
stages libavfilter 12. FFmpeg 7/8 libraries are incompatible.

Search order includes `ANYKVM_FFMPEG_LIBDIR`, executable `lib/`, the executable
directory and system paths; macOS also checks Homebrew FFmpeg paths. Libraries,
codec dependencies and application must match OS/CPU. A standalone executable
cannot substitute for shared libraries.

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

Platform-specific codec benchmarks and media regression tools are documented in
the [codec benchmark guide](../tools/codec-bench/README.md). Current tools cover
macOS VideoToolbox; the guide describes the files, build commands and output paths.

The production VideoToolbox encoder also has an opt-in latency test:

```sh
ANYKVM_TEST_VT_LATENCY=1 go test ./internal/videoenc -run '^TestVideoToolboxLatency$' -count=2 -v
```

## References

- [FFmpeg](https://ffmpeg.org/)
- [Linux](https://github.com/torvalds/linux)
- [SDL](https://github.com/libsdl-org/SDL)
- [nv-codec-headers](https://github.com/FFmpeg/nv-codec-headers)
- [AMF](https://github.com/GPUOpen-LibrariesAndSDKs/AMF)
- [libvpl](https://github.com/intel/libvpl)
- [hls.js](https://github.com/video-dev/hls.js)
