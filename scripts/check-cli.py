#!/usr/bin/env python3
"""M2 gate: exercise a released boxpool CLI identically on macOS and Linux.

Start the daemon separately with `boxpool up`. Run this as your user on a
Mac, or under sudo on Linux when the daemon uses /var/lib/boxpool. Requires
Python 3's standard library only; no script(1), Go toolchain or root on Mac.
Every timed shell gets a new sandbox with the default resource sizes.
"""

import argparse
import errno
import fcntl
import json
import math
import os
from pathlib import Path
import platform
import pty
import re
import select
import shlex
import signal
import statistics
import struct
import subprocess
import sys
import tempfile
import termios
import time
import uuid


ESCAPE = re.compile(r"\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\))")
PROMPT = re.compile(r"root@[^:\n]+:~# $")
TIMING = re.compile(r"(?:^|\n)time to prompt: ([^\s]+)\n")


def duration_ms(value):
    units = {"h": 3600000, "m": 60000, "s": 1000, "ms": 1,
             "us": .001, "µs": .001, "ns": .000001}
    tokens = list(re.finditer(r"([0-9]+(?:\.[0-9]+)?)(ms|us|µs|ns|h|m|s)", value))
    if not tokens or "".join(token[0] for token in tokens) != value:
        raise ValueError("invalid time to prompt: " + repr(value))
    return int(sum(float(token[1]) * units[token[2]] for token in tokens) + .5)


def clean(data):
    return ESCAPE.sub("", data.decode("utf-8", errors="replace")).replace("\r", "")


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


class Gate:
    def __init__(self, binary, backend, log_dir, timeout):
        self.binary = str(Path(binary).resolve())
        self.backend = backend
        self.log_dir = log_dir
        self.timeout = timeout
        self.prefix = "m2-" + uuid.uuid4().hex[:12]
        self.owned = set()
        self.log_dir.mkdir(parents=True, exist_ok=True)

    def command(self, *args, input=None, code=0):
        argv = [self.binary, *args]
        print("$ " + shlex.join(argv), flush=True)
        result = subprocess.run(argv, input=input, capture_output=True, timeout=self.timeout)
        with (self.log_dir / "commands.log").open("ab") as log:
            log.write(("$ " + shlex.join(argv) + "\n").encode())
            log.write(b"stdout: " + repr(result.stdout).encode() + b"\n")
            log.write(b"stderr: " + repr(result.stderr).encode() + b"\n")
            log.write(("exit: " + str(result.returncode) + "\n").encode())
        require(result.returncode == code,
                f"{args}: exit {result.returncode}, expected {code}: "
                + result.stderr.decode(errors="replace"))
        return result

    def listed(self):
        return json.loads(self.command("ls", "--json").stdout)

    def sandbox(self, name):
        matches = [item for item in self.listed() if item["name"] == name]
        require(len(matches) == 1, "expected exactly one sandbox named " + name)
        return matches[0]

    def reserve_name(self, suffix):
        name = self.prefix + "-" + suffix
        require(all(item["name"] != name for item in self.listed()),
                "sandbox already exists; refusing to touch " + name)
        self.owned.add(name)
        return name

    def remove(self, name):
        self.command("rm", "--force", name)
        require(all(item["name"] != name for item in self.listed()), "rm left " + name)
        self.owned.remove(name)

    def cleanup(self):
        failures = []
        for name in list(self.owned):
            try:
                # A failed sh might not have created the sandbox at all.
                if any(item["name"] == name for item in self.listed()):
                    self.remove(name)
                else:
                    self.owned.remove(name)
            except (RuntimeError, subprocess.TimeoutExpired) as error:
                failures.append(str(error))
        require(not failures, "cleanup failed: " + "; ".join(failures))

    def shell(self, name, transcript):
        """Wait for the actual prompt before sending input; reject failed shells."""
        master, slave = pty.openpty()
        process = None
        data = bytearray()
        sample = None
        try:
            fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 24, 80, 0, 0))
            argv = [self.binary, "sh", "--timing", "--name", name]
            print("$ " + shlex.join(argv), flush=True)
            process = subprocess.Popen(argv, stdin=slave, stdout=slave, stderr=slave,
                                       start_new_session=True, close_fds=True)
            os.close(slave)
            slave = None
            deadline = time.monotonic() + self.timeout
            with transcript.open("wb") as log:
                while True:
                    remaining = deadline - time.monotonic()
                    require(remaining > 0, "shell timed out; transcript: " + str(transcript))
                    ready, _, _ = select.select([master], [], [], remaining)
                    require(ready, "shell timed out; transcript: " + str(transcript))
                    try:
                        chunk = os.read(master, 65536)
                    except OSError as error:
                        if error.errno != errno.EIO:  # Linux PTY EOF; macOS returns b''.
                            raise
                        chunk = b""
                    if not chunk:
                        break
                    log.write(chunk)
                    log.flush()
                    data.extend(chunk)
                    if sample is None and PROMPT.search(clean(data)):
                        text = clean(data)
                        timing = TIMING.search(text)
                        require(timing is not None, "prompt arrived without a timing")
                        require(PROMPT.fullmatch(text[timing.end():]) is not None,
                                "first guest output was not the prompt; transcript: " + str(transcript))
                        sample = duration_ms(timing[1])
                        os.write(master, b"exit\n")
                        deadline = time.monotonic() + self.timeout
            code = process.wait(timeout=self.timeout)
            require(code == 0, f"sh exited {code}; transcript: {transcript}")
            require(sample is not None, "sh ended without a prompt; transcript: " + str(transcript))
            return sample
        finally:
            if process is not None and process.poll() is None:
                try:
                    os.killpg(process.pid, signal.SIGTERM)
                except ProcessLookupError:
                    pass
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait()
            os.close(master)
            if slave is not None:
                os.close(slave)

    def cold_boots(self, runs):
        values = []
        for index in range(1, runs + 1):
            name = self.reserve_name(str(index))
            elapsed = self.shell(name, self.log_dir / (name + ".log"))
            sandbox = self.sandbox(name)
            require(sandbox["backend"] == self.backend and sandbox["state"] == "running",
                    "unexpected backend/state: " + repr(sandbox))
            require((sandbox["cpu"], sandbox["mem_mb"], sandbox["disk_gb"]) == (2, 2048, 10),
                    "timed sandbox did not use the default resources")
            self.remove(name)
            values.append(elapsed)
            print(f"run {index}: {elapsed} ms", flush=True)
        return values

    def cli_session(self):
        name = self.reserve_name("session")
        self.shell(name, self.log_dir / "session-new.log")
        original = self.sandbox(name)
        require(original["backend"] == self.backend, "session used the wrong backend")
        self.shell(name, self.log_dir / "session-attach.log")
        require(self.sandbox(name)["id"] == original["id"], "reattach created a different sandbox")
        result = self.command("exec", name, "--", "sh", "-c",
                              "printf gate-out; printf gate-err >&2; exit 7", code=7)
        require(result.stdout == b"gate-out" and result.stderr == b"gate-err",
                "exec did not preserve stdout/stderr or exit status")
        payload = b"gate stdin\n"
        require(self.command("exec", "-i", name, "--", "cat", input=payload).stdout == payload,
                "exec -i did not forward stdin/EOF")
        arch = self.command("exec", name, "--", "uname", "-m").stdout.strip().decode()
        require(arch == {"arm64": "aarch64"}.get(platform.machine(), platform.machine()),
                "unexpected guest architecture: " + arch)
        source = self.log_dir / "copy source.bin"
        destination = self.log_dir / "copy destination.bin"
        source.write_bytes(b"boxpool gate\n\x00" + bytes(range(256)))
        self.command("cp", str(source), name + ":/tmp/")
        self.command("cp", name + ":/tmp/" + source.name, str(destination))
        require(destination.read_bytes() == source.read_bytes(), "cp round trip changed bytes")
        table = self.command("ls").stdout.decode()
        require(name in table and self.backend in table and "running" in table,
                "ls table omitted the running sandbox")
        self.command("rm", name, code=1)  # Refuse to delete a running sandbox without --force.
        self.command("stop", name)
        require(self.sandbox(name)["state"] == "stopped", "stop did not stop the sandbox")
        self.command("exec", name, "--", "true", code=125)
        self.command("rm", name)
        require(all(item["name"] != name for item in self.listed()), "rm left the session sandbox")
        self.owned.remove(name)
        return "PASS: sh/reattach, exec streams/exit/stdin, cp binary round trip, ls, stop, rm"


