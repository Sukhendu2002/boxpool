package daemon

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"boxpool.run/boxpool/internal/auth"
	"boxpool.run/boxpool/internal/store"
)

type exposeCookiesKey struct{}

type exposeCookieRequest struct {
	cookies *exposeCookies
	path    string
}

// Ports are not a cookie boundary. Only authenticated envelopes for this
// expose reach the app. Even apps' non-HttpOnly cookies must become HttpOnly:
// JavaScript on a sibling port could otherwise steal and replay the envelope.
type exposeCookies struct {
	aead   cipher.AEAD
	prefix string
}

// Leave room for attributes below browsers' 4096-byte per-cookie limit.
// Two pieces accommodate an original browser cookie of up to 4096 bytes
// plus the encryption/base64 overhead. Names include the original path so
// equally named cookies at different paths cannot mix their pieces.
const appCookiePieceSize = 3000

func newExposeCookies(dir string, e store.Expose) (*exposeCookies, error) {
	_, key, err := auth.LoadOrCreateCA(dir)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, der)
	fmt.Fprintf(mac, "boxpool app cookies v1\x00%d\x00%s\x00%d\x00%s", e.Port, e.Sandbox, e.SandboxPort, e.CreatedAt.Format(time.RFC3339Nano))
	block, err := aes.NewCipher(mac.Sum(nil))
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &exposeCookies{aead: aead, prefix: "boxpool_app_" + strconv.Itoa(e.Port) + "_"}, nil
}

func (c *exposeCookies) open(r *http.Request) {
	cookies := r.Cookies()
	r.Header.Del("Cookie")
	for _, cookie := range cookies {
		if !strings.HasPrefix(cookie.Name, c.prefix) {
			continue
		}
		pieces, value, ok := strings.Cut(cookie.Value, ".")
		if !ok || pieces != "1" && pieces != "2" {
			continue
		}
		if pieces == "2" {
			tail, err := rCookie(cookies, cookie.Name+"_1")
			if err != nil {
				continue
			}
			value += tail.Value
		}
		b, err := base64.RawURLEncoding.DecodeString(value)
		if err != nil || len(b) < c.aead.NonceSize() {
			continue
		}
		b, err = c.aead.Open(nil, b[:c.aead.NonceSize()], b[c.aead.NonceSize():], []byte(cookie.Name))
		if err != nil {
			continue
		}
		original := strings.Split(string(b), "\x00")
		if len(original) != 3 || !cookiePathMatches(r.URL.EscapedPath(), original[2]) {
			continue
		}
		r.AddCookie(&http.Cookie{Name: original[0], Value: original[1]})
	}
}

func rCookie(cookies []*http.Cookie, name string) (*http.Cookie, error) {
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie, nil
		}
	}
	return nil, http.ErrNoCookie
}

func cookiePathMatches(request, cookie string) bool {
	return request == cookie || strings.HasPrefix(request, cookie) && (strings.HasSuffix(cookie, "/") || len(request) > len(cookie) && request[len(cookie)] == '/')
}

// protectExposeCookies runs at the response boundary on the head, both for
// local ports and node forwards. ModifyResponse also covers 101 handshakes.
// The context value is never sent across a node tunnel.
func protectExposeCookies(resp *http.Response) error {
	req, ok := resp.Request.Context().Value(exposeCookiesKey{}).(exposeCookieRequest)
	if !ok {
		return nil
	}
	c := req.cookies
	cookies := resp.Cookies()
	resp.Header.Del("Set-Cookie")
	for _, cookie := range cookies {
		if !strings.HasPrefix(cookie.Path, "/") {
			cookie.Path = "/"
			if i := strings.LastIndex(req.path, "/"); i > 0 {
				cookie.Path = req.path[:i]
			}
		}
		b := []byte(strings.Join([]string{cookie.Name, cookie.Value, cookie.Path}, "\x00"))
		nameHash := sha256.Sum256([]byte(cookie.Name + "\x00" + cookie.Path))
		cookie.Name = c.prefix + base64.RawURLEncoding.EncodeToString(nameHash[:])
		nonce := make([]byte, c.aead.NonceSize())
		rand.Read(nonce)
		value := base64.RawURLEncoding.EncodeToString(c.aead.Seal(nonce, nonce, b, []byte(cookie.Name)))
		if len(value) > 2*appCookiePieceSize {
			return fmt.Errorf("app cookie exceeds the browser cookie size limit")
		}
		pieces := "1."
		if len(value) > appCookiePieceSize {
			pieces = "2."
		}
		cookie.Value = pieces + value[:min(len(value), appCookiePieceSize)]
		cookie.Domain = ""
		cookie.Secure, cookie.HttpOnly = true, true
		resp.Header.Add("Set-Cookie", cookie.String())
		if len(value) > appCookiePieceSize || cookie.MaxAge < 0 {
			tail := *cookie
			tail.Name += "_1"
			tail.Value = value[min(len(value), appCookiePieceSize):]
			resp.Header.Add("Set-Cookie", tail.String())
		}
	}
	return nil
}
