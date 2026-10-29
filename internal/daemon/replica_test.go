//go:build linux || darwin

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"boxpool.run/boxpool/internal/auth"
	"boxpool.run/boxpool/internal/store"
)

type memSink struct{ bytes.Buffer }

func (*memSink) ID() string    { return "" }
func (*memSink) Cancel() error { return nil }
func (*memSink) Close() error  { return nil }

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), StoreName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestFSM(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	f := fsm{st}
	t0 := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	log := []command{
		{Op: "state", State: &store.PoolState{Tokens: []store.JoinToken{{Hash: "old", ExpiresAt: t0.Add(time.Hour)}}}},
		{Op: "token", Hash: "new", Time: t0.Add(time.Hour)},
		{Op: "node", Node: &store.Node{ID: "n1", Name: "mac", OS: "darwin", Arch: "arm64", JoinedAt: t0}},
		{Op: "report", Node: &store.Node{ID: "n1", Report: []byte(`{"cpu":2}`), SeenAt: t0}},
		{Op: "use", Hash: "old", Time: t0},
	}
	apply := func() {
		t.Helper()
		for _, c := range log {
			b, _ := json.Marshal(c)
			if res := f.Apply(&raft.Log{Data: b}); res != nil {
				t.Fatalf("Apply(%s) = %v", c.Op, res)
			}
		}
	}
	apply()
	want := store.PoolState{
		Nodes:  []store.Node{{ID: "n1", Name: "mac", OS: "darwin", Arch: "arm64", JoinedAt: t0, Report: []byte(`{"cpu":2}`), SeenAt: t0}},
		Tokens: []store.JoinToken{{Hash: "new", ExpiresAt: t0.Add(time.Hour)}},
	}
	if got, err := st.PoolState(ctx); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("state = %+v, %v; want %+v", got, err, want)
	}
	// A change's error is its result; the token was used once.
	b, _ := json.Marshal(command{Op: "use", Hash: "old", Time: t0})
	if err, _ := f.Apply(&raft.Log{Data: b}).(error); !errors.Is(err, store.ErrTokenInvalid) {
		t.Errorf("second use = %v, want ErrTokenInvalid", err)
	}
	// Raft replays the log over the tables on start: the same state.
	apply()
	if got, err := st.PoolState(ctx); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("state after a replay = %+v, %v; want %+v", got, err, want)
	}

	snap, err := f.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var sink memSink
	if err := snap.Persist(&sink); err != nil {
		t.Fatal(err)
	}
	other := openStore(t)
	other.CreateJoinToken(ctx, "stale", t0.Add(time.Hour))
	if err := (fsm{other}).Restore(io.NopCloser(&sink)); err != nil {
		t.Fatal(err)
	}
	if got, err := other.PoolState(ctx); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("restored state = %+v, %v; want %+v", got, err, want)
	}
}

// fastPool shortens the pool's timers for a test.
func fastPool(t *testing.T) {
	for v, d := range map[*time.Duration]time.Duration{&heartbeatInterval: 50 * time.Millisecond, &unreachableAfter: 300 * time.Millisecond, &tunnelPoll: 200 * time.Millisecond, &tunnelWait: 200 * time.Millisecond} {
		old := *v
		*v = d
		t.Cleanup(func() { *v = old })
	}
}

// poolDaemon is a machine of a pool in this process: its local API, its
// pool port on loopback, and its node side, until stop.
type poolDaemon struct {
	local *httptest.Server
	s     *server
	stop  func()
}

func startPoolDaemon(t *testing.T, label string) *poolDaemon {
	t.Helper()
	local, s, _ := poolMachine(t, "linux")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.poolAddr = l.Addr().String()
	l.Close()
	if code, body := send(t, local, "PUT", "/v1/labels", `{"labels":["`+label+`"]}`); code != http.StatusOK {
		t.Fatalf("set labels = %d %s", code, body)
	}
	return runPool(t, local, s)
}

