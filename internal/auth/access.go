package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
)

const accessPrefix = "bpat1_"

// Scopes of an access token: cli and api reach the head's API for
// sandboxes, snapshots and nodes; expose only opens the URLs boxpool
// expose makes.
var Scopes = []string{"cli", "api", "expose"}

// AccessToken is a bearer token for the head's API on the pool port: a
// secret, and the head CA to pin, so a client needs nothing else to trust
// the head.
type AccessToken struct {
	CA     [32]byte // SHA-256 of the head CA certificate's DER
	Secret [16]byte
}

// NewAccessToken returns a token pinning ca with a random secret.
func NewAccessToken(ca [32]byte) AccessToken {
	t := AccessToken{CA: ca}
	rand.Read(t.Secret[:])
	return t
}

// String encodes the token for the user to copy.
func (t AccessToken) String() string {
	return accessPrefix + base64.RawURLEncoding.EncodeToString(append(t.Secret[:], t.CA[:]...))
}

// Hash is the hex SHA-256 of the secret, the only part the head stores.
func (t AccessToken) Hash() string {
	h := sha256.Sum256(t.Secret[:])
	return hex.EncodeToString(h[:])
}

// TLS returns a client config that trusts only the head the token pins.
func (t AccessToken) TLS() *tls.Config { return pinnedTLS(t.CA) }

// ParseAccessToken decodes a token made by String.
func ParseAccessToken(s string) (AccessToken, error) {
	var t AccessToken
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, accessPrefix))
	if err != nil || !strings.HasPrefix(s, accessPrefix) || len(b) != len(t.Secret)+len(t.CA) {
		return AccessToken{}, errors.New("not a boxpool access token")
	}
	copy(t.CA[:], b[copy(t.Secret[:], b):])
	return t, nil
}
