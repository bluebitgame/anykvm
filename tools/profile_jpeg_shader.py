"""Isolate slow encodeMCUs compilation with the application's system D3DCompiler.

Run on Windows without a GPU or build toolchain. Diagnostic variants are compiled
only and must never be used for rendering. --prepare-only also works on macOS/Linux.
"""

import argparse
import ctypes
import hashlib
import json
from pathlib import Path
import platform
import re
import statistics
import subprocess
import sys
import time

import build_shaders

ROOT = Path(__file__).resolve().parent.parent
O3 = 0x8000 | 0x0800  # D3DCOMPILE_OPTIMIZATION_LEVEL3 | ENABLE_STRICTNESS
O1 = 0x0800  # D3DCOMPILE_OPTIMIZATION_LEVEL1 is zero; retain strictness.
SKIP_OPT = 0x0004 | 0x0800
PREFER_FLOW = 0x0400
LEGACY = ROOT / "internal/jpegcodec/testdata/entropy_writer_legacy.hlsl"


def function_span(source, name):
    clean = re.sub(r"//[^\n]*|/\*.*?\*/", lambda match: "".join(
        "\n" if char == "\n" else " " for char in match.group()), source, flags=re.S)
    match = re.search(r"(?m)^\w+\s+" + re.escape(name) + r"\s*\([^)]*\)\s*\{", clean)
    if not match:
        raise ValueError(f"HLSL function not found: {name}")
    first = clean.index("{", match.start())
    depth = 0
    for index in range(first, len(clean)):
        depth += (clean[index] == "{") - (clean[index] == "}")
        if depth == 0:
            return match.start(), index + 1
    raise ValueError(f"unterminated HLSL function: {name}")


def function(source, name):
    first, last = function_span(source, name)
    return source[first:last]


def replace_function(source, name, replacement):
    first, last = function_span(source, name)
    return source[:first] + replacement + source[last:]


def variants(source):
    runtime = "#define ANYKVM_RUNTIME_COMPILE 1\n" + source
    legacy = runtime
    previous = LEGACY.read_text(encoding="utf-8")
    for name in ("writePackedBytes", "appendBits", "writeBits"):
        legacy = replace_function(legacy, name, function(previous, name))
    no_writer = replace_function(legacy, "writeBits", """void writeBits(inout EntropyWriter writer, uint value, uint count) {
    writer.pendingWord ^= value;
    writer.byteCount += count;
    writer.wordBytes = 1;
}""")
    append = function(legacy, "appendBits").split("    uint inverted = ~packed;", 1)[0]
    no_stuffing = replace_function(legacy, "appendBits", append + "    writePackedBytes(writer, packed, bytes);\n}")
    no_combined = replace_function(legacy, "encodeBlock", function(legacy, "encodeBlock").replace(
        "if (uint(coefficient + 31) < 63)", "if (false)", 1))
    no_solid = replace_function(legacy, "encodeMCUs", function(legacy, "encodeMCUs").replace(
        "if (tileState.z != 0)", "if (false)", 1))
    dc_only = replace_function(legacy, "encodeBlock", """bool encodeBlock(inout EntropyWriter writer, uint blockOffset, uint dcTable, inout int previousDC) {
    int dc = loadCoefficient(blockOffset, 0), difference = dc - previousDC;
    previousDC = dc;
    uint count = magnitudeBitCount(difference);
    writeCoefficient(writer, dcTable, count, difference, count);
    writeSymbol(writer, dcTable + 1, 0);
    return true;
}""")
    return (
        ("legacy_o3", legacy, O3, "Previous production writer; baseline for the reported 20s"),
        ("current_o3", runtime, O3, "Single append site at the previous runtime optimization level"),
        ("current_offline_o3", source, O3, "Release offline policy: O3 without entropy fastopt"),
        ("current_runtime_o1", runtime, O1, "Cross-build runtime policy: O1 with entropy fastopt"),
        ("legacy_skip_optimization", legacy, SKIP_OPT, "Separate optimization cost from parsing/code generation"),
        ("legacy_prefer_flow", legacy, O3 | PREFER_FLOW, "Test branch flattening/flow-control heuristics"),
        ("legacy_no_writer", no_writer, O3, "Remove bit assembly/stuffing while retaining symbol generation"),
        ("legacy_no_stuffing", no_stuffing, O3, "Remove the FF-stuffing branch/loop"),
        ("legacy_no_combined_ac", no_combined, O3, "Use only generic AC Huffman lookup, without the small-AC table"),
        ("legacy_no_solid", no_solid, O3, "Remove solid-MCU descriptor handling"),
        ("legacy_dc_only", dc_only, O3, "Remove sparse AC scanning, ZRL and AC emission"),
    )


