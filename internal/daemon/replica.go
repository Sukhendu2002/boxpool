package daemon

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/raft"

	"boxpool.run/boxpool/internal/auth"
	"boxpool.run/boxpool/internal/store"
	"boxpool.run/boxpool/internal/transport"
)

// The head's state, its nodes with their last reports and its join tokens,
// is replicated with raft to up to maxReplicas machines, which also hold
// the head CA (#41). The raft leader is the head. With three or more
// machines in the pool, the replicas vote, so a new head is elected when
// the old one goes; with two, the second keeps a copy without a vote, as
// two voters would stop the head whenever the other machine slept, and
// boxpool promote makes it the head (#42).
//
// Raft runs on the pool port: a connection that negotiates raftProtocol
// is raft's, and is accepted only from a replica named in the raft
// configuration.
const (
	maxReplicas  = 3
	raftProtocol = "boxpool-raft"
	alwaysOn     = "always-on"
)

// command is a change to the head's state. Raft applies it on every
// replica, so it holds every value the change needs, such as times.
type command struct {
	Op    string           `json:"op"`
	Hash  string           `json:"hash,omitempty"`
	Time  time.Time        `json:"time,omitzero"`
	Node  *store.Node      `json:"node,omitempty"`
	State *store.PoolState `json:"state,omitempty"`
}

func (c command) apply(ctx context.Context, st *store.Store) error {
	switch c.Op {
	case "token":
		return st.CreateJoinToken(ctx, c.Hash, c.Time)
	case "use":
		return st.UseJoinToken(ctx, c.Hash, c.Time)
	case "revoke":
		return st.RevokeJoinToken(ctx, c.Hash)
	case "node":
		_, err := st.CreateNode(ctx, *c.Node)
		return err
	case "report":
		return st.SetNodeReport(ctx, c.Node.ID, c.Node.Report, c.Node.SeenAt)
	case "state":
		return st.SetPoolState(ctx, *c.State)
	}
	return fmt.Errorf("unknown pool change %q", c.Op)
}

// fsm applies commands to the store. Raft replays the log over the store's
// tables on start, which converges because the log begins with a "state"
// command and the last command on each row wins.
type fsm struct{ st *store.Store }

func (f fsm) Apply(l *raft.Log) any {
	var c command
	if err := json.Unmarshal(l.Data, &c); err != nil {
		return err
	}
	return c.apply(context.Background(), f.st)
}

func (f fsm) Snapshot() (raft.FSMSnapshot, error) {
	state, err := f.st.PoolState(context.Background())
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(state)
	return fsmSnapshot(b), err
}

func (f fsm) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	var state store.PoolState
	if err := json.NewDecoder(rc).Decode(&state); err != nil {
		return err
	}
	return f.st.SetPoolState(context.Background(), state)
}

type fsmSnapshot []byte

func (b fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	if _, err := sink.Write(b); err != nil {
		sink.Cancel()
		return err
	}
	return sink.Close()
}

func (fsmSnapshot) Release() {}

// replica is this machine's running raft.
type replica struct {
	*raft.Raft
	layer    *raftLayer
	admitter string        // the head that made this replica, trusted until the configuration names one
	stop     chan struct{} // closed once raft is shut down
}

// identity is what a replica keeps of itself, in the raft stable store.
type identity struct {
	ID       string `json:"id"`
	Addr     string `json:"addr"`
	Admitter string `json:"admitter,omitempty"`
}

var identityKey = []byte("boxpool-identity")

// storedIdentity returns this machine's identity as a replica, if it is one.
func (s *server) storedIdentity() (identity, bool) {
	var me identity
	b, err := s.st.Raft().Get(identityKey)
	return me, err == nil && json.Unmarshal(b, &me) == nil
}

type raftStart int

const (
	raftResume    raftStart = iota // a replica, from its state or from the admitter's
	raftBootstrap                  // a new pool's head, the one voter
	raftRecover                    // boxpool promote: the one voter, from this replica's state
)

