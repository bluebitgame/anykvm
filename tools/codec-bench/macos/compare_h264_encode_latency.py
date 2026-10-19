#!/usr/bin/env python3
"""Compare native callbacks, FFmpeg bytes, and project-style AUD framing.

Usage: python3 compare_h264_encode_latency.py NATIVE_BINARY FFMPEG OUTPUT_JSON
Requires the executable built from videotoolbox_encode_bench.m. Uses only synthetic frames.
"""
import argparse
import json
import subprocess
from pathlib import Path

from ffmpeg_videotoolbox_encode_bench import measure

parser = argparse.ArgumentParser()
parser.add_argument('native')
parser.add_argument('ffmpeg')
parser.add_argument('output')
args = parser.parse_args()
results = []
for fps in [30, 60]:
    for repeat in [1, 2]:
        proc = subprocess.run([args.native, '2560', '1440', str(fps), '180',
                               '1', '-1', 'baseline', 'moving'],
                              capture_output=True, text=True, timeout=20)
        if proc.returncode:
            raise SystemExit(proc.stderr)
        native = json.loads(proc.stdout)
        ffmpeg = measure(args.ffmpeg, 2560, 1440, True, 'baseline', frames=180, fps=fps)
        if ffmpeg['exit'] or ffmpeg['output_auds'] != 180 or ffmpeg['parsed_frames'] != 179:
            raise SystemExit(str(ffmpeg))
        # The last AU remains pending without another AUD, as in AUParser.
        results.append({'fps': fps, 'repeat': repeat, 'native': native, 'ffmpeg': ffmpeg})
        Path(args.output).write_text(json.dumps(results, indent=2) + '\n')
        print(json.dumps({'fps': fps, 'repeat': repeat,
                          'native_median_ms': native['median_ms'],
                          'ffmpeg_median_ms': ffmpeg['median_ms'],
                          'parsed_median_ms': ffmpeg['parsed_median_ms']}), flush=True)
