package auth

import (
	"bytes"
	"cmp"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// HeadName is the DNS name in the head's certificate. Nodes verify it
// whatever address they dial, so the head needs no certificate per address.
const HeadName = "boxpool-head"

// PoolDir is the directory in a joined node's state directory that holds
// its key (key.pem), certificate (cert.pem), the head CA (ca.pem) and the
// head's address (head).
const PoolDir = "pool"

// JoinRequest is the body of the head's POST /v1/nodes.
type JoinRequest struct {
	Token string `json:"token"`
	CSR   []byte `json:"csr"` // DER
	Name  string `json:"name"`
	OS    string `json:"os"`
	Arch  string `json:"arch"`
}

// JoinResponse is the head's answer to a JoinRequest.
type JoinResponse struct {
	ID   string `json:"id"`
	Cert []byte `json:"cert"` // DER, signed by CA
	CA   []byte `json:"ca"`   // DER
}

// HeadTLS returns the head's server config: a fresh certificate for
// HeadName signed by the CA, and client certificates verified against the
// CA when given. A client without one can only join.
func HeadTLS(ca *x509.Certificate, caKey crypto.Signer) (*tls.Config, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := issue(&x509.Certificate{
		Subject:     pkix.Name{CommonName: HeadName},
		DNSNames:    []string{HeadName},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, caKey, ca, &key.PublicKey)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		// The CA goes in the chain for joining nodes to match their pin.
		Certificates: []tls.Certificate{{Certificate: [][]byte{der, ca.Raw}, PrivateKey: key}},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    pool,
	}, nil
}

// ParseCSR parses a node's certificate request and checks its signature.
func ParseCSR(der []byte) (*x509.CertificateRequest, error) {
	csr, err := x509.ParseCertificateRequest(der)
	if err == nil {
		err = csr.CheckSignature()
	}
	if err != nil {
		return nil, fmt.Errorf("bad certificate request: %w", err)
	}
	return csr, nil
}

// SignNode returns a client certificate for node id with the CSR's key.
//
// shortcut: node certificates last as long as the CA and cannot be revoked;
// add rotation and revocation with node removal.
func SignNode(ca *x509.Certificate, caKey crypto.Signer, csr *x509.CertificateRequest, id string) ([]byte, error) {
	return issue(&x509.Certificate{
		Subject:     pkix.Name{CommonName: id},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, caKey, ca, csr.PublicKey)
}

// Head returns the address of the head this node joined, or "" if it has
// not joined a pool.
func Head(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, PoolDir, "head"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return strings.TrimSpace(string(b)), err
}

// Join registers this node with the head named in tok, then checks that
// the head accepts the signed certificate, and saves it in dir/PoolDir.
// The node's key never leaves it: the head gets a CSR. req.Token and
// req.CSR are set here.
func Join(ctx context.Context, tok JoinToken, dir string, req JoinRequest) (string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	if req.CSR, err = x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key); err != nil {
		return "", err
	}
	req.Token = tok.String()
	body, _ := json.Marshal(req)
	var out JoinResponse
	if err := call(ctx, pinnedTLS(tok.CA), http.MethodPost, tok.Addr, body, &out); err != nil {
		return "", err
	}
	ca, err := x509.ParseCertificate(out.CA)
	if err != nil || sha256.Sum256(out.CA) != tok.CA {
		return "", errors.New("the head returned a CA that does not match the join token")
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	mtls := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{{Certificate: [][]byte{out.Cert}, PrivateKey: key}},
		RootCAs:      roots,
		ServerName:   HeadName,
	}
	if err := call(ctx, mtls, http.MethodGet, tok.Addr, nil, nil); err != nil {
		return "", fmt.Errorf("check the signed certificate: %w", err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", err
	}
	tmp := filepath.Join(dir, PoolDir+".tmp")
	if err := os.RemoveAll(tmp); err != nil {
		return "", err
	}
	if err := os.Mkdir(tmp, 0700); err != nil {
		return "", err
	}
	for name, data := range map[string][]byte{
		"key.pem":  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		"cert.pem": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: out.Cert}),
		"ca.pem":   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: out.CA}),
		"head":     []byte(tok.Addr + "\n"),
	} {
		if err := os.WriteFile(filepath.Join(tmp, name), data, 0600); err != nil {
			return "", err
		}
	}
	// The rename makes the join visible at once: a node has all four files
	// or none.
	return out.ID, os.Rename(tmp, filepath.Join(dir, PoolDir))
}

// pinnedTLS trusts a head whose chain includes the CA with the SHA-256 fp
// and whose certificate that CA signed for HeadName.
func pinnedTLS(fp [32]byte) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // VerifyConnection checks the chain against the pin instead
		VerifyConnection: func(cs tls.ConnectionState) error {
			for _, c := range cs.PeerCertificates {
				if sha256.Sum256(c.Raw) == fp {
					roots := x509.NewCertPool()
					roots.AddCert(c)
					_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: roots, DNSName: HeadName})
					return err
				}
			}
			return errors.New("the head's certificate authority does not match the join token")
		},
	}
}

// call sends body to the head's /v1/nodes and decodes the answer into out
// if it is not nil.
func call(ctx context.Context, cfg *tls.Config, method, addr string, body []byte, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "https://"+addr+"/v1/nodes", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
	defer hc.CloseIdleConnections()
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("reach the head at %s: %w", addr, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var e struct{ Message string }
		json.NewDecoder(io.LimitReader(resp.Body, 1024)).Decode(&e)
		return fmt.Errorf("the head at %s: %s", addr, cmp.Or(e.Message, resp.Status))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
