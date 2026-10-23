//go:build linux || darwin

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"boxpool.run/boxpool/internal/agent"
	"boxpool.run/boxpool/internal/backend"
	"boxpool.run/boxpool/internal/store"
)

// fakeDriver keeps sandboxes in memory, and Dial connects to a real agent
// on a unix socket, running commands on this machine.
type fakeDriver struct {
	sock string
	mu   sync.Mutex
	spec map[string]backend.Spec
	up   map[string]bool
	n    struct{ create, start int }
	conn net.Conn // the last connection Dial returned
}

func (d *fakeDriver) Name() string { return "fake" }

func (d *fakeDriver) Create(ctx context.Context, id string, spec backend.Spec) (backend.Sandbox, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.spec[id]; ok {
		return backend.Sandbox{}, fmt.Errorf("sandbox %s already exists", id)
	}
	d.spec[id] = spec
	d.n.create++
	return backend.Sandbox{ID: id, Name: spec.Name, State: backend.StateStopped}, nil
}

func (d *fakeDriver) Start(ctx context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.spec[id]; !ok {
		return fmt.Errorf("no sandbox %s", id)
	}
	d.up[id] = true
	d.n.start++
	return nil
}

func (d *fakeDriver) Stop(ctx context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.up[id] = false
	return nil
}

func (d *fakeDriver) Pause(context.Context, string) error             { return backend.ErrNotSupported }
func (d *fakeDriver) Resume(context.Context, string) error            { return backend.ErrNotSupported }
func (d *fakeDriver) Remove(context.Context, string) error            { return backend.ErrNotSupported }
func (d *fakeDriver) List(context.Context) ([]backend.Sandbox, error) { return nil, nil }

func (d *fakeDriver) Dial(ctx context.Context, id string) (net.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.up[id] {
		return nil, fmt.Errorf("sandbox %s is not running", id)
	}
	c, err := (&net.Dialer{}).DialContext(ctx, "unix", d.sock)
	d.conn = c
	return c, err
}

// newAPI serves the API with a fake driver and a fresh store.
func newAPI(t *testing.T) (*httptest.Server, *fakeDriver) {
	t.Helper()
	tmp, err := os.MkdirTemp("", "bp") // short: macOS caps socket paths at ~104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	st, err := store.Open(filepath.Join(tmp, StoreName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	d := &fakeDriver{sock: filepath.Join(tmp, "agent.sock"), spec: map[string]backend.Spec{}, up: map[string]bool{}}
	l, err := agent.Listen("unix:" + d.sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- agent.Serve(ctx, l, "test") }()
	t.Cleanup(func() { cancel(); <-served })
	srv := httptest.NewServer(Handler(Info{}, st, d))
	t.Cleanup(srv.Close)
	return srv, d
}

func post(t *testing.T, srv *httptest.Server, body string) (int, Sandbox, string) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/v1/sandboxes", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	b.ReadFrom(resp.Body)
	var sb Sandbox
	json.Unmarshal(b.Bytes(), &sb)
	return resp.StatusCode, sb, b.String()
}

