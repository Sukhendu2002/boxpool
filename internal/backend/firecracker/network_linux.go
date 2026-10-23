//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// checkNetwork reports what sandbox networking lacks on this host.
func checkNetwork() error {
	if os.Geteuid() != 0 {
		return errors.New("sandbox networking needs root to create tap devices and firewall rules: run the daemon as root")
	}
	for _, cmd := range []string{"ip", "nft"} {
		if _, err := exec.LookPath(cmd); err != nil {
			return fmt.Errorf("sandbox networking needs %s (iproute2 and nftables): %w", cmd, err)
		}
	}
	return nil
}

// startNetwork gives a sandbox its tap device, with the firewall rules in
// place first, and returns its interface. After an error, stopNetwork
// removes what it set up.
func (d *Driver) startNetwork(ctx context.Context, id string) (*nic, error) {
	slot, err := d.store.NetSlot(ctx, id, netSlots)
	if err != nil {
		return nil, fmt.Errorf("sandbox %s: %w", id, err)
	}
	n := sandboxNIC(id, slot)
	d.netMu.Lock()
	defer d.netMu.Unlock()
	uplink, err := routeDev(ctx, "1.1.1.1")
	if err != nil {
		return nil, fmt.Errorf("find the host's outbound interface: %w", err)
	}
	if err := enableForwarding(); err != nil {
		return nil, err
	}
	if _, err := run(ctx, ruleset(uplink), "nft", "-f", "-"); err != nil {
		return nil, err
	}
	if err := addForwardAccepts(ctx); err != nil {
		return nil, err
	}
	n.dns = resolvers(ctx, uplink)
	deleteTap(ctx, n.tap) // left by a crash between creating and recording it
	uid := strconv.Itoa(os.Geteuid())
	for _, args := range [][]string{
		{"tuntap", "add", "dev", n.tap, "mode", "tap", "user", uid},
		{"addr", "add", n.gateway.String() + "/30", "dev", n.tap},
		{"link", "set", n.tap, "up"},
	} {
		if _, err := run(ctx, "", "ip", args...); err != nil {
			return nil, err
		}
	}
	return &n, nil
}

// stopNetwork deletes a sandbox's tap device and, once no sandbox has one,
// boxpool's firewall rules.
func (d *Driver) stopNetwork(ctx context.Context, id string) error {
	if d.store == nil {
		return nil
	}
	d.netMu.Lock()
	defer d.netMu.Unlock()
	if err := deleteTap(ctx, tapName(id)); err != nil {
		return err
	}
	return removeRulesIfUnused(ctx)
}

// sweepNetwork deletes every boxpool tap device, and then the firewall
// rules, after killOrphans has killed every sandbox's VM. It frees the slots
// of sandboxes that are gone.
// shortcut: deletes taps of VMs it could re-adopt; #27 must keep those.
func (d *Driver) sweepNetwork(ctx context.Context) error {
	ifaces, err := net.Interfaces()
	if err != nil {
		return err
	}
	for _, i := range ifaces {
		if strings.HasPrefix(i.Name, tapPrefix) {
			slog.Warn("deleting orphaned tap device", "tap", i.Name)
			if err := deleteTap(ctx, i.Name); err != nil {
				return err
			}
		}
	}
	if err := removeRulesIfUnused(ctx); err != nil {
		return err
	}
	slots, err := d.store.NetSlots(ctx)
	if err != nil {
		return err
	}
	for id := range slots {
		if _, err := os.Stat(sandboxLayout(d.root, id).dir); errors.Is(err, fs.ErrNotExist) {
			if err := d.store.FreeNetSlot(ctx, id); err != nil {
				return err
			}
		}
	}
	return nil
}

func deleteTap(ctx context.Context, tap string) error {
	if _, err := net.InterfaceByName(tap); err != nil {
		return nil // already gone
	}
	_, err := run(ctx, "", "ip", "link", "del", "dev", tap)
	return err
}

