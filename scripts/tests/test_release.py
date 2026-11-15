import base64
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("release", Path(__file__).resolve().parents[1] / "release.py")
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)
SHA = "a" * 40


class ReleaseTests(unittest.TestCase):
    def bundle(self, value="v1.2.3"):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        directory = Path(temp.name)
        for name in release.bundle_files(release.version(value)):
            (directory / name).write_bytes(name.encode())
        release.manifest(value, SHA, directory)
        return directory

    def test_versions_and_channels_are_strict_and_numeric(self):
        self.assertEqual(release.version("1.2.3")["version"], "v1.2.3")
        for kind in ("alpha", "beta", "rc"):
            info = release.version(f"v1.2.3-{kind}.1")
            self.assertEqual((info["channel"], info["formula"]), ("next", "boxpool-beta"))
        self.assertEqual(release.version("v1.2.3")["formula"], "boxpool")
        for value in ("1.2", "1.2.3;whoami", "1.2.3\n", "01.2.3", "1.2.3-beta.01", "1.2.3-image.1", "1.2.3-dev.abc", "1.2.3+metadata", "1.2.3-rc"):
            with self.subTest(value=value), self.assertRaises(ValueError):
                release.version(value)
        for old, new in (("v1.9.0", "v1.10.0"), ("v1.2.3-beta.2", "v1.2.3-beta.10"), ("v1.2.3-rc.2", "v1.2.3")):
            release.advancing(new, old)
            with self.assertRaises(ValueError):
                release.advancing(old, new)
        release.advancing("1.2.3", "v1.2.3")

    def test_manifest_binds_exact_source_version_and_bytes(self):
        directory = self.bundle()
        self.assertEqual(release.verify_bundle("1.2.3", SHA, directory)["commit"], SHA)
        for value, sha in (("v1.2.4", SHA), ("v1.2.3", "b" * 40)):
            with self.assertRaises(ValueError):
                release.verify_bundle(value, sha, directory)
        (directory / release.BINARIES[0]).write_text("tampered")
        with self.assertRaisesRegex(ValueError, "checksum mismatch"):
            release.verify_bundle("1.2.3", SHA, directory)

    def test_manifest_rejects_unexpected_asset_paths(self):
        directory = self.bundle()
        path = directory / "release-manifest.json"
        data = json.loads(path.read_text())
        data["assets"]["../escape"] = "a" * 64
        path.write_text(json.dumps(data))
        with self.assertRaisesRegex(ValueError, "unexpected"):
            release.verify_bundle("1.2.3", SHA, directory)

    def test_source_binding_peels_annotated_tags_and_rejects_another_commit(self):
        with patch.object(release, "api", side_effect=[{"object": {"type": "tag", "sha": "tag"}}, {"object": {"type": "commit", "sha": SHA}}]):
            self.assertEqual(release.source_matches("v1.2.3", SHA), SHA)
        with patch.object(release, "tag_commit", return_value="b" * 40), self.assertRaisesRegex(ValueError, "another source"):
            release.source_matches("v1.2.3", SHA)

    def test_preflight_checks_npm_github_and_tap_without_moving_channels(self):
        info = release.version("v1.2.3-beta.1")
        formula = base64.b64encode(b'  version "1.2.3-beta.2"\n').decode()
        with patch.object(release, "registry", return_value={"dist-tags": {"next": "1.2.3-beta.0"}}), patch.object(release, "api", side_effect=[{"tag_name": "v1.2.2"}, {"content": formula}]), self.assertRaisesRegex(ValueError, "backwards"):
            release.check_channels(info)
        with patch.object(release, "registry", return_value={"dist-tags": {"latest": "1.3.0"}}), self.assertRaisesRegex(ValueError, "backwards"):
            release.check_channels(release.version("1.2.3"))

    def github_fixture(self, directory, *, conflict=False, missing=()):
        expected = ["release-manifest.json", *release.bundle_files(release.version("1.2.3"))]
        assets = [{"name": name, "id": index} for index, name in enumerate(expected) if name not in missing]
        def api(endpoint, **kwargs):
            if "/assets?" in endpoint:
                return assets
            return {"id": 1, "draft": False, "prerelease": False}
        def run(*args):
            if args[:3] == ("gh", "release", "download"):
                name, target = args[args.index("--pattern") + 1], args[args.index("--dir") + 1]
                data = (directory / name).read_bytes()
                if conflict and name == expected[-1]:
                    data = b"different bytes"
                (Path(target) / name).write_bytes(data)
            return ""
        return api, run

    def test_github_retry_skips_identical_assets_and_only_uploads_missing(self):
        directory = self.bundle()
        api, runner = self.github_fixture(directory, missing=("checksums.txt",))
        with patch.object(release, "check_channels"), patch.object(release, "source_matches", return_value=SHA), patch.object(release, "api", side_effect=api), patch.object(release, "run", side_effect=runner) as commands:
            release.github_release("v1.2.3", SHA, directory)
            uploads = [c.args for c in commands.call_args_list if c.args[:3] == ("gh", "release", "upload")]
        self.assertEqual(len(uploads), 1)
        self.assertEqual(uploads[0][-1], str(directory / "checksums.txt"))

    def test_github_conflict_is_detected_before_any_upload(self):
        directory = self.bundle()
        api, runner = self.github_fixture(directory, conflict=True, missing=("checksums.txt",))
        with patch.object(release, "check_channels"), patch.object(release, "source_matches", return_value=SHA), patch.object(release, "api", side_effect=api), patch.object(release, "run", side_effect=runner) as commands:
            with self.assertRaisesRegex(ValueError, "published asset differs"):
                release.github_release("v1.2.3", SHA, directory)
            self.assertFalse(any(c.args[:3] == ("gh", "release", "upload") for c in commands.call_args_list))

    def test_npm_retry_verifies_integrity_without_republishing(self):
        directory = self.bundle()
        metadata = {"dist": {"integrity": release.npm_integrity(directory / "boxpool-1.2.3.tgz")}}
        tags = {"dist-tags": {"latest": "1.2.3"}}
        with patch.object(release, "check_channels"), patch.object(release, "registry", side_effect=lambda value=None: metadata if value else tags), patch.object(release, "run") as commands:
            release.npm_release("1.2.3", SHA, directory)
            commands.assert_not_called()
        with patch.object(release, "check_channels"), patch.object(release, "registry", return_value={"dist": {"integrity": "wrong"}}), patch.object(release, "run") as commands:
            with self.assertRaisesRegex(ValueError, "npm package differs"):
                release.npm_release("1.2.3", SHA, directory)
            commands.assert_not_called()

    def test_new_npm_publication_selects_beta_without_changing_latest(self):
        directory = self.bundle("v1.2.3-beta.1")
        metadata = {"dist": {"integrity": release.npm_integrity(directory / "boxpool-1.2.3-beta.1.tgz")}}
        tags = {"dist-tags": {"next": "1.2.3-beta.1", "latest": "1.2.2"}}
        with patch.object(release, "check_channels"), patch.object(release, "registry", side_effect=[None, metadata, tags]), patch.object(release, "run") as command:
            release.npm_release("1.2.3-beta.1", SHA, directory)
            args = command.call_args.args
            self.assertEqual(args[:2], ("npm", "publish"))
            self.assertEqual(args[args.index("--tag") + 1], "next")

    def test_matching_tap_is_a_noop_and_beta_does_not_change_stable(self):
        directory = self.bundle("v1.2.3-beta.1")
        tap = directory / "tap"
        (tap / "Formula").mkdir(parents=True)
        stable = tap / "Formula/boxpool.rb"
        stable.write_text("leave stable alone")
        with patch.object(release, "check_channels"), patch.object(release, "run") as commands:
            release.tap_release("1.2.3-beta.1", SHA, directory, tap)
            self.assertTrue(any(c.args[-2:] == ("origin", "HEAD:main") for c in commands.call_args_list))
            commands.reset_mock()
            release.tap_release("1.2.3-beta.1", SHA, directory, tap)
            commands.assert_not_called()
        self.assertEqual(stable.read_text(), "leave stable alone")

    def test_api_never_treats_auth_or_network_errors_as_missing_releases(self):
        for error, missing in (("gh: Not Found (HTTP 404)", True), ("gh: forbidden (HTTP 403)", False), ("connection reset", False)):
            result = subprocess.CompletedProcess([], 1, "", error)
            with patch.object(release.subprocess, "run", return_value=result):
                if missing:
                    self.assertIsNone(release.api("fixture", missing=True))
                else:
                    with self.assertRaises(RuntimeError):
                        release.api("fixture", missing=True)

    def test_zero_byte_failed_upload_can_resume_but_real_assets_are_never_deleted(self):
        directory = self.bundle()
        api, runner = self.github_fixture(directory, missing=("checksums.txt",))
        def fake(endpoint, **kwargs):
            result = api(endpoint, **kwargs)
            if "/assets?" in endpoint:
                result.append({"name": "checksums.txt", "id": 99, "state": "starter", "size": 0})
            return result
        with patch.object(release, "check_channels"), patch.object(release, "source_matches", return_value=SHA), patch.object(release, "api", side_effect=fake) as service, patch.object(release, "run", side_effect=runner) as commands:
            release.github_release("1.2.3", SHA, directory)
        deletes = [c for c in service.call_args_list if c.kwargs.get("method") == "DELETE"]
        self.assertEqual([c.args[0] for c in deletes], [f"repos/{release.REPO}/releases/assets/99"])
        self.assertTrue(any(c.args[-1] == str(directory / "checksums.txt") for c in commands.call_args_list))

    def test_new_release_uploads_every_asset_before_publishing_the_draft(self):
        directory = self.bundle()
        state = {"exists": False}
        def api(endpoint, **kwargs):
            if endpoint.endswith("/git/refs"):
                self.assertEqual(kwargs["sha"], SHA)
                return None
            if "/assets?" in endpoint:
                return []
            if "/releases/tags/" in endpoint:
                if not kwargs.get("missing"):
                    raise RuntimeError("published tag endpoint returns HTTP 404 for drafts")
                return None
            if "/releases?per_page=" in endpoint:
                return [{"id": 1, "tag_name": "v1.2.3", "draft": True, "prerelease": False}] if state["exists"] else []
            raise AssertionError(endpoint)
        def runner(*args):
            if args[:3] == ("gh", "release", "create"):
                state["exists"] = True
            return ""
        with patch.object(release, "check_channels"), patch.object(release, "source_matches", side_effect=[None, SHA]), patch.object(release, "api", side_effect=api), patch.object(release, "run", side_effect=runner) as commands:
            release.github_release("1.2.3", SHA, directory)
        actions = [c.args[2] for c in commands.call_args_list]
        self.assertEqual(actions[0], "create")
        self.assertEqual(actions[-1], "edit")
        self.assertEqual(actions.count("upload"), len(release.bundle_files(release.version("1.2.3"))) + 1)
        self.assertIn("--latest=false", commands.call_args_list[-1].args)

    def test_npm_publication_propagation_retries_without_republishing(self):
        directory = self.bundle()
        info = release.version("1.2.3")
        archive = directory / "boxpool-1.2.3.tgz"
        package = {"dist": {"integrity": release.npm_integrity(archive)}}
        with patch.object(release, "registry", side_effect=[None, {"dist-tags": {"latest": "1.2.2"}}, package, {"dist-tags": {"latest": "1.2.3"}}]), patch.object(release.time, "sleep") as sleep:
            release.verify_npm(info, archive)
            sleep.assert_called_once_with(2)

    def test_finalize_checks_each_destination_and_never_promotes_a_beta_to_latest(self):
        for value in ("1.2.3", "1.2.3-beta.1"):
            directory = self.bundle(value)
            info = release.version(value)
            formula = base64.b64encode((directory / f"{info['formula']}.rb").read_bytes()).decode()
            metadata = [{"draft": False, "prerelease": info["channel"] == "next"}, {"content": formula}]
            with patch.object(release, "check_channels"), patch.object(release, "source_matches", return_value=SHA), patch.object(release, "verify_npm") as npm, patch.object(release, "api", side_effect=metadata), patch.object(release, "run") as commands:
                release.finalize(value, SHA, directory)
            npm.assert_called_once()
            if info["channel"] == "latest":
                self.assertEqual(commands.call_args.args[-1], "--latest")
            else:
                commands.assert_not_called()

    def test_npm_retry_repairs_only_the_missing_channel_tag(self):
        directory = self.bundle()
        package = {"dist": {"integrity": release.npm_integrity(directory / "boxpool-1.2.3.tgz")}}
        older = {"dist-tags": {"latest": "1.2.2"}}
        selected = {"dist-tags": {"latest": "1.2.3"}}
        with patch.object(release, "check_channels"), patch.object(release, "registry", side_effect=[package, older, package, selected]), patch.object(release, "run") as commands:
            release.npm_release("1.2.3", SHA, directory)
            commands.assert_called_once_with("npm", "dist-tag", "add", "boxpool@1.2.3", "latest")

    def test_tap_rejects_conflicting_same_version_or_newer_or_unreadable_formula(self):
        for value, current in (("1.2.3", "1.2.3"), ("1.2.3", "1.2.4"),
                               ("1.2.3-beta.1", "1.2.3-beta.1"),
                               ("1.2.3-beta.1", "1.2.3-beta.2"), ("1.2.3", None)):
            with self.subTest(value=value, current=current):
                directory = self.bundle(value)
                tap = directory / "tap"
                target = tap / "Formula" / f"{release.version(value)['formula']}.rb"
                target.parent.mkdir(parents=True)
                content = f'  version "{current}"\n  sha256 "conflicting digest"\n' if current else 'unreadable formula\n'
                target.write_text(content)
                with patch.object(release, "check_channels"), patch.object(release, "run") as commands:
                    with self.assertRaises(ValueError):
                        release.tap_release(value, SHA, directory, tap)
                    commands.assert_not_called()
                self.assertEqual(target.read_text(), content)

    def test_tap_still_updates_from_an_older_version(self):
        for value, current in (("1.2.3", "1.2.2"), ("1.2.3-beta.1", "1.2.3-beta.0")):
            with self.subTest(value=value):
                directory = self.bundle(value)
                tap = directory / "tap"
                name = f"{release.version(value)['formula']}.rb"
                target = tap / "Formula" / name
                target.parent.mkdir(parents=True)
                target.write_text(f'  version "{current}"\n')
                with patch.object(release, "check_channels"), patch.object(release, "run") as commands:
                    release.tap_release(value, SHA, directory, tap)
                    self.assertTrue(any(c.args[-2:] == ("origin", "HEAD:main") for c in commands.call_args_list))
                self.assertEqual(target.read_bytes(), (directory / name).read_bytes())

    def test_npm_retry_rejects_a_newer_channel_on_the_fresh_read_before_repair(self):
        for value, newer in (("1.2.3", "1.2.4"), ("1.2.3-beta.1", "1.2.3-beta.2")):
            with self.subTest(value=value):
                directory = self.bundle(value)
                info = release.version(value)
                package = {"dist": {"integrity": release.npm_integrity(directory / f"boxpool-{value}.tgz")}}
                tags = {"dist-tags": {info["channel"]: newer}}
                with patch.object(release, "check_channels"), patch.object(release, "registry", side_effect=lambda version=None: package if version else tags), patch.object(release, "run") as commands:
                    with self.assertRaisesRegex(ValueError, "backwards"):
                        release.npm_release(value, SHA, directory)
                    commands.assert_not_called()

    def test_release_lookup_paginates_drafts_after_published_tag_404(self):
        draft = {"id": 77, "tag_name": "v1.2.3", "draft": True, "prerelease": False}
        first = [{"tag_name": f"other-{n}"} for n in range(100)]
        with patch.object(release, "api", side_effect=[None, first, [draft]]) as service:
            self.assertEqual(release.release_by_tag("v1.2.3"), draft)
        self.assertEqual(service.call_args_list[-1].args[0], f"repos/{release.REPO}/releases?per_page=100&page=2")

    def test_release_lookup_keeps_published_fast_path_and_propagates_auth_failure(self):
        published = {"id": 1, "tag_name": "v1.2.3", "draft": False}
        with patch.object(release, "api", return_value=published) as service:
            self.assertEqual(release.release_by_tag("v1.2.3"), published)
            service.assert_called_once()
        with patch.object(release, "api", side_effect=RuntimeError("HTTP 403")) as service:
            with self.assertRaisesRegex(RuntimeError, "HTTP 403"):
                release.release_by_tag("v1.2.3", missing=True)
            service.assert_called_once()

    def test_release_lookup_missing_is_optional_only_when_requested(self):
        with patch.object(release, "api", side_effect=[None, []]):
            self.assertIsNone(release.release_by_tag("v1.2.3", missing=True))
        with patch.object(release, "api", side_effect=[None, []]):
            with self.assertRaisesRegex(ValueError, "release not found"):
                release.release_by_tag("v1.2.3")

    def test_github_retry_reuses_a_draft_hidden_from_the_tag_endpoint(self):
        directory = self.bundle()
        draft = {"id": 7, "tag_name": "v1.2.3", "draft": True, "prerelease": False}
        def api(endpoint, **kwargs):
            if "/releases/tags/" in endpoint:
                return None
            if "/releases?" in endpoint:
                return [draft]
            if "/assets?" in endpoint:
                return []
            raise AssertionError(endpoint)
        with patch.object(release, "check_channels"), patch.object(release, "source_matches", return_value=SHA), patch.object(release, "api", side_effect=api), patch.object(release, "run") as commands:
            release.github_release("1.2.3", SHA, directory)
        actions = [c.args[2] for c in commands.call_args_list]
        self.assertNotIn("create", actions)
        self.assertEqual(actions.count("upload"), 7)
        self.assertEqual(actions[-1], "edit")