def expanded_writer_calls(source):
    # Syntactic call-tree expansion, not measured DXBC instructions. Useful for
    # verifying that a source change removes duplicate inlining opportunities.
    names = set(re.findall(r"(?m)^\w+\s+(\w+)\s*\([^)]*\)\s*\{", source))
    counts = {"writeBits": 0, "appendBits": 0, "writeByte": 0, "writePackedBytes": 0}

    def visit(name, multiplier, stack):
        if name in stack:
            raise ValueError("recursive HLSL call graph")
        if name in counts:
            counts[name] += multiplier
        body = function(source, name).split("{", 1)[1]
        for called in names:
            count = len(re.findall(r"\b" + re.escape(called) + r"\s*\(", body))
            if count:
                visit(called, multiplier * count, (*stack, name))
    visit("encodeMCUs", 1, ())
    return counts


def assembly_stats(assembly):
    slots = re.search(r"approximately (\d+) instruction slots", assembly)
    temps = re.search(r"(?m)^\s*dcl_temps\s+(\d+)", assembly)
    return {"instruction_slots": int(slots.group(1)) if slots else None,
            "temporary_registers": int(temps.group(1)) if temps else None,
            "loops": len(re.findall(r"(?m)^\s*loop\b", assembly)),
            "branches": len(re.findall(r"(?m)^\s*if(?:_\w+)?\b", assembly))}


def compile_windows(source_path, flags, destination):
    # Use the same system DLL, entry, profile and source name as the application,
    # rather than SDK FXC's potentially different bundled D3DCompiler version.
    kernel = ctypes.WinDLL("kernel32", use_last_error=True)
    kernel.GetSystemDirectoryW.argtypes = [ctypes.c_wchar_p, ctypes.c_uint]
    directory = ctypes.create_unicode_buffer(32768)
    if not kernel.GetSystemDirectoryW(directory, len(directory)):
        raise ctypes.WinError(ctypes.get_last_error())
    compiler_path = Path(directory.value) / "d3dcompiler_47.dll"
    library = ctypes.WinDLL(str(compiler_path), winmode=0x0800)
    pointer = ctypes.c_void_p
    compile_shader = library.D3DCompile
    compile_shader.argtypes = [pointer, ctypes.c_size_t, ctypes.c_char_p, pointer, pointer,
                              ctypes.c_char_p, ctypes.c_char_p, ctypes.c_uint, ctypes.c_uint,
                              ctypes.POINTER(pointer), ctypes.POINTER(pointer)]
    compile_shader.restype = ctypes.c_long

    def read_blob(blob):
        table = ctypes.cast(blob, ctypes.POINTER(ctypes.POINTER(pointer))).contents
        address = ctypes.WINFUNCTYPE(pointer, pointer)(table[3])(blob)
        size = ctypes.WINFUNCTYPE(ctypes.c_size_t, pointer)(table[4])(blob)
        return ctypes.string_at(address, size)

    def release(blob):
        if blob:
            table = ctypes.cast(blob, ctypes.POINTER(ctypes.POINTER(pointer))).contents
            ctypes.WINFUNCTYPE(ctypes.c_ulong, pointer)(table[2])(blob)

    data = source_path.read_bytes()
    storage = ctypes.create_string_buffer(data)
    code, messages, listing = pointer(), pointer(), pointer()
    try:
        started = time.perf_counter()
        hr = compile_shader(storage, len(data), b"anykvm-jpeg", None, None, b"encodeMCUs", b"cs_5_0",
                            flags, 0, ctypes.byref(code), ctypes.byref(messages))
        elapsed = (time.perf_counter() - started) * 1000
        result = {"status": "ok" if hr >= 0 else "compile_error", "compile_ms": elapsed,
                  "hresult": f"0x{hr & 0xffffffff:08x}", "compiler": str(compiler_path),
                  "diagnostics": read_blob(messages).decode("utf-8", errors="replace") if messages else ""}
        if hr >= 0 and code:
            bytecode = read_blob(code)
            destination.with_suffix(".dxbc").write_bytes(bytecode)
            result["dxbc_bytes"] = len(bytecode)
            disassemble = library.D3DDisassemble
            disassemble.argtypes = [pointer, ctypes.c_size_t, ctypes.c_uint, ctypes.c_char_p, ctypes.POINTER(pointer)]
            disassemble.restype = ctypes.c_long
            bytecode_storage = ctypes.create_string_buffer(bytecode)
            if disassemble(bytecode_storage, len(bytecode), 0, None, ctypes.byref(listing)) >= 0:
                assembly = read_blob(listing).decode("utf-8", errors="replace")
                destination.with_suffix(".asm").write_text(assembly, encoding="utf-8")
                result.update(assembly_stats(assembly))
        # This records the exact DLL bits without requiring version-resource
        # APIs, and makes SDK/system/compiler-build differences visible.
        result["compiler_sha256"] = hashlib.sha256(compiler_path.read_bytes()).hexdigest()
        return result
    finally:
        release(listing)
        release(messages)
        release(code)


