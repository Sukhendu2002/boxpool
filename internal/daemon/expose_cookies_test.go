package daemon

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/store"
)

func TestExposeCookieEnvelopes(t *testing.T) {
	e := store.Expose{Port: 12345, Sandbox: "app", SandboxPort: 8000, CreatedAt: time.Now().UTC()}
	dir := t.TempDir()
	c, err := newExposeCookies(dir, e)
	if err != nil {
		t.Fatal(err)
	}
	seal := func(header, requestPath string) *http.Cookie {
		t.Helper()
		r := httptest.NewRequest("GET", "https://head.test"+requestPath, nil)
		r = r.WithContext(context.WithValue(r.Context(), exposeCookiesKey{}, exposeCookieRequest{cookies: c, path: r.URL.EscapedPath()}))
		resp := &http.Response{Request: r, Header: http.Header{"Set-Cookie": {header}}}
		if err := protectExposeCookies(resp); err != nil {
			t.Fatal(err)
		}
		cookies := resp.Cookies()
		if len(cookies) != 1 && !(len(cookies) == 2 && cookies[0].MaxAge < 0) {
			t.Fatalf("cookies=%v", cookies)
		}
		return cookies[0]
	}
	app := seal("session=secret; Max-Age=3600; SameSite=Strict", "/account/login")
	if app.Path != "/account" || app.MaxAge != 3600 || app.SameSite != http.SameSiteStrictMode || !app.HttpOnly || !app.Secure {
		t.Fatalf("attributes=%+v", app)
	}
	open := func(codec *exposeCookies, path string, cookies ...*http.Cookie) string {
		r := httptest.NewRequest("GET", "https://head.test"+path, nil)
		for _, cookie := range cookies {
			r.AddCookie(cookie)
		}
		codec.open(r)
		return r.Header.Get("Cookie")
	}
	if got := open(c, "/account/profile", app); got != "session=secret" {
		t.Fatalf("round trip=%q", got)
	}
	for _, path := range []string{"/", "/accounts/profile"} {
		if got := open(c, path, app); got != "" {
			t.Errorf("path %s received %q", path, got)
		}
	}
	renamed := *app
	renamed.Name = c.prefix + base64.RawURLEncoding.EncodeToString([]byte("other"))
	tampered := *app
	tampered.Value = "AAAA" + tampered.Value[4:]
	if got := open(c, "/account", &renamed, &tampered, &http.Cookie{Name: "session", Value: "raw-secret"}); got != "" {
		t.Fatalf("forged cookies=%q", got)
	}
	restarted, err := newExposeCookies(dir, e)
	if err != nil {
		t.Fatal(err)
	}
	if got := open(restarted, "/account", app); got != "session=secret" {
		t.Fatalf("restart lost session=%q", got)
	}
	e.CreatedAt = e.CreatedAt.Add(time.Second)
	reused, err := newExposeCookies(dir, e)
	if err != nil {
		t.Fatal(err)
	}
	if got := open(reused, "/account", app); got != "" {
		t.Fatalf("reused port accepted old session=%q", got)
	}
	escaped := seal("session=escaped; Path=/a%2Fb", "/login")
	if got := open(c, "/a%2Fb/x", escaped); got != "session=escaped" {
		t.Fatalf("escaped path=%q", got)
	}
	deleted := seal("session=; Path=/account; Max-Age=0", "/logout")
	if deleted.Name != app.Name || deleted.Path != app.Path || deleted.MaxAge != -1 {
		t.Fatalf("logout=%+v", deleted)
	}
	for _, name := range []string{cookiePrefix + "12345", "boxpool_app_999_session"} {
		r := httptest.NewRequest("GET", "https://head.test/", nil)
		r = r.WithContext(context.WithValue(r.Context(), exposeCookiesKey{}, exposeCookieRequest{cookies: c, path: "/"}))
		resp := &http.Response{Request: r, Header: http.Header{"Set-Cookie": {name + "=forged"}}}
		protectExposeCookies(resp)
		if got := resp.Cookies(); len(got) != 1 || got[0].Name == name || !got[0].HttpOnly {
			t.Fatalf("app cookie collided with infrastructure cookie: %v", got)
		}
		if got := open(c, "/", resp.Cookies()...); got != name+"=forged" {
			t.Fatalf("namespaced app cookie failed: %q", got)
		}
	}
	for _, value := range []string{strings.Repeat("a", 3100), strings.Repeat("<", 3900)} {
		r := httptest.NewRequest("GET", "https://head.test/login", nil)
		r = r.WithContext(context.WithValue(r.Context(), exposeCookiesKey{}, exposeCookieRequest{cookies: c, path: "/login"}))
		resp := &http.Response{Request: r, Header: http.Header{"Set-Cookie": {"session=" + value + "; Path=/; HttpOnly"}}}
		if err := protectExposeCookies(resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Cookies()) != 2 {
			t.Fatalf("large session not split: %v", resp.Cookies())
		}
		for _, cookie := range resp.Cookies() {
			if len(cookie.String()) > 4096 {
				t.Fatalf("browser cookie too large: %d", len(cookie.String()))
			}
		}
		if got := open(c, "/", resp.Cookies()...); got != "session="+value {
			t.Fatalf("large session round trip failed: length=%d", len(got))
		}
		if got := open(c, "/", resp.Cookies()[0]); got != "" {
			t.Fatal("incomplete session accepted")
		}
	}
	if strings.Contains(app.Value, "secret") {
		t.Fatal("plaintext cookie value")
	}
}
