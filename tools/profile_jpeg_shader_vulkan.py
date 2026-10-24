"""Compare JPEG entropy compilation with the production Vulkan HLSL compiler."""

import argparse
from collections import Counter
import hashlib
import json
from pathlib import Path
import platform
import shutil
import statistics
import struct
import subprocess
import time

try:
    import resource
except ImportError:
    resource = None

import build_shaders
import profile_jpeg_shader

ROOT = Path(__file__).resolve().parent.parent
MODES = {"production": build_shaders.GLSLANG_OPTIMIZATION,
         "default_optimization": (), "no_optimization": ("-Od",)}


def spirv_stats(data):
    if len(data) < 20 or len(data) % 4:
        raise ValueError("invalid SPIR-V length")
    words = struct.unpack("<" + str(len(data) // 4) + "I", data)
    if words[0] != 0x07230203:
        raise ValueError("invalid SPIR-V magic")
    counts = Counter()
    pos = 5
    while pos < len(words):
        size, opcode = words[pos] >> 16, words[pos] & 65535
        if size < 1 or pos + size > len(words):
            raise ValueError("invalid SPIR-V instruction")
        counts[opcode] += 1
        pos += size
    return {"spirv_bytes": len(data), "instructions": sum(counts.values()), "blocks": counts[248],
            "loops": counts[246], "conditional_branches": counts[250], "function_calls": counts[57],
            "phi": counts[245], "loads": counts[61], "stores": counts[62], "id_bound": words[3]}


def child_cpu_seconds():
    if resource is None:
        return None
    usage = resource.getrusage(resource.RUSAGE_CHILDREN)
    return usage.ru_utime + usage.ru_stime


def run(output, compiler, repeats=7, timeout=120, diagnostics=False, modes=tuple(MODES)):
    output.mkdir(parents=True, exist_ok=True)
    source = build_shaders.shader_source(build_shaders.JPEG_SOURCE).decode("utf-8")
    cases = [(name, text) for name, text, flags, _ in profile_jpeg_shader.variants(source)
             if flags == profile_jpeg_shader.O3 and (diagnostics or name in ("legacy_o3", "current_o3"))]
    report = {"backend": "glslang", "compiler": compiler, "platform": platform.platform(),
              "compiler_version": subprocess.check_output([compiler, "--version"], text=True),
              "entry": "encodeMCUs", "repeats": repeats, "warmups": 1,
              "timing": "Process wall time; child user+system CPU time when available. Alternating case order.",
              "cases": []}
    for name, text in cases:
        (output / (name + ".hlsl")).write_bytes(text.encode("utf-8"))
    for mode in modes:
        records = {name: {"name": name, "mode": mode, "flags": list(MODES[mode]), "runs": [],
                          "source_sha256": hashlib.sha256(text.encode()).hexdigest()} for name, text in cases}
        for repeat in range(repeats + 1):
            # Each compiler runs in a fresh process; do not time the first pass.
            for name, _ in (cases if repeat % 2 == 0 else list(reversed(cases))):
                binary = output / f"{name}-{mode}.spv"
                command = [compiler, "-D", "-D__spirv__=1", "-V", "--target-env", "vulkan1.1", "-S", "comp",
                           "-e", "encodeMCUs", *MODES[mode], "-o", str(binary), str(output / (name + ".hlsl"))]
                cpu = child_cpu_seconds()
                started = time.perf_counter()
                completed = subprocess.run(command, check=False, capture_output=True, text=True, timeout=timeout)
                elapsed = (time.perf_counter() - started) * 1000
                cpu_elapsed = (child_cpu_seconds() - cpu) * 1000 if cpu is not None else None
                if completed.returncode:
                    raise ValueError(completed.stdout + completed.stderr)
                if repeat:
                    records[name]["runs"].append({"wall_ms": elapsed, "cpu_ms": cpu_elapsed})
        for name, _ in cases:
            record = records[name]
            record.update(spirv_stats((output / f"{name}-{mode}.spv").read_bytes()))
            record["median_ms"] = statistics.median(item["wall_ms"] for item in record["runs"])
            cpu_times = [item["cpu_ms"] for item in record["runs"] if item["cpu_ms"] is not None]
            record["median_cpu_ms"] = statistics.median(cpu_times) if cpu_times else None
            report["cases"].append(record)
            print(f"{mode}/{name}: wall={record['median_ms']:.3f}ms cpu={record['median_cpu_ms']}ms "
                  f"SPIR-V={record['spirv_bytes']} bytes instructions={record['instructions']} "
                  f"blocks={record['blocks']}", flush=True)
        (output / "report.json").write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    print(f"Vulkan compile report: {output / 'report.json'}", flush=True)
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--compiler", help="glslang/glslangValidator executable")
    parser.add_argument("--output", type=Path, default=ROOT / "build/shader-profile-vulkan")
    parser.add_argument("--repeats", type=int, default=7)
    parser.add_argument("--timeout", type=float, default=120)
    parser.add_argument("--diagnostics", action="store_true", help="also compile the diagnostic ablations")
    parser.add_argument("--modes", nargs="+", choices=tuple(MODES), default=list(MODES))
    args = parser.parse_args()
    if args.repeats < 1 or args.timeout <= 0:
        parser.error("--repeats and --timeout must be positive")
    try:
        compiler = args.compiler or shutil.which("glslangValidator") or shutil.which("glslang") or build_shaders.pinned_compiler(False)
        run(args.output.resolve(), compiler, args.repeats, args.timeout, args.diagnostics, args.modes)
    except (OSError, ValueError, subprocess.TimeoutExpired) as exc:
        parser.exit(1, f"Vulkan shader profiling failed: {exc}\n")


if __name__ == "__main__":
    main()
