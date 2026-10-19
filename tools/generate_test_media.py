#!/usr/bin/env python3
"""Generate synthetic media fixtures for Go and browser tests."""

import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import shutil
import struct
import subprocess
import sys
import tempfile
import wave


ROOT = Path(__file__).resolve().parent.parent
MEDIA_DIRECTORY = "internal/testdata"
VIDEO_FILES = (
    f"{MEDIA_DIRECTORY}/passthrough.mp4",
    f"{MEDIA_DIRECTORY}/passthrough.h264",
    f"{MEDIA_DIRECTORY}/passthrough_hevc.mp4",
    f"{MEDIA_DIRECTORY}/passthrough.hevc",
    f"{MEDIA_DIRECTORY}/stream.ivf",
    *(f"{MEDIA_DIRECTORY}/url/" + name for name in (
        "movie.mp4", "movie.m3u8", "master.m3u8", "segment0.ts", "segment1.ts",
        "fragmented.m3u8", "init.mp4", "fragment0.m4s", "fragment1.m4s")),
)
AUDIO_FILES = (
    f"{MEDIA_DIRECTORY}/tone.aaceld",
    f"{MEDIA_DIRECTORY}/tone.alac",
    f"{MEDIA_DIRECTORY}/tone48.aaceld",
)
GROUPS = {"video": VIDEO_FILES, "audio": AUDIO_FILES}


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def boxes(data, start=0, end=None):
    """Read ISO BMFF boxes, including extended sizes and size-to-EOF boxes."""
    end = len(data) if end is None else end
    while start < end:
        if end - start < 8:
            raise ValueError("Truncated MP4 box header")
        size, kind = struct.unpack_from(">I4s", data, start)
        header = 8
        if size == 1:
            if end - start < 16:
                raise ValueError("Truncated extended MP4 box header")
            size = struct.unpack_from(">Q", data, start + 8)[0]
            header = 16
        elif size == 0:
            size = end - start
        if size < header or size > end - start:
            raise ValueError("Invalid MP4 box size")
        yield kind, start + header, start + size
        start += size


def audio_packets(data):
    """Extract raw packets from a single-track, non-fragmented audio MP4."""
    start, end = 0, len(data)
    for kind in (b"moov", b"trak", b"mdia", b"minf", b"stbl"):
        matches = [(first, last) for name, first, last in boxes(data, start, end) if name == kind]
        if len(matches) != 1:
            raise ValueError("Expected one audio sample table")
        start, end = matches[0]
    table = {name: data[first:last] for name, first, last in boxes(data, start, end)}
    try:
        fixed, count = struct.unpack_from(">II", table[b"stsz"], 4)
        sizes = [fixed] * count if fixed else struct.unpack_from(f">{count}I", table[b"stsz"], 12)
        offsets = table.get(b"stco", table.get(b"co64"))
        chunks = struct.unpack_from(">I", offsets, 4)[0]
        offsets = struct.unpack_from(f">{chunks}{'I' if b'stco' in table else 'Q'}", offsets, 8)
        entries = struct.unpack_from(">I", table[b"stsc"], 4)[0]
        mapping = [struct.unpack_from(">III", table[b"stsc"], 8 + 12 * i) for i in range(entries)]
    except (KeyError, TypeError, struct.error) as exc:
        raise ValueError("Invalid MP4 audio sample table") from exc
    if not mapping or mapping[0][0] != 1 or any(a[0] >= b[0] for a, b in zip(mapping, mapping[1:])):
        raise ValueError("Invalid MP4 chunk mapping")
    packets, entry = [], 0
    media = [(first, last) for name, first, last in boxes(data) if name == b"mdat"]
    for chunk, offset in enumerate(offsets, 1):
        while entry + 1 < len(mapping) and mapping[entry + 1][0] <= chunk:
            entry += 1
        for _ in range(mapping[entry][1]):
            if len(packets) >= count:
                raise ValueError("Too many MP4 audio samples")
            size = sizes[len(packets)]
            if not 0 < size <= 65535 or not any(first <= offset and offset + size <= last
                                               for first, last in media):
                raise ValueError("Invalid MP4 audio sample range")
            packets.append(data[offset:offset + size])
            offset += size
    if len(packets) != count:
        raise ValueError("Incomplete MP4 audio sample table")
    return packets


