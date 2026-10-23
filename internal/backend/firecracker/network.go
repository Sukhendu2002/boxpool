package firecracker

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"strings"
)

// subnet holds every sandbox's /30: the host's end of its tap device, the
// gateway, then the guest. It avoids Docker's 172.17.0.0/16 default and the
// 192.168.0.0/16 and 10.0.0.0/24 ranges common on LANs.
var subnet = netip.MustParsePrefix("10.200.0.0/16")

// netSlots is how many /30s subnet holds.
const netSlots = 1 << (32 - 16 - 2)

// tapPrefix starts every boxpool tap device's name; the firewall rules match
// on it.
const tapPrefix = "bp-"

// fallbackDNS is used when the host has no resolver a guest can reach.
var fallbackDNS = []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("8.8.8.8")}

// nic is a sandbox's network interface.
type nic struct {
	tap            string
	gateway, guest netip.Addr
	dns            []netip.Addr
}

// tapName is the host name of a sandbox's tap device. Linux allows 15 bytes
// and sandbox IDs can be longer, so it is a hash of the ID.
func tapName(id string) string {
	h := sha256.Sum256([]byte(id))
	return tapPrefix + hex.EncodeToString(h[:6])
}

func sandboxNIC(id string, slot int) nic {
	a := subnet.Addr().As4()
	a[2], a[3] = byte(slot>>6), byte(slot<<2)
	gw := netip.AddrFrom4(a)
	return nic{tap: tapName(id), gateway: gw.Next(), guest: gw.Next().Next()}
}

// bootArg configures the guest's eth0 with the kernel's IP autoconfiguration
// (CONFIG_IP_PNP): the image has no ip command to do it after boot.
func (n nic) bootArg() string {
	return fmt.Sprintf("ip=%s::%s:255.255.255.252::eth0:off", n.guest, n.gateway)
}

// resolvConf is the guest's /etc/resolv.conf.
func (n nic) resolvConf() string {
	var b strings.Builder
	for _, a := range n.dns {
		fmt.Fprintf(&b, "nameserver %s\n", a)
	}
	return b.String()
}

// nameservers returns the IPv4 nameservers in a resolv.conf.
func nameservers(conf []byte) []netip.Addr {
	var addrs []netip.Addr
	s := bufio.NewScanner(bytes.NewReader(conf))
	for s.Scan() {
		f := strings.Fields(s.Text())
		if len(f) < 2 || f[0] != "nameserver" {
			continue
		}
		if a, err := netip.ParseAddr(f[1]); err == nil && a.Is4() {
			addrs = append(addrs, a)
		}
	}
	return addrs
}

// tableName is boxpool's nftables table.
const tableName = "inet boxpool"

// ruleset is boxpool's nftables table for sandboxes behind uplink, the host's
// outbound interface. nft -f applies it in one transaction that replaces any
// earlier version of the table. A drop in any table is final, so the drops
// here hold whatever other tables accept; the accepts do not override other
// tables' drops (see forwardAccepts).
func ruleset(uplink string) string {
	return fmt.Sprintf(`add table %[1]s
delete table %[1]s
table %[1]s {
	chain prerouting {
		type filter hook prerouting priority raw; policy accept;
		iifname "%[2]s*" fib saddr . iif oif missing drop comment "source address not routed through this tap"
	}
	chain input {
		type filter hook input priority filter; policy accept;
		iifname "%[2]s*" drop comment "no host services"
	}
	chain forward {
		type filter hook forward priority filter; policy accept;
		iifname "%[2]s*" oifname "%[3]s" ip saddr %[4]s accept
		iifname "%[2]s*" drop comment "nothing else, other sandboxes included"
		oifname "%[2]s*" ct state established,related accept
		oifname "%[2]s*" drop
	}
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		oifname "%[3]s" ip saddr %[4]s masquerade
	}
}
`, tableName, tapPrefix, uplink, subnet)
}

// deleteTable removes boxpool's table if it exists.
const deleteTable = "add table " + tableName + "\ndelete table " + tableName + "\n"

// forwardAccepts let sandbox traffic through an iptables FORWARD chain with
// policy DROP, as Docker and ufw set it up. The first accepts more than the
// forward chain in ruleset does, which still drops the rest.
var forwardAccepts = [][]string{
	{"-i", tapPrefix + "+", "-m", "comment", "--comment", "boxpool", "-j", "ACCEPT"},
	{"-o", tapPrefix + "+", "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-m", "comment", "--comment", "boxpool", "-j", "ACCEPT"},
}
