#!/usr/bin/env python3
"""Exercise real BOOTSEL/CDC round trips; requires picotool and attached hardware."""

import argparse
import glob
import hashlib
import json
import os
from pathlib import Path
import plistlib
import select
import subprocess
import sys
import termios
import time
import tty


def crc16(data):
    crc = 0xFFFF
    for value in data:
        crc ^= value
        for _ in range(8):
            crc = (crc >> 1) ^ 0xA001 if crc & 1 else crc >> 1
    return crc


def probe(serial, timeout):
    deadline = time.monotonic() + timeout
    fd = None
    buffer = b""
    first_ping = None
    last_send = 0
    replies = []
    transactions = []
    pending = {}
    try:
        while time.monotonic() < deadline:
            if fd is None:
                ports = [serial] if serial else sorted(
                    glob.glob("/dev/cu.usbmodem*") + glob.glob("/dev/ttyACM*"))
                ports = [port for port in ports if os.path.exists(port)]
                if len(ports) > 1:
                    raise RuntimeError("Multiple serial devices; specify --serial")
                if not ports:
                    time.sleep(0.02)
                    continue
                serial = ports[0]
                fd = os.open(serial, os.O_RDWR | os.O_NOCTTY | os.O_NONBLOCK)
                tty.setraw(fd)
                attrs = termios.tcgetattr(fd)
                attrs[4] = attrs[5] = termios.B115200
                termios.tcsetattr(fd, termios.TCSANOW, attrs)
                termios.tcflush(fd, termios.TCIFLUSH)
            if time.monotonic() - last_send >= 0.3:
                # Use the host discovery query, and require the complete reply
                # sequence rather than accepting a capability frame alone.
                os.write(fd, bytes.fromhex("332200000000dffd"))
                last_send = time.monotonic()
                pending = {}
            if select.select([fd], [], [], 0.02)[0]:
                buffer += os.read(fd, 128)
            while len(buffer) >= 8:
                if buffer[0] != 0x33 or crc16(buffer[:6]) != int.from_bytes(buffer[6:8], "big"):
                    buffer = buffer[1:]
                    continue
                response, buffer = buffer[:8], buffer[8:]
                if response[1] == 0x83 and response[2:6] == bytes([1, 0x27, 0, 1]):
                    if first_ping is None:
                        first_ping = time.clock_gettime(time.CLOCK_MONOTONIC)
                    pending = {"capabilities": response.hex()}
                elif response[1] == 0x80 and "capabilities" in pending and response[2:6] == bytes(4):
                    pending["ready"] = response.hex()
                elif response[1] == 0x81 and "ready" in pending:
                    pending["status"] = response.hex()
                elif response[1] == 0x84 and "status" in pending and response[2:6] == bytes([0x22, 0, 0, 0]):
                    replies.append(pending["capabilities"])
                    transactions.append({**pending, "ack": response.hex()})
                    pending = {}
            if len(replies) >= 3:
                return {"serial": serial, "first_ping_monotonic": first_ping,
                        "responses": replies, "transactions": transactions}
        raise RuntimeError("No three complete AnyKVM discovery replies before timeout; serial=%s, opened=%s, valid=%d" % (serial, fd is not None, len(replies)))
    finally:
        if fd is not None:
            os.close(fd)