// startRaft runs raft on this machine as me.
func (s *server) startRaft(me identity, how raftStart) error {
	cfg := raft.DefaultConfig()
	cfg.LocalID = raft.ServerID(me.ID)
	cfg.Logger = hclog.FromStandardLogger(slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn), &hclog.LoggerOptions{Name: "raft", Level: hclog.Warn})
	// Every heartbeat is an entry: snapshot and trim the log far sooner than
	// the defaults, which keep 10240 entries.
	cfg.SnapshotThreshold, cfg.TrailingLogs = 1024, 1024
	snaps, err := raft.NewFileSnapshotStoreWithLogger(filepath.Join(s.info.StateDir, "raft"), 2, cfg.Logger)
	if err != nil {
		return err
	}
	layer := &raftLayer{conns: make(chan net.Conn), closed: make(chan struct{}), addr: headAddr(me.Addr), dir: s.info.StateDir}
	trans := raft.NewNetworkTransportWithConfig(&raft.NetworkTransportConfig{Stream: layer, MaxPool: 3, Timeout: 10 * time.Second, Logger: cfg.Logger})
	one := raft.Configuration{Servers: []raft.Server{{ID: cfg.LocalID, Address: raft.ServerAddress(me.Addr)}}}
	db := s.st.Raft()
	switch how {
	case raftBootstrap:
		err = raft.BootstrapCluster(cfg, db, db, snaps, trans, one)
	case raftRecover:
		err = raft.RecoverCluster(cfg, fsm{s.st}, db, db, snaps, trans, one)
	}
	if err != nil {
		trans.Close()
		return err
	}
	r, err := raft.NewRaft(cfg, fsm{s.st}, db, db, snaps, trans)
	if err != nil {
		trans.Close()
		return err
	}
	rep := &replica{Raft: r, layer: layer, admitter: me.Admitter, stop: make(chan struct{})}
	// Saved once raft runs, so that a failed start does not leave a replica
	// that waits for a head forever.
	b, _ := json.Marshal(me)
	if err := s.st.Raft().Set(identityKey, b); err != nil {
		r.Shutdown()
		return err
	}
	s.id.Store(me.ID)
	s.raft.Store(rep)
	go s.watchLeadership(rep)
	return nil
}

// stopRaft shuts down this machine's raft, if it runs.
func (s *server) stopRaft() error {
	rep := s.raft.Load()
	if rep == nil {
		return nil
	}
	err := rep.Shutdown().Error()
	select {
	case <-rep.stop:
	default:
		close(rep.stop)
	}
	s.leading.Store(false)
	return err
}

// watchLeadership acts as the head while rep leads.
func (s *server) watchLeadership(rep *replica) {
	stopLead := func() {}
	defer func() { stopLead() }()
	for {
		select {
		case <-rep.stop:
			return
		case leader := <-rep.LeaderCh():
			stopLead()
			s.leading.Store(false)
			if !leader {
				continue
			}
			ctx, cancel := context.WithCancel(context.Background())
			stopLead = cancel
			go s.lead(ctx, rep)
		}
	}
}

