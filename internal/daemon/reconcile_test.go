//go:build linux || darwin

package daemon

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"boxpool.run/boxpool/internal/backend"
	"boxpool.run/boxpool/internal/store"
)

func TestReconcile(t *testing.T) {
	ctx := context.Background()
	tmp, err := os.MkdirTemp("", "bp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	st, err := store.Open(filepath.Join(tmp, StoreName))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	d := newFakeDriver(t, tmp)

	// live "" means the backend does not have the sandbox.
	cases := map[string]struct {
		recorded backend.State
		restart  string
		live     backend.State
		want     backend.State
	}{
		"rebooted-always": {backend.StateRunning, "always", backend.StateStopped, backend.StateRunning},
		"rebooted":        {backend.StateRunning, "", backend.StateStopped, backend.StateStopped},
		"stopped-always":  {backend.StateStopped, "always", backend.StateStopped, backend.StateStopped},
		"adopted":         {backend.StateRunning, "", backend.StateRunning, backend.StateRunning},
		"half-started":    {backend.StateStopped, "", backend.StateRunning, backend.StateRunning},
		"missing":         {backend.StateRunning, "always", "", backend.StateRunning},
	}
	ids := map[string]string{}
	for name, c := range cases {
		sb, err := st.Create(ctx, store.Sandbox{Backend: "fake", Spec: backend.Spec{Name: name}, State: c.recorded, Restart: c.restart})
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = sb.ID
		if c.live != "" {
			d.spec[sb.ID] = sb.Spec
			d.up[sb.ID] = c.live == backend.StateRunning
		}
	}
	other, err := st.Create(ctx, store.Sandbox{Backend: "docker", Spec: backend.Spec{Name: "other"}, State: backend.StateRunning, Restart: "always"})
	if err != nil {
		t.Fatal(err)
	}
	d.spec["orphan"] = backend.Spec{Name: "orphan"}

	if err := reconcile(ctx, st, d); err != nil {
		t.Fatal(err)
	}
	for name, c := range cases {
		sb, err := st.Get(ctx, ids[name])
		if err != nil || sb.State != c.want {
			t.Errorf("%s: recorded %q, %v; want %q", name, sb.State, err, c.want)
		}
		if c.live != "" && d.up[ids[name]] != (c.want == backend.StateRunning) {
			t.Errorf("%s: running = %v, want state %q", name, d.up[ids[name]], c.want)
		}
	}
	if d.n.start != 1 || d.n.stop != 0 || d.n.remove != 0 {
		t.Errorf("driver calls = %+v, want one start", d.n)
	}
	if sb, _ := st.Get(ctx, other.ID); sb.State != backend.StateRunning {
		t.Errorf("another backend's sandbox recorded %q, want it untouched", sb.State)
	}
	if _, ok := d.spec["orphan"]; !ok {
		t.Error("reconcile removed a sandbox with no record")
	}
}

func TestCreateRestartPolicy(t *testing.T) {
	srv, _, st := newAPIStore(t)
	if code, sb, body := post(t, srv, `{"name":"web","restart":"always"}`); code != http.StatusCreated || sb.Restart != "always" {
		t.Fatalf("POST restart always = %d %s", code, body)
	}
	if sb, err := st.GetByName(context.Background(), "web"); err != nil || sb.Restart != "always" {
		t.Errorf("stored restart = %q, %v", sb.Restart, err)
	}
	if code, sb, body := post(t, srv, `{"name":"db","restart":"no"}`); code != http.StatusCreated || sb.Restart != "" {
		t.Errorf("POST restart no = %d %s", code, body)
	}
	if code, _, body := post(t, srv, `{"name":"bad","restart":"sometimes"}`); code != http.StatusBadRequest {
		t.Errorf("POST restart sometimes = %d %s, want 400", code, body)
	}
}
