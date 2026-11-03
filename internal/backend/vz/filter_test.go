package vz

import (
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"
)

func testPacketFilter() *packetFilter {
	return &packetFilter{
		prefix: netip.MustParsePrefix("192.168.64.0/24"), gateway: netip.MustParseAddr("192.168.64.1"),
		mac: net.HardwareAddr{2, 0, 0, 0, 0, 2}, now: time.Now, replies: make(map[packetFlow]replyState),
		addrs: func() ([]net.Addr, error) {
			return []net.Addr{
				&net.IPNet{IP: net.ParseIP("192.168.64.1"), Mask: net.CIDRMask(24, 32)},
				&net.IPNet{IP: net.ParseIP("192.168.0.10"), Mask: net.CIDRMask(24, 32)},
				&net.IPNet{IP: net.ParseIP("100.64.0.10"), Mask: net.CIDRMask(32, 32)},
			}, nil
		},
	}
}

// Ethernet + IPv4 with a valid IP checksum. The policy deliberately leaves
// transport checksum validation to vmnet, as it cannot change a destination.
func testFrame(src, dst string, proto byte, sport, dport uint16) []byte {
	n := 20
	if proto == 17 {
		n = 8
	}
	f := make([]byte, 14+20+n)
	copy(f[:6], []byte{2, 0, 0, 0, 0, 1})
	copy(f[6:12], []byte{2, 0, 0, 0, 0, 2})
	binary.BigEndian.PutUint16(f[12:14], 0x0800)
	ip := f[14:]
	ip[0], ip[8], ip[9] = 0x45, 64, proto
	binary.BigEndian.PutUint16(ip[2:4], uint16(20+n))
	s, d := netip.MustParseAddr(src).As4(), netip.MustParseAddr(dst).As4()
	copy(ip[12:16], s[:])
	copy(ip[16:20], d[:])
	binary.BigEndian.PutUint16(ip[20:22], sport)
	binary.BigEndian.PutUint16(ip[22:24], dport)
	if proto == 17 {
		binary.BigEndian.PutUint16(ip[24:26], uint16(n))
	} else {
		ip[32], ip[33] = 0x50, 0x02
	}
	checksumFrame(f)
	return f
}

func checksumFrame(frame []byte) {
	ip := frame[14:]
	ip[10], ip[11] = 0, 0
	var sum uint32
	for i := 0; i < 20; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(ip[i : i+2]))
	}
	for sum > 0xffff {
		sum = sum&0xffff + sum>>16
	}
	binary.BigEndian.PutUint16(ip[10:12], ^uint16(sum))
}

func TestPacketFilter(t *testing.T) {
	for _, tc := range []struct {
		name, src, dst string
		proto          byte
		sport, dport   uint16
		allow          bool
	}{
		{"internet TCP", "192.168.64.2", "93.184.215.14", 6, 50000, 80, true},
		{"internet UDP", "192.168.64.2", "1.1.1.1", 17, 50000, 53, true},
		{"other LAN machine", "192.168.64.2", "192.168.0.20", 6, 50000, 80, true},
		{"gateway", "192.168.64.2", "192.168.64.1", 6, 50000, 8000, false},
		{"host LAN", "192.168.64.2", "192.168.0.10", 6, 50000, 8000, false},
		{"host VPN", "192.168.64.2", "100.64.0.10", 17, 50000, 7443, false},
		{"host ICMP", "192.168.64.2", "192.168.0.10", 1, 0, 0, false},
		{"loopback", "192.168.64.2", "127.1.2.3", 6, 50000, 8000, false},
		{"unspecified", "192.168.64.2", "0.0.0.0", 6, 50000, 8000, false},
		{"link local", "192.168.64.2", "169.254.1.1", 17, 50000, 8000, false},
		{"broadcast", "192.168.64.2", "255.255.255.255", 17, 50000, 8000, false},
		{"guest subnet broadcast", "192.168.64.2", "192.168.64.255", 17, 50000, 8000, false},
		{"LAN subnet broadcast", "192.168.64.2", "192.168.0.255", 17, 50000, 8000, false},
		{"LAN subnet zero", "192.168.64.2", "192.168.0.0", 17, 50000, 8000, false},
		{"multicast", "192.168.64.2", "224.0.0.251", 17, 50000, 5353, false},
		{"DHCP discover", "0.0.0.0", "255.255.255.255", 17, 68, 67, true},
		{"DHCP renew", "192.168.64.2", "192.168.64.1", 17, 68, 67, true},
		{"DHCP LAN host", "192.168.64.2", "192.168.0.10", 17, 68, 67, false},
		{"gateway TCP DNS", "192.168.64.2", "192.168.64.1", 6, 50000, 53, true},
		{"gateway UDP DNS", "192.168.64.2", "192.168.64.1", 17, 50000, 53, true},
		{"LAN host DNS", "192.168.64.2", "192.168.0.10", 17, 50000, 53, false},
		{"off subnet source", "10.250.0.2", "93.184.215.14", 17, 50000, 80, false},
		{"gateway source", "192.168.64.1", "93.184.215.14", 6, 50000, 80, false},
		{"broadcast source", "192.168.64.255", "93.184.215.14", 6, 50000, 80, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := testPacketFilter().allow(testFrame(tc.src, tc.dst, tc.proto, tc.sport, tc.dport)); got != tc.allow {
				t.Errorf("allow = %t, want %t", got, tc.allow)
			}
		})
	}
}