// lead brings a new head's state up to date, then keeps its own report,
// and the pool's replicas, current until ctx ends.
func (s *server) lead(ctx context.Context, rep *replica) {
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	for {
		// All earlier changes applied, the state goes in whole, so the log
		// starts with it for any replica whose tables hold something else.
		err := rep.Barrier(10 * time.Second).Error()
		if err == nil {
			var state store.PoolState
			if state, err = s.st.PoolState(ctx); err == nil {
				err = applyTo(rep.Raft, command{Op: "state", State: &state})
			}
		}
		if err == nil {
			break
		}
		slog.Warn("cannot take over as head", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
	if ctx.Err() != nil { // leadership lost meanwhile
		return
	}
	s.leading.Store(true)
	slog.Info("this machine is the pool's head")
	for {
		// After a tick, so that a new pool's head has its node first.
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if rep.State() == raft.Leader {
			if err := s.reportSelf(ctx, rep); err != nil && ctx.Err() == nil {
				slog.Warn("cannot record this head's report", "err", err)
			}
			s.addReplicas(ctx, rep)
			s.checkReplaced(ctx, rep)
		}
	}
}

// apply makes the change c to the head's state: through raft once this
// machine has it, and directly before the pool has a second machine.
func (s *server) apply(ctx context.Context, c command) error {
	if rep := s.raft.Load(); rep != nil {
		return applyTo(rep.Raft, c)
	}
	return c.apply(ctx, s.st)
}

func applyTo(r *raft.Raft, c command) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	f := r.Apply(b, 10*time.Second)
	if err := f.Error(); err != nil {
		return err
	}
	err, _ = f.Response().(error)
	return err
}

func (s *server) selfID() string {
	id, _ := s.id.Load().(string)
	return id
}

// errNoHead is the error for a pool without a head: during an election,
// or in a pool of two that lost its head.
var errNoHead = errors.New("the pool has no head right now: wait for an election, or run boxpool promote on the replica of a pool of two")

// poolHead returns "" when this machine is the head of its pool or has
// none, and otherwise the head's address.
func (s *server) poolHead() (string, error) {
	if rep := s.raft.Load(); rep != nil {
		if s.leading.Load() {
			return "", nil
		}
		if addr, _ := rep.LeaderWithID(); addr != "" {
			return string(addr), nil
		}
		return "", errNoHead
	}
	return auth.Head(s.info.StateDir)
}

// bootstrap makes this head, at addr, the one voter of a new raft cluster,
// and a node of its own pool.
//
// shortcut: a crash between starting raft and Enroll leaves a head that
// cannot follow another one after a failover; redo Enroll on start if a
// pool ever hits it.
func (s *server) bootstrap(ctx context.Context, addr string) error {
	me := identity{ID: store.NewID(), Addr: addr}
	if err := s.startRaft(me, raftBootstrap); err != nil {
		return err
	}
	if err := s.waitLeading(ctx); err != nil {
		return err
	}
	ca, caKey, err := auth.LoadOrCreateCA(s.info.StateDir)
	if err != nil {
		return err
	}
	if err := auth.Enroll(s.info.StateDir, ca, caKey, me.ID, addr); err != nil {
		return err
	}
	host, _ := os.Hostname()
	self := store.Node{ID: me.ID, Name: host, OS: s.info.OS, Arch: s.info.Arch, JoinedAt: time.Now().UTC()}
	return s.apply(ctx, command{Op: "node", Node: &self})
}

func (s *server) waitLeading(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for !s.leading.Load() {
		select {
		case <-ctx.Done():
			return fmt.Errorf("raft did not elect this machine: %w", ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	return nil
}

// reportSelf records the head's own report, so that its sandboxes are
// listed, unreachable, if another machine takes over.
func (s *server) reportSelf(ctx context.Context, rep *replica) error {
	// Keep collecting and applying a report together: a periodic report
	// collected before a local change must not overwrite its newer report.
	s.reportMu.Lock()
	defer s.reportMu.Unlock()
	r, err := s.report(ctx)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(r)
	return applyTo(rep.Raft, command{Op: "report", Node: &store.Node{ID: s.selfID(), Report: b, SeenAt: time.Now()}})
}

// headReplica captures whether a local sandbox change needs replication.
// Keep this replica across the backend operation so losing leadership
// meanwhile fails the acknowledgement instead of skipping replication.
func (s *server) headReplica() *replica {
	if s.leading.Load() {
		return s.raft.Load()
	}
	return nil
}

// syncLocal records a local change before its successful response. The
// caller holds mu, so another local mutation cannot race its report.
func (s *server) syncLocal(w http.ResponseWriter, r *http.Request, rep *replica) bool {
	if rep != nil {
		if err := s.reportSelf(context.WithoutCancel(r.Context()), rep); err != nil {
			writeError(w, http.StatusServiceUnavailable, fmt.Errorf("local sandbox change could not be replicated: %w", err))
			return false
		}
	}
	return true
}

// addReplicas makes ready nodes replicas until there are maxReplicas:
// always-on nodes first, then the oldest. They vote once the pool has
// maxReplicas machines.
func (s *server) addReplicas(ctx context.Context, rep *replica) {
	f := rep.GetConfiguration()
	if f.Error() != nil {
		return
	}
	in := map[raft.ServerID]raft.ServerSuffrage{}
	for _, sv := range f.Configuration().Servers {
		in[sv.ID] = sv.Suffrage
	}
	members, err := s.members(ctx)
	if err != nil {
		return
	}
	vote := len(members)+1 >= maxReplicas
	slices.SortStableFunc(members, func(a, b member) int {
		return boolInt(slices.Contains(b.Labels, alwaysOn)) - boolInt(slices.Contains(a.Labels, alwaysOn))
	})
	for _, m := range members {
		suffrage, ok := in[raft.ServerID(m.ID)]
		if !m.ready || m.Addr == "" || ok && (suffrage == raft.Voter || !vote) || !ok && len(in) >= maxReplicas {
			continue
		}
		if !ok {
			if err := s.makeReplica(ctx, m); err != nil {
				slog.Warn("cannot make a node a replica of the pool's state", "node", m.Name, "err", err)
				continue
			}
		}
		// A voter the head cannot reach would leave it no quorum.
		if _, err := s.peerStatus(ctx, m.Addr); err != nil {
			slog.Warn("cannot reach a node's pool port to replicate to it", "node", m.Name, "err", err)
			continue
		}
		add := rep.AddNonvoter
		if vote {
			add = rep.AddVoter
		}
		if err := add(raft.ServerID(m.ID), raft.ServerAddress(m.Addr), 0, 10*time.Second).Error(); err != nil {
			slog.Warn("cannot add a replica of the pool's state", "node", m.Name, "err", err)
			continue
		}
		in[raft.ServerID(m.ID)] = suffrage
		slog.Info("added a replica of the pool's state", "node", m.Name, "addr", m.Addr, "voter", vote)
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ReplicaRequest is the body of POST /v1/pool/replica, which the head sends
// to a node through its tunnel: the head CA, its PKCS #8 key, the node's
// address as the head reaches it, and the head's node ID.
type ReplicaRequest struct {
	CA       []byte `json:"ca"`
	Key      []byte `json:"key"`
	Addr     string `json:"addr"`
	Admitter string `json:"admitter"`
}

// makeReplica gives node m the head CA and starts raft on it.
func (s *server) makeReplica(ctx context.Context, m member) error {
	ca, caKey, err := auth.LoadOrCreateCA(s.info.StateDir)
	if err != nil {
		return err
	}
	key, err := x509.MarshalPKCS8PrivateKey(caKey)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(ReplicaRequest{CA: ca.Raw, Key: key, Addr: m.Addr, Admitter: s.selfID()})
	hc := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return s.tunnels.take(ctx, m.ID) },
		DisableKeepAlives: true,
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://node/v1/pool/replica", bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("the node answered %s", resp.Status)
	}
	return nil
}

// becomeReplica is POST /v1/pool/replica: it installs the head CA, serves
// the pool port, and starts raft, which the head then adds.
func (s *server) becomeReplica(w http.ResponseWriter, r *http.Request) {
	var req ReplicaRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("bad request body: %w", err))
		return
	}
	s.poolMu.Lock()
	defer s.poolMu.Unlock()
	if s.raft.Load() != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	cert, err := tls.LoadX509KeyPair(filepath.Join(s.info.StateDir, auth.PoolDir, "cert.pem"), filepath.Join(s.info.StateDir, auth.PoolDir, "key.pem"))
	if err != nil {
		writeError(w, http.StatusConflict, fmt.Errorf("this machine has not joined a pool: %w", err))
		return
	}
	if err := auth.InstallCA(s.info.StateDir, req.CA, req.Key); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	if err := errors.Join(s.loadPoolTLS(), s.servePool()); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.startRaft(identity{ID: cert.Leaf.Subject.CommonName, Addr: req.Addr, Admitter: req.Admitter}, raftResume); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	slog.Info("keeping a replica of the pool's state", "addr", req.Addr)
	w.WriteHeader(http.StatusNoContent)
}

// RaftStatus is the body of the pool API's GET /v1/pool/raft.
type RaftStatus struct {
	Leader  bool     `json:"leader"`
	Servers []string `json:"servers"` // node IDs
}

func (s *server) raftStatus(w http.ResponseWriter, r *http.Request) {
	st := RaftStatus{Servers: []string{}}
	if rep := s.raft.Load(); rep != nil {
		st.Leader = rep.State() == raft.Leader
		if f := rep.GetConfiguration(); f.Error() == nil {
			for _, sv := range f.Configuration().Servers {
				st.Servers = append(st.Servers, string(sv.ID))
			}
		}
	}
	writeJSON(w, st)
}

// peerStatus asks the replica at addr for its RaftStatus.
func (s *server) peerStatus(ctx context.Context, addr string) (RaftStatus, error) {
	var st RaftStatus
	cfg, err := auth.NodeTLS(s.info.StateDir)
	if err != nil {
		return st, err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg, DialContext: transport.DialContext}}
	defer hc.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+addr+"/v1/pool/raft", nil)
	if err != nil {
		return st, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return st, fmt.Errorf("%s answered %s", addr, resp.Status)
	}
	return st, json.NewDecoder(resp.Body).Decode(&st)
}

