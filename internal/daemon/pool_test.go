package daemon

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/auth"
	"boxpool.run/boxpool/internal/store"
)

// send sends a request with body and returns the status and the body.
func send(t *testing.T, srv *httptest.Server, method, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// machine is a daemon's local API on a new state directory.
func machine(t *testing.T, osName string) (*httptest.Server, *server) {
	t.Helper()
	info := Info{OS: osName, Arch: "arm64", StateDir: t.TempDir()}
	st, err := store.Open(filepath.Join(info.StateDir, StoreName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := newServer(info, st, nil)
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	return srv, s
}

func poolServer(t *testing.T, s *server, wrap func(http.Handler) http.Handler) *httptest.Server {
	t.Helper()
	ca, caKey, err := auth.LoadOrCreateCA(s.info.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := auth.HeadTLS(ca, caKey)
	if err != nil {
		t.Fatal(err)
	}
	h := s.poolHandler(ca, caKey)
	if wrap != nil {
		h = wrap(h)
	}
	pool := httptest.NewUnstartedServer(h)
	pool.TLS = cfg
	pool.StartTLS()
	t.Cleanup(pool.Close)
	return pool
}

func issueToken(t *testing.T, local, pool *httptest.Server) string {
	t.Helper()
	code, body := send(t, local, "POST", "/v1/tokens", `{"addr":"`+pool.Listener.Addr().String()+`"}`)
	var tok Token
	if err := json.Unmarshal([]byte(body), &tok); code != http.StatusCreated || err != nil {
		t.Fatalf("create token = %d %s", code, body)
	}
	return tok.Token
}

func registration(t *testing.T, token string) auth.JoinRequest {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	return auth.JoinRequest{Token: token, CSR: csr, Name: "node.local", OS: "linux", Arch: "amd64"}
}

func registerThroughPool(t *testing.T, s *server, pool *httptest.Server, req auth.JoinRequest) (int, string) {
	t.Helper()
	ca, _, err := auth.LoadOrCreateCA(s.info.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: roots, ServerName: auth.HeadName,
	}}}
	defer hc.CloseIdleConnections()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := hc.Post(pool.URL+"/v1/nodes", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

// TestJoinPool joins a node to a head, both in this process, and checks
// that the node's certificate opens the head's authenticated endpoint.
func TestJoinPool(t *testing.T) {
	head, headServer := machine(t, "linux")
	pool := poolServer(t, headServer, nil)
	token := func() string {
		t.Helper()
		return issueToken(t, head, pool)
	}
	join := func(srv *httptest.Server, tok string) (int, string) {
		t.Helper()
		return send(t, srv, "POST", "/v1/pool/join", `{"token":"`+tok+`"}`)
	}

	node, nodeServer := machine(t, "darwin")
	tok := token()
	// A token whose CA pin does not match the head fails before the head
	// sees it, so the real token still works afterwards.
	bad, _ := auth.ParseJoinToken(tok)
	bad.CA[0] ^= 1
	if code, body := join(node, bad.String()); code != http.StatusBadGateway || !strings.Contains(body, "does not match the join token") {
		t.Errorf("join with a wrong CA pin = %d %s", code, body)
	}
	code, body := join(node, tok)
	var res JoinPoolResponse
	if err := json.Unmarshal([]byte(body), &res); code != http.StatusOK || err != nil || res.ID == "" || res.Head != pool.Listener.Addr().String() {
		t.Fatalf("join = %d %s", code, body)
	}
	if fi, err := os.Stat(filepath.Join(nodeServer.info.StateDir, auth.PoolDir, "key.pem")); err != nil || fi.Mode().Perm() != 0600 {
		t.Errorf("node key: %v", err)
	}

	code, body = send(t, head, "GET", "/v1/nodes", "")
	var nodes []Node
	if err := json.Unmarshal([]byte(body), &nodes); code != http.StatusOK || err != nil || len(nodes) != 2 ||
		nodes[0].Role != "head" || nodes[0].OS != "linux" || nodes[1].ID != res.ID || nodes[1].Role != "node" || nodes[1].OS != "darwin" || nodes[1].JoinedAt.IsZero() {
		t.Errorf("head nodes = %d %s", code, body)
	}

	// The token worked once.
	other, _ := machine(t, "linux")
	if code, body := join(other, tok); code != http.StatusBadGateway || !strings.Contains(body, "token is invalid or already used") {
		t.Errorf("second join with one token = %d %s", code, body)
	}
	// Without a client certificate, only joining is open.
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	defer hc.CloseIdleConnections()
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/nodes"},
		{http.MethodPost, "/v1/heartbeat"},
		{http.MethodGet, "/v1/tunnel"},
	} {
		req, _ := http.NewRequest(tc.method, pool.URL+tc.path, nil)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without a certificate = %s", tc.method, tc.path, resp.Status)
		}
	}

	// A node is in one pool and is not a head.
	for _, tc := range []struct{ method, path, body, want string }{
		{"POST", "/v1/pool/join", `{"token":"` + token() + `"}`, "it can be in only one"},
		{"POST", "/v1/tokens", `{}`, "create join tokens on the head"},
		{"GET", "/v1/nodes", "", "run boxpool ls --nodes on the head"},
	} {
		if code, body := send(t, node, tc.method, tc.path, tc.body); code != http.StatusConflict || !strings.Contains(body, tc.want) {
			t.Errorf("%s %s on a node = %d %s", tc.method, tc.path, code, body)
		}
	}
	// A head with nodes cannot join another pool.
	if code, body := join(head, token()); code != http.StatusConflict || !strings.Contains(body, "head of a pool with 1 nodes") {
		t.Errorf("join from a head = %d %s", code, body)
	}
}

func TestRegisterNodeNames(t *testing.T) {
	maxName := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)
	for _, tc := range []struct {
		name, host, osName, arch string
		want                     int
	}{
		{"macOS runner", "sat12-bq159-2ad1278e-f583-419e-b8d0-356f33b24d80-CE41999A5792.local", "darwin", "arm64", http.StatusOK},
		{"maximum hostname", maxName, "linux", "amd64", http.StatusOK},
		{"too long hostname", maxName + "a", "linux", "amd64", http.StatusBadRequest},
		{"empty hostname", "", "linux", "amd64", http.StatusBadRequest},
		{"control in hostname", "node\n.local", "linux", "amd64", http.StatusBadRequest},
		{"too long OS", "node.local", strings.Repeat("a", 64), "amd64", http.StatusBadRequest},
		{"too long architecture", "node.local", "linux", strings.Repeat("a", 64), http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local, s := machine(t, "linux")
			pool := poolServer(t, s, nil)
			tok := issueToken(t, local, pool)
			req := registration(t, tok)
			req.Name, req.OS, req.Arch = tc.host, tc.osName, tc.arch
			if code, body := registerThroughPool(t, s, pool, req); code != tc.want {
				t.Fatalf("register = %d %s; want %d", code, body, tc.want)
			}
			nodes, err := s.st.ListNodes(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == http.StatusOK {
				if len(nodes) != 1 || nodes[0].Name != tc.host {
					t.Fatalf("nodes = %+v; want hostname %q", nodes, tc.host)
				}
			} else {
				if len(nodes) != 0 {
					t.Fatalf("invalid registration added nodes: %+v", nodes)
				}
				parsed, err := auth.ParseJoinToken(tok)
				if err != nil {
					t.Fatal(err)
				}
				if err := s.st.UseJoinToken(context.Background(), parsed.Hash(), time.Now()); err != nil {
					t.Fatalf("invalid registration consumed the token: %v", err)
				}
			}
		})
	}
}

func TestRejectSelfJoin(t *testing.T) {
	local, s := machine(t, "linux")
	pool := poolServer(t, s, nil)
	tok := issueToken(t, local, pool)
	parsed, err := auth.ParseJoinToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	// CA identity must detect self-join regardless of the address or alias
	// in the token, before any network request consumes its secret.
	for _, addr := range []string{parsed.Addr, "localhost:7443", "127.0.0.1:1"} {
		parsed.Addr = addr
		if code, body := send(t, local, "POST", "/v1/pool/join", `{"token":"`+parsed.String()+`"}`); code != http.StatusConflict || !strings.Contains(body, "cannot join its own pool") {
			t.Fatalf("self-join via %q = %d %s", addr, code, body)
		}
	}
	if head, err := auth.Head(s.info.StateDir); err != nil || head != "" {
		t.Fatalf("self-join changed head to %q: %v", head, err)
	}
	if nodes, err := s.st.ListNodes(context.Background()); err != nil || len(nodes) != 0 {
		t.Fatalf("self-join added nodes: %+v, %v", nodes, err)
	}
	// The rejected attempt leaves the head and its token usable by a real node.
	other, _ := machine(t, "darwin")
	if code, body := send(t, other, "POST", "/v1/pool/join", `{"token":"`+tok+`"}`); code != http.StatusOK {
		t.Fatalf("join from another machine = %d %s", code, body)
	}
}

func TestPoolRoleTransition(t *testing.T) {
	parentLocal, parent := machine(t, "linux")
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	finish := func() { releaseOnce.Do(func() { close(release) }) }
	parentPool := poolServer(t, parent, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				close(entered)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	})
	defer finish()
	local, s := machine(t, "darwin")
	pool := poolServer(t, s, nil)
	oldToken := issueToken(t, local, pool)
	parentToken := issueToken(t, parentLocal, parentPool)

	joined := make(chan *httptest.ResponseRecorder, 1)
	joinDone := make(chan struct{})
	go func() {
		defer close(joinDone)
		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/pool/join", strings.NewReader(`{"token":"`+parentToken+`"}`)))
		joined <- rec
	}()
	t.Cleanup(func() {
		finish()
		select {
		case <-joinDone:
		case <-time.After(10 * time.Second):
			t.Error("join handler did not stop before cleanup")
		}
	})
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("join did not reach the parent")
	}

	// A join has passed the no-nodes check and is waiting on the parent.
	// Both APIs must refuse new head activity during that window.
	for _, path := range []string{"/v1/tokens", "/v1/pool/join"} {
		if code, body := send(t, local, "POST", path, `{"token":"`+parentToken+`"}`); code != http.StatusConflict || !strings.Contains(body, "joining another pool") {
			t.Errorf("%s during join = %d %s", path, code, body)
		}
	}
	req := registration(t, oldToken)
	if code, body := registerThroughPool(t, s, pool, req); code != http.StatusConflict || !strings.Contains(body, "joining another pool") {
		t.Errorf("register during join = %d %s", code, body)
	}
	finish()
	select {
	case rec := <-joined:
		if rec.Code != http.StatusOK {
			t.Fatalf("join parent = %d %s", rec.Code, rec.Body.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("join did not finish")
	}

	// The listener remains open until restart, but old tokens must no
	// longer register nodes now that this machine has become a node.
	if code, body := registerThroughPool(t, s, pool, req); code != http.StatusConflict || !strings.Contains(body, "this machine is a node") {
		t.Errorf("register after join = %d %s", code, body)
	}
	if nodes, err := s.st.ListNodes(context.Background()); err != nil || len(nodes) != 0 {
		t.Errorf("role transition left child nodes: %+v, %v", nodes, err)
	}
	parsed, err := auth.ParseJoinToken(oldToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.st.UseJoinToken(context.Background(), parsed.Hash(), time.Now()); err != nil {
		t.Errorf("rejected registrations consumed the old token: %v", err)
	}
}