// runPool serves s's pool port and node side, resuming its replica as Run
// does.
func runPool(t *testing.T, local *httptest.Server, s *server) *poolDaemon {
	t.Helper()
	if me, ok := s.storedIdentity(); ok {
		if err := s.startRaft(me, raftResume); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.servePool(); err != nil {
		t.Fatal(err)
	}
	stopNode := serveNode(s)
	var once sync.Once
	p := &poolDaemon{local: local, s: s, stop: func() { once.Do(func() { stopNode(); s.closePool() }) }}
	t.Cleanup(p.stop)
	return p
}

// restart runs a new daemon on p's stopped machine.
func restart(t *testing.T, p *poolDaemon) *poolDaemon {
	t.Helper()
	s := newServer(p.s.info, p.s.st, p.s.d)
	s.capacity.Probe, s.poolAddr = p.s.capacity.Probe, p.s.poolAddr
	t.Cleanup(s.closePool)
	local := httptest.NewServer(s.handler())
	t.Cleanup(local.Close)
	return runPool(t, local, s)
}

func token(t *testing.T, head *poolDaemon) string {
	t.Helper()
	code, body := send(t, head.local, "POST", "/v1/tokens", `{"addr":"`+head.s.poolAddr+`"}`)
	var tok Token
	if err := json.Unmarshal([]byte(body), &tok); code != http.StatusCreated || err != nil {
		t.Fatalf("create token = %d %s", code, body)
	}
	return tok.Token
}

func joinTo(t *testing.T, head, node *poolDaemon) {
	t.Helper()
	if code, body := send(t, node.local, "POST", "/v1/pool/join", `{"token":"`+token(t, head)+`"}`); code != http.StatusOK {
		t.Fatalf("join = %d %s", code, body)
	}
}

// replicas returns the IDs in p's raft configuration that vote, and those
// that do not.
func replicas(p *poolDaemon) (voters, others []string) {
	rep := p.s.raft.Load()
	if rep == nil {
		return nil, nil
	}
	f := rep.GetConfiguration()
	if f.Error() != nil {
		return nil, nil
	}
	for _, sv := range f.Configuration().Servers {
		if sv.Suffrage == raft.Voter {
			voters = append(voters, string(sv.ID))
		} else {
			others = append(others, string(sv.ID))
		}
	}
	return voters, others
}

// sandboxes is p's list of sandboxes, name=state, sorted, or "" if it
// does not answer.
func sandboxes(t *testing.T, p *poolDaemon) string {
	t.Helper()
	code, body := call(t, p.local, http.MethodGet, "/v1/sandboxes")
	var l []Sandbox
	if code != http.StatusOK || json.Unmarshal([]byte(body), &l) != nil {
		return ""
	}
	got := strings.Fields(states(l))
	slices.Sort(got)
	return strings.Join(got, " ")
}

func nodeStates(t *testing.T, head *poolDaemon) map[string]string {
	t.Helper()
	code, body := send(t, head.local, "GET", "/v1/nodes", "")
	var nodes []Node
	if code != http.StatusOK || json.Unmarshal([]byte(body), &nodes) != nil {
		return nil
	}
	m := map[string]string{}
	for _, n := range nodes {
		m[n.ID] = n.State
	}
	return m
}

func revoke(t *testing.T, p *poolDaemon, tok string) (int, string) {
	t.Helper()
	parsed, err := auth.ParseJoinToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	return send(t, p.local, "DELETE", "/v1/tokens/"+parsed.Hash(), "")
}

// TestFailover runs a pool of three in this process: they replicate the
// head's state with votes, and when the head stops, a new one is elected
// within 15 s with every sandbox and token.
func TestFailover(t *testing.T) {
	fastPool(t)
	a, b, c := startPoolDaemon(t, "a"), startPoolDaemon(t, "b"), startPoolDaemon(t, "c")
	joinTo(t, a, b)
	joinTo(t, a, c)
	waitFor(t, "three voting replicas", func() bool { v, o := replicas(a); return len(v) == 3 && len(o) == 0 })
	for _, name := range []string{"a", "b", "c"} {
		if code, _, body := post(t, a.local, `{"name":"on-`+name+`","node":"`+name+`"}`); code != http.StatusCreated {
			t.Fatalf("create on %s = %d %s", name, code, body)
		}
	}
	tok := token(t, a)
	all := "on-a=running on-b=running on-c=running"
	waitFor(t, "the replicas to list every sandbox", func() bool { return sandboxes(t, b) == all && sandboxes(t, c) == all })

	a.stop()
	stopped := time.Now()
	var head, other *poolDaemon
	for head == nil {
		switch {
		case b.s.leading.Load():
			head, other = b, c
		case c.s.leading.Load():
			head, other = c, b
		case time.Since(stopped) > 15*time.Second:
			t.Fatal("no new head within 15 s")
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}
	t.Logf("new head after %v", time.Since(stopped).Round(time.Millisecond))
	aID, otherID := a.s.selfID(), other.s.selfID()
	waitFor(t, "the old head unreachable and the other node reporting", func() bool {
		st := nodeStates(t, head)
		return st[aID] == "unreachable" && st[otherID] == "ready"
	})
	// ls works from both, with the old head's sandbox kept.
	want := "on-a=unreachable on-b=running on-c=running"
	waitFor(t, "both to list every sandbox", func() bool { return sandboxes(t, head) == want && sandboxes(t, other) == want })
	// The new head forwards to the other node once its tunnels follow.
	onOther := "/v1/sandboxes/on-" + map[*poolDaemon]string{b: "b", c: "c"}[other]
	waitFor(t, "the new head to reach the other node", func() bool { code, _ := call(t, head.local, "GET", onOther); return code == http.StatusOK })
	if code, body := call(t, head.local, "POST", onOther+"/stop"); code != http.StatusOK {
		t.Errorf("stop through the new head = %d %s", code, body)
	}
	if code, body := revoke(t, head, tok); code != http.StatusNoContent {
		t.Errorf("revoke a token from before the failover = %d %s", code, body)
	}
	if code, body := revoke(t, other, tok); code != http.StatusConflict || !strings.Contains(body, "revoke join tokens on the head") {
		t.Errorf("revoke on a replica = %d %s", code, body)
	}

	// The original head never received a heartbeat response while leading,
	// so it has no saved peers. Raft must still direct its reports and
	// tunnels to the elected head after it restarts as a follower.
	if peers, err := auth.Peers(a.s.info.StateDir); err != nil || len(peers) != 0 {
		t.Fatalf("original head peers = %v, %v; want none", peers, err)
	}
	a = restart(t, a)
	waitFor(t, "the original head to report to the elected head", func() bool {
		return !a.s.leading.Load() && a.s.reportTo() == head.s.poolAddr && nodeStates(t, head)[aID] == "ready"
	})
	waitFor(t, "the elected head to reach the original head's sandbox", func() bool {
		code, _ := call(t, head.local, "GET", "/v1/sandboxes/on-a")
		return code == http.StatusOK
	})
}

// localHead starts a single-voter pool without periodic reports, so a
// successful mutation must commit its inventory rather than wait for a tick.
func localHead(t *testing.T) (*httptest.Server, *server) {
	t.Helper()
	old := heartbeatInterval
	heartbeatInterval = time.Hour
	t.Cleanup(func() { heartbeatInterval = old })
	local, s, _ := poolMachine(t, "linux")
	if err := s.bootstrap(context.Background(), "127.0.0.1:"+auth.JoinPort); err != nil {
		t.Fatal(err)
	}
	return local, s
}

func TestLocalSandboxReplication(t *testing.T) {
	local, s := localHead(t)
	for _, step := range []struct {
		name, method, path, body, want string
		status                         int
	}{
		{"create", "POST", "/v1/sandboxes", `{"name":"local"}`, "local=running", http.StatusCreated},
		{"pause", "POST", "/v1/sandboxes/local/pause", "", "local=paused", http.StatusOK},
		{"resume", "POST", "/v1/sandboxes/local/resume", "", "local=running", http.StatusOK},
		{"stop", "POST", "/v1/sandboxes/local/stop", "", "local=stopped", http.StatusOK},
		{"start", "POST", "/v1/sandboxes/local/start", "", "local=running", http.StatusOK},
		{"delete", "DELETE", "/v1/sandboxes/local?force=true", "", "", http.StatusOK},
	} {
		t.Run(step.name, func(t *testing.T) {
			if code, body := send(t, local, step.method, step.path, step.body); code != step.status {
				t.Fatalf("mutation = %d %s", code, body)
			}
			state, err := s.st.PoolState(context.Background())
			if err != nil || len(state.Nodes) != 1 {
				t.Fatalf("replicated state = %+v, %v", state, err)
			}
			var report Report
			if err := json.Unmarshal(state.Nodes[0].Report, &report); err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(states(report.Sandboxes)); got != step.want {
				t.Fatalf("inventory at acknowledgement = %q, want %q", got, step.want)
			}
		})
	}
}

type loseLeadershipDriver struct {
	Driver
	stopRaft func() error
}

func (d loseLeadershipDriver) Start(ctx context.Context, id string) error {
	if err := d.Driver.Start(ctx, id); err != nil {
		return err
	}
	return d.stopRaft()
}

func TestLocalSandboxReplicationLosesLeadership(t *testing.T) {
	local, s := localHead(t)
	s.d = loseLeadershipDriver{Driver: s.d, stopRaft: s.stopRaft}
	code, _, body := post(t, local, `{"name":"local"}`)
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "could not be replicated") {
		t.Fatalf("create after leadership loss = %d %s", code, body)
	}
}

func TestLocalSandboxImmediateFailover(t *testing.T) {
	fastPool(t)
	a, b, c := startPoolDaemon(t, "a"), startPoolDaemon(t, "b"), startPoolDaemon(t, "c")
	joinTo(t, a, b)
	joinTo(t, a, c)
	waitFor(t, "three voting replicas", func() bool { v, o := replicas(a); return len(v) == 3 && len(o) == 0 })
	if code, _, body := post(t, a.local, `{"name":"just-created","node":"a"}`); code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}
	// No wait for a heartbeat or for followers to apply the report.
	a.stop()
	waitFor(t, "a new head", func() bool { return b.s.leading.Load() || c.s.leading.Load() })
	head := b
	if c.s.leading.Load() {
		head = c
	}
	waitFor(t, "the acknowledged sandbox on the new head", func() bool {
		return sandboxes(t, head) == "just-created="+string(StateUnreachable)
	})
	if code, _, body := post(t, head.local, `{"name":"just-created"}`); code != http.StatusBadGateway || !strings.Contains(body, "unreachable") {
		t.Fatalf("create with the unreachable sandbox's name = %d %s", code, body)
	}
}

