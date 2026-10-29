// Package auth holds the head's join tokens and its certificate authority,
// which join pins and which signs node certificates.
package auth

import (
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

// LoadOrCreateCA returns the head CA certificate in dir, creating it and
// its key first if there is none.
func LoadOrCreateCA(dir string) (*x509.Certificate, error) {
	certPath := filepath.Join(dir, "ca.pem")
	if b, err := os.ReadFile(certPath); err == nil {
		p, _ := pem.Decode(b)
		if p == nil {
			return nil, fmt.Errorf("%s: no PEM certificate", certPath)
		}
		return x509.ParseCertificate(p.Bytes)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "boxpool head CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	// The key first: a key without a certificate is regenerated next time.
	if err := os.WriteFile(filepath.Join(dir, "ca-key.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		return nil, err
	}
	tmp := certPath + ".tmp"
	if err := os.WriteFile(tmp, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, certPath); err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}
