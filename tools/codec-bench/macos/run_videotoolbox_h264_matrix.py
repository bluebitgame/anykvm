#!/usr/bin/env python3
"""Compare VideoToolbox H.264 profiles, low-latency mode and frame-delay limits."""
import argparse
import json
import subprocess
from pathlib import Path

parser = argparse.ArgumentParser()
parser.add_argument('binary')
parser.add_argument('output')
parser.add_argument('--width', type=int, default=2560)
parser.add_argument('--height', type=int, default=1440)
parser.add_argument('--pattern', choices=['flat', 'moving'], default='moving')
parser.add_argument('--repeat', type=int, default=2)
args = parser.parse_args()
modes = [('realtime', 0, -1, 'baseline'),
         ('low_baseline', 1, -1, 'baseline'),
         ('low_high', 1, -1, 'high'),
         ('delay1', 0, 1, 'baseline'),
         ('low_high_delay1', 1, 1, 'high')]
results = []
for repeat in range(args.repeat):
    for name, low, delay, profile in modes:
        proc = subprocess.run([args.binary, str(args.width), str(args.height), '30', '120',
                               str(low), str(delay), profile, args.pattern],
                              capture_output=True, text=True, timeout=20)
        result = {'repeat': repeat + 1, 'mode': name, 'exit': proc.returncode,
                  'result': json.loads(proc.stdout) if proc.stdout else None,
                  'stderr': proc.stderr}
        results.append(result)
        print(json.dumps(result), flush=True)
        Path(args.output).write_text(json.dumps(results, indent=2) + '\n')
        if proc.returncode:
            raise SystemExit('Native encoder failed; see saved results')
