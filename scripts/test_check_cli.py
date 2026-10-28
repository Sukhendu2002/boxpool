"""Regression tests for the gate's ability to reject false timing successes."""

import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location("check_cli", Path(__file__).with_name("check-cli.py"))
gate_module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gate_module)


STUB = '''#!/usr/bin/env python3
import json
import os
from pathlib import Path
import sys
import time

state = Path(os.environ["BOXPOOL_GATE_STUB_STATE"])
items = json.loads(state.read_text())
mode = os.environ.get("BOXPOOL_GATE_STUB_MODE", "prompt")
command = sys.argv[1]
if command == "ls":
    print(json.dumps(items))
elif command == "rm":
    if mode == "remove-fails":
        sys.exit(1)
    state.write_text(json.dumps([item for item in items if item["name"] != sys.argv[-1]]))
elif command == "sh":
    name = sys.argv[-1]
    items.append({"id": name, "name": name, "backend": "vz", "state": "running",
                  "cpu": 2, "mem_mb": 2048, "disk_gb": 10})
    state.write_text(json.dumps(items))
    if mode != "no-timing":
        print("time to prompt: 5ms", flush=True)
    if mode == "no-prompt":
        sys.exit(0)
    if mode == "banner-after":
        print("Welcome", flush=True)
    print("\\x1b[?2004hroot@10:~# ", end="", flush=True)
    sys.stdin.readline()
    if mode == "hang":
        time.sleep(60)
    print("logout", flush=True)
    sys.exit(9 if mode == "exit-fails" else 0)
'''


class GateTests(unittest.TestCase):
    def setUp(self):
        printer = patch("builtins.print")
        printer.start()
        self.addCleanup(printer.stop)
        self.temp = tempfile.TemporaryDirectory(prefix="boxpool-gate-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.state = self.root / "state.json"
        self.state.write_text("[]")
        self.binary = self.root / "boxpool"
        self.binary.write_text(STUB)
        self.binary.chmod(0o755)
        env = patch.dict(os.environ, {"BOXPOOL_GATE_STUB_STATE": str(self.state)})
        env.start()
        self.addCleanup(env.stop)
        self.gate = gate_module.Gate(self.binary, "vz", self.root / "logs", 1)

    def test_duration_and_terminal_escapes(self):
        for duration, expected in [("437ms", 437), ("1.2s", 1200), ("1m2.5s", 62500),
                                   ("0s", 0), ("1.5ms", 2), ("100us", 0)]:
            with self.subTest(duration=duration):
                self.assertEqual(gate_module.duration_ms(duration), expected)
        for duration in ["", "1", "1x", "ms", "1m2", "-1s", "1.s"]:
            with self.subTest(duration=duration):
                with self.assertRaises(ValueError):
                    gate_module.duration_ms(duration)
        self.assertEqual(gate_module.clean(b"\x1b]0;title\x07\x1b[?2004hroot@10:~# \r"),
                         "root@10:~# ")

    def test_prompt_is_measured_and_only_owned_sandboxes_are_removed(self):
        existing = [{"id": "keep", "name": "unrelated"}]
        self.state.write_text(json.dumps(existing))
        try:
            self.assertEqual(self.gate.cold_boots(1), [5])
        finally:
            self.gate.cleanup()
        self.assertEqual(json.loads(self.state.read_text()), existing)
        self.assertEqual(self.gate.owned, set())

    def test_timing_never_hides_bad_or_failed_sessions(self):
        for mode in ["no-timing", "no-prompt", "banner-after", "exit-fails", "hang"]:
            with self.subTest(mode=mode), patch.dict(os.environ, {"BOXPOOL_GATE_STUB_MODE": mode}):
                with self.assertRaises(RuntimeError):
                    try:
                        self.gate.cold_boots(1)
                    finally:
                        self.gate.cleanup()
                self.assertEqual(json.loads(self.state.read_text()), [])
                self.assertEqual(self.gate.owned, set())

    def test_failed_removal_fails_the_gate_and_retains_ownership(self):
        with patch.dict(os.environ, {"BOXPOOL_GATE_STUB_MODE": "remove-fails"}):
            with self.assertRaises(RuntimeError):
                self.gate.cold_boots(1)
            self.assertEqual(len(self.gate.owned), 1)
            with self.assertRaisesRegex(RuntimeError, "cleanup failed"):
                self.gate.cleanup()
        self.gate.cleanup()
        self.assertEqual(json.loads(self.state.read_text()), [])


if __name__ == "__main__":
    unittest.main()