// otherServers returns the addresses of the other replicas in rep's
// configuration.
func (s *server) otherServers(rep *replica) []string {
	var addrs []string
	if f := rep.GetConfiguration(); f.Error() == nil {
		for _, sv := range f.Configuration().Servers {
			if string(sv.ID) != s.selfID() {
				addrs = append(addrs, string(sv.Address))
			}
		}
	}
	return addrs
}

// checkReplaced steps this head down if a replica it lists leads without
// it: boxpool promote made that replica the head while this one was away.
// This machine then follows it as a node.
func (s *server) checkReplaced(ctx context.Context, rep *replica) {
	for _, addr := range s.otherServers(rep) {
		st, err := s.peerStatus(ctx, addr)
		if err != nil || !st.Leader || slices.Contains(st.Servers, s.selfID()) {
			continue
		}
		slog.Warn("another machine was promoted to head; following it", "head", addr)
		s.poolMu.Lock()
		if s.raft.Load() != rep { // promoted or stopped meanwhile
			s.poolMu.Unlock()
			return
		}
		err = errors.Join(s.stopRaft(), s.st.Raft().ResetRaft(), os.RemoveAll(filepath.Join(s.info.StateDir, "raft")), auth.SetHead(s.info.StateDir, addr))
		s.raft.Store(nil)
		s.head.Store(addr)
		s.poolMu.Unlock()
		if err != nil {
			slog.Error("cannot step down as head", "err", err)
		}
		return
	}
}

