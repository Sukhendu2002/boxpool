package preflight

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"boxpool.run/boxpool/internal/backend/docker"
)

func TestResizeCheck(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	if c := resizeCheck(); c.OK || !c.Required {
		t.Fatalf("missing tool: %+v", c)
	}
	if err := os.WriteFile(filepath.Join(dir, "resize2fs"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if c := resizeCheck(); !c.OK || !c.Required {
		t.Fatalf("available tool: %+v", c)
	}
}

// BOXPOOL_BACKEND=docker picks Docker, even with KVM, when the engine
// answers, and the report carries the fallback warning. Without an engine
// it picks nothing.
func TestRunForcedDocker(t *testing.T) {
	t.Setenv("BOXPOOL_BACKEND", "docker")
	dir, err := os.MkdirTemp("/tmp", "bp")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "docker.sock")

	t.Setenv("DOCKER_HOST", "unix://"+sock)
	r := Run()
	if r.Backend != "none" || r.Warning != "" || r.Ready() || !hasCheck(r, "BOXPOOL_BACKEND", false) {
		t.Errorf("forced docker without an engine: %+v", r)
	}

	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Version":"28.0.0","ApiVersion":"1.48","MinAPIVersion":"1.24","Os":"linux"}`))
	})}
	go srv.Serve(l)
	defer srv.Close()
	r = Run()
	if r.Backend != "docker" || r.Warning != docker.Warning || !hasCheck(r, "BOXPOOL_BACKEND", true) || !hasCheck(r, "docker engine (fallback)", true) {
		t.Errorf("forced docker with an engine: %+v", r)
	}
}

func hasCheck(r Report, name string, ok bool) bool {
	for _, c := range r.Checks {
		if c.Name == name && c.OK == ok {
			return true
		}
	}
	return false
}
