//go:build linux || darwin

package daemon

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"boxpool.run/boxpool/internal/backend"
)

func TestSleepAssertionLifecycle(t *testing.T) {
	srv, s, _ := poolMachine(t, "darwin")
	created, released := 0, 0
	s.preventSleep = func() (func() error, error) {
		created++
		return func() error { released++; return nil }, nil
	}
	for _, name := range []string{"a", "b"} {
		if code, _, body := post(t, srv, `{"name":"`+name+`"}`); code != http.StatusCreated {
			t.Fatal(body)
		}
	}
	if created != 1 || released != 0 {
		t.Fatalf("two running: created %d, released %d", created, released)
	}
	for _, path := range []string{"/v1/sandboxes/a/stop", "/v1/sandboxes/b/pause"} {
		if code, body := call(t, srv, "POST", path); code != http.StatusOK {
			t.Fatal(body)
		}
	}
	if released != 1 || s.releaseSleep != nil {
		t.Fatalf("none running: released %d", released)
	}
	if code, body := call(t, srv, "POST", "/v1/sandboxes/b/resume"); code != http.StatusOK {
		t.Fatal(body)
	}
	if created != 2 {
		t.Fatalf("resume did not reacquire: %d", created)
	}
	if code, body := call(t, srv, "DELETE", "/v1/sandboxes/b?force=true"); code != http.StatusOK {
		t.Fatal(body)
	}
	if released != 2 {
		t.Fatalf("remove did not release: %d", released)
	}
	s.info.AllowSleep = true
	post(t, srv, `{"name":"allowed"}`)
	if created != 2 {
		t.Fatal("allow-sleep still acquired an assertion")
	}
}

func TestSleepAssertionRetryAndExit(t *testing.T) {
	_, s, d := poolMachine(t, "darwin")
	ctx := context.Background()
	d.spec["box"], d.up["box"] = backend.Spec{Name: "box"}, true
	attempts, released := 0, 0
	s.preventSleep = func() (func() error, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("unavailable")
		}
		return func() error { released++; return nil }, nil
	}
	s.refreshSleep(ctx)
	s.refreshSleep(ctx)
	if attempts != 2 || s.releaseSleep == nil {
		t.Fatal("failed assertion was not retried")
	}
	d.up["box"] = false // VM exit outside the lifecycle API
	s.refreshSleep(ctx)
	if released != 1 {
		t.Fatal("VM exit did not release assertion")
	}
}