def positive(value):
    number = int(value)
    if number <= 0:
        raise argparse.ArgumentTypeError("must be positive")
    return number


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--runs", type=positive, default=10)
    parser.add_argument("--backend", choices=["vz", "firecracker"], required=True)
    parser.add_argument("--timeout", type=positive, default=60, help="seconds per command/session phase")
    parser.add_argument("--log-dir", type=Path)
    parser.add_argument("binary", help="path to the installed/released boxpool executable")
    args = parser.parse_args()
    log_dir = args.log_dir or Path(tempfile.mkdtemp(prefix="boxpool-cli-gate-"))
    gate = Gate(args.binary, args.backend, log_dir, args.timeout)
    baseline = gate.listed()
    print("logs: " + str(log_dir), flush=True)
    try:
        version = gate.command("version").stdout.decode().strip()
        samples = gate.cold_boots(args.runs)
        session = gate.cli_session()
        require({item["id"] for item in gate.listed()} == {item["id"] for item in baseline},
                "sandbox list changed beyond this gate's own sandboxes")
        result = {"version": version, "host": platform.platform(), "backend": args.backend,
                  "samples_ms": samples, "median_ms": statistics.median(samples),
                  "p90_ms": sorted(samples)[math.ceil(.9 * len(samples)) - 1],
                  "cli_session": session, "remaining_owned_sandboxes": sorted(gate.owned)}
        (log_dir / "results.json").write_text(json.dumps(result, indent=2) + "\n")
        print(json.dumps(result, indent=2), flush=True)
        require(result["median_ms"] < 2000, "cold boot median did not meet the under-2-second gate")
    finally:
        gate.cleanup()


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, ValueError, OSError, subprocess.TimeoutExpired, KeyboardInterrupt) as error:
        print("FAIL: " + str(error), file=sys.stderr)
        sys.exit(1)
