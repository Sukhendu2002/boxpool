package firecracker

import (
	"net/netip"
	"reflect"
	"testing"
)

func TestSandboxNIC(t *testing.T) {
	for _, tc := range []struct {
		slot           int
		gateway, guest string
	}{
		{0, "10.200.0.1", "10.200.0.2"},
		{1, "10.200.0.5", "10.200.0.6"},
		{64, "10.200.1.1", "10.200.1.2"},
		{netSlots - 1, "10.200.255.253", "10.200.255.254"},
	} {
		n := sandboxNIC("abc", tc.slot)
		if n.gateway.String() != tc.gateway || n.guest.String() != tc.guest {
			t.Errorf("slot %d: gateway %s, guest %s; want %s, %s", tc.slot, n.gateway, n.guest, tc.gateway, tc.guest)
		}
	}
	tap := tapName("0123456789ab")
	if len(tap) > 15 || tap[:len(tapPrefix)] != tapPrefix || tap == tapName("0123456789ac") || tap != tapName("0123456789ab") {
		t.Errorf("tap name %q: want a stable, distinct name of at most 15 bytes starting with %s", tap, tapPrefix)
	}
}

func TestNameservers(t *testing.T) {
	conf := "# comment\nnameserver 192.168.0.1\nnameserver fe80::1%eth0\nsearch lan\nnameserver  9.9.9.9 \nnameserver\n"
	want := []netip.Addr{netip.MustParseAddr("192.168.0.1"), netip.MustParseAddr("9.9.9.9")}
	if got := nameservers([]byte(conf)); !reflect.DeepEqual(got, want) {
		t.Errorf("nameservers = %v, want %v", got, want)
	}
	n := nic{dns: want}
	if got := n.resolvConf(); got != "nameserver 192.168.0.1\nnameserver 9.9.9.9\n" {
		t.Errorf("resolvConf = %q", got)
	}
}

func TestRuleset(t *testing.T) {
	want := `add table inet boxpool
delete table inet boxpool
table inet boxpool {
	chain prerouting {
		type filter hook prerouting priority raw; policy accept;
		iifname "bp-*" fib saddr . iif oif missing drop comment "source address not routed through this tap"
	}
	chain input {
		type filter hook input priority filter; policy accept;
		iifname "bp-*" drop comment "no host services"
	}
	chain forward {
		type filter hook forward priority filter; policy accept;
		iifname "bp-*" oifname "eth0" ip saddr 10.200.0.0/16 accept
		iifname "bp-*" drop comment "nothing else, other sandboxes included"
		oifname "bp-*" ct state established,related accept
		oifname "bp-*" drop
	}
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		oifname "eth0" ip saddr 10.200.0.0/16 masquerade
	}
}
`
	if got := ruleset("eth0"); got != want {
		t.Errorf("ruleset:\n%s\nwant:\n%s", got, want)
	}
}
