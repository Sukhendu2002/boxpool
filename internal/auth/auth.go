// Package auth holds the head's join tokens and its certificate authority,
// which join pins and which signs node certificates.
package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// JoinPort is the head's port for joining nodes.
const JoinPort = "7443"

const tokenPrefix = "bpjoin1_"

// JoinToken is what a node needs to join a pool: where the head is, the
// head CA to pin, and a single-use secret.
type JoinToken struct {
	Addr   string   // host:port
	CA     [32]byte // SHA-256 of the head CA certificate's DER
	Secret [16]byte
}

// NewJoinToken returns a token for the head at addr with a random secret.
func NewJoinToken(addr string, ca [32]byte) JoinToken {
	t := JoinToken{Addr: addr, CA: ca}
	rand.Read(t.Secret[:])
	return t
}

// String encodes the token for the user to copy.
func (t JoinToken) String() string {
	b := append(append(t.Secret[:], t.CA[:]...), t.Addr...)
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(b)
}

// Hash is the hex SHA-256 of the secret, the only part the head stores.
func (t JoinToken) Hash() string {
	h := sha256.Sum256(t.Secret[:])
	return hex.EncodeToString(h[:])
}

// ParseJoinToken decodes a token made by String.
func ParseJoinToken(s string) (JoinToken, error) {
	var t JoinToken
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, tokenPrefix))
	if err != nil || !strings.HasPrefix(s, tokenPrefix) || len(b) <= len(t.Secret)+len(t.CA) {
		return JoinToken{}, errors.New("not a boxpool join token")
	}
	n := copy(t.Secret[:], b)
	n += copy(t.CA[:], b[n:])
	t.Addr = string(b[n:])
	if err := ValidateJoinAddr(t.Addr); err != nil {
		return JoinToken{}, fmt.Errorf("join token has a bad head address: %w", err)
	}
	return t, nil
}

// ValidateJoinAddr checks for a nonempty host and a numeric TCP port in
// the range 1 through 65535, without requiring the host to resolve locally.
func ValidateJoinAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err == nil && host != "" {
		n, err := strconv.ParseUint(port, 10, 16)
		if err == nil && n > 0 {
			return nil
		}
	}
	return fmt.Errorf("invalid head address %q: use host:port with a numeric port from 1 to 65535", addr)
}

// LoadOrCreateCA returns the head CA certificate and key in dir, creating
// them first if there are none.
func LoadOrCreateCA(dir string) (*x509.Certificate, crypto.Signer, error) {
	certPath, keyPath := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca-key.pem")
	if b, err := os.ReadFile(certPath); err == nil {
		return loadCA(certPath, b, keyPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		Subject:               pkix.Name{CommonName: "boxpool head CA"},
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := issue(tmpl, key, tmpl, &key.PublicKey)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	if err := writeCA(dir, der, keyDER); err != nil {
		return nil, nil, err
	}
	ca, err := x509.ParseCertificate(der)
	return ca, key, err
}

// writeCA saves the head CA certificate and key in dir.
func writeCA(dir string, der, keyDER []byte) error {
	certPath, keyPath := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca-key.pem")
	// The key first: a key without a certificate is regenerated next time.
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		return err
	}
	return writeFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func loadCA(certPath string, certPEM []byte, keyPath string) (*x509.Certificate, crypto.Signer, error) {
	p, _ := pem.Decode(certPEM)
	if p == nil {
		return nil, nil, fmt.Errorf("%s: no PEM certificate", certPath)
	}
	ca, err := x509.ParseCertificate(p.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", certPath, err)
	}
	b, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, err
	}
	if p, _ = pem.Decode(b); p == nil {
		return nil, nil, fmt.Errorf("%s: no PEM key", keyPath)
	}
	k, err := x509.ParsePKCS8PrivateKey(p.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", keyPath, err)
	}
	key, ok := k.(crypto.Signer)
	if !ok {
		return nil, nil, fmt.Errorf("%s: not a signing key", keyPath)
	}
	return ca, key, nil
}

// issue signs tmpl with the parent's key, giving it a random serial and,
// unless set, the parent's expiry.
func issue(tmpl *x509.Certificate, key crypto.Signer, parent *x509.Certificate, pub any) ([]byte, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl.SerialNumber = serial
	if tmpl.NotBefore.IsZero() {
		tmpl.NotBefore = time.Now().Add(-time.Hour) // tolerate clock skew between machines
	}
	if tmpl.NotAfter.IsZero() {
		tmpl.NotAfter = parent.NotAfter
	}
	return x509.CreateCertificate(rand.Reader, tmpl, parent, pub, key)
}
