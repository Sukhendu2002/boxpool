//go:build linux || darwin

package daemon

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/agent"
	"boxpool.run/boxpool/internal/backend"
	"boxpool.run/boxpool/internal/resources"
	"boxpool.run/boxpool/internal/store"
)

func TestReady(t *testing.T) {
	seen := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		seen time.Time
		now  time.Time
		want bool
	}{
		{"never reported", time.Time{}, seen, false},
		{"just reported", seen, seen, true},
		{"last heartbeat 30s ago", seen, seen.Add(unreachableAfter), false},
		{"missed heartbeats", seen, seen.Add(unreachableAfter + time.Second), false},
		{"reported again", seen.Add(time.Minute), seen.Add(time.Minute + heartbeatInterval), true},
	} {
		if got := ready(tc.seen, tc.now); got != tc.want {
			t.Errorf("%s: ready = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPick(t *testing.T) {
	node := func(id, osName, arch string, cpus int, mem int64, sandboxes int, labels ...string) member {
		return member{Node: store.Node{ID: id, Name: id, OS: osName, Arch: arch},
			Report: Report{CPUs: cpus, MemoryMB: mem, Labels: labels, Sandboxes: make([]Sandbox, sandboxes)}}
	}
	self := node("", "linux", "amd64", 4, 4096, 0)
	mac := node("mac", "darwin", "arm64", 6, 8192, 2)
	gpu := node("gpu", "linux", "amd64", 8, 2048, 0, "gpu", "always-on")
	twin := node("twin", "linux", "amd64", 4, 4096, 3)
	spec := backend.Spec{CPUs: 2, MemoryMB: 2048}
	for _, tc := range []struct {
		name  string
		cands []member
		req   CreateRequest
		spec  backend.Spec
		want  string // node ID, or an error substring after "error: "
	}{
		{"most free memory", []member{self, mac, gpu}, CreateRequest{}, spec, "mac"},
		{"os", []member{self, mac, gpu}, CreateRequest{OS: "linux"}, spec, ""},
		{"arch", []member{self, mac, gpu}, CreateRequest{Arch: "arm64"}, spec, "mac"},
		{"label", []member{self, mac, gpu}, CreateRequest{Node: "gpu"}, spec, "gpu"},
		{"ties go to fewer sandboxes", []member{twin, self}, CreateRequest{}, spec, ""},
		{"skips a node without the memory", []member{self, mac, gpu}, CreateRequest{}, backend.Spec{CPUs: 1, MemoryMB: 6144}, "mac"},
		{"skips a node without the CPUs", []member{self, mac, gpu}, CreateRequest{OS: "linux"}, backend.Spec{CPUs: 6, MemoryMB: 1024}, "gpu"},
		{"a lone match still needs capacity", []member{self, gpu}, CreateRequest{Node: "gpu"}, backend.Spec{CPUs: 16, MemoryMB: 65536}, "error: none of the 1 matching nodes has 16 CPUs"},
		{"nothing has the capacity", []member{self, gpu, twin}, CreateRequest{}, backend.Spec{CPUs: 16, MemoryMB: 1024}, "error: none of the 3 matching nodes has 16 CPUs"},
		{"no such os", []member{self, mac}, CreateRequest{OS: "windows"}, spec, "error: no reachable node matches --os windows"},
		{"constraints together", []member{self, mac, gpu}, CreateRequest{OS: "darwin", Node: "gpu"}, spec, "error: no reachable node matches --node gpu --os darwin"},
		{"no nodes", nil, CreateRequest{}, spec, "error: no reachable node matches the request"},
	} {
		got, err := pick(tc.cands, tc.req, tc.spec)
		if want, ok := strings.CutPrefix(tc.want, "error: "); ok {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s: pick = %q, %v; want an error with %q", tc.name, got.ID, err, want)
			}
		} else if err != nil || got.ID != tc.want {
			t.Errorf("%s: pick = %q, %v; want %q", tc.name, got.ID, err, tc.want)
		}
	}
}

func TestLabels(t *testing.T) {
	local, s := machine(t, "linux")
	for _, tc := range []struct {
		body string
		code int
		want string
	}{
		{`{"labels":["gpu","always-on","gpu"]}`, http.StatusOK, "always-on,gpu"},
		{`{"labels":["GPU"]}`, http.StatusBadRequest, "always-on,gpu"},
		{`{"labels":["has space"]}`, http.StatusBadRequest, "always-on,gpu"},
		{`{"labels":["` + strings.Repeat("a", 64) + `"]}`, http.StatusBadRequest, "always-on,gpu"},
		{`not json`, http.StatusBadRequest, "always-on,gpu"},
		{`{"labels":[]}`, http.StatusOK, ""},
	} {
		if code, body := send(t, local, "PUT", "/v1/labels", tc.body); code != tc.code {
			t.Fatalf("set labels %s = %d %s", tc.body, code, body)
		}
		got, err := readLabels(s.info.StateDir)
		if err != nil || strings.Join(got, ",") != tc.want {
			t.Errorf("saved labels = %v, %v; want %q", got, err, tc.want)
		}
	}
	fi, err := os.Stat(filepath.Join(s.info.StateDir, labelsName))
	if err != nil || fi.Mode().Perm() != 0600 {
		t.Errorf("labels file = %v, %v; want mode 0600", fi, err)
	}
}

func TestReportsSurviveRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, StoreName)
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.CreateNode(ctx, store.Node{Name: "sleeping-mac", OS: "darwin", Arch: "arm64"})
	if err != nil {
		t.Fatal(err)
	}
	report, _ := json.Marshal(Report{CPUs: 3, MemoryMB: 2048, TotalCPUs: 7, TotalMemoryMB: 8192,
		Warning: "on battery", Sandboxes: []Sandbox{{ID: "a", Name: "kept", State: backend.StateRunning}}})
	if err := st.SetNodeReport(ctx, node.ID, report, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := httptest.NewServer(Handler(Info{StateDir: dir}, st, nil))
	defer srv.Close()
	if got := states(list(t, srv)); got != "kept=unreachable" {
		t.Errorf("list after head restart = %s", got)
	}
	_, body := call(t, srv, "GET", "/v1/nodes")
	var nodes []Node
	if err := json.Unmarshal([]byte(body), &nodes); err != nil || len(nodes) != 2 {
		t.Fatalf("nodes after restart = %s, %v", body, err)
	}
	if n := nodes[1]; n.State != "unreachable" || n.TotalCPUs != 7 || n.TotalMemoryMB != 8192 || n.Warning != "on battery" {
		t.Errorf("node report after restart = %+v", n)
	}
	// On reconnect, the node's new report is authoritative, including
	// sandboxes removed or created while it was offline.
	report, _ = json.Marshal(Report{Sandboxes: []Sandbox{{ID: "b", Name: "new", State: backend.StatePaused}}})
	if err := st.SetNodeReport(ctx, node.ID, report, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := states(list(t, srv)); got != "new=paused" {
		t.Errorf("list after reconnect = %s", got)
	}
}

func TestTrackCapacity(t *testing.T) {
	_, s := machine(t, "linux")
	ctx := context.Background()
	node, err := s.st.CreateNode(ctx, store.Node{Name: "worker", OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	report, _ := json.Marshal(Report{CPUs: 4, MemoryMB: 4096})
	if err := s.st.SetNodeReport(ctx, node.ID, report, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path string
		cpu          int
		memory       int64
		count        int
	}{
		{"POST", "/v1/sandboxes", 2, 3584, 1},
		{"POST", "/v1/sandboxes", 2, 3584, 1},          // attach, no new reservation
		{"POST", "/v1/sandboxes/box/stop", 2, 3584, 1}, // stop retains resources
		{"DELETE", "/v1/sandboxes/box", 4, 4096, 0},
	} {
		req, _ := http.NewRequest(tc.method, "http://node"+tc.path, nil)
		resp := &http.Response{StatusCode: http.StatusOK, Request: req,
			Body: io.NopCloser(strings.NewReader(`{"id":"a","name":"box","cpu":2,"mem_mb":512}`))}
		if err := s.track(resp, node.ID); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		members, err := s.members(ctx)
		if err != nil || len(members) != 1 {
			t.Fatalf("members = %+v, %v", members, err)
		}
		m := members[0]
		if m.CPUs != tc.cpu || m.MemoryMB != tc.memory || len(m.Sandboxes) != tc.count {
			t.Errorf("after %s %s: report = %+v", tc.method, tc.path, m.Report)
		}
	}
}

// poolMachine is a daemon with a fake backend on a short state directory,
// for its agent's socket.
func poolMachine(t *testing.T, osName string) (*httptest.Server, *server, *fakeDriver) {
	t.Helper()
	dir, err := os.MkdirTemp("", "bp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	st, err := store.Open(filepath.Join(dir, StoreName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	d := newFakeDriver(t, dir)
	s := newServer(Info{OS: osName, Arch: "arm64", StateDir: dir}, st, d)
	t.Cleanup(s.closePool) // before the store closes
	s.capacity.Probe = func(string) (resources.Capacity, error) {
		return resources.Capacity{CPUs: []int{0, 1, 2, 3, 4, 5, 6, 7}, MemoryMB: 16384, AvailableMB: 16384}, nil
	}
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	return srv, s, d
}

// serveNode runs the node side of the pool for s until the returned stop.
func serveNode(s *server) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	srv := &http.Server{Handler: s.handler()}
	done := make(chan struct{})
	go func() { s.serveHead(ctx, srv); close(done) }()
	return func() { cancel(); srv.Close(); <-done }
}

func nodeState(t *testing.T, head *httptest.Server) string {
	t.Helper()
	_, body := send(t, head, "GET", "/v1/nodes", "")
	var nodes []Node
	if err := json.Unmarshal([]byte(body), &nodes); err != nil || len(nodes) != 2 {
		t.Fatalf("nodes = %s", body)
	}
	return nodes[1].State
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !ok(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// TestPoolRouting runs a Linux head and a Mac node in this process: a
// sandbox placed on the node by OS is listed, exec'd into, copied to and
// from, stopped, resumed and removed through the head, and listed
// unreachable while the node is away.
func TestPoolRouting(t *testing.T) {
	fastPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	head, headServer, headDriver := poolMachine(t, "linux")
	logShutdown, cancelFollowers := context.WithCancel(ctx)
	headServer.logShutdown = logShutdown
	defer cancelFollowers()
	pool := poolServer(t, headServer, nil)
	node, nodeServer, nodeDriver := poolMachine(t, "darwin")
	consolePath := filepath.Join(nodeServer.info.StateDir, "console.log")
	if err := os.WriteFile(consolePath, []byte("remote console\n"), 0600); err != nil {
		t.Fatal(err)
	}
	nodeServer.d = &consoleDriver{nodeDriver, consolePath}
	if code, body := send(t, node, "PUT", "/v1/labels", `{"labels":["gpu","always-on","gpu"]}`); code != http.StatusOK || body != `{"labels":["always-on","gpu"]}`+"\n" {
		t.Fatalf("set labels = %d %s", code, body)
	}
	if code, body := send(t, node, "POST", "/v1/pool/join", `{"token":"`+issueToken(t, head, pool)+`"}`); code != http.StatusOK {
		t.Fatalf("join = %d %s", code, body)
	}
	stop := serveNode(nodeServer)
	defer func() { stop() }()
	waitFor(t, "the node's first heartbeat", func() bool { return nodeState(t, head) == "ready" })

	// Placed by OS, then by label; an impossible request says why.
	code, sb, body := post(t, head, `{"name":"mac","os":"macos"}`)
	if code != http.StatusCreated || sb.Name != "mac" || len(nodeDriver.spec) != 1 || len(headDriver.spec) != 0 {
		t.Fatalf("create --os macos = %d %s; node has %d, head %d", code, body, len(nodeDriver.spec), len(headDriver.spec))
	}
	if code, _, body := post(t, head, `{"name":"gpu","node":"gpu"}`); code != http.StatusCreated || len(nodeDriver.spec) != 2 {
		t.Fatalf("create --node gpu = %d %s", code, body)
	}
	if code, _, body := post(t, head, `{"name":"local","os":"linux"}`); code != http.StatusCreated || len(headDriver.spec) != 1 {
		t.Fatalf("create --os linux = %d %s", code, body)
	}
	if code, _, body := post(t, head, `{"os":"windows"}`); code != http.StatusConflict || !strings.Contains(body, "no reachable node matches --os windows") {
		t.Errorf("create --os windows = %d %s", code, body)
	}
	// Creating an existing name attaches to it on its node, whatever the
	// constraints.
	if code, _, body := post(t, head, `{"name":"mac"}`); code != http.StatusOK || len(nodeDriver.spec) != 2 {
		t.Errorf("create an existing remote sandbox = %d %s", code, body)
	}
	if got := states(list(t, head)); got != "local=running gpu=running mac=running" && got != "local=running mac=running gpu=running" {
		t.Errorf("head list = %s", got)
	}
	for _, sb := range list(t, head) {
		wantOS := "darwin"
		if sb.Name == "local" {
			wantOS = "linux"
		}
		if sb.OS != wantOS {
			t.Errorf("sandbox %s OS = %q, want %q", sb.Name, sb.OS, wantOS)
		}
		if (sb.Node == "") != (sb.Name == "local") {
			t.Errorf("head lists sandbox %s on node %q", sb.Name, sb.Node)
		}
	}

	if code, body := call(t, head, "GET", "/v1/sandboxes/mac/logs"); code != http.StatusOK || body != "remote console\n" {
		t.Errorf("logs through the head = %d %q", code, body)
	}
	reqLogs, _ := http.NewRequestWithContext(ctx, "GET", head.URL+"/v1/sandboxes/mac/logs?follow=true", nil)
	respLogs, err := head.Client().Do(reqLogs)
	if err != nil {
		t.Fatal(err)
	}
	logBytes := make([]byte, len("remote console\n"))
	if _, err := io.ReadFull(respLogs.Body, logBytes); err != nil || string(logBytes) != "remote console\n" {
		t.Fatalf("follow through the head = %q, %v", logBytes, err)
	}
	cancelFollowers()
	logsDone := make(chan struct{})
	go func() { io.Copy(io.Discard, respLogs.Body); close(logsDone) }()
	select {
	case <-logsDone:
	case <-time.After(time.Second):
		t.Error("head shutdown did not cancel the remote log follower")
	}
	respLogs.Body.Close()
	<-logsDone

	// exec, over a WebSocket through the tunnel.
	conn, _, err := execWS(ctx, t, head, "mac")
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	session, err := agent.Start(ctx, conn, agent.ExecRequest{Argv: []string{"sh", "-c", "echo hi; exit 3"}}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if code, err := session.Wait(); code != 3 || err != nil || stdout.String() != "hi\n" {
		t.Errorf("exec through the head = %d, %v, %q", code, err, stdout.String())
	}

	// cp both ways.
	guest := t.TempDir()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	tw.WriteHeader(&tar.Header{Name: "hello", Typeflag: tar.TypeReg, Mode: 0644, Size: 3})
	tw.Write([]byte("hi\n"))
	tw.Close()
	req, _ := http.NewRequest(http.MethodPut, filesURL(head.URL, "mac", guest), &b)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got, _ := os.ReadFile(filepath.Join(guest, "hello")); resp.StatusCode != http.StatusNoContent || string(got) != "hi\n" {
		t.Errorf("upload through the head = %d, %q", resp.StatusCode, got)
	}
	resp, err = http.Get(filesURL(head.URL, "mac", filepath.Join(guest, "hello")))
	if err != nil {
		t.Fatal(err)
	}
	h, err := tar.NewReader(resp.Body).Next()
	resp.Body.Close()
	if err != nil || h.Name != "hello" {
		t.Errorf("download through the head = %v, %v", h, err)
	}

	// Lifecycle commands, and the head's list follows at once.
	for _, tc := range []struct{ method, path, want string }{
		{"POST", "/v1/sandboxes/mac/stop", "stopped"},
		{"POST", "/v1/sandboxes/mac/start", "running"},
		{"POST", "/v1/sandboxes/mac/pause", "paused"},
		{"POST", "/v1/sandboxes/mac/resume", "running"},
		{"POST", "/v1/sandboxes/mac/stop", "stopped"},
	} {
		code, body := call(t, head, tc.method, tc.path)
		var got Sandbox
		json.Unmarshal([]byte(body), &got)
		if code != http.StatusOK || got.State != backend.State(tc.want) || !strings.Contains(states(list(t, head)), "mac="+tc.want) {
			t.Errorf("%s %s = %d %s; list %s", tc.method, tc.path, code, body, states(list(t, head)))
		}
	}
	if code, body := call(t, head, "DELETE", "/v1/sandboxes/mac"); code != http.StatusOK || len(nodeDriver.spec) != 1 || strings.Contains(states(list(t, head)), "mac=") {
		t.Errorf("rm through the head = %d %s; list %s", code, body, states(list(t, head)))
	}

	// Offline: listed unreachable, kept, and requests say which node.
	stop()
	waitFor(t, "the node to be unreachable", func() bool { return nodeState(t, head) == "unreachable" })
	if got := states(list(t, head)); got != "local=running gpu=unreachable" {
		t.Errorf("head list with the node away = %s", got)
	}
	if code, body := call(t, head, "POST", "/v1/sandboxes/gpu/stop"); code != http.StatusBadGateway || !strings.Contains(body, "is unreachable") {
		t.Errorf("stop on an unreachable node = %d %s", code, body)
	}
	if code, _, body := post(t, head, `{"os":"darwin"}`); code != http.StatusConflict {
		t.Errorf("create on an unreachable node's OS = %d %s", code, body)
	}
	stop = serveNode(nodeServer)
	waitFor(t, "the node to return", func() bool { return nodeState(t, head) == "ready" })
	if got := states(list(t, head)); got != "local=running gpu=running" {
		t.Errorf("head list with the node back = %s", got)
	}
}