func TestCreateSandbox(t *testing.T) {
	srv, d := newAPI(t)
	code, sb, body := post(t, srv, `{}`)
	if code != http.StatusCreated {
		t.Fatalf("POST = %d %s", code, body)
	}
	if !regexp.MustCompile(`^sb-[0-9a-f]{6}$`).MatchString(sb.Name) || sb.ID == "" || sb.Backend != "fake" || sb.State != backend.StateRunning ||
		sb.CPUs != DefaultCPUs || sb.MemoryMB != DefaultMemoryMB || sb.DiskGB != DefaultDiskGB {
		t.Errorf("POST {} = %+v", sb)
	}
	d.mu.Lock()
	if spec := d.spec[sb.ID]; spec.Name != sb.Name || spec.CPUs != DefaultCPUs || !d.up[sb.ID] {
		t.Errorf("driver has %+v, running %v", spec, d.up[sb.ID])
	}
	d.mu.Unlock()

	resp, err := http.Get(srv.URL + "/v1/sandboxes/" + sb.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got Sandbox
	if json.NewDecoder(resp.Body).Decode(&got); resp.StatusCode != http.StatusOK || !got.CreatedAt.Equal(sb.CreatedAt) || got.ID != sb.ID {
		t.Errorf("GET = %d %+v, want %+v", resp.StatusCode, got, sb)
	}
}

func TestCreateSandboxIdempotent(t *testing.T) {
	srv, d := newAPI(t)
	code, first, body := post(t, srv, `{"name":"web","cpu":1,"mem_mb":512,"disk_gb":3}`)
	if code != http.StatusCreated || first.CPUs != 1 || first.MemoryMB != 512 || first.DiskGB != 3 {
		t.Fatalf("first POST = %d %s", code, body)
	}
	d.Stop(context.Background(), first.ID)
	code, second, body := post(t, srv, `{"name":"web","cpu":4}`)
	if code != http.StatusOK || second.ID != first.ID || second.CPUs != 1 || second.State != backend.StateRunning {
		t.Errorf("second POST = %d %s, want the first sandbox, %+v", code, body, first)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.n.create != 1 || d.n.start != 2 || !d.up[first.ID] {
		t.Errorf("driver: %d creates, %d starts, running %v; want 1, 2 (the second restarts it), true", d.n.create, d.n.start, d.up[first.ID])
	}
}

func TestCreateSandboxConcurrent(t *testing.T) {
	srv, d := newAPI(t)
	ids := make(chan string, 4)
	for range 4 {
		go func() { _, sb, _ := post(t, srv, `{"name":"same"}`); ids <- sb.ID }()
	}
	first := <-ids
	for range 3 {
		if id := <-ids; id != first || id == "" {
			t.Errorf("concurrent creates returned %q and %q", first, id)
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.n.create != 1 {
		t.Errorf("driver got %d creates, want 1", d.n.create)
	}
}

func TestCreateSandboxInvalid(t *testing.T) {
	srv, d := newAPI(t)
	for _, body := range []string{
		`{"name":"Web"}`, `{"name":"-web"}`, `{"name":"a/b"}`, `{"name":"a b"}`, `{"name":"a.b"}`,
		`{"name":"` + strings.Repeat("a", 64) + `"}`, `{"cpu":-1}`, `{"mem_mb":-1}`, `not json`,
	} {
		if code, _, resp := post(t, srv, body); code != http.StatusBadRequest || !strings.Contains(resp, `"message"`) {
			t.Errorf("POST %s = %d %s, want 400 with a message", body, code, resp)
		}
	}
	d.mu.Lock()
	if d.n.create != 0 {
		t.Errorf("driver got %d creates", d.n.create)
	}
	d.mu.Unlock()
	if code, _, body := post(t, srv, `{"name":"`+strings.Repeat("a", 63)+`"}`); code != http.StatusCreated {
		t.Errorf("63-character name: %d %s", code, body)
	}
}

func TestSandboxNotFound(t *testing.T) {
	srv, _ := newAPI(t)
	for _, path := range []string{"/v1/sandboxes/nope", "/v1/sandboxes/nope/exec"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		var e Error
		json.NewDecoder(resp.Body).Decode(&e)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound || e.Message != `sandbox "nope" not found` {
			t.Errorf("GET %s = %d %q", path, resp.StatusCode, e.Message)
		}
	}
}

func TestNoBackend(t *testing.T) {
	for _, r := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/v1/sandboxes", strings.NewReader(`{}`)),
		httptest.NewRequest(http.MethodGet, "/v1/sandboxes/web", nil),
		httptest.NewRequest(http.MethodGet, "/v1/sandboxes/web/exec", nil),
	} {
		rec := httptest.NewRecorder()
		Handler(Info{}, nil, nil).ServeHTTP(rec, r)
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "boxpool doctor") {
			t.Errorf("%s %s = %d %s", r.Method, r.URL, rec.Code, rec.Body)
		}
	}
}

// execWS opens the daemon's exec WebSocket for the sandbox called name.
func execWS(ctx context.Context, t *testing.T, srv *httptest.Server, name string) (*websocket.Conn, *http.Response, error) {
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/v1/sandboxes/"+name+"/exec", nil)
}

func TestExec(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv, _ := newAPI(t)
	post(t, srv, `{"name":"web"}`)

	conn, _, err := execWS(ctx, t, srv, "web")
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	s, err := agent.Start(ctx, conn, agent.ExecRequest{Argv: []string{"sh", "-c", "echo hi; echo oops >&2; exit 3"}}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if code, err := s.Wait(); code != 3 || err != nil || stdout.String() != "hi\n" || stderr.String() != "oops\n" {
		t.Errorf("exec = %d, %v, stdout %q, stderr %q", code, err, stdout.String(), stderr.String())
	}

	// A resize reaches the agent's PTY.
	conn, _, err = execWS(ctx, t, srv, "web")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	s, err = agent.Start(ctx, conn, agent.ExecRequest{Argv: []string{"sh", "-c", "read x; stty size"}, TTY: true, Rows: 24, Cols: 80}, &out, &out)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Resize(40, 120); err != nil {
		t.Fatal(err)
	}
	s.Write([]byte("\r"))
	if code, err := s.Wait(); code != 0 || err != nil || !strings.Contains(out.String(), "40 120") {
		t.Errorf("stty size after resize = %d, %v, %q", code, err, out.String())
	}
}

func TestExecStoppedSandbox(t *testing.T) {
	srv, d := newAPI(t)
	_, sb, _ := post(t, srv, `{"name":"web"}`)
	d.Stop(context.Background(), sb.ID)
	_, resp, err := execWS(context.Background(), t, srv, "web")
	if err == nil || resp == nil || resp.StatusCode != http.StatusBadGateway {
		t.Errorf("exec on a stopped sandbox = %v, %v; want 502", resp, err)
	}
}

// TestExecClientGone checks that the command is killed when the client
// disconnects without closing the session.
func TestExecClientGone(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv, _ := newAPI(t)
	post(t, srv, `{"name":"web"}`)
	conn, _, err := execWS(ctx, t, srv, "web")
	if err != nil {
		t.Fatal(err)
	}
	out := make(writes, 16)
	if _, err := agent.Start(ctx, conn, agent.ExecRequest{Argv: []string{"sh", "-c", "echo $$; exec sleep 30"}}, out, out); err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(<-out))
	if err != nil {
		t.Fatal(err)
	}
	conn.CloseNow()
	for syscall.Kill(pid, 0) == nil {
		if ctx.Err() != nil {
			t.Fatalf("command %d still runs after the client went away", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestExecAgentGone checks that the client's session ends with an error
// when the connection to the agent drops.
func TestExecAgentGone(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv, d := newAPI(t)
	post(t, srv, `{"name":"web"}`)
	conn, _, err := execWS(ctx, t, srv, "web")
	if err != nil {
		t.Fatal(err)
	}
	out := make(writes, 16)
	s, err := agent.Start(ctx, conn, agent.ExecRequest{Argv: []string{"sh", "-c", "echo started; exec sleep 30"}}, out, out)
	if err != nil {
		t.Fatal(err)
	}
	<-out
	d.mu.Lock()
	d.conn.Close()
	d.mu.Unlock()
	if _, err := s.Wait(); err == nil || ctx.Err() != nil {
		t.Errorf("Wait() = %v (context: %v), want a lost connection before the deadline", err, ctx.Err())
	}
}

// writes receives each write as a string.
type writes chan string

func (w writes) Write(p []byte) (int, error) {
	w <- string(p)
	return len(p), nil
}
