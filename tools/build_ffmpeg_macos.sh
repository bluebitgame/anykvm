#!/bin/bash
# Build the FFmpeg 9 runtime used by AnyKVM, with x264/Opus linked statically.
# Usage: bash tools/build_ffmpeg_macos.sh FFMPEG_SOURCE X264_PREFIX OPUS_PREFIX [OUTPUT_PREFIX]
set -euo pipefail

if [[ $(uname -s) != Darwin || $# -lt 3 || $# -gt 4 ]]; then
    echo "Usage (macOS): $0 FFMPEG_SOURCE X264_PREFIX OPUS_PREFIX [OUTPUT_PREFIX]" >&2
    exit 1
fi
task_root=$(cd "$(dirname "$0")/.." && pwd)
ffmpeg_source=$(cd "$1" && pwd)
x264_prefix=$(cd "$2" && pwd)
opus_prefix=$(cd "$3" && pwd)
output_prefix=${4:-"$task_root/build/ffmpeg-macos/lean"}
mkdir -p "$output_prefix"
output_prefix=$(cd "$output_prefix" && pwd)
build_dir="$output_prefix/work"
static_prefix="$output_prefix/static-deps"
for tool in clang make pkg-config; do
    command -v "$tool" >/dev/null
done
if [[ $(uname -m) == x86_64 ]]; then
    command -v nasm >/dev/null
fi
if ! grep -Eq '^#define LIBAVCODEC_VERSION_MAJOR +63$' "$ffmpeg_source/libavcodec/version_major.h"; then
    echo "FFmpeg 9 sources (libavcodec 63) are required" >&2
    exit 1
fi
test -f "$x264_prefix/lib/libx264.a"
test -f "$opus_prefix/lib/libopus.a"
mkdir -p "$static_prefix/lib/pkgconfig" "$static_prefix/include" "$build_dir"
cp "$x264_prefix/lib/libx264.a" "$opus_prefix/lib/libopus.a" "$static_prefix/lib/"
cp "$x264_prefix/include/x264.h" "$x264_prefix/include/x264_config.h" "$static_prefix/include/"
cp -R "$opus_prefix/include/opus" "$static_prefix/include/"
# Only static codec libraries are visible to configure and the linker here.
cat > "$static_prefix/lib/pkgconfig/x264.pc" <<EOF
prefix=$static_prefix
Name: x264
Description: Static x264 for AnyKVM
Version: $(pkg-config --define-variable=prefix="$x264_prefix" --modversion "$x264_prefix/lib/pkgconfig/x264.pc")
Libs: -L\${prefix}/lib -lx264 -lpthread -lm
Cflags: -I\${prefix}/include
EOF
cat > "$static_prefix/lib/pkgconfig/opus.pc" <<EOF
prefix=$static_prefix
Name: Opus
Description: Static Opus for AnyKVM
Version: $(pkg-config --modversion "$opus_prefix/lib/pkgconfig/opus.pc")
Libs: -L\${prefix}/lib -lopus -lm
Cflags: -I\${prefix}/include/opus
EOF
export PKG_CONFIG_LIBDIR="$static_prefix/lib/pkgconfig"
export PKG_CONFIG_PATH=""
cd "$build_dir"
"$ffmpeg_source/configure" \
    --prefix="$output_prefix" \
    --disable-autodetect --disable-everything --disable-programs \
    --disable-doc --disable-debug --disable-static \
    --enable-network --enable-securetransport --disable-sdl2 --disable-xlib \
    --enable-shared --enable-gpl --enable-pthreads \
    --enable-libx264 --enable-libopus --enable-zlib \
    --enable-avfoundation --enable-videotoolbox --enable-audiotoolbox \
    --enable-encoder=libx264,h264_videotoolbox,hevc_videotoolbox,mjpeg,libopus,rawvideo,pcm_s16le \
    --enable-decoder=h264,hevc,mjpeg,mjpegb,rawvideo,wrapped_avframe,png,libopus,pcm_s16le,pcm_f32le,aac,alac \
    --enable-parser=h264,hevc,mjpeg,opus,aac \
    --enable-bsf=h264_mp4toannexb,hevc_mp4toannexb,extract_extradata,aac_adtstoasc \
    --enable-demuxer=mov,h264,hevc,mjpeg,rawvideo,wav,pcm_s16le,hls,mpegts,aac \
    --enable-protocol=file,pipe,http,https,tcp,tls,crypto \
    --enable-indev=avfoundation,lavfi \
    --enable-filter=buffer,abuffer,buffersink,abuffersink,scale,format,aresample,aformat,null,anull,testsrc,testsrc2,sine
make -j "${ANYKVM_FFMPEG_JOBS:-$(sysctl -n hw.logicalcpu 2>/dev/null || echo 4)}"
make install
mkdir -p "$output_prefix/lib/licenses"
cp "$ffmpeg_source/COPYING.GPLv2" "$ffmpeg_source/COPYING.LGPLv2.1" "$output_prefix/lib/licenses/"
for codec in x264 opus; do
    if [[ $codec == x264 ]]; then codec_prefix=$x264_prefix; else codec_prefix=$opus_prefix; fi
    if [[ -f "$codec_prefix/COPYING" ]]; then
        cp "$codec_prefix/COPYING" "$output_prefix/lib/licenses/$codec-COPYING"
    fi
done
python3 - "$output_prefix/lib" <<'PY'
import pathlib, subprocess, sys
root = pathlib.Path(sys.argv[1]).resolve()
libraries = {p.resolve() for p in root.glob("*.dylib")}
for library in sorted(libraries):
    lines = subprocess.check_output(["otool", "-L", str(library)], text=True).splitlines()[2:]
    for line in lines:
        dependency = line.strip().split(" (", 1)[0]
        if dependency.startswith(("/usr/lib/", "/System/Library/", str(root) + "/")):
            continue
        raise SystemExit(f"unexpected external dependency: {library.name}: {dependency}")
print(f"Verified {len(libraries)} dylibs: only FFmpeg siblings and macOS system libraries")
PY
echo "Built FFmpeg libraries in $output_prefix/lib"
echo "Stage them with: python3 $task_root/tools/package_ffmpeg.py $output_prefix/lib /path/to/app/lib"
