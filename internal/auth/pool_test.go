package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// nodeCert returns a client certificate signed by ca, expiring at
// notAfter, or the CA's expiry when zero.
func nodeCert(t *testing.T, ca *x509.Certificate, caKey crypto.Signer, notAfter time.Time) tls.Certificate {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := issue(&x509.Certificate{
		Subject:     pkix.Name{CommonName: "node"},
		NotAfter:    notAfter,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, caKey, ca, &k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}
}

func TestHeadTLS(t *testing.T) {
	ca, caKey, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	other, otherKey, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := HeadTLS(ca, caKey)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, len(r.TLS.VerifiedChains))
	}))
	srv.TLS = cfg
	srv.StartTLS()
	defer srv.Close()

	roots := x509.NewCertPool()
	roots.AddCert(ca)
	get := func(cfg *tls.Config) (string, error) {
		hc := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
		defer hc.CloseIdleConnections()
		resp, err := hc.Get(srv.URL)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		return string(b), err
	}
	verified := func(certs ...tls.Certificate) *tls.Config {
		return &tls.Config{RootCAs: roots, ServerName: HeadName, Certificates: certs}
	}
	for name, tc := range map[string]struct {
		cfg  *tls.Config
		want string // "" for a failed handshake
	}{
		"good certificate":    {verified(nodeCert(t, ca, caKey, time.Time{})), "1"},
		"no certificate":      {verified(), "0"},
		"wrong CA":            {verified(nodeCert(t, other, otherKey, time.Time{})), ""},
		"expired certificate": {verified(nodeCert(t, ca, caKey, time.Now().Add(-time.Minute))), ""},
		"pinned CA":           {pinnedTLS(sha256.Sum256(ca.Raw)), "0"},
		"wrong pin":           {pinnedTLS(sha256.Sum256(other.Raw)), ""},
	} {
		got, err := get(tc.cfg)
		if got != tc.want || (tc.want == "") != (err != nil) {
			t.Errorf("%s: got %q, %v; want %q", name, got, err, tc.want)
		}
	}
}

func TestSignNode(t *testing.T) {
	ca, caKey, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, k)
	csr, err := ParseCSR(der)
	if err != nil {
		t.Fatal(err)
	}
	certDER, err := SignNode(ca, caKey, csr, "abc")
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(certDER)
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil || cert.Subject.CommonName != "abc" {
		t.Errorf("node certificate %q does not verify as a client: %v", cert.Subject.CommonName, err)
	}
	// A node certificate must not pass as the head's.
	if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: HeadName}); err == nil {
		t.Error("node certificate verifies as the head")
	}
	der[len(der)-1] ^= 1
	if _, err := ParseCSR(der); err == nil {
		t.Error("ParseCSR accepted a request with a bad signature")
	}
}
