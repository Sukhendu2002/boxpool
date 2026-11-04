#!/usr/bin/env python3
"""Stage a version-pinned npm package and Homebrew formula from release assets."""

import argparse
import hashlib
import json
from pathlib import Path
import re
import shutil
from string import Template

ROOT = Path(__file__).resolve().parent.parent
ASSETS = ("boxpool-linux-amd64", "boxpool-linux-arm64", "boxpool-darwin-arm64")


def prepare(version, artifacts, output):
    if not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9_.-]+)?", version):
        raise ValueError("expected a vX.Y.Z release tag")
    checksums = {}
    for line in (artifacts / "checksums.txt").read_text().splitlines():
        fields = line.split()
        if len(fields) != 2 or not re.fullmatch(r"[a-f0-9]{64}", fields[0]):
            raise ValueError("invalid checksum manifest")
        digest, name = fields
        if name in checksums:
            raise ValueError(f"duplicate checksum: {name}")
        checksums[name] = digest
    for name in ASSETS:
        if name not in checksums:
            raise ValueError(f"missing checksum: {name}")
        if hashlib.sha256((artifacts / name).read_bytes()).hexdigest() != checksums[name]:
            raise ValueError(f"checksum mismatch: {name}")
    output.mkdir(parents=True, exist_ok=True)
    package = output / "npm"
    package.mkdir()
    for name in ("bin", "lib"):
        shutil.copytree(ROOT / "npm" / name, package / name)
    for name in ("README.md", "package.json"):
        shutil.copyfile(ROOT / "npm" / name, package / name)
    shutil.copyfile(ROOT / "LICENSE", package / "LICENSE")
    manifest = json.loads((package / "package.json").read_text())
    manifest["version"] = version[1:]
    (package / "package.json").write_text(json.dumps(manifest, indent=2) + "\n")
    (package / "release.json").write_text(json.dumps({
        "version": version, "checksums": {name: checksums[name] for name in ASSETS}
    }, indent=2) + "\n")
    formula = Template((ROOT / "packaging" / "boxpool.rb.tmpl").read_text()).substitute(
        version=version[1:],
        base=f"https://github.com/Sukhendu2002/boxpool/releases/download/{version}",
        **{name.removeprefix("boxpool-").replace("-", "_"): checksums[name] for name in ASSETS},
    )
    (output / "boxpool.rb").write_text(formula)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("version")
    parser.add_argument("artifacts", type=Path)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    prepare(args.version, args.artifacts, args.output)
