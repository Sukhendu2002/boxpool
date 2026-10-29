package daemon

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"boxpool.run/boxpool/internal/auth"
	"boxpool.run/boxpool/internal/backend"
	"boxpool.run/boxpool/internal/store"
)

// A node reports to the head every heartbeatInterval; the head marks it
// unreachable once it has not for unreachableAfter (#36).
var (
	heartbeatInterval = 5 * time.Second
	unreachableAfter  = 30 * time.Second
)

// StateUnreachable is the state the head lists a node's sandboxes in while
// the node misses heartbeats. The head keeps them; the node's next report
// replaces them.
const StateUnreachable backend.State = "unreachable"

// Report is the body of a node's heartbeat, POST /v1/heartbeat on the head.
// CPUs and MemoryMB are what a new sandbox could have, after the host
// reserve and existing sandboxes.
type Report struct {
	CPUs      int       `json:"cpu"`
	MemoryMB  int64     `json:"mem_mb"`
	Labels    []string  `json:"labels"`
	Sandboxes []Sandbox `json:"sandboxes"`
}

// Labels is the body of PUT /v1/labels: this machine's labels, which
// boxpool sh --node picks nodes by.
type Labels struct {
	Labels []string `json:"labels"`
}

// labelsName is the file in the state directory with this machine's
// labels, one per line.
const labelsName = "labels"

