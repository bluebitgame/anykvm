#!/usr/bin/env python3
"""Measure FFmpeg h264_videotoolbox output/framing latency with synthetic NV12."""
import argparse
import json
import re
import statistics
import subprocess
import threading
import time
from pathlib import Path


def measure(ffmpeg, width, height, low, profile, frames=120, fps=30,
            pixel_format='nv12', allow_sw=False, pattern='moving'):
    args = [ffmpeg, '-hide_banner', '-loglevel', 'warning', '-f', 'rawvideo',
            '-pixel_format', pixel_format, '-video_size', f'{width}x{height}',
            '-framerate', str(fps), '-probesize', '32', '-analyzeduration', '0',
            '-i', 'pipe:0', '-an', '-c:v', 'h264_videotoolbox',
            '-profile:v', profile, '-realtime', 'true', '-prio_speed', 'true',
            '-allow_sw', str(allow_sw).lower(), '-bf', '0', '-b:v', '12M' if width > 640 else '4M', '-g', str(fps),
            '-r', str(fps), '-pix_fmt', pixel_format, '-flush_packets', '1',
            '-bsf:v', 'h264_metadata=aud=insert']
    if low:
        args += ['-flags', '+low_delay']
    args += ['-f', 'h264', 'pipe:1']
    sent, arrivals, parsed, stderr = [], [], [], []
    proc = subprocess.Popen(args, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, bufsize=0)

    def read_output():
        tail = b''
        pending = b''
        synced = False
        has_au = False
        while True:
            chunk = proc.stdout.read(65536)
            if not chunk:
                break
            data = tail + chunk
            arrivals.extend([time.monotonic()] * len(re.findall(b'\x00\x00\x01\x09', data)))
            tail = data[-3:]
            # Same boundary rule as capture.AUParser in audDelimited mode:
            # delimit each NAL, then emit the previous AU at the next AUD.
            pending += chunk
            if not synced:
                first = pending.find(b'\x00\x00\x01')
                if first < 0:
                    pending = pending[-3:]
                    continue
                pending = pending[first:]
                synced = True
            while True:
                boundary = pending.find(b'\x00\x00\x01', 3)
                if boundary < 0:
                    break
                nal, pending = pending[:boundary], pending[boundary:]
                if len(nal) >= 4 and nal[3] & 31 == 9 and has_au:
                    parsed.append(time.monotonic())
                has_au = True

    reader = threading.Thread(target=read_output)
    err_reader = threading.Thread(target=lambda: stderr.append(proc.stderr.read().decode(errors='replace')))
    reader.start()
    err_reader.start()
    watchdog = threading.Timer(20, proc.kill)
    watchdog.start()
    uv = bytes([128]) * (width * height // 2)
    # Same moving 32-pixel checkerboard as the native benchmark.
    patterns = []
    for phase in range(8):
        row = bytes(32 + (((col + phase * 8) // 32) % 2) * 160 for col in range(width))
        inverse = bytes(224 - v for v in row)
        patterns.append(b''.join(row if (y // 32) % 2 == 0 else inverse for y in range(height)) + uv)
    previous = 0
    try:
        for i in range(frames):
            time.sleep(max(0, previous + 1 / fps - time.monotonic()))
            previous = time.monotonic()
            # Neutral chroma bytes are identical in NV12 and yuv420p layouts.
            frame = (bytes([32 + i % 160]) * (width * height) + uv
                     if pattern == 'flat' else patterns[i % 8])
            data = memoryview(frame)
            sent.append(time.monotonic())
            while data:
                data = data[proc.stdin.write(data):]
        drain_start = time.monotonic()
        proc.stdin.close()
    except (BrokenPipeError, OSError):
        drain_start = time.monotonic()
    proc.wait()
    reader.join()
    err_reader.join()
    watchdog.cancel()
    values = [(arrivals[i] - sent[i]) * 1000
              for i in range(30, min(len(sent), len(arrivals), frames - 15))
              if arrivals[i] < drain_start]
    result = {'low_latency': low, 'profile': profile, 'width': width, 'height': height,
              'fps': fps, 'input_frames': len(sent), 'output_auds': len(arrivals),
              'samples': len(values), 'exit': proc.returncode, 'stderr': ''.join(stderr),
              'command': args, 'pixel_format': pixel_format, 'allow_sw': allow_sw, 'pattern': pattern}
    if values:
        result.update(median_ms=statistics.median(values),
                      p95_ms=sorted(values)[int((len(values) - 1) * .95)])
    au_values = [(parsed[i] - sent[i]) * 1000
                 for i in range(30, min(len(sent), len(parsed), frames - 15))
                 if parsed[i] < drain_start]
    result['parsed_frames'] = len(parsed)
    result['parsed_samples'] = len(au_values)
    if au_values:
        result.update(parsed_median_ms=statistics.median(au_values),
                      parsed_p95_ms=sorted(au_values)[int((len(au_values) - 1) * .95)])
    return result


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('ffmpeg')
    parser.add_argument('output')
    parser.add_argument('--repeat', type=int, default=2)
    args = parser.parse_args()
    results = []
    for repeat in range(args.repeat):
        for low, profile in [(False, 'baseline'), (True, 'baseline'), (True, 'high')]:
            result = measure(args.ffmpeg, 2560, 1440, low, profile)
            result['repeat'] = repeat + 1
            results.append(result)
            print(json.dumps(result), flush=True)
            Path(args.output).write_text(json.dumps(results, indent=2) + '\n')
            if result['exit'] or result['input_frames'] != result['output_auds']:
                raise SystemExit('FFmpeg failed or input/output frame count differed')
