# Codec benchmarks

Keep each platform's codec benchmarks and related media regression tests in its
own directory. Current tools target macOS and use synthetic media; they do not
capture a display or camera. Encoder benchmarks use VideoToolbox.

## Layout

- `macos/`: VideoToolbox and FFmpeg benchmarks and Python runners.
- `macos/tests/`: cursor/screen geometry, capture-buffer ownership, screen encoder
  queues and H.264 compatibility checks against production code.

Add `linux/` and `windows/` alongside `macos/` when their implementations exist.
Name benchmarks by backend and operation, such as `videotoolbox_encode_bench.m`
or `vaapi_decode_bench.c`. Use `run_<backend>_<operation>_matrix.py` for parameter
sweeps and `<subject>_test` for regression tests. Move reusable helpers to
`common/` when another platform needs them.

Store binaries, result JSON and generated bitstreams under
`build/codec-bench/<platform>/`, which is ignored by Git.

## macOS tools

- [`videotoolbox_encode_bench.m`](macos/videotoolbox_encode_bench.m): direct H.264/HEVC
  encoding, callback latency and optional bitstream output.
- [`run_videotoolbox_encode_matrix.py`](macos/run_videotoolbox_encode_matrix.py):
  H.264/HEVC pixel formats, low-latency settings and HEVC speed priority.
- [`run_videotoolbox_h264_matrix.py`](macos/run_videotoolbox_h264_matrix.py):
  H.264 profiles, low-latency settings and frame-delay limits.
- [`ffmpeg_videotoolbox_encode_bench.py`](macos/ffmpeg_videotoolbox_encode_bench.py):
  FFmpeg H.264 output latency, including project-style AUD framing.
- [`compare_h264_encode_latency.py`](macos/compare_h264_encode_latency.py):
  direct VideoToolbox callbacks versus FFmpeg output and framing latency.

Run from the repository root with Apple Command Line Tools and a Mac hardware
encoder. FFmpeg comparisons also require an executable with `h264_videotoolbox`.

```sh
mkdir -p build/codec-bench/macos
clang -O2 -fobjc-arc tools/codec-bench/macos/videotoolbox_encode_bench.m \
  -framework Foundation -framework VideoToolbox -framework CoreMedia -framework CoreVideo \
  -o build/codec-bench/macos/videotoolbox_encode_bench
python3 tools/codec-bench/macos/run_videotoolbox_encode_matrix.py \
  build/codec-bench/macos/videotoolbox_encode_bench \
  build/codec-bench/macos/encode-matrix.json --focus --repeat 3 --frames 180
python3 tools/codec-bench/macos/compare_h264_encode_latency.py \
  build/codec-bench/macos/videotoolbox_encode_bench /path/to/ffmpeg \
  build/codec-bench/macos/h264-encode-comparison.json
```

Run comparisons without concurrent encoders or compilation. These measurements
cover encoding and output handling, excluding capture, networking and physical
display. Results depend on the hardware, codec, pixel format and parameters.
