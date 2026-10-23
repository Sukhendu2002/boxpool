package docker

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"boxpool.run/boxpool/internal/backend"
)

func TestContainerConfig(t *testing.T) {
	l := sandboxLayout("/state/docker", "abc")
	cfg := newContainerConfig("/state/docker", "abc", backend.Spec{Name: "web", CPUs: 2, MemoryMB: 512}, DefaultImage, "boxpool-x-abc", l)
	// Decode the request body generically: the Engine API's field names
	// matter, not the Go struct's.
	b, _ := json.Marshal(cfg)
	var got struct {
		Image      string
		Cmd        []string
		WorkingDir string
		User       *string
		Labels     map[string]string
		HostConfig map[string]any
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Image != DefaultImage || got.WorkingDir != "/root" || got.User != nil {
		t.Errorf("image %q, working dir %q, user %v", got.Image, got.WorkingDir, got.User)
	}
	if want := map[string]string{labelOwner: "/state/docker", labelSandbox: "abc", labelName: "web"}; !reflect.DeepEqual(got.Labels, want) {
		t.Errorf("labels %v, want %v", got.Labels, want)
	}
	if len(got.Cmd) != 3 || got.Cmd[0] != "/bin/sh" || !strings.Contains(got.Cmd[2], "--listen unix:/run/boxpool/agent.sock") || !strings.Contains(got.Cmd[2], "chmod 0666 /run/boxpool/agent.sock") {
		t.Errorf("cmd %q", got.Cmd)
	}
	h := got.HostConfig
	want := map[string]any{
		"NetworkMode":    "boxpool-x-abc",
		"CapDrop":        []any{"ALL"},
		"CapAdd":         []any{},
		"SecurityOpt":    []any{"no-new-privileges:true"},
		"ReadonlyRootfs": true,
		"Privileged":     false,
		"NanoCpus":       2e9,
		"Memory":         float64(512 << 20),
		"MemorySwap":     float64(512 << 20),
		"PidsLimit":      float64(pidsLimit),
		"CpusetCpus":     "",
		"Mounts": []any{
			map[string]any{"Type": "volume", "Source": "boxpool-x-abc", "Target": "/root"},
			map[string]any{"Type": "bind", "Source": "/state/docker/abc/agent", "Target": "/run/boxpool"},
		},
		"Tmpfs": map[string]any{
			"/tmp":     "rw,nosuid,nodev,size=256m,mode=1777",
			"/var/tmp": "rw,nosuid,nodev,size=256m,mode=1777",
			"/run":     "rw,nosuid,nodev,noexec,size=16m,mode=0755",
		},
	}
	// Exactly these keys: nothing that adds privileges, host namespaces,
	// devices, published ports or other mounts.
	if !reflect.DeepEqual(h, want) {
		gb, _ := json.MarshalIndent(h, "", "  ")
		t.Errorf("HostConfig:\n%s", gb)
	}
}

func TestSplitImage(t *testing.T) {
	for _, c := range []struct{ ref, repo, tag string }{
		{DefaultImage, "ghcr.io/sukhendu2002/boxpool-image", "sha256:747ea3a41c0dd069a535af68807ec03794b4876676bbc5aabbb71b0801301eba"},
		{"ghcr.io/sukhendu2002/boxpool-image:v0.0.1-alpha.1", "ghcr.io/sukhendu2002/boxpool-image", "v0.0.1-alpha.1"},
		{"boxpool-image:v1@sha256:abc", "boxpool-image", "sha256:abc"},
		{"localhost:5000/img:v1", "localhost:5000/img", "v1"},
		{"localhost:5000/img@sha256:abc", "localhost:5000/img", "sha256:abc"},
	} {
		repo, tag, err := splitImage(c.ref)
		if err != nil || repo != c.repo || tag != c.tag {
			t.Errorf("splitImage(%q) = %q, %q, %v; want %q, %q", c.ref, repo, tag, err, c.repo, c.tag)
		}
	}
	for _, ref := range []string{"", "debian", "localhost:5000/img", "img:", "img@", "@sha256:abc"} {
		if _, _, err := splitImage(ref); err == nil {
			t.Errorf("splitImage(%q) succeeded", ref)
		}
	}
}

func TestCheckID(t *testing.T) {
	for _, id := range []string{"a", "abc123", "A.b_c-d", strings.Repeat("x", 64)} {
		if err := checkID(id); err != nil {
			t.Errorf("checkID(%q): %v", id, err)
		}
	}
	for _, id := range []string{"", ".", "..", "-a", ".a", "a/b", "../x", "a b", "a:b", strings.Repeat("x", 65)} {
		if err := checkID(id); err == nil {
			t.Errorf("checkID(%q) succeeded", id)
		}
	}
}

func TestPrefix(t *testing.T) {
	a, b := prefix("/var/lib/boxpool/docker"), prefix("/home/u/.boxpool/docker")
	if a == b || !strings.HasPrefix(a, "boxpool-") || len(a) != len("boxpool-")+12+1 {
		t.Errorf("prefixes %q and %q", a, b)
	}
}

func TestHost(t *testing.T) {
	for _, c := range []struct{ env, want string }{
		{"", "/var/run/docker.sock"},
		{"unix:///run/user/1000/docker.sock", "/run/user/1000/docker.sock"},
	} {
		t.Setenv("DOCKER_HOST", c.env)
		if got, err := host(); err != nil || got != c.want {
			t.Errorf("DOCKER_HOST=%q: %q, %v", c.env, got, err)
		}
	}
	for _, env := range []string{"tcp://10.0.0.1:2375", "ssh://host", "unix://", "/var/run/docker.sock"} {
		t.Setenv("DOCKER_HOST", env)
		if _, err := host(); err == nil {
			t.Errorf("DOCKER_HOST=%q accepted", env)
		}
	}
}
