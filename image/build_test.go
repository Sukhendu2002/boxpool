package image_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Copy just the script: the Linux development sync deliberately excludes .git.
func buildSource(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script, err := os.ReadFile("build.sh")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "image"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "image", "build.sh"), script, 0755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func buildCommand(t *testing.T, dir, version, jobs, revision string, args ...string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, "bash", append([]string{"image/build.sh"}, args...)...)
	cmd.WaitDelay = time.Second
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"),
		"GIT_CEILING_DIRECTORIES="+dir, "IMAGE_VERSION="+version, "JOBS="+jobs,
		"IMAGE_REVISION="+revision, "OUTPUT_DIR="+filepath.Join(dir, "out"),
		"MARKER="+filepath.Join(dir, "contacted-docker"))
	return cmd
}

func assertNoBuild(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"contacted-docker", "out"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("created %s before rejecting inputs: %v", name, err)
		}
	}
}

// Bad inputs must fail before resolving revision, contacting a daemon or
// creating output files, even in a synchronized source tree without .git.
func TestBuildRejectsInvalidInputs(t *testing.T) {
	for _, tc := range []struct {
		name, version, jobs, revision, want string
		args                                []string
	}{
		{name: "missing arch", want: "usage:"},
		{name: "host spelling", args: []string{"x86_64"}, want: "unsupported architecture"},
		{name: "extra arg", args: []string{"amd64", "extra"}, want: "usage:"},
		{name: "version", args: []string{"amd64"}, version: "$(touch bad)", want: "IMAGE_VERSION"},
		{name: "tag prefix", args: []string{"arm64"}, version: ".invalid", want: "IMAGE_VERSION"},
		{name: "tag too long with arch", args: []string{"amd64"}, version: strings.Repeat("a", 123), want: "IMAGE_VERSION"},
		{name: "jobs", args: []string{"arm64"}, jobs: "99", want: "JOBS"},
		{name: "short revision", args: []string{"amd64"}, revision: "abcdef123456", want: "IMAGE_REVISION must be"},
		{name: "nonhex revision", args: []string{"amd64"}, revision: strings.Repeat("z", 40), want: "IMAGE_REVISION must be"},
		{name: "revision expression", args: []string{"amd64"}, revision: "HEAD", want: "IMAGE_REVISION must be"},
		{name: "missing revision", args: []string{"amd64"}, want: "set IMAGE_REVISION to the full source commit SHA"},
		{name: "version still needs revision", args: []string{"amd64"}, version: "v0.1.0", want: "set IMAGE_REVISION to the full source commit SHA"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := buildSource(t)
			if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\ntouch \"$MARKER\"\nexit 1\n"), 0755); err != nil {
				t.Fatal(err)
			}
			out, err := buildCommand(t, dir, tc.version, tc.jobs, tc.revision, tc.args...).CombinedOutput()
			if err == nil || !strings.Contains(string(out), tc.want) {
				t.Fatalf("got %q (%v), want failure containing %q", out, err, tc.want)
			}
			assertNoBuild(t, dir)
		})
	}
}

// A stub stops at the first build invocation. This checks metadata passed to
// Docker, without a daemon, downloads or any claim of building a real image.
func TestBuildRevisionMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, version, revision string
		checkout                bool
	}{
		{name: "source copy default version", revision: strings.Repeat("a", 40)},
		{name: "source copy explicit version", version: "v0.1.0", revision: strings.Repeat("b", 40)},
		{name: "sha256 revision", revision: strings.Repeat("c", 64)},
		{name: "maximum version", version: strings.Repeat("v", 122), revision: strings.Repeat("d", 40)},
		{name: "checkout HEAD", checkout: true},
		{name: "explicit revision overrides HEAD", checkout: true, revision: strings.Repeat("e", 40)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := buildSource(t)
			revision := tc.revision
			if tc.checkout {
				for _, args := range [][]string{
					{"init", "--quiet", dir},
					{"-C", dir, "-c", "user.name=Image Test", "-c", "user.email=image-test@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null", "commit", "--quiet", "--allow-empty", "-m", "test source"},
				} {
					if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
						t.Fatalf("git %v: %s: %v", args, out, err)
					}
				}
				if revision == "" {
					out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
					if err != nil {
						t.Fatal(err)
					}
					revision = strings.TrimSpace(string(out))
				}
			}
			stub := "#!/bin/sh\ncase \"$*\" in\n'buildx version'|info) exit 0 ;;\nesac\nprintf '%s\\n' \"$@\" > \"$MARKER\"\necho stopped-before-build >&2\nexit 1\n"
			if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(stub), 0755); err != nil {
				t.Fatal(err)
			}
			out, err := buildCommand(t, dir, tc.version, "2", tc.revision, "amd64").CombinedOutput()
			if err == nil || !strings.Contains(string(out), "stopped-before-build") {
				t.Fatalf("got %q (%v), want stub build failure", out, err)
			}
			args, err := os.ReadFile(filepath.Join(dir, "contacted-docker"))
			if err != nil {
				t.Fatal(err)
			}
			version := tc.version
			if version == "" {
				version = "0.0.0-dev." + revision[:12]
			}
			for _, want := range []string{"IMAGE_REVISION=" + revision, "IMAGE_VERSION=" + version, "boxpool-image:" + version + "-amd64", "JOBS=2"} {
				if !strings.Contains("\n"+string(args), "\n"+want+"\n") {
					t.Fatalf("Docker arguments %q do not contain %q", args, want)
				}
			}
			files, err := os.ReadDir(filepath.Join(dir, "out"))
			if err != nil || len(files) != 0 {
				t.Fatalf("failed build left partial artifacts: %v (%v)", files, err)
			}
		})
	}
}
