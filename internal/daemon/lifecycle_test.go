//go:build linux || darwin

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"boxpool.run/boxpool/internal/backend"
)

// call sends a bodyless request and returns the status and the body.
func call(t *testing.T, srv *httptest.Server, method, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func list(t *testing.T, srv *httptest.Server) []Sandbox {
	t.Helper()
	code, body := call(t, srv, http.MethodGet, "/v1/sandboxes")
	var l []Sandbox
	if err := json.Unmarshal([]byte(body), &l); code != http.StatusOK || err != nil || l == nil {
		t.Fatalf("GET /v1/sandboxes = %d %s", code, body)
	}
	return l
}

func states(l []Sandbox) string {
	var s []string
	for _, sb := range l {
		s = append(s, sb.Name+"="+string(sb.State))
	}
	return strings.Join(s, " ")
}

func TestListSandboxes(t *testing.T) {
	srv, d := newAPI(t)
	if l := list(t, srv); len(l) != 0 {
		t.Fatalf("empty list = %+v", l)
	}
	post(t, srv, `{"name":"a"}`)
	_, b, _ := post(t, srv, `{"name":"b","cpu":1}`)
	post(t, srv, `{"name":"gone"}`)
	d.Stop(context.Background(), b.ID)
	_, gone, _ := post(t, srv, `{"name":"gone"}`)
	d.Remove(context.Background(), gone.ID) // deleted behind the daemon's back

	l := list(t, srv)
	if got := states(l); got != "a=running b=stopped gone=missing" {
		t.Errorf("states = %q", got)
	}
	if l[1].ID != b.ID || l[1].CPUs != 1 || l[1].Backend != "fake" || !l[1].CreatedAt.Equal(b.CreatedAt) {
		t.Errorf("b = %+v, want %+v", l[1], b)
	}
	if code, body := call(t, srv, http.MethodGet, "/v1/sandboxes/b"); code != http.StatusOK || !strings.Contains(body, `"state":"stopped"`) {
		t.Errorf("GET b = %d %s, want its live state", code, body)
	}
}

// TestListAfterRestart checks that a new daemon on the same store lists the
// sandboxes an earlier one created, with the state its driver reports.
func TestListAfterRestart(t *testing.T) {
	srv, d, st := newAPIStore(t)
	post(t, srv, `{"name":"a"}`)
	post(t, srv, `{"name":"b"}`)

	// A new daemon: same store, a driver that sees the disks but no VMs.
	d2 := newFakeDriver(t, t.TempDir())
	d.mu.Lock()
	for id, spec := range d.spec {
		d2.spec[id] = spec
	}
	d.mu.Unlock()
	srv2 := httptest.NewServer(Handler(Info{}, st, d2))
	t.Cleanup(srv2.Close)
	if got := states(list(t, srv2)); got != "a=stopped b=stopped" {
		t.Errorf("after restart: %q", got)
	}
}

func TestStopSandbox(t *testing.T) {
	srv, d, st := newAPIStore(t)
	_, sb, _ := post(t, srv, `{"name":"web"}`)
	code, body := call(t, srv, http.MethodPost, "/v1/sandboxes/web/stop")
	var got Sandbox
	json.Unmarshal([]byte(body), &got)
	if code != http.StatusOK || got.State != backend.StateStopped || got.ID != sb.ID || got.Warning != "" {
		t.Fatalf("stop = %d %s", code, body)
	}
	d.mu.Lock()
	if d.up[sb.ID] || d.n.stop != 1 {
		t.Errorf("driver: running %v, %d stops", d.up[sb.ID], d.n.stop)
	}
	d.mu.Unlock()
	if rec, err := st.Get(context.Background(), sb.ID); err != nil || rec.State != backend.StateStopped {
		t.Errorf("record = %+v, %v", rec, err)
	}

	// An unclean stop is still a stop.
	post(t, srv, `{"name":"web"}`)
	d.stopErr = fmt.Errorf("stop sandbox %s: %w: freeze timed out", sb.ID, backend.ErrUncleanStop)
	code, body = call(t, srv, http.MethodPost, "/v1/sandboxes/web/stop")
	json.Unmarshal([]byte(body), &got)
	if code != http.StatusOK || got.State != backend.StateStopped || !strings.Contains(got.Warning, "not stopped cleanly") {
		t.Errorf("unclean stop = %d %s", code, body)
	}

	d.stopErr = fmt.Errorf("engine down")
	if code, body := call(t, srv, http.MethodPost, "/v1/sandboxes/web/stop"); code != http.StatusInternalServerError || !strings.Contains(body, "engine down") {
		t.Errorf("failed stop = %d %s", code, body)
	}
	if code, _ := call(t, srv, http.MethodPost, "/v1/sandboxes/nope/stop"); code != http.StatusNotFound {
		t.Errorf("stop nope = %d", code)
	}
}

func TestRemoveSandbox(t *testing.T) {
	srv, d := newAPI(t)
	_, web, _ := post(t, srv, `{"name":"web"}`)
	_, db, _ := post(t, srv, `{"name":"db"}`)

	if code, body := call(t, srv, http.MethodDelete, "/v1/sandboxes/web"); code != http.StatusConflict || !strings.Contains(body, `sandbox \"web\" is running`) {
		t.Errorf("rm running = %d %s, want 409", code, body)
	}
	d.mu.Lock()
	if d.n.remove != 0 || !d.up[web.ID] {
		t.Errorf("refused rm touched the driver: %d removes, running %v", d.n.remove, d.up[web.ID])
	}
	d.mu.Unlock()

	d.stopErr = fmt.Errorf("stop: %w", backend.ErrUncleanStop)
	code, body := call(t, srv, http.MethodDelete, "/v1/sandboxes/web?force=1")
	var got Sandbox
	json.Unmarshal([]byte(body), &got)
	if code != http.StatusOK || got.ID != web.ID || !strings.Contains(got.Warning, "not stopped cleanly") {
		t.Errorf("rm --force = %d %s", code, body)
	}
	d.stopErr = nil
	d.mu.Lock()
	if _, ok := d.spec[web.ID]; ok || d.n.stop != 1 {
		t.Errorf("driver still has web, or stopped it %d times", d.n.stop)
	}
	d.mu.Unlock()
	if code, _ := call(t, srv, http.MethodGet, "/v1/sandboxes/web"); code != http.StatusNotFound {
		t.Errorf("GET after rm = %d", code)
	}

	// A stopped sandbox needs no force.
	d.Stop(context.Background(), db.ID)
	if code, body := call(t, srv, http.MethodDelete, "/v1/sandboxes/db"); code != http.StatusOK {
		t.Errorf("rm stopped = %d %s", code, body)
	}
	if l := list(t, srv); len(l) != 0 {
		t.Errorf("list after rm = %+v", l)
	}
	if code, _ := call(t, srv, http.MethodDelete, "/v1/sandboxes/db"); code != http.StatusNotFound {
		t.Errorf("second rm = %d", code)
	}
	// The name is free again.
	if code, sb, body := post(t, srv, `{"name":"web"}`); code != http.StatusCreated || sb.ID == web.ID {
		t.Errorf("recreate = %d %s", code, body)
	}
}

func TestStartSandbox(t *testing.T) {
	srv, d, st := newAPIStore(t)
	_, sb, _ := post(t, srv, `{"name":"web"}`)
	call(t, srv, http.MethodPost, "/v1/sandboxes/web/stop")
	code, body := call(t, srv, http.MethodPost, "/v1/sandboxes/web/start")
	var got Sandbox
	json.Unmarshal([]byte(body), &got)
	if code != http.StatusOK || got.State != backend.StateRunning || got.ID != sb.ID {
		t.Fatalf("start = %d %s", code, body)
	}
	d.mu.Lock()
	if !d.up[sb.ID] || d.n.start != 2 || d.n.create != 1 {
		t.Errorf("driver: running %v, %d starts, %d creates", d.up[sb.ID], d.n.start, d.n.create)
	}
	d.mu.Unlock()
	if rec, err := st.Get(context.Background(), sb.ID); err != nil || rec.State != backend.StateRunning {
		t.Errorf("record = %+v, %v", rec, err)
	}
	if code, _ := call(t, srv, http.MethodPost, "/v1/sandboxes/nope/start"); code != http.StatusNotFound {
		t.Errorf("start nope = %d", code)
	}
	// The driver lost the sandbox: start fails and the record stays stopped.
	call(t, srv, http.MethodPost, "/v1/sandboxes/web/stop")
	d.Remove(context.Background(), sb.ID)
	if code, _ := call(t, srv, http.MethodPost, "/v1/sandboxes/web/start"); code != http.StatusInternalServerError {
		t.Errorf("start missing = %d", code)
	}
	if rec, _ := st.Get(context.Background(), sb.ID); rec.State != backend.StateStopped {
		t.Errorf("record after failed start = %s", rec.State)
	}
}