func TestPacketFilterEncodings(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func([]byte) []byte
	}{
		{"IPv6", func(f []byte) []byte { binary.BigEndian.PutUint16(f[12:14], 0x86dd); return f }},
		{"VLAN", func(f []byte) []byte { binary.BigEndian.PutUint16(f[12:14], 0x8100); return f }},
		{"QinQ", func(f []byte) []byte { binary.BigEndian.PutUint16(f[12:14], 0x88a8); return f }},
		{"unknown EtherType", func(f []byte) []byte { binary.BigEndian.PutUint16(f[12:14], 0x8864); return f }},
		{"wrong MAC", func(f []byte) []byte { f[6]++; return f }},
		{"IP options", func(f []byte) []byte { f[14] = 0x46; checksumFrame(f); return f }},
		{"bad checksum", func(f []byte) []byte { f[24]++; return f }},
		{"bad total length", func(f []byte) []byte { f[16], f[17] = 255, 255; checksumFrame(f); return f }},
		{"short total length", func(f []byte) []byte { f[16], f[17] = 0, 19; checksumFrame(f); return f }},
		{"reserved flag", func(f []byte) []byte { f[20] = 0x80; checksumFrame(f); return f }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.edit(testFrame("192.168.64.2", "93.184.215.14", 6, 50000, 80))
			if testPacketFilter().allow(f) {
				t.Fatal("forwarded unsupported or malformed frame")
			}
		})
	}
	for n := 0; n < 34; n++ {
		if testPacketFilter().allow(make([]byte, n)) {
			t.Fatalf("forwarded %d-byte frame", n)
		}
	}
	for _, dst := range []string{"192.168.64.1", "192.168.0.10"} {
		for _, fragment := range []uint16{0x2000, 1, 0x2001} {
			f := testFrame("192.168.64.2", dst, 17, 50000, 53)
			binary.BigEndian.PutUint16(f[20:22], fragment)
			checksumFrame(f)
			if testPacketFilter().allow(f) {
				t.Fatalf("fragment %#x to %s bypassed host filtering", fragment, dst)
			}
		}
	}
}

func TestPacketFilterLiveAddresses(t *testing.T) {
	f := testPacketFilter()
	frame := testFrame("192.168.64.2", "192.168.0.20", 6, 50000, 8000)
	if !f.allow(frame) {
		t.Fatal("external destination refused")
	}
	f.addrs = func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("192.168.0.20"), Mask: net.CIDRMask(24, 32)}}, nil
	}
	if f.allow(frame) {
		t.Fatal("new host address remained reachable")
	}
	f.addrs = func() ([]net.Addr, error) { return nil, errors.New("lookup failed") }
	if f.allow(frame) {
		t.Fatal("lookup failure allowed unchecked traffic")
	}
}

func TestPacketFilterHostReplies(t *testing.T) {
	for _, proto := range []byte{6, 17} {
		f := testPacketFilter()
		now := time.Now()
		f.now = func() time.Time { return now }
		request := testFrame("192.168.0.10", "192.168.64.2", proto, 50000, 8080)
		reply := testFrame("192.168.64.2", "192.168.0.10", proto, 8080, 50000)
		if f.allow(reply) {
			t.Fatal("guest opened its own reply exception")
		}
		if proto == 6 {
			request[47] = 0x10 // unsolicited ACK is not an opener
			f.receive(request)
			if f.allow(reply) {
				t.Fatal("unsolicited ACK opened a reply exception")
			}
			request[47] = 0x02
		}
		f.receive(request)
		if !f.allow(reply) {
			t.Fatalf("host-initiated protocol %d reply refused", proto)
		}
		if f.allow(testFrame("192.168.64.2", "192.168.0.10", proto, 8080, 7443)) ||
			f.allow(testFrame("192.168.64.3", "192.168.0.10", proto, 8080, 50000)) {
			t.Fatal("reply exception allowed another flow")
		}
		now = now.Add(25 * time.Hour)
		if f.allow(reply) {
			t.Fatal("expired host reply exception remained usable")
		}
	}
}

