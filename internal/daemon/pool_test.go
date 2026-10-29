package daemon

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// machine is a daemon's local API on a new state directory.
func machine(t *testing.T, osName string) (*httptest.Server, Info, *store.Store) {
	t.Helper()
	info := Info{OS: osName, Arch: "arm64", StateDir: t.TempDir()}
	st, err := store.Open(filepath.Join(info.StateDir, StoreName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv := httptest.NewServer(Handler(info, st, nil))
	t.Cleanup(srv.Close)
	return srv, info, st
}

// TestJoinPool joins a node to a head, both in this process, and checks
// that the node's certificate opens the head's authenticated endpoint.
func TestJoinPool(t *testing.T) {
	head, headInfo, headSt := machine(t, "linux")
	ca, caKey, err := auth.LoadOrCreateCA(headInfo.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := auth.HeadTLS(ca, caKey)
	if err != nil {
		t.Fatal(err)
	}
	pool := httptest.NewUnstartedServer(PoolHandler(headInfo, headSt, ca, caKey))
	pool.TLS = cfg
	pool.StartTLS()
	defer pool.Close()
	token := func() string {
		t.Helper()
		code, body := send(t, head, "POST", "/v1/tokens", `{"addr":"`+pool.Listener.Addr().String()+`"}`)
		var tok Token
		if err := json.Unmarshal([]byte(body), &tok); code != http.StatusCreated || err != nil {
			t.Fatalf("create token = %d %s", code, body)
		}
		return tok.Token
	}
	join := func(srv *httptest.Server, tok string) (int, string) {
		t.Helper()
		return send(t, srv, "POST", "/v1/pool/join", `{"token":"`+tok+`"}`)
	}

	node, nodeInfo, _ := machine(t, "darwin")
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
	if fi, err := os.Stat(filepath.Join(nodeInfo.StateDir, auth.PoolDir, "key.pem")); err != nil || fi.Mode().Perm() != 0600 {
		t.Errorf("node key: %v", err)
	}

	code, body = send(t, head, "GET", "/v1/nodes", "")
	var nodes []Node
	if err := json.Unmarshal([]byte(body), &nodes); code != http.StatusOK || err != nil || len(nodes) != 2 ||
		nodes[0].Role != "head" || nodes[0].OS != "linux" || nodes[1].ID != res.ID || nodes[1].Role != "node" || nodes[1].OS != "darwin" || nodes[1].JoinedAt.IsZero() {
		t.Errorf("head nodes = %d %s", code, body)
	}

	// The token worked once.
	other, _, _ := machine(t, "linux")
	if code, body := join(other, tok); code != http.StatusBadGateway || !strings.Contains(body, "token is invalid or already used") {
		t.Errorf("second join with one token = %d %s", code, body)
	}
	// Without a client certificate, only joining is open.
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	if resp, err := hc.Get(pool.URL + "/v1/nodes"); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /v1/nodes without a certificate = %v, %v", resp, err)
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
