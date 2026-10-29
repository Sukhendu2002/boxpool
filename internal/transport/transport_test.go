package transport_test

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/auth"
	"boxpool.run/boxpool/internal/transport"
)

// Both system transports use ordinary sockets, so their contract runs on
// loopback in CI without creating tailnet identities or needing auth keys.
func TestTransportContract(t *testing.T) {
	for _, name := range []string{"direct", "tailscale"} {
		t.Run(name, func(t *testing.T) {
			tr, err := transport.New(name)
			if err != nil {
				t.Fatal(err)
			}
			transportContract(t, tr, "127.0.0.1:0")
		})
	}
}

func TestTailnetContract(t *testing.T) {
	ip := os.Getenv("BOXPOOL_TEST_TAILNET_IP")
	if ip == "" {
		t.Skip("set BOXPOOL_TEST_TAILNET_IP to this machine's connected tailnet IP")
	}
	if net.ParseIP(ip) == nil {
		t.Fatal("BOXPOOL_TEST_TAILNET_IP must be an IP address")
	}
	transportContract(t, transport.Tailscale{}, net.JoinHostPort(ip, "0"))
}

func transportContract(t *testing.T, tr transport.Transport, addr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	t.Run("cancellation", func(t *testing.T) {
		cancelled, stop := context.WithCancel(ctx)
		stop()
		if c, err := tr.Dial(cancelled, addr); !errors.Is(err, context.Canceled) {
			if c != nil {
				c.Close()
			}
			t.Fatalf("cancelled Dial = %v", err)
		}
		if l, err := tr.Listen(cancelled, addr); !errors.Is(err, context.Canceled) {
			if l != nil {
				l.Close()
			}
			t.Fatalf("cancelled Listen = %v", err)
		}
	})
	t.Run("listener close", func(t *testing.T) {
		l, err := tr.Listen(ctx, addr)
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		done := make(chan error, 1)
		go func() {
			c, err := l.Accept()
			if c != nil {
				c.Close()
			}
			done <- err
		}()
		if err := l.Close(); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if !errors.Is(err, net.ErrClosed) {
				t.Fatalf("Accept after Close = %v", err)
			}
		case <-ctx.Done():
			t.Fatal("Close did not unblock Accept")
		}
	})
	t.Run("mutual TLS", func(t *testing.T) {
		dir := t.TempDir()
		ca, key, err := auth.LoadOrCreateCA(dir)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := auth.HeadTLS(ca, key)
		if err != nil {
			t.Fatal(err)
		}
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
		if err := auth.Enroll(dir, ca, key, "contract-node", "head:7443"); err != nil {
			t.Fatal(err)
		}
		clientTLS, err := auth.NodeTLS(dir)
		if err != nil {
			t.Fatal(err)
		}
		l, err := tr.Listen(ctx, addr)
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(r.TLS.VerifiedChains) == 0 {
				t.Error("unverified connection reached handler")
			}
			io.WriteString(w, "pool stream")
		}))
		srv.Listener.Close()
		srv.Listener, srv.TLS = l, cfg
		srv.Config.ErrorLog = log.New(io.Discard, "", 0)
		srv.StartTLS()
		defer srv.Close()
		for _, authenticated := range []bool{true, false} {
			cfg := clientTLS.Clone()
			if !authenticated {
				cfg.Certificates = nil
			}
			ht := &http.Transport{TLSClientConfig: cfg, DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				return tr.Dial(ctx, addr)
			}}
			defer ht.CloseIdleConnections()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
			resp, err := (&http.Client{Transport: ht}).Do(req)
			if !authenticated {
				if err == nil {
					resp.Body.Close()
					t.Fatal("accepted a client without a pool certificate")
				}
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || resp.StatusCode != http.StatusOK || string(body) != "pool stream" {
				t.Fatalf("authenticated request = %s %q, %v", resp.Status, body, err)
			}
		}
	})
}

func TestDialTLSDeadline(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		c, err := l.Accept()
		if err == nil {
			defer c.Close()
			<-done // accept TCP but never answer the TLS handshake
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	c, err := transport.DialTLS(ctx, l.Addr().String(), &tls.Config{ServerName: auth.HeadName})
	if c != nil {
		c.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled TLS handshake = %v, want deadline exceeded", err)
	}
}

func TestTransportSelection(t *testing.T) {
	for _, addr := range []string{"100.64.0.7:7443", "[fd7a:115c:a1e0::7]:7443"} {
		if _, ok := transport.ForAddress(addr).(transport.Tailscale); !ok {
			t.Errorf("%s did not select tailscale", addr)
		}
	}
	for _, addr := range []string{":7443", "head.local:7443", "192.168.1.2:7443", "[::1]:7443"} {
		if _, ok := transport.ForAddress(addr).(transport.Direct); !ok {
			t.Errorf("%s did not select direct", addr)
		}
	}
	if _, err := transport.New("unknown"); err == nil {
		t.Error("accepted an unknown transport")
	}
}
