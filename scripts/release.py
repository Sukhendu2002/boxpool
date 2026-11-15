#!/usr/bin/env python3
"""Version selection and resumable publication for the manual Release workflow."""

import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request

REPO = "Sukhendu2002/boxpool"
TAP = "Sukhendu2002/homebrew-tap"
ROOT = Path(__file__).resolve().parent.parent
BINARIES = ("boxpool-linux-amd64", "boxpool-linux-arm64", "boxpool-darwin-arm64")
VERSION = re.compile(r"v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-(alpha|beta|rc)\.(0|[1-9][0-9]*))?")


def version(value):
    match = VERSION.fullmatch(value)
    if not match:
        raise ValueError("use X.Y.Z or X.Y.Z-alpha.N / -beta.N / -rc.N (optional v prefix)")
    major, minor, patch, kind, number = match.groups()
    rank = {"alpha": 0, "beta": 1, "rc": 2, None: 3}[kind]
    tag = value if value.startswith("v") else "v" + value
    return {"version": tag, "channel": "next" if kind else "latest",
            "formula": "boxpool-beta" if kind else "boxpool",
            "order": (int(major), int(minor), int(patch), rank, int(number or 0))}


def advancing(target, current):
    if current and version(target)["order"] < version(current)["order"]:
        raise ValueError(f"refuse to move a release channel backwards: {current} -> {target}")


def run(*args):
    return subprocess.run(args, check=True, text=True, stdin=subprocess.DEVNULL,
                          stdout=subprocess.PIPE).stdout.strip()