def run(command, cwd=None):
    result = subprocess.run([str(arg) for arg in command], cwd=cwd, capture_output=True, text=True)
    if result.returncode:
        raise ValueError(f"{Path(command[0]).name} failed:\n{result.stderr[-5000:]}")
    return result.stdout


def generate_video(directory, ffmpeg):
    def encode(*args, cwd=None):
        run([ffmpeg, "-hide_banner", "-loglevel", "error", "-nostdin", "-y", *args], cwd)

    media = directory / MEDIA_DIRECTORY
    for codec, encoder, name in (("h264", "libx264", "passthrough.mp4"),
                                 ("hevc", "libx265", "passthrough_hevc.mp4")):
        options = ["-tune", "zerolatency"] if codec == "h264" else [
            "-x265-params", "pools=none:frame-threads=1:keyint=30:scenecut=0"]
        encode("-f", "lavfi", "-i", "testsrc2=size=64x48:rate=30", "-frames:v", "4",
               "-c:v", encoder, "-preset", "ultrafast", *options, "-threads", "1",
               "-pix_fmt", "yuv420p", "-bf", "0", "-g", "30", "-map_metadata", "-1",
               "-movflags", "+faststart", media / name)
        encode("-i", media / name, "-map", "0:v:0", "-c", "copy", "-bsf:v",
               codec + "_mp4toannexb", "-f", codec, media / ("passthrough." + codec))

    pattern = bytearray()
    for frame in range(6):
        for y in range(48):
            for x in range(64):
                pattern.extend(((x * 3 + frame * 18) % 256, y * 4, (x + y + frame * 12) % 256))
    raw = directory / "pattern.rgb"
    raw.write_bytes(pattern)
    encode("-f", "rawvideo", "-pixel_format", "rgb24", "-video_size", "64x48",
           "-framerate", "30", "-i", raw, "-frames:v", "6", "-c:v", "libaom-av1",
           "-usage", "realtime", "-cpu-used", "8", "-lag-in-frames", "0", "-g", "3",
           "-threads", "1", "-pix_fmt", "yuv420p", "-f", "ivf",
           media / "stream.ivf")

    url = media / "url"
    encode("-f", "lavfi", "-i", "testsrc2=size=64x48:rate=10", "-f", "lavfi", "-i",
           "sine=frequency=440:sample_rate=44100", "-t", "2", "-c:v", "libx264",
           "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-g", "10", "-keyint_min", "10",
           "-sc_threshold", "0", "-bf", "2", "-threads", "1", "-c:a", "aac", "-ac", "2",
           "-b:a", "64k", "-map_metadata", "-1", "-movflags", "+faststart", url / "movie.mp4")
    encode("-i", "movie.mp4", "-c", "copy", "-hls_time", "1", "-hls_playlist_type", "vod",
           "-hls_segment_filename", "segment%d.ts", "-master_pl_name", "master.m3u8",
           "movie.m3u8", cwd=url)
    encode("-i", "movie.mp4", "-c", "copy", "-hls_time", "1", "-hls_playlist_type", "vod",
           "-hls_segment_type", "fmp4", "-hls_fmp4_init_filename", "init.mp4",
           "-hls_segment_filename", "fragment%d.m4s", "fragmented.m3u8", cwd=url)


def generate_audio(directory, afconvert):
    for rate, codec, frames, destination in (
        (44100, "aace#480", 22050, AUDIO_FILES[0]),
        (44100, "alac#352", 64 * 352, AUDIO_FILES[1]),
        (48000, "aace#480", 24000, AUDIO_FILES[2]),
    ):
        pcm = bytearray()
        for frame in range(frames):
            pcm.extend(struct.pack("<hh", *(int(8000 * math.sin(2 * math.pi * hz * frame / rate))
                                           for hz in (440, 660))))
        source = directory / "tone.wav"
        with wave.open(str(source), "wb") as output:
            output.setparams((2, 2, rate, frames, "NONE", "not compressed"))
            output.writeframes(pcm)
        encoded = directory / "tone.m4a"
        encoded.unlink(missing_ok=True)
        options = ["-b", "128000"] if codec.startswith("aace") else []
        run([afconvert, "-f", "m4af", "-d", codec, *options, source, encoded])
        packets = audio_packets(encoded.read_bytes())
        if codec.startswith("alac"):
            if len(packets) < 64:
                raise ValueError("ALAC encoder produced fewer than 64 packets")
            packets = packets[:64]  # Omit the final flush packet.
        elif len(packets) != math.ceil(frames / 480) + 1:
            raise ValueError("AAC-ELD encoder produced an unexpected packet count")
        (directory / destination).write_bytes(b"".join(struct.pack(">H", len(p)) + p for p in packets))