var validLabel = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`).MatchString

func readLabels(dir string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(dir, labelsName))
	if errors.Is(err, fs.ErrNotExist) {
		return []string{}, nil
	}
	return strings.Fields(string(b)), err
}

// setLabels replaces this machine's labels. The next heartbeat reports them.
func (s *server) setLabels(w http.ResponseWriter, r *http.Request) {
	var req Labels
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("bad request body: %w", err))
		return
	}
	for _, l := range req.Labels {
		if !validLabel(l) {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid label %q: use 1 to 63 lower-case letters, digits, dots, hyphens and underscores, starting with a letter or digit", l))
			return
		}
	}
	slices.Sort(req.Labels)
	req.Labels = slices.Compact(req.Labels)
	data := strings.Join(req.Labels, "\n") + "\n"
	if err := saveLabels(s.info.StateDir, []byte(data)); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, Labels{Labels: append([]string{}, req.Labels...)})
}

func saveLabels(dir string, data []byte) error {
	f, err := os.CreateTemp(dir, ".labels-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, labelsName))
}

// report describes this machine as the scheduler sees it. Without a
// backend it has no capacity and no sandboxes.
func (s *server) report(ctx context.Context) (Report, error) {
	labels, err := readLabels(s.info.StateDir)
	if err != nil || s.d == nil {
		return Report{Labels: labels, Sandboxes: []Sandbox{}}, err
	}
	r := Report{Labels: labels}
	if r.CPUs, r.MemoryMB, err = s.capacity.Free(ctx); err != nil {
		return r, err
	}
	records, err := s.st.List(ctx)
	if err == nil {
		r.Sandboxes, err = s.live(ctx, records)
	}
	return r, err
}

// serveHead waits until this machine joins a pool, then reports to the
// head and serves the head's requests with srv, until ctx ends.
func (s *server) serveHead(ctx context.Context, srv *http.Server) {
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	var head string
	for {
		var err error
		if head, err = auth.Head(s.info.StateDir); err == nil && head != "" {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
	cfg, err := auth.NodeTLS(s.info.StateDir)
	if err != nil {
		slog.Error("cannot load this node's pool certificate; the head cannot reach it", "err", err)
		return
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.heartbeat(ctx, cfg, head)
	}()
	srv.Serve(listenHead(ctx, cfg, head))
	stop()
	<-done
}

// heartbeat sends this node's report to the head every heartbeatInterval
// until ctx ends, logging when the head stops and starts answering.
func (s *server) heartbeat(ctx context.Context, cfg *tls.Config, head string) {
	hc := &http.Client{Timeout: heartbeatInterval, Transport: &http.Transport{TLSClientConfig: cfg}}
	defer hc.CloseIdleConnections()
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	reached := true
	for {
		err := s.sendReport(ctx, hc, head)
		if ctx.Err() != nil {
			return
		}
		if (err == nil) != reached {
			if reached = err == nil; reached {
				slog.Info("reaching the head again", "head", head)
			} else {
				slog.Warn("cannot report to the head", "head", head, "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *server) sendReport(ctx context.Context, hc *http.Client, head string) error {
	r, err := s.report(ctx)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(r)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+head+"/v1/heartbeat", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("the head answered %s", resp.Status)
	}
	return nil
}

// receiveReport is the head's POST /v1/heartbeat.
func (s *server) receiveReport(w http.ResponseWriter, r *http.Request) {
	var rep Report
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&rep); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("bad request body: %w", err))
		return
	}
	b, _ := json.Marshal(rep)
	s.reportMu.Lock()
	err := s.st.SetNodeReport(r.Context(), nodeID(r), b, time.Now())
	s.reportMu.Unlock()
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusForbidden, errors.New("this node is not in the pool"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// member is a node as the head last heard from it.
type member struct {
	store.Node
	Report
	ready bool
}

// ready reports whether a node last heard from at seen is up at now.
func ready(seen, now time.Time) bool {
	return !seen.IsZero() && now.Sub(seen) < unreachableAfter
}

// members returns the nodes that joined this head; none on a node.
func (s *server) members(ctx context.Context) ([]member, error) {
	nodes, err := s.st.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	list := make([]member, 0, len(nodes))
	for _, n := range nodes {
		m := member{Node: n, ready: ready(n.SeenAt, now)}
		if n.Report != nil {
			if err := json.Unmarshal(n.Report, &m.Report); err != nil {
				return nil, fmt.Errorf("node %s's report: %w", n.Name, err)
			}
		}
		list = append(list, m)
	}
	return list, nil
}

// owner returns the member that reported a sandbox called name.
func owner(members []member, name string) (member, bool) {
	for _, m := range members {
		if slices.ContainsFunc(m.Sandboxes, func(sb Sandbox) bool { return sb.Name == name }) {
			return m, true
		}
	}
	return member{}, false
}

// route serves h for a sandbox in this machine's store, and forwards the
// request to the node that reported one by that name otherwise.
func (s *server) route(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := s.st.GetByName(r.Context(), r.PathValue("name")); !errors.Is(err, store.ErrNotFound) {
			h(w, r)
			return
		}
		members, err := s.members(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if m, ok := owner(members, r.PathValue("name")); ok {
			s.forward(w, r, m)
			return
		}
		h(w, r) // not found
	}
}

// osNames and archNames map what users type to Go's names, which nodes
// report.
var (
	osNames   = map[string]string{"macos": "darwin", "mac": "darwin"}
	archNames = map[string]string{"x86_64": "amd64", "x64": "amd64", "aarch64": "arm64"}
)

// place sends a new sandbox to the node pick chooses, here or through its
// tunnel; a request naming an existing sandbox goes to its node.
func (s *server) place(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<16))
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("bad request body: %w", err))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var req CreateRequest
		if json.Unmarshal(body, &req) != nil {
			h(w, r) // which reports the error
			return
		}
		spec, err := (backend.Spec{CPUs: req.CPUs, MemoryMB: req.MemoryMB, DiskGB: req.DiskGB}).WithDefaults()
		if err != nil {
			h(w, r)
			return
		}
		s.placeMu.Lock()
		defer s.placeMu.Unlock()
		ctx := r.Context()
		members, err := s.members(ctx)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if req.Name != "" {
			if _, err := s.st.GetByName(ctx, req.Name); !errors.Is(err, store.ErrNotFound) {
				h(w, r)
				return
			}
			if m, ok := owner(members, req.Name); ok {
				s.forward(w, r, m)
				return
			}
		}
		// Leave unconstrained single-machine admission to its backend.
		// Without a backend or nodes, h explains that nothing can run here.
		if len(members) == 0 && (s.d == nil || req.OS == "" && req.Arch == "" && req.Node == "") {
			h(w, r)
			return
		}
		var cands []member
		if s.d != nil {
			rep, err := s.report(ctx)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			cands = append(cands, member{Node: store.Node{OS: s.info.OS, Arch: s.info.Arch}, Report: rep})
		}
		// Snapshots stay on the machine that took them.
		for _, m := range members {
			if m.ready && req.From == "" {
				cands = append(cands, m)
			}
		}
		req.OS = cmp.Or(osNames[req.OS], req.OS)
		req.Arch = cmp.Or(archNames[req.Arch], req.Arch)
		c, err := pick(cands, req, spec)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		if c.ID == "" {
			h(w, r)
			return
		}
		s.forward(w, r, c)
	}
}

// pick returns where a new sandbox goes, of the candidates (ID "" is this
// machine): of those with req's OS, arch and label, the one with the most
// free memory, then the fewest sandboxes. All matches must have capacity.
func pick(cands []member, req CreateRequest, spec backend.Spec) (member, error) {
	var match []member
	for _, c := range cands {
		if (req.OS == "" || c.OS == req.OS) && (req.Arch == "" || c.Arch == req.Arch) && (req.Node == "" || slices.Contains(c.Labels, req.Node)) {
			match = append(match, c)
		}
	}
	if len(match) == 0 {
		return member{}, fmt.Errorf("no reachable node matches %s", constraints(req))
	}
	var best *member
	for i := range match {
		c := &match[i]
		if c.CPUs < spec.CPUs || c.MemoryMB < int64(spec.MemoryMB) {
			continue
		}
		if best == nil || c.MemoryMB > best.MemoryMB || c.MemoryMB == best.MemoryMB && len(c.Sandboxes) < len(best.Sandboxes) {
			best = c
		}
	}
	if best == nil {
		return member{}, fmt.Errorf("insufficient capacity for %s: none of the %d matching nodes has %d CPUs and %d MiB of memory free", constraints(req), len(match), spec.CPUs, spec.MemoryMB)
	}
	return *best, nil
}

func constraints(req CreateRequest) string {
	var want []string
	for flag, v := range map[string]string{"--os": req.OS, "--arch": req.Arch, "--node": req.Node} {
		if v != "" {
			want = append(want, flag+" "+v)
		}
	}
	slices.Sort(want)
	return cmp.Or(strings.Join(want, " "), "the request")
}

// forward relays the request, a plain exchange or a WebSocket, to the
// node's own API through one of its tunnels.
func (s *server) forward(w http.ResponseWriter, r *http.Request, m member) {
	if !m.ready {
		writeError(w, http.StatusBadGateway, fmt.Errorf("node %s is unreachable: no heartbeat for %s", m.Name, time.Since(m.SeenAt).Round(time.Second)))
		return
	}
	p := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host, pr.Out.Host = "http", "node", "node"
		},
		Transport: &http.Transport{
			DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return s.tunnels.take(ctx, m.ID) },
			DisableKeepAlives: true, // a tunnel carries one request
		},
		ModifyResponse: func(resp *http.Response) error { return s.track(resp, m.ID) },
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			writeError(w, http.StatusBadGateway, fmt.Errorf("node %s is unreachable: %w", m.Name, err))
		},
	}
	p.ServeHTTP(w, r)
}

// track records in node id's report the sandbox a forwarded change
// returned, so the next request finds it before the next heartbeat.
func (s *server) track(resp *http.Response, id string) error {
	r := resp.Request
	if resp.StatusCode/100 != 2 || r.Method != http.MethodDelete && (r.Method != http.MethodPost || strings.HasSuffix(r.URL.Path, "/snapshot")) {
		return nil
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(b))
	if err != nil {
		return err
	}
	var sb Sandbox
	if json.Unmarshal(b, &sb) != nil || sb.ID == "" {
		return nil
	}
	ctx := context.WithoutCancel(r.Context())
	s.reportMu.Lock()
	defer s.reportMu.Unlock()
	members, err := s.members(ctx)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(members, func(m member) bool { return m.ID == id })
	if i < 0 {
		return nil
	}
	m := members[i]
	j := slices.IndexFunc(m.Sandboxes, func(old Sandbox) bool { return old.ID == sb.ID })
	switch {
	case r.Method == http.MethodDelete && j >= 0:
		m.CPUs += m.Sandboxes[j].CPUs
		m.MemoryMB += int64(m.Sandboxes[j].MemoryMB)
		m.Sandboxes = slices.Delete(m.Sandboxes, j, j+1)
	case r.Method == http.MethodDelete:
	case j >= 0:
		m.Sandboxes[j] = sb
	default:
		m.CPUs = max(0, m.CPUs-sb.CPUs)
		m.MemoryMB = max(0, m.MemoryMB-int64(sb.MemoryMB))
		m.Sandboxes = append(m.Sandboxes, sb)
	}
	rep, _ := json.Marshal(m.Report)
	return s.st.SetNodeReport(ctx, id, rep, m.SeenAt)
}
