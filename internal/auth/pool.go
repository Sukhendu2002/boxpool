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

	"boxpool.run/boxpool/internal/transport"
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
	mtls := nodeTLS(tls.Certificate{Certificate: [][]byte{out.Cert}, PrivateKey: key}, ca)
	if err := call(ctx, mtls, http.MethodGet, tok.Addr, nil, nil); err != nil {
		return "", fmt.Errorf("check the signed certificate: %w", err)
	}

	return out.ID, save(dir, key, out.Cert, out.CA, tok.Addr)
}

// Enroll makes the head that holds ca and caKey a node of its own pool,
// with ID id at addr, as Join does for other machines, so that it can
// follow another head after a failover.
func Enroll(dir string, ca *x509.Certificate, caKey crypto.Signer, id, addr string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		return err
	}
	csr, err := ParseCSR(der)
	if err != nil {
		return err
	}
	cert, err := SignNode(ca, caKey, csr, id)
	if err != nil {
		return err
	}
	return save(dir, key, cert, ca.Raw, addr)
}

// save writes a node's key, certificate, head CA and head address to
// dir/PoolDir.
func save(dir string, key *ecdsa.PrivateKey, cert, ca []byte, head string) error {
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, PoolDir+".tmp")
	if err := os.RemoveAll(tmp); err != nil {
		return err
	}
	if err := os.Mkdir(tmp, 0700); err != nil {
		return err
	}
	for name, data := range map[string][]byte{
		"key.pem":  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		"cert.pem": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert}),
		"ca.pem":   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca}),
		"head":     []byte(head + "\n"),
	} {
		if err := os.WriteFile(filepath.Join(tmp, name), data, 0600); err != nil {
			return err
		}
	}
	// The rename makes the join visible at once: a node has all four files
	// or none.
	return os.Rename(tmp, filepath.Join(dir, PoolDir))
}

// SetHead records addr as the head of the pool this node joined.
func SetHead(dir, addr string) error {
	return writeFile(filepath.Join(dir, PoolDir, "head"), []byte(addr+"\n"))
}

// Peers returns the addresses of the machines that keep a copy of the
// pool's state, as the head last listed them: where a node looks for a new
// head.
func Peers(dir string) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(dir, PoolDir, "peers"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return strings.Fields(string(b)), err
}

// SetPeers records the addresses Peers returns.
func SetPeers(dir string, addrs []string) error {
	return writeFile(filepath.Join(dir, PoolDir, "peers"), []byte(strings.Join(addrs, "\n")+"\n"))
}

// writeFile replaces the file at path with data, whole.
func writeFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// InstallCA saves the head CA's key, which a machine needs to keep a copy
// of the pool's state and take over as head. The CA must be the one this
// node joined with, so only a holder of that CA's key can install it.
func InstallCA(dir string, certDER, keyDER []byte) error {
	b, err := os.ReadFile(filepath.Join(dir, PoolDir, "ca.pem"))
	if err != nil {
		return err
	}
	if p, _ := pem.Decode(b); p == nil || !bytes.Equal(p.Bytes, certDER) {
		return errors.New("not the CA of the pool this machine joined")
	}
	k, err := x509.ParsePKCS8PrivateKey(keyDER)
	if err != nil {
		return err
	}
	ca, _ := x509.ParseCertificate(certDER)
	if signer, ok := k.(crypto.Signer); !ok || ca == nil || !publicKeyEqual(ca.PublicKey, signer.Public()) {
		return errors.New("the key does not belong to the pool's CA")
	}
	return writeCA(dir, certDER, keyDER)
}

func publicKeyEqual(a, b crypto.PublicKey) bool {
	k, ok := a.(interface{ Equal(crypto.PublicKey) bool })
	return ok && k.Equal(b)
}

// NodeTLS returns the client config of the node whose state directory is
// dir, from the key and certificates Join saved there.
func NodeTLS(dir string) (*tls.Config, error) {
	d := filepath.Join(dir, PoolDir)
	cert, err := tls.LoadX509KeyPair(filepath.Join(d, "cert.pem"), filepath.Join(d, "key.pem"))
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(d, "ca.pem"))
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, fmt.Errorf("%s: no certificate", filepath.Join(d, "ca.pem"))
	}
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	return nodeTLS(cert, ca), nil
}

// nodeTLS presents cert and trusts only a head certificate signed by ca.
func nodeTLS(cert tls.Certificate, ca *x509.Certificate) *tls.Config {
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		RootCAs:      roots,
		ServerName:   HeadName,
	}
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
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg, DialContext: transport.DialContext}}
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
