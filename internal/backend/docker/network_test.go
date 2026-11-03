package docker

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRestoreNetworkProtection(t *testing.T) {
	for _, failure := range []string{"none", "firewall", "rootless", "engine"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			f := newFakeEngine(t)
			d := f.driver(ctx, f.serve())
			if _, err := d.Create(ctx, "abc", spec); err != nil {
				t.Fatal(err)
			}
			if err := d.Start(ctx, "abc"); err != nil {
				t.Fatal(err)
			}
			// Tool lookup is tested independently of the host's installed tools.
			bin := t.TempDir()
			for _, tool := range []string{"nft", "ip"} {
				if err := os.WriteFile(filepath.Join(bin, tool), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", bin)
			var calls int
			d.network = func(context.Context, string, string) error {
				calls++
				if failure == "firewall" {
					return errors.New("firewall failed")
				}
				return nil
			}
			f.rootless = failure == "rootless"
			if failure == "engine" {
				f.fail["GET /networks"] = 500
			}
			err := d.protectNetworks(ctx)
			if failure == "none" {
				if err != nil || calls != 1 || !f.containers[d.name("abc")].running {
					t.Fatalf("restore: %d calls, %v", calls, err)
				}
			} else {
				if err == nil || f.containers[d.name("abc")].running {
					t.Fatalf("failed restore left an unprotected guest running: %v", err)
				}
			}
		})
	}
}

func TestNetworkProtectionLifecycle(t *testing.T) {
	ctx := context.Background()
	f := newFakeEngine(t)
	d := f.driver(ctx, f.serve())
	var installed bool
	d.network = func(_ context.Context, table, bridge string) error {
		if table != d.networkTable("abc") {
			t.Fatalf("wrong table: %s", table)
		}
		installed = bridge != ""
		return nil
	}
	f.hook = func(method, path string) {
		if method == http.MethodPost && path == "/containers/create" && !installed {
			t.Error("container created before isolation")
		}
	}
	if _, err := d.Create(ctx, "abc", spec); err != nil {
		t.Fatal(err)
	}
	if err := d.Start(ctx, "abc"); err != nil {
		t.Fatal(err)
	}
	// Reinstall even on the already-running path; on failure, stop the guest.
	d.network = func(context.Context, string, string) error { return errors.New("firewall failed") }
	if err := d.Start(ctx, "abc"); err == nil || !strings.Contains(err.Error(), "firewall failed") {
		t.Fatalf("failed firewall: %v", err)
	}
	if f.containers[d.name("abc")].running {
		t.Error("unprotected container left running")
	}
	d.network = func(_ context.Context, _, bridge string) error { installed = bridge != ""; return nil }
	f.fail["DELETE /networks/"+f.networks[d.name("abc")].id] = 500
	if err := d.Remove(ctx, "abc"); err == nil {
		t.Fatal("failed network removal accepted")
	}
	if !installed {
		t.Fatal("protection removed while network still exists")
	}
	delete(f.fail, "DELETE /networks/"+f.networks[d.name("abc")].id)
	if err := d.Remove(ctx, "abc"); err != nil {
		t.Fatal(err)
	}
	if installed {
		t.Error("protection left after network removal")
	}
}

func TestNetworkProtectionRejectsForeignAndNonstandardBridges(t *testing.T) {
	f := newFakeEngine(t)
	d := f.driver(context.Background(), f.serve())
	n := networkInfo{ID: strings.Repeat("a", 64), Name: d.name("abc"), Driver: "bridge", Labels: labels(d.root, "abc")}
	d.network = func(context.Context, string, string) error { t.Fatal("invalid network reached enforcer"); return nil }
	for _, change := range []func(*networkInfo){
		func(n *networkInfo) { n.Labels = labels("other", "abc") },
		func(n *networkInfo) { n.Name = "other" },
		func(n *networkInfo) { n.ID = "bad" },
		func(n *networkInfo) { n.Driver = "host" },
		func(n *networkInfo) { n.Options = map[string]string{"com.docker.network.bridge.name": "custom"} },
	} {
		bad := n
		change(&bad)
		if err := d.protect(context.Background(), "abc", bad); err == nil {
			t.Error("unsafe network accepted")
		}
	}
}

func TestNetworkProtectionFailureRollsBackCreate(t *testing.T) {
	f := newFakeEngine(t)
	d := f.driver(context.Background(), f.serve())
	d.network = func(_ context.Context, _, bridge string) error {
		if bridge != "" {
			return errors.New("no firewall")
		}
		return nil
	}
	if _, err := d.Create(context.Background(), "abc", spec); err == nil {
		t.Fatal("created without firewall")
	}
	if len(f.containers)+len(f.networks)+len(f.volumes) != 0 {
		t.Fatal("failed create leaked resources")
	}
	if f.count("POST /containers/create") != 0 {
		t.Fatal("created container despite firewall failure")
	}
}