def prepare(root=ROOT, groups=None, ffmpeg="ffmpeg", force=False, check=False):
    groups = groups if groups is not None else (["video", "audio"] if sys.platform == "darwin" else ["video"])
    manifest_path = root / "build/test-media.json"
    signature = digest(Path(__file__))
    try:
        manifest = json.loads(manifest_path.read_text())
    except (OSError, ValueError):
        manifest = {}
    if (not isinstance(manifest, dict) or manifest.get("generator") != signature or
            not isinstance(manifest.get("files"), dict)):
        manifest = {"generator": signature, "files": {}}
    pending = [group for group in groups if force or any(
        not (root / name).is_file() or manifest["files"].get(name) != digest(root / name)
        for name in GROUPS[group])]
    if check:
        if pending:
            raise ValueError("Missing or outdated fixtures; run tools/generate_test_media.py --groups " + " ".join(pending))
        print("Test media is ready.")
        return
    if not pending:
        print("Test media is ready; use --force to regenerate.")
        return
    if "video" in pending:
        ffmpeg = shutil.which(ffmpeg)
        if not ffmpeg:
            raise ValueError("FFmpeg executable not found; install it or pass --ffmpeg /path/to/ffmpeg")
        ffmpeg = str(Path(ffmpeg).resolve())
        encoders = run([ffmpeg, "-hide_banner", "-encoders"])
        missing = [name for name in ("libx264", "libx265", "libaom-av1", "aac")
                   if not any(name in line.split() for line in encoders.splitlines())]
        if missing:
            raise ValueError("FFmpeg is missing encoders: " + ", ".join(missing))
    afconvert = shutil.which("afconvert")
    if "audio" in pending and (sys.platform != "darwin" or not afconvert):
        raise ValueError("Audio fixtures require macOS afconvert; use --groups video on other systems")
    with tempfile.TemporaryDirectory(prefix="anykvm-test-media-") as temporary:
        staging = Path(temporary)
        names = [name for group in pending for name in GROUPS[group]]
        for name in names:
            (staging / name).parent.mkdir(parents=True, exist_ok=True)
        for group in pending:
            print(f"Generating {group} fixtures...", flush=True)
            if group == "video":
                generate_video(staging, ffmpeg)
            else:
                generate_audio(staging, afconvert)
        for name in names:
            if not (staging / name).is_file() or not (staging / name).stat().st_size:
                raise ValueError("Encoder did not produce " + name)
        for name in names:
            destination = root / name
            destination.parent.mkdir(parents=True, exist_ok=True)
            descriptor, temporary_file = tempfile.mkstemp(prefix=".media-", dir=destination.parent)
            try:
                with os.fdopen(descriptor, "wb") as output:
                    output.write((staging / name).read_bytes())
                os.chmod(temporary_file, 0o644)
                os.replace(temporary_file, destination)
            finally:
                Path(temporary_file).unlink(missing_ok=True)
            manifest["files"][name] = digest(destination)
    manifest_path.parent.mkdir(parents=True, exist_ok=True)
    manifest_path.write_text(json.dumps(manifest, indent=2) + "\n")
    print(f"Generated {len(names)} fixtures. Outputs are ignored by Git.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--ffmpeg", default=os.environ.get("ANYKVM_TEST_FFMPEG", "ffmpeg"),
                        help="FFmpeg executable with libx264, libx265, libaom-av1 and AAC")
    parser.add_argument("--groups", nargs="+", choices=GROUPS,
                        help="default: video and audio on macOS, video elsewhere")
    parser.add_argument("--force", action="store_true", help="regenerate cached fixtures")
    parser.add_argument("--check", action="store_true", help="check files without running encoders")
    args = parser.parse_args()
    try:
        prepare(groups=args.groups, ffmpeg=args.ffmpeg, force=args.force, check=args.check)
    except (OSError, ValueError) as exc:
        parser.exit(1, f"Test media preparation failed: {exc}\n")


if __name__ == "__main__":
    main()
