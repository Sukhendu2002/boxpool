#!/usr/bin/env python3
"""M3 gate (#33): resume time, daemon kill -9 recovery and disks across a
host reboot. See docs/benchmarks/m3-gate.md.

Start the daemon with `boxpool up` first. Run as your user on a Mac, under
sudo on Linux. Python 3 standard library only.

  bench-m3.py BOXPOOL resume [--runs 10]
  bench-m3.py BOXPOOL crash
  bench-m3.py BOXPOOL reboot-prepare STATE   # then reboot the host
  bench-m3.py BOXPOOL reboot-check STATE     # after it is back

Each command creates sandboxes named m3-<random>-*, removes only those, and
exits nonzero on the first failed check.
"""

import argparse
import json
import os
import platform
import pty
import statistics
import subprocess
import sys
import time
import uuid

BP = "boxpool"


def run(*args, check=True):
    p = subprocess.run([BP, *args], capture_output=True, text=True, timeout=300)
    if check and p.returncode != 0:
        sys.exit(f"boxpool {' '.join(args)}: exit {p.returncode}: {p.stderr.strip()}")
    return p


def create(name, restart="no", mem=""):
    # sh needs a terminal, even with a command.
    argv = [BP, "sh", "--name", name, "--restart", restart] + (["--mem", mem] if mem else [])
    master, slave = pty.openpty()
    try:
        p = subprocess.run(argv + ["--", "true"], stdin=slave, stdout=slave, stderr=slave, timeout=300)
        os.set_blocking(master, False)
        out = os.read(master, 65536).decode(errors="replace") if p.returncode else ""
    finally:
        os.close(slave)
        os.close(master)
    if p.returncode != 0:
        sys.exit(f"boxpool sh --name {name}: exit {p.returncode}: {out.strip()}")


def sh(name, cmd):
    return run("exec", name, "--", "sh", "-c", cmd).stdout


def state(name):
    for sb in json.loads(run("ls", "--json").stdout):
        if sb["name"] == name:
            return sb["state"]
    return None


def expect(what, got, want):
    if got != want:
        sys.exit(f"FAIL: {what}: got {got!r}, want {want!r}")


def wait(what, fn, timeout_s):
    end = time.monotonic() + timeout_s
    while time.monotonic() < end:
        try:
            if fn():
                return
        except (OSError, ValueError, subprocess.SubprocessError):
            pass
        time.sleep(0.1)
    sys.exit(f"FAIL: {what} not within {timeout_s}s")


def healthy():
    return run("ls", "--json", check=False).returncode == 0


def daemon_pid():
    if platform.system() == "Darwin":
        out = subprocess.run(["launchctl", "print", f"gui/{os.getuid()}/run.boxpool.daemon"],
                             capture_output=True, text=True).stdout
        pids = [line.split("=")[1].strip() for line in out.splitlines() if line.strip().startswith("pid =")]
        return int(pids[0]) if pids else 0
    out = subprocess.run(["systemctl", "show", "-p", "MainPID", "--value", "boxpool"],
                         capture_output=True, text=True).stdout
    return int(out.strip() or 0)


