package docker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
)

type networkInfo struct {
	ID              string `json:"Id"`
	Name, Driver    string
	Labels, Options map[string]string
}

func (d *Driver) networkTable(id string) string {
	return fmt.Sprintf("bp_d_%x", sha256.Sum256([]byte(d.root+"\x00"+id)))[:29]
}

func (d *Driver) protectNetwork(ctx context.Context, id string) error {
	if d.network == nil { // in-memory Engine tests; New always installs the host enforcer
		return nil
	}
	var n networkInfo
	if err := d.e.do(ctx, http.MethodGet, "/networks/"+d.name(id), nil, nil, &n); err != nil {
		return err
	}
	return d.protect(ctx, id, n)
}

func (d *Driver) protect(ctx context.Context, id string, n networkInfo) error {
	if n.Name != d.name(id) || !d.owns(n.Labels, id) {
		return fmt.Errorf("network %s %w", n.Name, errForeign)
	}
	if n.Driver != "bridge" || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(n.ID) || n.Options["com.docker.network.bridge.name"] != "" {
		return fmt.Errorf("network %s: expected an Engine-managed default bridge", n.Name)
	}
	return d.network(ctx, d.networkTable(id), "br-"+n.ID[:12])
}

// Restore protection for every owned network, including orphaned networks
// retained by a failed rollback, before adopting any running containers.
func (d *Driver) protectNetworks(ctx context.Context) (err error) {
	defer func() {
		if err == nil {
			return
		}
		cleanup, cancel := cleanupContext(ctx)
		defer cancel()
		list, e := d.List(cleanup)
		err = errors.Join(err, e)
		for _, sb := range list {
			c, e := d.inspect(cleanup, sb.ID)
			if e == nil && c.State.Running {
				e = d.stop(cleanup, c.ID, 0)
			}
			err = errors.Join(err, e)
		}
	}()
	for _, tool := range []string{"nft", "ip"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("Docker isolation requires %s: %w", tool, err)
		}
	}
	var info struct{ SecurityOptions []string }
	if err := d.e.do(ctx, http.MethodGet, "/info", nil, nil, &info); err != nil {
		return err
	}
	for _, opt := range info.SecurityOptions {
		if strings.HasPrefix(opt, "name=rootless") {
			return errors.New("Docker isolation requires a local rootful Engine")
		}
	}
	filters, _ := json.Marshal(map[string][]string{"label": {labelOwner + "=" + d.root}})
	var networks []networkInfo
	if err := d.e.do(ctx, http.MethodGet, "/networks", url.Values{"filters": {string(filters)}}, nil, &networks); err != nil {
		return err
	}
	for _, n := range networks {
		id := n.Labels[labelSandbox]
		if err := checkID(id); err != nil {
			return err
		}
		if err := d.protect(ctx, id, n); err != nil {
			return err
		}
	}
	return nil
}

func (d *Driver) removeNetworkRules(ctx context.Context, id string) error {
	if d.network == nil {
		return nil
	}
	return d.network(ctx, d.networkTable(id), "")
}

// Each network has its own table: concurrent sandboxes and state directories
// never replace one another's rules. Delete only after Engine network removal.
func hostNetwork(ctx context.Context, table, bridge string) error {
	script := "add table inet " + table + "\ndelete table inet " + table + "\n"
	if bridge != "" {
		// A local socket could refer to an Engine in another network namespace.
		// Refuse to protect a bridge that is not actually on this host.
		out, err := networkCommand(ctx, "", "ip", "-d", "-j", "link", "show", "dev", bridge)
		if err != nil {
			return err
		}
		var links []struct {
			Info struct {
				Kind string `json:"info_kind"`
			} `json:"linkinfo"`
		}
		if json.Unmarshal(out, &links) != nil || len(links) != 1 || links[0].Info.Kind != "bridge" {
			return fmt.Errorf("%s is not a local Docker bridge", bridge)
		}
		out, err = networkCommand(ctx, "", "ip", "-j", "route", "get", "1.1.1.1")
		if err != nil {
			return err
		}
		var routes []struct{ Dev string }
		if json.Unmarshal(out, &routes) != nil || len(routes) == 0 || !regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,15}$`).MatchString(routes[0].Dev) {
			return errors.New("cannot determine Docker outbound interface")
		}
		script += dockerRules(table, bridge, routes[0].Dev)
	}
	_, err := networkCommand(ctx, script, "nft", "-f", "-")
	return err
}

func dockerRules(table, bridge, uplink string) string {
	return fmt.Sprintf(`table inet %s {
	chain prerouting {
		type filter hook prerouting priority -110; policy accept;
		iifname "%s" meta nfproto ipv6 drop
		iifname "%s" fib saddr . iif oif missing drop
		iifname "%s" ct direction original fib daddr type local drop
	}
	chain input {
		type filter hook input priority filter; policy accept;
		iifname "%s" ct direction reply ct state established,related accept
		iifname "%s" drop
	}
	chain forward {
		type filter hook forward priority filter; policy accept;
		iifname "%s" oifname != "%s" drop
		oifname "%s" ct direction reply ct state established,related accept
		oifname "%s" drop
	}
}
`, table, bridge, bridge, bridge, bridge, bridge, bridge, uplink, bridge, bridge)
}

func networkCommand(ctx context.Context, stdin, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("Docker isolation: %s %s: %w: %s", name, strings.Join(args, " "), err, out)
	}
	return out, nil
}
