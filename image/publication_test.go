package image_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Docker exports small, checksummed stand-ins for the real BuildKit artifacts.
// The production script still stages, saves, checksums and promotes these files.
const buildDockerStub = `#!/usr/bin/env bash
set -eu
case "$1 ${2:-}" in
    'buildx version'|'info ') exit 0 ;;
    'buildx build')
        target= dest=
        while [[ $# -gt 0 ]]; do
            case $1 in
                --target) target=$2; shift ;;
                --output) dest=${2#type=local,dest=}; shift ;;
            esac
            shift
        done
        if [[ $target == userland ]]; then
            [[ $BUILD_FAILURE != userland ]] || { echo stub-userland-failure >&2; exit 41; }
        else
            if [[ $BUILD_FAILURE == artifacts ]]; then
                cp "$FIXTURE/image-$STUB_ARCH.txt" "$dest/"
                echo stub-artifacts-failure >&2
                exit 42
            fi
            cp "$FIXTURE/"* "$dest/"
        fi
        ;;
    'image save')
        while [[ $1 != -o ]]; do shift; done
        printf '%s\n' "$STUB_VERSION:$STUB_REVISION:$STUB_ARCH:oci" > "$2"
        [[ $BUILD_FAILURE != save ]] || { echo stub-save-failure >&2; exit 43; }
        ;;
    *) echo "unexpected Docker invocation: $*" >&2; exit 44 ;;
esac
`

// Inject errors or TERM immediately on either side of the actual rename.
// In particular, after-switch exercises the gap before the shell can record
// success; cleanup must not remove the directory current now points into.
const promotionStub = `#!/usr/bin/env bash
set -eu
case $PROMOTION in
    failure) echo stub-promotion-failure >&2; exit 74 ;;
    before-switch) kill -TERM "$PPID"; exit 143 ;;
    after-switch) "$REAL_MV" "$@"; kill -TERM "$PPID" ;;
    *) exec "$REAL_MV" "$@" ;;
esac
`

func writeTestFile(t *testing.T, path, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}

func publicationSource(t *testing.T) string {
	t.Helper()
	dir := buildSource(t)
	// macOS temp paths can pass through /var -> /private/var.
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "docker"), buildDockerStub, 0755)
	writeTestFile(t, filepath.Join(dir, "mv"), promotionStub, 0755)
	return dir
}

func artifactContents(version, revision, arch string) map[string]string {
	files := map[string]string{
		"image-" + arch + ".txt": fmt.Sprintf("version=%s\nrevision=%s\narch=%s\n", version, revision, arch),
	}
	for _, name := range []string{"vmlinux-" + arch, "rootfs-" + arch + ".ext4.zst", "kernel-config-" + arch, "kernel-" + arch + ".txt"} {
		files[name] = fmt.Sprintf("%s:%s:%s:%s\n", version, revision, arch, name)
	}
	if arch == "amd64" {
		files["bzImage-amd64"] = version + ":bzImage\n"
	}
	return files
}

func runFixtureBuild(t *testing.T, dir, version, arch, failure, promotion string) (string, error) {
	t.Helper()
	revision := strings.Repeat("a", 40)
	if version == "v2" {
		revision = strings.Repeat("b", 40)
	}
	fixture := t.TempDir()
	var sums strings.Builder
	for name, content := range artifactContents(version, revision, arch) {
		writeTestFile(t, filepath.Join(fixture, name), content, 0644)
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256([]byte(content)), name)
	}
	writeTestFile(t, filepath.Join(fixture, "SHA256SUMS-"+arch), sums.String(), 0644)
	realMV, err := exec.LookPath("mv")
	if err != nil {
		t.Fatal(err)
	}
	cmd := buildCommand(t, dir, version, "2", revision, arch)
	cmd.Env = append(cmd.Env, "FIXTURE="+fixture, "STUB_VERSION="+version,
		"STUB_REVISION="+revision, "STUB_ARCH="+arch, "BUILD_FAILURE="+failure,
		"PROMOTION="+promotion, "REAL_MV="+realMV)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func assertCompleteGeneration(t *testing.T, path, version, arch string) {
	t.Helper()
	revision := strings.Repeat("a", 40)
	if version == "v2" {
		revision = strings.Repeat("b", 40)
	}
	files := artifactContents(version, revision, arch)
	files["oci-"+arch+".tar"] = fmt.Sprintf("%s:%s:%s:oci\n", version, revision, arch)
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(path, name))
		if err != nil || string(got) != want {
			t.Fatalf("%s: got %q (%v), want %q", name, got, err, want)
		}
	}
	checked := make(map[string]bool)
	for _, manifest := range []string{"SHA256SUMS-" + arch, "oci-" + arch + ".tar.sha256"} {
		data, err := os.ReadFile(filepath.Join(path, manifest))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 {
				t.Fatalf("invalid checksum line %q", line)
			}
			content, err := os.ReadFile(filepath.Join(path, fields[1]))
			if err != nil || fmt.Sprintf("%x", sha256.Sum256(content)) != fields[0] {
				t.Fatalf("%s: checksum mismatch (%v)", fields[1], err)
			}
			checked[fields[1]] = true
		}
	}
	if len(checked) != len(files) {
		t.Fatalf("checksummed %d artifacts, want %d", len(checked), len(files))
	}
}

func selectedGeneration(t *testing.T, dir string) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(filepath.Join(dir, "out", "current"))
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func assertNoTemporaryPointers(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".current") {
			t.Fatalf("left temporary pointer %s", entry.Name())
		}
	}
}

