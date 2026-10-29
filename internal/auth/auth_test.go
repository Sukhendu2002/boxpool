package auth

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJoinTokenRoundTrip(t *testing.T) {
	tok := NewJoinToken("head.local:7443", sha256.Sum256([]byte("ca")))
	got, err := ParseJoinToken(tok.String())
	if err != nil || got != tok {
		t.Fatalf("ParseJoinToken(%q) = %+v, %v; want %+v", tok, got, err, tok)
	}
	if other := NewJoinToken(tok.Addr, tok.CA); other.Secret == tok.Secret || other.Hash() == tok.Hash() {
		t.Error("two tokens share a secret")
	}
	if strings.Contains(tok.String(), tok.Hash()) || len(tok.Hash()) != 64 {
		t.Errorf("Hash() = %q", tok.Hash())
	}
}

func TestParseJoinTokenRejects(t *testing.T) {
	good := NewJoinToken("head:7443", [32]byte{}).String()
	for _, s := range []string{
		"",
		"bpjoin1_",
		strings.TrimPrefix(good, tokenPrefix),
		good[:len(tokenPrefix)+60],
		good + "!",
		NewJoinToken("no-port", [32]byte{}).String(),
		NewJoinToken(":7443", [32]byte{}).String(),
		NewJoinToken("head:", [32]byte{}).String(),
		NewJoinToken("head:0", [32]byte{}).String(),
		NewJoinToken("head:-1", [32]byte{}).String(),
		NewJoinToken("head:65536", [32]byte{}).String(),
		NewJoinToken("head:https", [32]byte{}).String(),
	} {
		if _, err := ParseJoinToken(s); err == nil {
			t.Errorf("ParseJoinToken(%q) succeeded", s)
		}
	}
}

func TestJoinTokenValidAddresses(t *testing.T) {
	for _, addr := range []string{"head:1", "head:65535", "127.0.0.1:7443", "[::1]:7443", "[fe80::1%en0]:7443"} {
		t.Run(addr, func(t *testing.T) {
			tok := NewJoinToken(addr, [32]byte{})
			got, err := ParseJoinToken(tok.String())
			if err != nil || got != tok {
				t.Fatalf("token round trip for %q = %+v, %v; want %+v", addr, got, err, tok)
			}
		})
	}
}

func TestLoadOrCreateCA(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !ca.IsCA || ca.CheckSignatureFrom(ca) != nil {
		t.Errorf("CA is not a self-signed CA: IsCA=%v", ca.IsCA)
	}
	again, err := LoadOrCreateCA(dir)
	if err != nil || !again.Equal(ca) {
		t.Fatalf("second LoadOrCreateCA returned a different CA: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, "ca-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Errorf("ca-key.pem mode = %v, want 0600", fi.Mode())
	}
}
