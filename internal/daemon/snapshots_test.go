//go:build linux || darwin

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"boxpool.run/boxpool/internal/backend"
)

func postSnapshot(t *testing.T, srv *httptest.Server, sandbox, name string) (int, Snapshot, string) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/v1/sandboxes/"+sandbox+"/snapshot", "application/json", strings.NewReader(`{"name":"`+name+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b strings.Builder
	var snap Snapshot
	json.NewDecoder(resp.Body).Decode(&snap)
	fmt.Fprintf(&b, "%+v", snap)
	return resp.StatusCode, snap, b.String()
}

// TestSnapshot checks that sandboxes started from a snapshot get the
// source's disk as it was when the snapshot was taken, each its own.
func TestSnapshot(t *testing.T) {
	srv, d, st := newAPIStore(t)
	_, box, _ := post(t, srv, `{"name":"box","disk_gb":20}`)
	d.mu.Lock()
	d.disk[box.ID] = "jq"
	d.mu.Unlock()

	code, snap, body := postSnapshot(t, srv, "box", "withjq")
	if code != http.StatusCreated || snap.Name != "withjq" || snap.Sandbox != "box" || snap.Backend != "fake" || snap.DiskGB != 20 || snap.Warning != "" {
		t.Fatalf("snapshot = %d %s", code, body)
	}
	// The running sandbox was stopped for the copy and started again.
	if got := states(list(t, srv)); got != "box=running" || d.n.stop != 1 || d.n.start != 2 {
		t.Errorf("after snapshot: %q, %d stops, %d starts", got, d.n.stop, d.n.start)
	}
	if rec, _ := st.Get(context.Background(), box.ID); rec.State != backend.StateRunning {
		t.Errorf("record after snapshot = %s", rec.State)
	}
	d.mu.Lock()
	d.disk[box.ID] = "jq vi"
	d.mu.Unlock()

	for body, want := range map[string]int{
		`{"name":"a","from":"withjq"}`:                  http.StatusCreated,
		`{"name":"b","from":"withjq"}`:                  http.StatusCreated,
		`{"name":"small","from":"withjq","disk_gb":10}`: http.StatusBadRequest,
		`{"name":"box","from":"withjq"}`:                http.StatusConflict,
		`{"name":"c","from":"nope"}`:                    http.StatusNotFound,
	} {
		if code, _, got := post(t, srv, body); code != want {
			t.Errorf("POST %s = %d %s, want %d", body, code, got, want)
		}
	}
	a, _ := st.GetByName(context.Background(), "a")
	b, _ := st.GetByName(context.Background(), "b")
	if a.Spec.DiskGB != 20 || a.State != backend.StateRunning {
		t.Errorf("sandbox from the snapshot = %+v", a)
	}
	d.mu.Lock()
	d.disk[a.ID] += " curl"
	if d.disk[a.ID] != "jq curl" || d.disk[b.ID] != "jq" {
		t.Errorf("disks: a %q, b %q; want jq curl and jq", d.disk[a.ID], d.disk[b.ID])
	}
	d.mu.Unlock()

	for _, c := range []struct {
		sandbox, name string
		want          int
	}{{"box", "withjq", http.StatusConflict}, {"box", "Bad_Name", http.StatusBadRequest}, {"nope", "x", http.StatusNotFound}} {
		if code, _, body := postSnapshot(t, srv, c.sandbox, c.name); code != c.want {
			t.Errorf("snapshot %s as %s = %d %s, want %d", c.sandbox, c.name, code, body, c.want)
		}
	}
	call(t, srv, http.MethodPost, "/v1/sandboxes/b/pause")
	if code, _, body := postSnapshot(t, srv, "b", "paused"); code != http.StatusConflict {
		t.Errorf("snapshot of a paused sandbox = %d %s", code, body)
	}

	code, body = call(t, srv, http.MethodGet, "/v1/snapshots")
	var snaps []Snapshot
	if json.Unmarshal([]byte(body), &snaps); code != http.StatusOK || len(snaps) != 1 || snaps[0].ID != snap.ID {
		t.Errorf("GET /v1/snapshots = %d %s", code, body)
	}
	file := filepath.Join(filepath.Dir(d.sock), "snapshots", snap.ID)
	if _, err := os.Stat(file); err != nil {
		t.Errorf("snapshot file: %v", err)
	}
	if code, body := call(t, srv, http.MethodDelete, "/v1/snapshots/withjq"); code != http.StatusOK {
		t.Errorf("DELETE = %d %s", code, body)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("snapshot file after DELETE: %v", err)
	}
	if code, _ := call(t, srv, http.MethodDelete, "/v1/snapshots/withjq"); code != http.StatusNotFound {
		t.Errorf("second DELETE = %d", code)
	}
	// Sandboxes started from it keep their disks.
	if got := states(list(t, srv)); got != "box=running a=running b=paused" {
		t.Errorf("after DELETE: %q", got)
	}
}

// TestSnapshotFailure checks that a failed snapshot leaves no record, and the
// sandbox running, and that an unclean stop is a warning.
func TestSnapshotFailure(t *testing.T) {
	srv, d, st := newAPIStore(t)
	post(t, srv, `{"name":"box"}`)
	dir := filepath.Join(filepath.Dir(d.sock), "snapshots")
	os.WriteFile(dir, nil, 0600) // MkdirAll fails
	if code, _, body := postSnapshot(t, srv, "box", "s"); code != http.StatusInternalServerError {
		t.Errorf("snapshot = %d %s", code, body)
	}
	if list, _ := st.ListSnapshots(context.Background()); len(list) != 0 {
		t.Errorf("snapshots after a failure: %+v", list)
	}
	if got := states(list(t, srv)); got != "box=running" {
		t.Errorf("after a failed snapshot: %q", got)
	}

	os.Remove(dir)
	// A stop that fails after stopping still has the sandbox started again.
	d.stopErr = errors.New("stop: lost the answer")
	if code, _, body := postSnapshot(t, srv, "box", "s"); code != http.StatusInternalServerError {
		t.Errorf("snapshot with a failed stop = %d %s", code, body)
	}
	if got := states(list(t, srv)); got != "box=running" {
		t.Errorf("after a failed stop: %q", got)
	}

	// A copy a crash interrupted does not block the next snapshot, and
	// removePartialSnapshots deletes the others.
	box, _ := st.GetByName(context.Background(), "box")
	os.Mkdir(dir, 0700)
	os.WriteFile(filepath.Join(dir, box.ID+tmpSuffix), []byte("partial"), 0600)
	other := filepath.Join(dir, "gone"+tmpSuffix)
	os.WriteFile(other, nil, 0600)
	d.stopErr = fmt.Errorf("stop: %w", backend.ErrUncleanStop)
	code, snap, body := postSnapshot(t, srv, "box", "s")
	if code != http.StatusCreated || !strings.Contains(snap.Warning, "not stopped cleanly") {
		t.Errorf("snapshot after an unclean stop = %d %s", code, body)
	}
	if b, err := os.ReadFile(filepath.Join(dir, snap.ID)); err != nil || string(b) == "partial" {
		t.Errorf("snapshot file = %q, %v", b, err)
	}
	if err := removePartialSnapshots(filepath.Dir(dir)); err != nil {
		t.Fatal(err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 1 || left[0] != filepath.Join(dir, snap.ID) {
		t.Errorf("snapshots dir after removePartialSnapshots: %q", left)
	}
}
