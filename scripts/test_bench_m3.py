"""Regression tests: the M3 reboot check must not report a false pass."""

import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location("bench_m3", Path(__file__).with_name("bench-m3.py"))
bench = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bench)


# A boxpool that keeps {"name": {"state": ..., "disk": ...}} in a JSON file.
STUB = '''#!/usr/bin/env python3
import json, os, sys
path = os.environ["BENCH_M3_STUB"]
boxes = json.load(open(path))
cmd, args = sys.argv[1], sys.argv[2:]
if cmd == "ls":
    print(json.dumps([{"name": n, "state": b["state"]} for n, b in boxes.items()]))
elif cmd == "start":
    boxes[args[0]]["state"] = "running"
elif cmd == "rm":
    boxes.pop(args[-1], None)
elif cmd == "exec":
    b = boxes[args[0]]
    print(b["disk"] if "/root/" in args[-1] else "gone")
json.dump(boxes, open(path, "w"))
'''


class RebootCheck(unittest.TestCase):
    def setUp(self):
        d = Path(tempfile.mkdtemp())
        self.stub, self.boxes, self.prep = d / "boxpool", d / "boxes.json", d / "prep.json"
        self.stub.write_text(STUB)
        self.stub.chmod(0o755)
        os.environ["BENCH_M3_STUB"] = str(self.boxes)
        bench.BP = str(self.stub)
        self.prep.write_text(json.dumps({"boot_id": "old", "boxes": {
            "a": {"restart": "always", "token": "t1"}, "n": {"restart": "no", "token": "t2"}}}))

    def check(self, a, n, boot="new"):
        self.boxes.write_text(json.dumps({"a": a, "n": n}))
        args = type("A", (), {"state": str(self.prep)})
        with patch.object(bench, "boot_id", return_value=boot), patch.object(bench, "wait", lambda *a: a[1]()):
            bench.cmd_reboot_check(args, "")

    def test_pass_removes_sandboxes(self):
        self.check({"state": "running", "disk": "t1"}, {"state": "stopped", "disk": "t2"})
        self.assertEqual(json.loads(self.boxes.read_text()), {})
        self.assertFalse(self.prep.exists())

    def test_lost_disk_fails(self):
        with self.assertRaises(SystemExit):
            self.check({"state": "running", "disk": "t1"}, {"state": "stopped", "disk": ""})

    def test_no_restart_box_left_running_fails(self):
        with self.assertRaises(SystemExit):
            self.check({"state": "running", "disk": "t1"}, {"state": "running", "disk": "t2"})

    def test_not_rebooted_keeps_sandboxes(self):
        with self.assertRaises(SystemExit):
            self.check({"state": "running", "disk": "t1"}, {"state": "running", "disk": "t2"}, boot="old")
        self.assertEqual(len(json.loads(self.boxes.read_text())), 2)
        self.assertTrue(self.prep.exists())


if __name__ == "__main__":
    unittest.main()