def run(output, repeats=1, timeout=120, prepare_only=False):
    if not prepare_only and platform.system() != "Windows":
        raise ValueError("Run on Windows for actual system FXC timings; use --prepare-only to inspect variants elsewhere")
    output.mkdir(parents=True, exist_ok=True)
    source = build_shaders.shader_source(build_shaders.JPEG_SOURCE).decode("utf-8")
    report = {"entry": "encodeMCUs", "profile": "cs_5_0", "platform": platform.platform(),
              "python_bits": ctypes.sizeof(ctypes.c_void_p) * 8, "cases": []}
    failed = False
    for name, variant, flags, purpose in variants(source):
        hlsl = output / (name + ".hlsl")
        hlsl.write_bytes(variant.encode("utf-8"))
        case = {"name": name, "purpose": purpose, "flags": f"0x{flags:08x}",
                "source_sha256": hashlib.sha256(hlsl.read_bytes()).hexdigest(),
                "expanded_calls": expanded_writer_calls(variant), "runs": []}
        report["cases"].append(case)
        if not prepare_only:
            for repeat in range(repeats):
                destination = output / f"{name}_{repeat + 1}"
                print(f"{name} [{repeat + 1}/{repeats}]: compile started", flush=True)
                completed = None
                try:
                    completed = subprocess.run([sys.executable, str(Path(__file__).resolve()), "--worker", str(hlsl),
                                                "--flags", str(flags), "--output", str(destination)],
                                               check=False, capture_output=True, text=True, timeout=timeout)
                    result = json.loads(destination.with_suffix(".json").read_text(encoding="utf-8"))
                    if completed.returncode and result["status"] == "ok":
                        result["status"] = "worker_error"
                except subprocess.TimeoutExpired:
                    result = {"status": "timeout", "timeout_seconds": timeout}
                except (OSError, ValueError) as exc:
                    result = {"status": "worker_error", "error": str(exc),
                              "stderr": completed.stderr if completed is not None else ""}
                case["runs"].append(result)
                print(f"{name}: {result['status']} compile_ms={result.get('compile_ms')} "
                      f"instructions={result.get('instruction_slots')} temps={result.get('temporary_registers')}", flush=True)
                if result["status"] != "ok":
                    failed = True
            times = [item["compile_ms"] for item in case["runs"] if item["status"] == "ok"]
            if times:
                case["median_compile_ms"] = statistics.median(times)
        (output / "report.json").write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    print(f"Shader compile report: {output / 'report.json'}", flush=True)
    return int(failed)


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--output", type=Path, default=ROOT / "build/shader-profile")
    parser.add_argument("--repeats", type=int, default=1)
    parser.add_argument("--timeout", type=float, default=120)
    parser.add_argument("--prepare-only", action="store_true")
    parser.add_argument("--worker", type=Path, help=argparse.SUPPRESS)
    parser.add_argument("--flags", type=int, default=O3, help=argparse.SUPPRESS)
    args = parser.parse_args()
    if args.repeats < 1 or args.timeout <= 0:
        parser.error("--repeats and --timeout must be positive")
    if args.worker:
        try:
            result = compile_windows(args.worker, args.flags, args.output)
        except Exception as exc:
            result = {"status": "worker_error", "error": str(exc)}
        args.output.with_suffix(".json").write_text(json.dumps(result, indent=2) + "\n", encoding="utf-8")
        return int(result["status"] != "ok")
    try:
        return run(args.output.resolve(), args.repeats, args.timeout, args.prepare_only)
    except (OSError, ValueError) as exc:
        parser.exit(1, f"Shader profiling failed: {exc}\n")


if __name__ == "__main__":
    sys.exit(main())