// TestPromote runs a pool of two in this process: the second machine keeps
// a replica without a vote, promote refuses while the head answers, makes
// it the head once the head is gone, and the old head, back, follows it.
func TestPromote(t *testing.T) {
	fastPool(t)
	a, b := startPoolDaemon(t, "a"), startPoolDaemon(t, "b")
	joinTo(t, a, b)
	waitFor(t, "a replica without a vote", func() bool { v, o := replicas(a); return len(v) == 1 && len(o) == 1 })
	for _, name := range []string{"a", "b"} {
		if code, _, body := post(t, a.local, `{"name":"on-`+name+`","node":"`+name+`"}`); code != http.StatusCreated {
			t.Fatalf("create on %s = %d %s", name, code, body)
		}
	}
	tok := token(t, a)
	waitFor(t, "the replica to list every sandbox", func() bool { return sandboxes(t, b) == "on-a=running on-b=running" })

	if code, body := send(t, b.local, "POST", "/v1/pool/promote", ""); code != http.StatusConflict || !strings.Contains(body, "still reachable") {
		t.Errorf("promote with the head up = %d %s", code, body)
	}
	if code, body := send(t, a.local, "POST", "/v1/pool/promote", ""); code != http.StatusConflict || !strings.Contains(body, "already the head") {
		t.Errorf("promote on the head = %d %s", code, body)
	}
	a.stop()
	code, body := send(t, b.local, "POST", "/v1/pool/promote", "")
	var res PromoteResponse
	if json.Unmarshal([]byte(body), &res); code != http.StatusOK || res.Head != b.s.poolAddr || !b.s.leading.Load() {
		t.Fatalf("promote = %d %s", code, body)
	}
	if got := sandboxes(t, b); got != "on-a=unreachable on-b=running" {
		t.Errorf("new head's sandboxes = %s", got)
	}
	if code, body := revoke(t, b, tok); code != http.StatusNoContent {
		t.Errorf("revoke a token from before the promotion = %d %s", code, body)
	}

	// Back, the old head finds it was replaced, and follows the new one.
	a = restart(t, a)
	aID := a.s.selfID()
	waitFor(t, "the old head to report to the new one", func() bool {
		return !a.s.leading.Load() && nodeStates(t, b)[aID] == "ready" && sandboxes(t, b) == "on-a=running on-b=running"
	})
	waitFor(t, "the old head to keep a replica again", func() bool { _, o := replicas(b); return slices.Contains(o, aID) })
	if v, _ := replicas(b); !slices.Equal(v, []string{b.s.selfID()}) {
		t.Errorf("voters after the old head returned = %v", v)
	}
}