// PromoteResponse is the body of POST /v1/pool/promote.
type PromoteResponse struct {
	Head string `json:"head"`
}

// promote is POST /v1/pool/promote: it makes this replica the pool's head,
// the one voter, after the other replicas are gone.
func (s *server) promote(w http.ResponseWriter, r *http.Request) {
	s.poolMu.Lock()
	defer s.poolMu.Unlock()
	rep := s.raft.Load()
	me, ok := s.storedIdentity()
	switch {
	case rep == nil || !ok:
		writeError(w, http.StatusConflict, errors.New("this machine has no replica of the pool's state to promote"))
		return
	case s.leading.Load():
		writeError(w, http.StatusConflict, errors.New("this machine is already the head"))
		return
	}
	for _, addr := range s.otherServers(rep) {
		if _, err := s.peerStatus(r.Context(), addr); err == nil {
			writeError(w, http.StatusConflict, fmt.Errorf("the replica at %s is still reachable: promote only after the head is gone, or two heads would split the pool", addr))
			return
		}
	}
	if err := s.stopRaft(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.startRaft(me, raftRecover); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.waitLeading(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	slog.Warn("promoted this machine to head", "addr", me.Addr)
	writeJSON(w, PromoteResponse{Head: me.Addr})
}

// raftLayer is raft's stream layer over the pool port: acceptRaft hands
// it the connections that negotiated raftProtocol.
type raftLayer struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
	addr   net.Addr
	dir    string // the state directory, for this node's certificate
}

func (l *raftLayer) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *raftLayer) Close() error   { l.once.Do(func() { close(l.closed) }); return nil }
func (l *raftLayer) Addr() net.Addr { return l.addr }

// Dial loads this node's certificate each time: a new pool's head starts
// raft before it has one, and dials no one until another replica joins.
func (l *raftLayer) Dial(addr raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	cfg, err := auth.NodeTLS(l.dir)
	if err != nil {
		return nil, err
	}
	cfg.NextProtos = []string{raftProtocol}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c, err := transport.DialTLS(ctx, string(addr), cfg)
	if err != nil {
		return nil, err
	}
	if c.ConnectionState().NegotiatedProtocol != raftProtocol {
		c.Close()
		return nil, fmt.Errorf("%s does not keep a replica of the pool's state", addr)
	}
	return c, nil
}

// acceptRaft is the pool server's handler for raftProtocol connections. It
// holds the connection open until raft or the server closes it.
func (s *server) acceptRaft(_ *http.Server, c *tls.Conn, _ http.Handler) {
	defer c.Close()
	rep := s.raft.Load()
	if rep == nil || !rep.admits(c.ConnectionState()) {
		return
	}
	conn := &notifyConn{Conn: c, done: make(chan struct{})}
	select {
	case rep.layer.conns <- conn:
	case <-rep.layer.closed:
		return
	}
	select {
	case <-conn.done:
	case <-rep.layer.closed:
	case <-s.poolDone:
	}
}

// admits reports whether a raft connection is from a replica in the
// configuration, or, before this replica has one, from its admitter. Only
// replicas hold the CA, but one that boxpool promote left out must not
// overwrite the new head's log.
func (rep *replica) admits(cs tls.ConnectionState) bool {
	if len(cs.VerifiedChains) == 0 {
		return false
	}
	id := cs.PeerCertificates[0].Subject.CommonName
	f := rep.GetConfiguration()
	if f.Error() != nil {
		return false
	}
	servers := f.Configuration().Servers
	if len(servers) == 0 {
		return id == rep.admitter
	}
	return slices.ContainsFunc(servers, func(sv raft.Server) bool { return string(sv.ID) == id })
}

type notifyConn struct {
	net.Conn
	once sync.Once
	done chan struct{}
}

func (c *notifyConn) Close() error {
	c.once.Do(func() { close(c.done) })
	return c.Conn.Close()
}