def summary(vals):
    s = sorted(vals)
    return {"median_ms": round(statistics.median(s)), "p90_ms": s[-(-9 * len(s) // 10) - 1]}


def write_tokens(name):
    # /root survives on the disk; /dev/shm only in a VM that kept running.
    tok = uuid.uuid4().hex
    sh(name, f"echo {tok} >/root/m3-token && echo {tok} >/dev/shm/m3-token && sync")
    return tok


def cmd_resume(args, prefix):
    name = f"{prefix}-resume"
    create(name)
    try:
        resume_ms, ready_ms = [], []
        for i in range(args.runs):
            tok = write_tokens(name)
            run("pause", name)
            expect("state after pause", state(name), "paused")
            t0 = time.monotonic()
            run("resume", name)
            t1 = time.monotonic()
            got = sh(name, "cat /dev/shm/m3-token").strip()
            t2 = time.monotonic()
            expect("memory after resume", got, tok)
            resume_ms.append(round((t1 - t0) * 1000))
            ready_ms.append(round((t2 - t0) * 1000))
            print(f"{i + 1} resume {resume_ms[-1]} ms, exec answered {ready_ms[-1]} ms", flush=True)
        print(json.dumps({"resume_ms": resume_ms, "ready_ms": ready_ms,
                          "resume": summary(resume_ms), "ready": summary(ready_ms)}))
    finally:
        run("rm", "--force", name, check=False)


def cmd_crash(args, prefix):
    names = [f"{prefix}-crash-{i}" for i in range(1, 4)]
    try:
        toks = {}
        for n in names:
            # Three default 2 GiB VMs do not fit a 16 GiB Mac's host reserve.
            create(n, restart="always", mem="512m")
            toks[n] = write_tokens(n)
        old = daemon_pid()
        if not old:
            sys.exit("FAIL: no daemon pid from the service manager")
        os.kill(old, 9)
        t0 = time.monotonic()
        wait("daemon restarted", lambda: daemon_pid() not in (0, old) and healthy(), 120)
        recover_ms = round((time.monotonic() - t0) * 1000)
        # Firecracker VMs outlive the daemon and are adopted; vz VMs run in the
        # daemon's process, so they die with it and restart: always boots them.
        kept = platform.system() != "Darwin"
        for n in names:
            expect(f"{n} state", state(n), "running")
            expect(f"{n} disk", sh(n, "cat /root/m3-token").strip(), toks[n])
            expect(f"{n} memory", sh(n, "cat /dev/shm/m3-token 2>/dev/null || echo gone").strip(),
                   toks[n] if kept else "gone")
        print(json.dumps({"old_pid": old, "new_pid": daemon_pid(), "healthy_after_ms": recover_ms,
                          "vms_kept_running": kept, "sandboxes": names, "result": "PASS"}))
    finally:
        for n in names:
            run("rm", "--force", n, check=False)


def cmd_reboot_prepare(args, prefix):
    boxes = {}
    for restart in ("always", "no"):
        n = f"{prefix}-reboot-{restart}"
        create(n, restart)
        boxes[n] = {"restart": restart, "token": write_tokens(n)}
    with open(args.state, "w") as f:
        json.dump({"boxes": boxes, "prepared_at": time.time(), "boot_id": boot_id()}, f)
    print(f"prepared {', '.join(boxes)}; reboot the host, then run reboot-check")


def boot_id():
    if platform.system() == "Darwin":
        return subprocess.run(["sysctl", "-n", "kern.boottime"], capture_output=True, text=True).stdout.strip()
    with open("/proc/sys/kernel/random/boot_id") as f:
        return f.read().strip()


def cmd_reboot_check(args, prefix):
    with open(args.state) as f:
        prep = json.load(f)
    if boot_id() == prep["boot_id"]:
        sys.exit("the host has not rebooted since reboot-prepare; reboot it, then run reboot-check again")
    try:
        wait("daemon healthy", healthy, 300)
        for n, b in prep["boxes"].items():
            if b["restart"] == "always":
                wait(f"{n} running", lambda: state(n) == "running", 300)
            else:
                expect(f"{n} state", state(n), "stopped")
                run("start", n)
            expect(f"{n} disk", sh(n, "cat /root/m3-token").strip(), b["token"])
            expect(f"{n} rebooted", sh(n, "cat /dev/shm/m3-token 2>/dev/null || echo gone").strip(), "gone")
        print(json.dumps({"boxes": list(prep["boxes"]), "result": "PASS"}))
    finally:
        for n in prep["boxes"]:
            run("rm", "--force", n, check=False)
        os.remove(args.state)


def main():
    global BP
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("boxpool")
    sub = ap.add_subparsers(dest="cmd", required=True)
    sub.add_parser("resume").add_argument("--runs", type=int, default=10)
    sub.add_parser("crash")
    for c in ("reboot-prepare", "reboot-check"):
        sub.add_parser(c).add_argument("state")
    args = ap.parse_args()
    BP = args.boxpool
    prefix = f"m3-{uuid.uuid4().hex[:8]}"
    {"resume": cmd_resume, "crash": cmd_crash, "reboot-prepare": cmd_reboot_prepare,
     "reboot-check": cmd_reboot_check}[args.cmd](args, prefix)


if __name__ == "__main__":
    main()
