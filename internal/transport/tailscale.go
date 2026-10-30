package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Tailscale uses system tailscaled (including clients managed by Headscale).
// It shares Direct's sockets: the OS routes tailnet addresses through the
// installed client. Userspace-only tailscaled networking is not supported.
type Tailscale struct{ Direct }

func (Tailscale) Address(ctx context.Context, port string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := tailscale(ctx, "ip", "-1")
	if err != nil {
		return "", fmt.Errorf("find this machine's tailnet address: %w; run tailscale up, or pass --addr", err)
	}
	ip := net.ParseIP(strings.TrimSpace(string(out)))
	if ip == nil {
		return "", fmt.Errorf("tailscale ip -1 printed %q, not an address", out)
	}
	return net.JoinHostPort(ip.String(), port), nil
}

// Serve has tailscaled serve https://<this machine's tailnet name>:port/
// to the tailnet, with a certificate for that name, from
// http://127.0.0.1:port, until Unserve. It returns that URL.
func (Tailscale) Serve(ctx context.Context, port int) (string, error) {
	// Serve waits for the user when HTTPS is not enabled on the tailnet.
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	host, err := (Tailscale{}).CheckServe(ctx)
	if err != nil {
		return "", err
	}
	p := strconv.Itoa(port)
	if _, err := tailscale(ctx, "serve", "--bg", "--https="+p, "http://127.0.0.1:"+p); err != nil {
		return "", fmt.Errorf("serve port %s over HTTPS: %w; run tailscale serve as the daemon user, or grant that user operator access with sudo tailscale set --operator=<daemon-user>", p, err)
	}
	return "https://" + host + ":" + p + "/", nil
}

// CheckServe checks the head's connection, MagicDNS and HTTPS capability
// without changing its Serve configuration or prompting interactively.
func (Tailscale) CheckServe(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := tailscale(ctx, "status", "--json")
	if err != nil {
		return "", fmt.Errorf("find this machine's tailnet name: %w; run tailscale up", err)
	}
	var st struct {
		BackendState   string
		Self           struct{ DNSName string }
		CurrentTailnet struct{ MagicDNSEnabled bool }
		CertDomains    []string
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return "", fmt.Errorf("parse tailscale status: %w", err)
	}
	if st.BackendState != "Running" {
		return "", fmt.Errorf("Tailscale is %q: start Tailscale and run tailscale up on the head", st.BackendState)
	}
	host := strings.TrimSuffix(st.Self.DNSName, ".")
	if !st.CurrentTailnet.MagicDNSEnabled || host == "" {
		return "", errors.New("enable MagicDNS at https://login.tailscale.com/admin/dns on the head's tailnet")
	}
	if !slices.Contains(st.CertDomains, host) {
		return "", errors.New("enable HTTPS Certificates at https://login.tailscale.com/admin/dns on the head's tailnet, then retry")
	}
	return host, nil
}

// Unserve stops serving port.
func (Tailscale) Unserve(ctx context.Context, port int) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, err := tailscale(ctx, "serve", "--https="+strconv.Itoa(port), "off")
	return err
}

// tailscale runs the tailscale CLI and returns its standard output.
func tailscale(ctx context.Context, args ...string) ([]byte, error) {
	bin, err := exec.LookPath("tailscale")
	if err != nil && runtime.GOOS == "darwin" {
		bin, err = "/Applications/Tailscale.app/Contents/MacOS/Tailscale", nil
	}
	if err != nil {
		return nil, errors.New("the tailscale CLI is not installed: install Tailscale on the head and make tailscale available in the daemon's PATH")
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	// A wrapper can leave a child holding stdout after it is killed.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		} else if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("tailscale %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}