// removeRulesIfUnused removes boxpool's firewall rules unless a tap device
// still needs them. The iptables accepts go first, so a tap is never
// forwarded without the isolation rules.
func removeRulesIfUnused(ctx context.Context) error {
	ifaces, err := net.Interfaces()
	if err != nil {
		return err
	}
	for _, i := range ifaces {
		if strings.HasPrefix(i.Name, tapPrefix) {
			return nil
		}
	}
	if err := removeForwardAccepts(ctx); err != nil {
		return err
	}
	_, err = run(ctx, deleteTable, "nft", "-f", "-")
	return err
}

// addForwardAccepts adds forwardAccepts where an iptables FORWARD policy of
// DROP would otherwise drop sandbox traffic: in DOCKER-USER, which Docker
// leaves to the user, or at the top of FORWARD.
func addForwardAccepts(ctx context.Context) error {
	if _, err := exec.LookPath("iptables"); err != nil {
		return nil
	}
	out, err := run(ctx, "", "iptables", "-S", "FORWARD")
	if err != nil || !strings.Contains(out, "-P FORWARD DROP") {
		return nil
	}
	chain := "FORWARD"
	if _, err := run(ctx, "", "iptables", "-S", "DOCKER-USER"); err == nil {
		chain = "DOCKER-USER"
	}
	for _, rule := range forwardAccepts {
		if _, err := run(ctx, "", "iptables", append([]string{"-C", chain}, rule...)...); err == nil {
			continue
		}
		if _, err := run(ctx, "", "iptables", append([]string{"-I", chain}, rule...)...); err != nil {
			return err
		}
	}
	return nil
}

// removeForwardAccepts deletes forwardAccepts from both chains that
// addForwardAccepts may have used.
func removeForwardAccepts(ctx context.Context) error {
	if _, err := exec.LookPath("iptables"); err != nil {
		return nil
	}
	for _, chain := range []string{"DOCKER-USER", "FORWARD"} {
		for _, rule := range forwardAccepts {
			for {
				if _, err := run(ctx, "", "iptables", append([]string{"-C", chain}, rule...)...); err != nil {
					break // absent, or no such chain
				}
				if _, err := run(ctx, "", "iptables", append([]string{"-D", chain}, rule...)...); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// enableForwarding turns on IPv4 forwarding, which NAT needs. It stays on.
func enableForwarding() error {
	const p = "/proc/sys/net/ipv4/ip_forward"
	b, err := os.ReadFile(p)
	if err != nil || strings.TrimSpace(string(b)) == "1" {
		return err
	}
	slog.Warn("enabling IPv4 forwarding for sandbox networking; it stays on until reboot")
	return os.WriteFile(p, []byte("1\n"), 0644)
}

// resolvers returns the host's upstream nameservers that are routed out of
// uplink. They exclude systemd-resolved's 127.0.0.53, any resolver on the
// host itself (input from sandboxes is dropped) and ones behind other
// interfaces such as a VPN's (forwarding there is dropped).
func resolvers(ctx context.Context, uplink string) []netip.Addr {
	var dns []netip.Addr
	for _, p := range []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, a := range nameservers(b) {
			if dev, err := routeDev(ctx, a.String()); err == nil && dev == uplink && len(dns) < 3 {
				dns = append(dns, a)
			}
		}
		break
	}
	if len(dns) == 0 {
		return fallbackDNS
	}
	return dns
}

// routeDev returns the interface the host routes dst through, "lo" for its
// own addresses.
func routeDev(ctx context.Context, dst string) (string, error) {
	out, err := run(ctx, "", "ip", "-j", "route", "get", dst)
	if err != nil {
		return "", err
	}
	var routes []struct{ Dev string }
	if err := json.Unmarshal([]byte(out), &routes); err != nil || len(routes) == 0 || routes[0].Dev == "" {
		return "", fmt.Errorf("ip route get %s: unexpected output %q", dst, out)
	}
	return routes[0].Dev, nil
}

// run runs a command with stdin and returns its output.
func run(ctx context.Context, stdin, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, bytes.TrimSpace(stderr.Bytes()))
	}
	return stdout.String(), nil
}
