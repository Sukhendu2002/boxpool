//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/sys/unix"
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
	// Refuse foreign collisions before changing the firewall or deleting anything.
	if err := d.deleteTap(ctx, id, slot); err != nil {
		return nil, err
	}
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
	if err := d.createTap(ctx, id, slot); err != nil {
		return nil, err
	}
	for _, args := range [][]string{
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
	slots, err := d.store.NetSlots(ctx)
	if err != nil {
		return err
	}
	var deleteErr error
	if slot, ok := slots[id]; ok {
		deleteErr = d.deleteTap(ctx, id, slot)
	}
	return errors.Join(deleteErr, d.removeRulesIfUnused(ctx))
}

// sweepNetwork removes the verified owned taps of VMs that are not running
// once adopt has run, then frees allocations whose sandbox directory is gone.
// The firewall rules stay while an adopted VM has its tap.
func (d *Driver) sweepNetwork(ctx context.Context) error {
	slots, err := d.store.NetSlots(ctx)
	if err != nil {
		return err
	}
	for id, slot := range slots {
		if _, ok := running(sandboxLayout(d.root, id)); ok {
			continue
		}
		// Foreign devices, even at an allocated name, are never ours to delete.
		link, err := lookupLink(ctx, tapName(id))
		if err != nil {
			return err
		}
		if link != nil && d.ownsTap(*link, id, slot) {
			slog.Warn("deleting orphaned tap device", "tap", link.Name)
			if err := d.deleteTap(ctx, id, slot); err != nil {
				return err
			}
		}
		if _, err := os.Stat(sandboxLayout(d.root, id).dir); errors.Is(err, fs.ErrNotExist) {
			if err := d.store.FreeNetSlot(ctx, id); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	return d.removeRulesIfUnused(ctx)
}

// tapAlias binds the kernel device to the durable allocation and state root.
// The full hash also distinguishes sandbox IDs with colliding short tap names.
func (d *Driver) tapAlias(id string, slot int) string {
	return fmt.Sprintf("boxpool:%x", sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d", d.root, id, slot))))
}

type networkLink struct {
	Name  string `json:"ifname"`
	Alias string `json:"ifalias"`
	Info  struct {
		Kind string `json:"info_kind"`
		Data struct {
			Type string `json:"type"`
		} `json:"info_data"`
	} `json:"linkinfo"`
}

func lookupLink(ctx context.Context, name string) (*networkLink, error) {
	// Listing avoids treating command failures as an absent device. It also
	// works in network namespaces where the host's sysfs mount sees other links.
	out, err := run(ctx, "", "ip", "-d", "-j", "link", "show")
	if err != nil {
		return nil, err
	}
	var links []networkLink
	if err := json.Unmarshal([]byte(out), &links); err != nil {
		return nil, err
	}
	for _, link := range links {
		if link.Name == name {
			return &link, nil
		}
	}
	return nil, nil
}

func (d *Driver) ownsTap(link networkLink, id string, slot int) bool {
	return link.Name == tapName(id) && link.Alias == d.tapAlias(id, slot) &&
		link.Info.Kind == "tun" && link.Info.Data.Type == "tap"
}

// createTap keeps the TAP nonpersistent until its ownership alias is set.
// A failure or crash before that point closes the fd and removes the device;
// after persistence, the stored allocation and alias make it recoverable.
func (d *Driver) createTap(ctx context.Context, id string, slot int) error {
	f, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	req, err := unix.NewIfreq(tapName(id))
	if err != nil {
		return err
	}
	req.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI | unix.IFF_TUN_EXCL)
	if err := unix.IoctlIfreq(int(f.Fd()), unix.TUNSETIFF, req); err != nil {
		return fmt.Errorf("create tap %s: %w", tapName(id), err)
	}
	if err := unix.IoctlSetInt(int(f.Fd()), unix.TUNSETOWNER, os.Geteuid()); err != nil {
		return err
	}
	if _, err := run(ctx, "", "ip", "link", "set", "dev", tapName(id), "alias", d.tapAlias(id, slot)); err != nil {
		return err
	}
	return unix.IoctlSetInt(int(f.Fd()), unix.TUNSETPERSIST, 1)
}

// deleteTap requires the caller to have read the persisted allocation. A
// foreign device at the exact generated name is an error, never a deletion.
func (d *Driver) deleteTap(ctx context.Context, id string, slot int) error {
	link, err := lookupLink(ctx, tapName(id))
	if err != nil || link == nil {
		return err
	}
	if !d.ownsTap(*link, id, slot) {
		return fmt.Errorf("refusing foreign interface at sandbox tap name %s", link.Name)
	}
	_, err = run(ctx, "", "ip", "link", "del", "dev", link.Name)
	return err
}

// removeRulesIfUnused counts only verified allocated TAPs. Unrelated bp-*
// devices must not keep boxpool rules alive. Accepts go first, so an owned
// TAP is never forwarded without the isolation drops.
func (d *Driver) removeRulesIfUnused(ctx context.Context) error {
	slots, err := d.store.NetSlots(ctx)
	if err != nil {
		return err
	}
	for id, slot := range slots {
		link, err := lookupLink(ctx, tapName(id))
		if err != nil {
			return err
		}
		if link != nil && d.ownsTap(*link, id, slot) {
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
