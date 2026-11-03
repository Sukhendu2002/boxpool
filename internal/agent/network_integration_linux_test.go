//go:build linux && integration

package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"
)

// Reproduce a VM booted without a NIC: a fresh network namespace starts with
// loopback down. Never change the caller's network namespace or interfaces.
func TestIntegrationGuestLoopback(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root and unshare for a disposable network namespace")
	}
	if os.Getenv("BOXPOOL_AGENT_NETNS_TEST") != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "unshare", "-n", os.Args[0], "-test.v", "-test.run=^TestIntegrationGuestLoopback$", "-test.timeout=20s")
		cmd.Env = append(os.Environ(), "BOXPOOL_AGENT_NETNS_TEST=1")
		out, err := cmd.CombinedOutput()
		t.Logf("isolated loopback regression:\n%s", out)
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	current, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	initial, err := os.Readlink("/proc/1/ns/net")
	if err != nil || current == initial {
		t.Fatal("refusing the host network namespace")
	}
	lo, err := net.InterfaceByName("lo")
	if err != nil || lo.Flags&net.FlagUp != 0 {
		t.Fatalf("expected initially down loopback: %v, %v", lo, err)
	}
	if err := PrepareGuestNetwork(); err != nil {
		t.Fatal(err)
	}
	if err := PrepareGuestNetwork(); err != nil {
		t.Fatalf("repeat setup: %v", err)
	}
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "loopback-ok") })}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	c, err := net.Dial("unix", serveSocket(t))
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c, err = DialTCP(testContext(t), c, l.Addr().(*net.TCPAddr).Port)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := io.WriteString(c, "GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK || string(b) != "loopback-ok" {
		t.Fatalf("forwarded response: status %d, body %q, %v", resp.StatusCode, b, err)
	}
}
