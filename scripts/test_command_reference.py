"""Guard generation against executing commands whose help has side effects."""

import importlib.util
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location(
    "command_reference", Path(__file__).with_name("generate-command-reference.py"))
reference = importlib.util.module_from_spec(spec)
spec.loader.exec_module(reference)


class CommandReferenceTests(unittest.TestCase):
    def root_help(self, extra=""):
        names = sorted(set(reference.HELP_COMMANDS) | reference.ROOT_ONLY)
        return "Usage: boxpool <command>\n\nCommands:\n" + "\n".join(
            f"  {name}    description" for name in names) + extra

    def test_never_executes_doctor_version_or_installers(self):
        def get_help(binary, args):
            if args == ["--help"]:
                return self.root_help()
            self.assertNotIn(args[0], reference.ROOT_ONLY)
            self.assertEqual(args[-1], "--help")
            return "Usage: boxpool " + " ".join(args[:-1])

        with patch.object(reference, "help_text", side_effect=get_help) as help_mock:
            page = reference.render(Path("boxpool"))
        self.assertIn("## doctor", page)
        self.assertIn("### token create", page)
        self.assertIn("### snapshot ls", page)
        self.assertIn("boxpool up --help", page)
        self.assertEqual(help_mock.call_count, 1 + len(reference.HELP_COMMANDS) + 2)

    def test_unknown_command_requires_safety_review_before_execution(self):
        with patch.object(reference, "help_text", return_value=self.root_help("\n  newcmd    new")) as help_mock:
            with self.assertRaisesRegex(RuntimeError, "review help safety"):
                reference.render(Path("boxpool"))
        self.assertEqual(help_mock.call_count, 1)

    def test_help_accepts_cli_usage_status_but_rejects_runtime_failures(self):
        for status, output, succeeds in [(0, "Usage: x", True), (2, "Usage: x", True),
                                         (125, "Usage: x", True), (1, "Usage: x", False),
                                         (0, "daemon started", False)]:
            with self.subTest(status=status, output=output):
                result = subprocess.CompletedProcess([], status, output)
                with patch.object(reference.subprocess, "run", return_value=result) as run:
                    if succeeds:
                        self.assertEqual(reference.help_text(Path("boxpool"), ["sh", "--help"]), output)
                    else:
                        with self.assertRaises(RuntimeError):
                            reference.help_text(Path("boxpool"), ["sh", "--help"])
                    self.assertEqual(run.call_args.kwargs["stdin"], subprocess.DEVNULL)
