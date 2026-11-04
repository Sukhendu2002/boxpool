import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("distribution", Path(__file__).with_name("prepare-distribution.py"))
distribution = importlib.util.module_from_spec(spec)
spec.loader.exec_module(distribution)


class DistributionTests(unittest.TestCase):
    def fixture(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        root = Path(directory.name)
        artifacts = root / "artifacts"
        artifacts.mkdir()
        lines = []
        for name in distribution.ASSETS:
            content = name.encode()
            (artifacts / name).write_bytes(content)
            lines.append(f"{hashlib.sha256(content).hexdigest()}  {name}\n")
        (artifacts / "checksums.txt").write_text("".join(lines))
        return artifacts, root / "output"

    def test_packages_pin_same_release_and_digests(self):
        artifacts, output = self.fixture()
        distribution.prepare("v1.2.3-beta.1", artifacts, output)
        package = json.loads((output / "npm/package.json").read_text())
        metadata = json.loads((output / "npm/release.json").read_text())
        self.assertEqual(package["version"], "1.2.3-beta.1")
        self.assertEqual(metadata["version"], "v1.2.3-beta.1")
        self.assertNotIn("postinstall", package["scripts"])
        formula = (output / "boxpool.rb").read_text()
        for name, digest in metadata["checksums"].items():
            self.assertIn(f"/download/v1.2.3-beta.1/{name}", formula)
            self.assertIn(f'sha256 "{digest}"', formula)
        self.assertTrue((output / "npm/LICENSE").exists())

    def test_rejects_missing_duplicate_or_mismatched_digests_before_staging(self):
        for failure in ("missing", "duplicate", "tamper"):
            with self.subTest(failure=failure):
                artifacts, output = self.fixture()
                manifest = artifacts / "checksums.txt"
                lines = manifest.read_text().splitlines(keepends=True)
                if failure == "missing":
                    manifest.write_text("".join(lines[1:]))
                elif failure == "duplicate":
                    manifest.write_text("".join(lines + lines[:1]))
                else:
                    (artifacts / distribution.ASSETS[0]).write_text("tampered")
                with self.assertRaises(ValueError):
                    distribution.prepare("v1.2.3", artifacts, output)
                self.assertFalse(output.exists())