func TestPacketFilterReplyBounds(t *testing.T) {
	f := testPacketFilter()
	now := time.Now()
	f.now = func() time.Time { return now }
	// Internet responses and the DNS proxy cannot use up host reply slots.
	for _, src := range []string{"93.184.215.14", "192.168.64.1"} {
		f.receive(testFrame(src, "192.168.64.2", 17, 53, 50000))
	}
	if len(f.replies) != 0 {
		t.Fatal("ordinary egress responses consumed host reply slots")
	}
	for port := 1; port < 5000; port++ {
		f.receive(testFrame("192.168.0.10", "192.168.64.2", 17, uint16(port), 8080))
	}
	if len(f.replies) > 4096 {
		t.Fatal("unbounded reply state")
	}
	now = now.Add(2 * time.Minute)
	f.receive(testFrame("192.168.0.10", "192.168.64.2", 17, 50000, 8080))
	if !f.allow(testFrame("192.168.64.2", "192.168.0.10", 17, 8080, 50000)) || len(f.replies) != 1 {
		t.Fatal("expired entries prevented a new host connection")
	}
	request := testFrame("192.168.0.10", "192.168.64.2", 6, 50000, 8080)
	reply := testFrame("192.168.64.2", "192.168.0.10", 6, 8080, 50000)
	f.receive(request)
	now = now.Add(time.Hour)
	if !f.allow(reply) {
		t.Fatal("idle TCP connection lost its reply state")
	}
	reply[47] = 0x11 // FIN/ACK
	if !f.allow(reply) {
		t.Fatal("TCP close refused")
	}
	now = now.Add(2 * time.Minute)
	if f.allow(reply) {
		t.Fatal("closed TCP connection retained its reply exception")
	}
}

func TestPacketFilterFragmentedHostReplies(t *testing.T) {
	f := testPacketFilter()
	now := time.Now()
	f.now = func() time.Time { return now }
	f.receive(testFrame("192.168.0.10", "192.168.64.2", 17, 50000, 8080))
	first := testFrame("192.168.64.2", "192.168.0.10", 17, 8080, 50000)
	binary.BigEndian.PutUint16(first[20:22], 0x2000) // first fragment, more follows
	binary.BigEndian.PutUint16(first[38:40], 2000)   // whole UDP datagram length
	checksumFrame(first)
	later := testFrame("192.168.64.2", "192.168.0.10", 17, 0, 0)
	binary.BigEndian.PutUint16(later[20:22], 1) // offset 8, no transport header
	checksumFrame(later)
	if f.allow(later) {
		t.Fatal("unmatched later fragment reached host")
	}
	if !f.allow(first) || !f.allow(later) {
		t.Fatal("fragmented reply to host-initiated UDP was dropped")
	}
	// Reusing the IP ID cannot authorize a new first fragment to a service.
	binary.BigEndian.PutUint16(first[36:38], 7443)
	if f.allow(first) {
		t.Fatal("fragment ID bypassed the host service port check")
	}
	now = now.Add(31 * time.Second)
	if f.allow(later) {
		t.Fatal("expired fragment exception remained usable")
	}
}

func TestPacketFilterARP(t *testing.T) {
	f := testPacketFilter()
	frame := make([]byte, 42)
	copy(frame[6:12], f.mac)
	binary.BigEndian.PutUint16(frame[12:14], 0x0806)
	arp := frame[14:]
	copy(arp, []byte{0, 1, 8, 0, 6, 4, 0, 2})
	copy(arp[8:14], f.mac)
	copy(arp[24:28], []byte{192, 168, 64, 1})
	for _, tc := range []struct {
		src   [4]byte
		allow bool
	}{
		{[4]byte{192, 168, 64, 2}, true},
		{[4]byte{}, true},
		{[4]byte{192, 168, 64, 1}, false},
		{[4]byte{192, 168, 0, 10}, false},
	} {
		copy(arp[14:18], tc.src[:])
		arp[7] = 2
		if tc.src == [4]byte{} {
			arp[7] = 1
		}
		if got := f.allow(frame); got != tc.allow {
			t.Errorf("ARP sender %v: allow %t, want %t", tc.src, got, tc.allow)
		}
	}
}

func FuzzPacketFilter(f *testing.F) {
	f.Add(testFrame("192.168.64.2", "192.168.0.10", 17, 50000, 53))
	f.Add(testFrame("192.168.0.10", "192.168.64.2", 6, 50000, 8080))
	fragment := testFrame("192.168.64.2", "192.168.0.10", 17, 8080, 50000)
	binary.BigEndian.PutUint16(fragment[20:22], 0x2000)
	checksumFrame(fragment)
	f.Add(fragment)
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, frame []byte) {
		filter := testPacketFilter()
		filter.receive(frame)
		filter.allow(frame)
	})
}