def api(endpoint, *, missing=False, method="GET", **fields):
    args = ["gh", "api", "--method", method, endpoint]
    for name, value in fields.items():
        args += ["-f", f"{name}={value}"]
    result = subprocess.run(args, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if result.returncode:
        if missing and "(HTTP 404)" in result.stderr:
            return None
        raise RuntimeError(f"GitHub API {endpoint}: {result.stderr.strip()}")
    return json.loads(result.stdout) if result.stdout.strip() else None


def registry(package_version=None):
    suffix = "/" + urllib.parse.quote(package_version, safe="") if package_version else ""
    request = urllib.request.Request("https://registry.npmjs.org/boxpool" + suffix,
                                     headers={"Accept": "application/json", "Cache-Control": "no-cache"})
    for attempt in range(4):
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                return json.load(response)
        except urllib.error.HTTPError as error:
            if error.code == 404:
                return None
            if error.code < 500 or attempt == 3:
                raise
        except (urllib.error.URLError, TimeoutError):
            if attempt == 3:
                raise
        time.sleep(2)


def tag_commit(tag):
    ref = api(f"repos/{REPO}/git/ref/tags/{tag}", missing=True)
    if ref is None:
        return None
    obj = ref["object"]
    for _ in range(8):
        if obj["type"] == "commit":
            return obj["sha"]
        if obj["type"] != "tag":
            break
        obj = api(f"repos/{REPO}/git/tags/{obj['sha']}")["object"]
    raise ValueError("release tag does not resolve to a commit")


def source_matches(tag, sha):
    if not re.fullmatch(r"[a-f0-9]{40}", sha):
        raise ValueError("expected a full source commit SHA")
    existing = tag_commit(tag)
    if existing and existing != sha:
        raise ValueError(f"{tag} already belongs to another source commit; choose a new version")
    return existing


def check_channels(info):
    metadata = registry()
    current = (metadata or {}).get("dist-tags", {}).get(info["channel"])
    advancing(info["version"], current)
    latest = api(f"repos/{REPO}/releases/latest", missing=True)
    # A beta older than the recommended stable version is never a new candidate.
    if latest:
        advancing(info["version"], latest["tag_name"])
    formula = api(f"repos/{TAP}/contents/Formula/{info['formula']}.rb", missing=True)
    if formula:
        text = base64.b64decode(formula["content"]).decode()
        found = re.search(r'^  version "([^"]+)"$', text, re.M)
        if not found:
            raise ValueError("cannot identify the current tap formula version")
        advancing(info["version"], found[1])


def plan(value, sha):
    info = version(value)
    source_matches(info["version"], sha)
    check_channels(info)
    return info


def bundle_files(info):
    return [*BINARIES, "checksums.txt", f"{info['formula']}.rb", f"boxpool-{info['version'][1:]}.tgz"]


def manifest(value, sha, directory):
    info = version(value)
    if not re.fullmatch(r"[a-f0-9]{40}", sha):
        raise ValueError("expected a full source commit SHA")
    image = re.search(r'imageVersion = "([^"]+)"', (ROOT / "internal/artifacts/artifacts.go").read_text())[1]
    data = {"schema": 1, "version": info["version"], "commit": sha, "image": image,
            "assets": {name: hashlib.sha256((directory / name).read_bytes()).hexdigest()
                       for name in bundle_files(info)}}
    (directory / "release-manifest.json").write_text(json.dumps(data, indent=2) + "\n")
    return data


def verify_bundle(value, sha, directory):
    info = version(value)
    data = json.loads((directory / "release-manifest.json").read_text())
    if data.get("schema") != 1 or data.get("version") != info["version"] or data.get("commit") != sha:
        raise ValueError("release manifest version/source mismatch")
    if set(data.get("assets", {})) != set(bundle_files(info)):
        raise ValueError("release manifest has unexpected or missing assets")
    for name, digest in data["assets"].items():
        if hashlib.sha256((directory / name).read_bytes()).hexdigest() != digest:
            raise ValueError(f"release bundle checksum mismatch: {name}")
    return data


def github_release(value, sha, directory):
    info = version(value)
    verify_bundle(value, sha, directory)
    check_channels(info)
    if not source_matches(info["version"], sha):
        api(f"repos/{REPO}/git/refs", method="POST", ref="refs/tags/" + info["version"], sha=sha)
    source_matches(info["version"], sha)
    release = api(f"repos/{REPO}/releases/tags/{info['version']}", missing=True)
    if release is None:
        notes = (f"Source: `{sha}`. Guest images remain pinned; see release-manifest.json.\n\n"
                 "The macOS binary is ad-hoc signed, not notarized. "
                 f"[Verification instructions](https://github.com/{REPO}/blob/{info['version']}/docs/development.md#releases).")
        args = ["gh", "release", "create", info["version"], "--repo", REPO, "--verify-tag", "--draft",
                "--latest=false", "--title", info["version"], "--generate-notes", "--notes", notes]
        if info["channel"] == "next":
            args.append("--prerelease")
        run(*args)
        release = api(f"repos/{REPO}/releases/tags/{info['version']}")
    if bool(release["prerelease"]) != (info["channel"] == "next"):
        raise ValueError("existing release has the wrong stable/beta classification")
    expected = ["release-manifest.json", *bundle_files(info)]
    assets = {item["name"]: item for item in api(f"repos/{REPO}/releases/{release['id']}/assets?per_page=100")}
    if set(assets) - set(expected):
        raise ValueError("existing release has unexpected assets; refuse to mix releases")
    incomplete = []
    for name, asset in assets.items():
        if asset.get("state", "uploaded") != "uploaded":
            if asset.get("state") != "starter" or asset.get("size") != 0:
                raise ValueError(f"unrecognized incomplete release asset: {name}")
            incomplete.append(name)
    # Validate ALL existing bytes before uploading anything; never --clobber.
    with tempfile.TemporaryDirectory() as temp:
        for name in assets:
            if name in incomplete:
                continue
            run("gh", "release", "download", info["version"], "--repo", REPO, "--pattern", name, "--dir", temp)
            if (Path(temp) / name).read_bytes() != (directory / name).read_bytes():
                raise ValueError(f"published asset differs: {name}; choose a new version")
    # GitHub can leave zero-byte starter records after a failed upload. Only
    # remove these expected, incomplete records, after checking all real bytes.
    for name in incomplete:
        api(f"repos/{REPO}/releases/assets/{assets.pop(name)['id']}", method="DELETE")
    for name in expected:
        if name not in assets:
            run("gh", "release", "upload", info["version"], "--repo", REPO, str(directory / name))
    if release["draft"]:
        run("gh", "release", "edit", info["version"], "--repo", REPO, "--draft=false", "--latest=false")


def npm_integrity(archive):
    return "sha512-" + base64.b64encode(hashlib.sha512(archive.read_bytes()).digest()).decode()


def npm_release(value, sha, directory):
    info = version(value)
    verify_bundle(value, sha, directory)
    check_channels(info)
    archive = directory / f"boxpool-{info['version'][1:]}.tgz"
    existing = registry(info["version"][1:])
    if existing is None:
        run("npm", "publish", str(archive.resolve()), "--access", "public", "--provenance",
            "--tag", info["channel"], "--ignore-scripts")
    else:
        if existing.get("dist", {}).get("integrity") != npm_integrity(archive):
            raise ValueError("published npm package differs; choose a new version")
        # Usually publish already set this tag. This repairs an interrupted/tag-only recovery.
        current = (registry() or {}).get("dist-tags", {}).get(info["channel"])
        if current != info["version"][1:]:
            run("npm", "dist-tag", "add", f"boxpool@{info['version'][1:]}", info["channel"])
    verify_npm(info, archive)


def verify_npm(info, archive):
    # Allow propagation after our own write, but never retry conflicting bytes
    # or a channel that has already advanced beyond this version.
    for attempt in range(6):
        published = registry(info["version"][1:])
        if published and published.get("dist", {}).get("integrity") != npm_integrity(archive):
            raise ValueError("npm did not return the expected package integrity")
        current = (registry() or {}).get("dist-tags", {}).get(info["channel"])
        advancing(info["version"], current)
        if published and current == info["version"][1:]:
            return
        if attempt < 5:
            time.sleep(2)
    raise ValueError("npm package/channel is not visible at the selected version yet; retry failed jobs")


def tap_release(value, sha, directory, tap):
    info = version(value)
    verify_bundle(value, sha, directory)
    check_channels(info)
    target = tap / "Formula" / f"{info['formula']}.rb"
    content = (directory / target.name).read_bytes()
    if target.exists() and target.read_bytes() == content:
        return
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_bytes(content)
    run("git", "-C", str(tap), "config", "user.name", "github-actions[bot]")
    run("git", "-C", str(tap), "config", "user.email", "41898282+github-actions[bot]@users.noreply.github.com")
    run("git", "-C", str(tap), "add", f"Formula/{target.name}")
    run("git", "-C", str(tap), "commit", "-m", f"build(release): update {info['formula']} to {info['version']}")
    run("git", "-C", str(tap), "push", "origin", "HEAD:main")


def finalize(value, sha, directory):
    info = version(value)
    verify_bundle(value, sha, directory)
    check_channels(info)
    if not source_matches(info["version"], sha):
        raise ValueError("release tag is missing")
    verify_npm(info, directory / f"boxpool-{info['version'][1:]}.tgz")
    published = api(f"repos/{REPO}/releases/tags/{info['version']}")
    if published["draft"] or bool(published["prerelease"]) != (info["channel"] == "next"):
        raise ValueError("GitHub release is not published in the selected channel")
    formula = api(f"repos/{TAP}/contents/Formula/{info['formula']}.rb")
    if base64.b64decode(formula["content"]) != (directory / f"{info['formula']}.rb").read_bytes():
        raise ValueError("tap does not contain the selected formula")
    if info["channel"] == "latest":
        run("gh", "release", "edit", info["version"], "--repo", REPO, "--latest")
    summary = (f"## Released {info['version']}\n\nSource: `{sha}`\n\n"
               f"- [GitHub release](https://github.com/{REPO}/releases/tag/{info['version']})\n"
               f"- npm: `boxpool@{info['channel']}`\n"
               f"- Homebrew: `sukhendu2002/tap/{info['formula']}`\n\n"
               "Public install checks run next. Existing daemons and workspaces are not upgraded.\n")
    with open(os.environ.get("GITHUB_STEP_SUMMARY", os.devnull), "a") as stream:
        stream.write(summary)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("plan", "manifest", "github", "npm", "tap", "finalize"))
    parser.add_argument("version")
    parser.add_argument("sha")
    parser.add_argument("directory", type=Path, nargs="?", default=Path("dist/bundle"))
    parser.add_argument("--tap", type=Path, default=Path("tap"))
    args = parser.parse_args()
    if args.action == "plan":
        info = plan(args.version, args.sha)
        for key in ("version", "channel", "formula"):
            print(f"{key}={info[key]}")
    elif args.action == "manifest":
        manifest(args.version, args.sha, args.directory)
    elif args.action == "tap":
        tap_release(args.version, args.sha, args.directory, args.tap)
    else:
        {"github": github_release, "npm": npm_release, "finalize": finalize}[args.action](
            args.version, args.sha, args.directory)


if __name__ == "__main__":
    main()
