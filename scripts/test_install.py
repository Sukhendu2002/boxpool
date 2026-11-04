"""Exercise the real installer with offline release downloads and host detection."""

import hashlib
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent.parent
INSTALLER = ROOT / "scripts" / "install.sh"
BINARY = b"#!/bin/sh\nprintf 'boxpool fixture\\n'\n"


class InstallTests(unittest.TestCase):
    def run_installer(self, host="Linux", arch="x86_64", version="v1.2.3", failure="", manifest=None, installed=False):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name)
        tools = root / "tools"
        tools.mkdir()
        (tools / "uname").write_text('#!/bin/sh\ncase "$1" in -s) echo "$TEST_OS";; -m) echo "$TEST_ARCH";; esac\n')
        (tools / "uname").chmod(0o755)
        # curl fixture respects the output/header destinations and records the
        # exact URLs. It never calls a network service.
        (tools / "curl").write_text('''#!/bin/sh
set -eu
headers= out= url= effective=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --dump-header) headers=$2; shift;;
    --output) out=$2; shift;;
    -w) effective=yes; shift;;
    https:*) url=$1;;
  esac
  shift
done
printf '%s\\n' "$url" >> "$TEST_ROOT/urls"
[ "$TEST_FAILURE" != download ] || exit 22
printf 'location: https://github.com/Sukhendu2002/boxpool/releases/download/v1.2.3/checksums.txt\\r\\n' > "$headers"
case "$url" in
  */checksums.txt) cp "$TEST_ROOT/manifest" "$out";;
  *) cp "$TEST_ROOT/binary" "$out";;
esac
if [ -n "$effective" ]; then printf 'https://release-assets.githubusercontent.com/fixture'; fi
''')
        (tools / "curl").chmod(0o755)
        (root / "binary").write_bytes(BINARY if failure != "tamper" else b"tampered")
        if manifest is None:
            digest = hashlib.sha256(BINARY).hexdigest()
            manifest = "".join(f"{digest}  boxpool-{platform}\n" for platform in ("linux-amd64", "linux-arm64", "darwin-arm64"))
        (root / "manifest").write_text(manifest)
        destination = root / "install with spaces"
        if installed:
            destination.mkdir()
            (destination / "boxpool").write_text("previous binary")
        env = dict(os.environ, PATH=str(tools) + os.pathsep + os.environ["PATH"],
                   TEST_OS=host, TEST_ARCH=arch, TEST_FAILURE=failure, TEST_ROOT=str(root),
                   BOXPOOL_VERSION=version, BOXPOOL_INSTALL_DIR=str(destination), TMPDIR=str(root))
        result = subprocess.run(["sh", str(INSTALLER)], env=env, capture_output=True, text=True)
        self.assertFalse(list(root.glob("tmp.*")), "installer left its download directory")
        self.assertFalse(list(destination.glob(".boxpool.*")), "installer left a staged binary")
        return result, root, destination

    def test_supported_hosts_and_version_pinning(self):
        for host, arch, asset in (("Linux", "x86_64", "linux-amd64"), ("Linux", "aarch64", "linux-arm64"), ("Darwin", "arm64", "darwin-arm64")):
            with self.subTest(host=host, arch=arch):
                result, root, destination = self.run_installer(host, arch)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual((destination / "boxpool").read_bytes(), BINARY)
                self.assertTrue(os.access(destination / "boxpool", os.X_OK))
                self.assertIn(f"/download/v1.2.3/boxpool-{asset}", (root / "urls").read_text())

    def test_latest_resolves_once(self):
        result, root, _ = self.run_installer(version="latest")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((root / "urls").read_text().splitlines(), [
            "https://github.com/Sukhendu2002/boxpool/releases/latest/download/checksums.txt",
            "https://github.com/Sukhendu2002/boxpool/releases/download/v1.2.3/boxpool-linux-amd64",
        ])

    def test_tampering_and_download_failure_preserve_previous_install(self):
        for failure in ("tamper", "download"):
            with self.subTest(failure=failure):
                result, _, destination = self.run_installer(failure=failure, installed=True)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual((destination / "boxpool").read_text(), "previous binary")
                if failure == "tamper":
                    self.assertIn("checksum mismatch", result.stderr)

    def test_reject_invalid_manifests(self):
        digest = hashlib.sha256(BINARY).hexdigest()
        for manifest in ("", "bad  boxpool-linux-amd64\n", f"{digest}  other\n", f"{digest}  boxpool-linux-amd64\n" * 2):
            with self.subTest(manifest=manifest):
                result, _, destination = self.run_installer(manifest=manifest)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(destination.exists())

    def test_unsupported_host_and_invalid_version_do_not_download(self):
        for options in ({"host": "Darwin", "arch": "x86_64"}, {"host": "Windows"}, {"version": "../../main"}):
            with self.subTest(options=options):
                result, root, destination = self.run_installer(**options)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse((root / "urls").exists())
                self.assertFalse(destination.exists())


if __name__ == "__main__":
    unittest.main()
