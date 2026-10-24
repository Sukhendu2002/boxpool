package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealth(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler(Info{}, nil, nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/health", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "{\"ok\":true}\n" {
		t.Errorf("GET /v1/health = %d %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestInfo(t *testing.T) {
	want := Info{Version: "1.2.3", OS: "linux", Arch: "amd64", StateDir: "/state", Backend: "docker", Warning: "fallback", PID: 42}
	rec := httptest.NewRecorder()
	Handler(want, nil, nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/info", nil))
	var got Info
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK || got != want {
		t.Errorf("GET /v1/info = %d %q (%v), want %+v", rec.Code, rec.Body.String(), err, want)
	}
}

func TestMethodAndPath(t *testing.T) {
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, "/v1/health", http.StatusMethodNotAllowed},
		{http.MethodGet, "/v1/missing", http.StatusNotFound},
	} {
		rec := httptest.NewRecorder()
		Handler(Info{}, nil, nil).ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != c.want {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, rec.Code, c.want)
		}
	}
}