func TestBuildPublishesCompleteGenerations(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			dir := publicationSource(t)
			if err := os.Mkdir(filepath.Join(dir, "out"), 0755); err != nil {
				t.Fatal(err)
			}
			// Original flat output and unrelated user files must survive migration.
			for _, name := range []string{"SHA256SUMS-" + arch, "image-" + arch + ".txt", "rootfs-" + arch + ".ext4.zst", "keep-me"} {
				writeTestFile(t, filepath.Join(dir, "out", name), "legacy:"+name, 0644)
			}
			var previous string
			for _, version := range []string{"v1", "v2"} {
				out, err := runFixtureBuild(t, dir, version, arch, "", "")
				if err != nil {
					t.Fatalf("build: %s: %v", out, err)
				}
				current := selectedGeneration(t, dir)
				if !strings.Contains(out, "artifacts="+current+"\n") {
					t.Fatalf("missing pinned artifact path: %s", out)
				}
				assertCompleteGeneration(t, current, version, arch)
				if previous != "" {
					if previous == current {
						t.Fatal("overwrote selected generation")
					}
					assertCompleteGeneration(t, previous, "v1", arch)
				}
				previous = current
			}
			for _, name := range []string{"SHA256SUMS-" + arch, "image-" + arch + ".txt", "rootfs-" + arch + ".ext4.zst", "keep-me"} {
				got, err := os.ReadFile(filepath.Join(dir, "out", name))
				if err != nil || string(got) != "legacy:"+name {
					t.Fatalf("changed existing file %s: %q (%v)", name, got, err)
				}
			}
			assertNoTemporaryPointers(t, dir)
		})
	}
}

func TestBuildFailurePreservesPublishedArtifacts(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, phase := range []string{"userland", "artifacts", "save"} {
			t.Run(fmt.Sprintf("existing=%t/%s", existing, phase), func(t *testing.T) {
				dir := publicationSource(t)
				var previous string
				if existing {
					if out, err := runFixtureBuild(t, dir, "v1", "amd64", "", ""); err != nil {
						t.Fatalf("initial build: %s: %v", out, err)
					}
					previous = selectedGeneration(t, dir)
				}
				out, err := runFixtureBuild(t, dir, "v2", "amd64", phase, "")
				if err == nil || !strings.Contains(out, "stub-"+phase+"-failure") {
					t.Fatalf("got %s (%v), want injected failure", out, err)
				}
				entries, err := os.ReadDir(filepath.Join(dir, "out"))
				if err != nil {
					t.Fatal(err)
				}
				if existing {
					if selectedGeneration(t, dir) != previous {
						t.Fatal("failed build switched current")
					}
					assertCompleteGeneration(t, previous, "v1", "amd64")
					if len(entries) != 2 { // current and the previous generation
						t.Fatalf("failed build left artifacts: %v", entries)
					}
				} else if len(entries) != 0 {
					t.Fatalf("first failed build left artifacts: %v", entries)
				}
			})
		}
	}
}

func TestBuildPromotionFailureAndInterruption(t *testing.T) {
	for _, phase := range []string{"failure", "before-switch", "after-switch"} {
		t.Run(phase, func(t *testing.T) {
			dir := publicationSource(t)
			if out, err := runFixtureBuild(t, dir, "v1", "amd64", "", ""); err != nil {
				t.Fatalf("initial build: %s: %v", out, err)
			}
			previous := selectedGeneration(t, dir)
			out, err := runFixtureBuild(t, dir, "v2", "amd64", "", phase)
			if err == nil {
				t.Fatalf("injected failure unexpectedly succeeded: %s", out)
			}
			if phase == "failure" && !strings.Contains(out, "cannot promote artifacts; prior output retained") {
				t.Fatalf("missing actionable promotion error: %s", out)
			}
			assertCompleteGeneration(t, previous, "v1", "amd64")
			current := selectedGeneration(t, dir)
			if phase == "after-switch" {
				if current == previous {
					t.Fatal("did not exercise completed rename")
				}
				assertCompleteGeneration(t, current, "v2", "amd64")
			} else if current != previous {
				t.Fatal("failed promotion switched current")
			}
			// Both retained directories must be complete, including the candidate
			// left by an interruption before the switch. Only current selects one.
			generations, err := filepath.Glob(filepath.Join(dir, "out", ".generation.*"))
			if err != nil || len(generations) != 2 {
				t.Fatalf("generations: %v (%v)", generations, err)
			}
			for _, generation := range generations {
				version := "v2"
				if generation == previous {
					version = "v1"
				}
				assertCompleteGeneration(t, generation, version, "amd64")
			}
			assertNoTemporaryPointers(t, dir)
		})
	}
}

func TestBuildRejectsCurrentDirectory(t *testing.T) {
	dir := publicationSource(t)
	current := filepath.Join(dir, "out", "current")
	if err := os.MkdirAll(current, 0755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(current, "keep-me"), "existing directory", 0644)
	out, err := runFixtureBuild(t, dir, "v2", "amd64", "", "")
	if err == nil || !strings.Contains(out, "move this existing entry aside explicitly") {
		t.Fatalf("got %s (%v), want explicit migration error", out, err)
	}
	data, err := os.ReadFile(filepath.Join(current, "keep-me"))
	if err != nil || string(data) != "existing directory" {
		t.Fatalf("changed existing current directory: %q (%v)", data, err)
	}
}