def wait_usb_a(timeout, absolute):
    """Require the Mac to bind both PIO HID interfaces after re-enumeration."""
    deadline = time.monotonic() + timeout
    expected_mouse_size = 6 if absolute else 4
    while time.monotonic() < deadline:
        result = subprocess.run(["ioreg", "-r", "-c", "IOHIDDevice", "-a"],
                                capture_output=True, check=True, timeout=3)
        interfaces = []

        def visit(nodes):
            for node in nodes:
                if node.get("VendorID") == 0xCAFE and node.get("ProductID") == 0x4001:
                    interfaces.append({"usage_page": node.get("PrimaryUsagePage"),
                                       "usage": node.get("PrimaryUsage"),
                                       "report_size": node.get("MaxInputReportSize")})
                visit(node.get("IORegistryEntryChildren", []))

        visit(plistlib.loads(result.stdout))
        usages = {(i["usage_page"], i["usage"], i["report_size"]) for i in interfaces}
        if (1, 6, 8) in usages and (1, 2, expected_mouse_size) in usages:
            return {"passed": True, "interfaces": interfaces}
        time.sleep(0.1)
    raise RuntimeError("USB-A keyboard/mouse did not enumerate with the expected reports")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--picotool", default="picotool")
    parser.add_argument("--serial", help="Require this control serial port")
    parser.add_argument("--cycles", type=int, default=20)
    parser.add_argument("--reset-method", choices=("vendor", "baud1200"), default="vendor")
    parser.add_argument("--timeout", type=float, default=15)
    parser.add_argument("--bootsel-settle", type=float, default=0)
    parser.add_argument("--load", type=Path, help="Verify-write this UF2 from BOOTSEL before testing")
    parser.add_argument("--require-usb-a", action="store_true",
                        help="Also require both USB-A HID interfaces on this Mac")
    parser.add_argument("--output", type=Path, default=Path("build/rp2350-usb-a/reconnect-results.json"))
    parser.add_argument("--probe", action="store_true", help=argparse.SUPPRESS)
    parser.add_argument("--touch1200", action="store_true", help=argparse.SUPPRESS)
    args = parser.parse_args()
    if args.probe:
        print(json.dumps(probe(args.serial, args.timeout)))
        return
    if args.touch1200:
        fd = os.open(args.serial, os.O_RDWR | os.O_NOCTTY | os.O_NONBLOCK)
        try:
            attrs = termios.tcgetattr(fd)
            attrs[4] = attrs[5] = termios.B1200
            termios.tcsetattr(fd, termios.TCSANOW, attrs)
        finally:
            os.close(fd)
        return
    if args.cycles < 1 or args.timeout <= 0 or args.bootsel_settle < 0:
        parser.error("cycles and timeout must be positive; settle must be nonnegative")
    if args.reset_method == "baud1200" and not args.serial:
        parser.error("baud1200 requires --serial")
    if args.require_usb_a and sys.platform != "darwin":
        parser.error("--require-usb-a currently requires macOS")

    report = {"cycles_requested": args.cycles, "bootsel_settle_seconds": args.bootsel_settle,
              "reset_method": args.reset_method,
              "cycles": [], "passed": False,
              "timing_clock": "clock_gettime(CLOCK_MONOTONIC), shared across processes"}

    def save():
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(report, indent=2) + "\n")

    def tool(*options, timeout=15):
        result = subprocess.run([args.picotool, *map(str, options)], text=True,
                                capture_output=True, timeout=timeout)
        if result.returncode:
            raise RuntimeError("picotool " + " ".join(map(str, options)) + ": "
                               + result.stdout + result.stderr)
        return result.stdout

    def ping():
        # Serial setup itself can block in the OS if the USB device hangs.
        # Isolate it so the hardware test still has a bounded timeout.
        command = [sys.executable, str(Path(__file__).resolve()), "--probe",
                   "--timeout", str(args.timeout)]
        if args.serial:
            command += ["--serial", args.serial]
        result = subprocess.run(command, text=True, capture_output=True,
                                timeout=args.timeout + 3)
        if result.returncode:
            raise RuntimeError(result.stderr.strip() or result.stdout.strip())
        return json.loads(result.stdout)

    def check_usb_a(response):
        if args.require_usb_a:
            status = bytes.fromhex(response["transactions"][-1]["status"])
            response["usb_a"] = wait_usb_a(args.timeout, bool(status[2] & 0x20))
        return response

    try:
        if args.load:
            report["firmware"] = str(args.load.resolve())
            report["firmware_sha256"] = hashlib.sha256(args.load.read_bytes()).hexdigest()
            tool("load", "-v", args.load, timeout=40)
            started = time.clock_gettime(time.CLOCK_MONOTONIC)
            tool("reboot", "-a")
            initial = check_usb_a(ping())
            initial["first_ping_seconds"] = initial.pop("first_ping_monotonic") - started
            report["initial_load"] = initial
            print("Loaded and verified; initial QUERY %.3fs" % initial["first_ping_seconds"], flush=True)
        else:
            report["initial_ping"] = check_usb_a(ping())
        save()
        for index in range(1, args.cycles + 1):
            row = {"cycle": index, "stage": "request_bootsel", "passed": False}
            report["cycles"].append(row)
            save()
            if args.reset_method == "vendor":
                tool("reboot", "-u", "-f")
            else:
                subprocess.run([sys.executable, str(Path(__file__).resolve()),
                                "--touch1200", "--serial", args.serial],
                               check=True, timeout=args.timeout)
            row["stage"] = "wait_bootsel"
            deadline = time.monotonic() + args.timeout
            while True:
                try:
                    row["rom_info"] = tool("info", "-d", timeout=3)
                    break
                except (RuntimeError, subprocess.TimeoutExpired):
                    if time.monotonic() >= deadline:
                        raise RuntimeError("BOOTSEL ROM did not enumerate")
                    time.sleep(0.1)
            time.sleep(args.bootsel_settle)
            row["stage"] = "reboot_application"
            started = time.clock_gettime(time.CLOCK_MONOTONIC)
            tool("reboot", "-a")
            row["stage"] = "wait_cdc_ping"
            response = check_usb_a(ping())
            row["first_ping_seconds"] = response.pop("first_ping_monotonic") - started
            row.update(response)
            row.update(stage="complete", passed=True)
            save()
            print("Cycle %02d/%02d PASS: QUERY %.3fs, %s" %
                  (index, args.cycles, row["first_ping_seconds"], row["serial"]), flush=True)
        report["passed"] = True
        save()
    except Exception as error:
        report["error"] = str(error)
        save()
        print("FAIL: " + str(error), file=sys.stderr, flush=True)
        sys.exit(1)


if __name__ == "__main__":
    main()
