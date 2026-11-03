package vz

import (
	"bytes"
	"encoding/binary"
	"net"
	"net/netip"
	"sync"
	"time"
)

// packetFilter is outside the guest, between its only NIC and vmnet. Never
// send an unparsed encapsulation to vmnet: it could hide a host destination.
// Only IPv4 without options and ARP for this network are supported. IPv6,
// VLANs and other Ethernet protocols are deliberately not forwarded.
type packetFilter struct {
	prefix    netip.Prefix
	gateway   netip.Addr
	mac       net.HardwareAddr
	addrs     func() ([]net.Addr, error)
	now       func() time.Time
	mu        sync.Mutex
	replies   map[packetFlow]replyState
	fragments map[fragmentFlow]time.Time
}

type fragmentFlow struct {
	src, dst netip.Addr
	id       uint16
	proto    byte
}

type replyState struct {
	until   time.Time
	closing bool
}

type packetFlow struct {
	src, dst     netip.Addr
	sport, dport uint16
	proto        byte
}

type ipv4Packet struct {
	src, dst   netip.Addr
	proto      byte
	fragment   bool
	offset, id uint16
	body       []byte
}

func parseIPv4(frame []byte) (ipv4Packet, bool) {
	if len(frame) < 34 || binary.BigEndian.Uint16(frame[12:14]) != 0x0800 {
		return ipv4Packet{}, false
	}
	ip := frame[14:]
	if ip[0] != 0x45 { // no IP options, including source routing
		return ipv4Packet{}, false
	}
	n := int(binary.BigEndian.Uint16(ip[2:4]))
	if n < 20 || n > len(ip) {
		return ipv4Packet{}, false
	}
	var sum uint32
	for i := 0; i < 20; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(ip[i : i+2]))
	}
	for sum > 0xffff {
		sum = sum&0xffff + sum>>16
	}
	if sum != 0xffff || ip[6]&0x80 != 0 {
		return ipv4Packet{}, false
	}
	return ipv4Packet{
		src: netip.AddrFrom4([4]byte(ip[12:16])), dst: netip.AddrFrom4([4]byte(ip[16:20])),
		proto: ip[9], fragment: binary.BigEndian.Uint16(ip[6:8])&0x3fff != 0, body: ip[20:n],
		offset: binary.BigEndian.Uint16(ip[6:8]) & 0x1fff, id: binary.BigEndian.Uint16(ip[4:6]),
	}, true
}

func (p ipv4Packet) flow() (packetFlow, bool) {
	if p.offset != 0 {
		return packetFlow{}, false
	}
	switch p.proto {
	case 6:
		if len(p.body) < 20 || int(p.body[12]>>4)*4 < 20 || int(p.body[12]>>4)*4 > len(p.body) {
			return packetFlow{}, false
		}
	case 17:
		if len(p.body) < 8 {
			return packetFlow{}, false
		}
		length := int(binary.BigEndian.Uint16(p.body[4:6]))
		if length < len(p.body) || !p.fragment && length != len(p.body) {
			return packetFlow{}, false
		}
	case 1:
		if len(p.body) < 8 || p.body[1] != 0 || (p.body[0] != 8 && p.body[0] != 0) {
			return packetFlow{}, false
		}
		return packetFlow{p.src, p.dst, binary.BigEndian.Uint16(p.body[4:6]), binary.BigEndian.Uint16(p.body[6:8]), p.proto}, true
	default:
		return packetFlow{}, false
	}
	return packetFlow{p.src, p.dst, binary.BigEndian.Uint16(p.body[:2]), binary.BigEndian.Uint16(p.body[2:4]), p.proto}, true
}

func (f *packetFilter) guestAddress(a netip.Addr) bool {
	return f.prefix.Contains(a) && a != f.gateway && a.IsGlobalUnicast() && a.As4()[3] != 0 && a.As4()[3] != 255
}

func (f *packetFilter) allow(frame []byte) bool {
	if len(frame) < 14 || !bytes.Equal(frame[6:12], f.mac) {
		return false
	}
	if binary.BigEndian.Uint16(frame[12:14]) == 0x0806 {
		return f.allowARP(frame[14:])
	}
	p, ok := parseIPv4(frame)
	if !ok {
		return false
	}
	flow, transport := p.flow()
	dhcp := transport && !p.fragment && p.proto == 17 && flow.sport == 68 && flow.dport == 67 &&
		(p.dst == f.gateway || p.dst == netip.MustParseAddr("255.255.255.255"))
	if dhcp && p.src.IsUnspecified() {
		return true
	}
	if !f.guestAddress(p.src) {
		return false
	}
	addrs, ok := f.localAddresses()
	if !ok || hostAddress(addrs, p.src) {
		return false
	}
	if dhcp {
		return true
	}
	if transport && !p.fragment && (p.proto == 6 || p.proto == 17) && p.dst == f.gateway && flow.dport == 53 {
		return true // vmnet's TCP/UDP DNS proxy, not other gateway services
	}
	if transport && (p.proto != 1 || p.body[0] == 0) && f.reply(flow, false, p.closing()) {
		return !p.fragment || f.fragmentReply(p, true)
	}
	if p.offset != 0 && f.fragmentReply(p, false) {
		return true
	}
	if p.dst == f.gateway || subnetBroadcast(f.prefix, p.dst) || !p.dst.IsGlobalUnicast() || p.dst.IsLinkLocalUnicast() || p.dst.IsLoopback() {
		return false
	}
	// Query each packet: VPN, DHCP and new vmnet addresses must take effect
	// during a running VM, including on connections that are already open.
	return !hostAddress(addrs, p.dst)
}

// Only a first fragment with an allowed host-reply tuple may authorize the
// rest. Nonzero offsets cannot replace transport ports (or echo identity)
// in the first eight bytes. A new offset-zero fragment must pass the tuple
// check again, even when its IP ID already has an entry.
func (f *packetFilter) fragmentReply(p ipv4Packet, first bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := fragmentFlow{p.src, p.dst, p.id, p.proto}
	now := f.now()
	if !first {
		return now.Before(f.fragments[key])
	}
	for key, until := range f.fragments {
		if !now.Before(until) {
			delete(f.fragments, key)
		}
	}
	if len(f.fragments) >= 1024 {
		return false
	}
	if f.fragments == nil {
		f.fragments = make(map[fragmentFlow]time.Time)
	}
	f.fragments[key] = now.Add(30 * time.Second)
	return true
}

// Read one live snapshot per frame. Failure cannot create a reply exception
// or allow a destination whose ownership has not been checked.
func (f *packetFilter) localAddresses() ([]netip.Prefix, bool) {
	addrs, err := f.addrs()
	if err != nil {
		return nil, false
	}
	prefixes := make([]netip.Prefix, 0, len(addrs))
	for _, addr := range addrs {
		prefix, err := netip.ParsePrefix(addr.String())
		if err != nil {
			return nil, false
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes, true
}

func hostAddress(addrs []netip.Prefix, ip netip.Addr) bool {
	for _, prefix := range addrs {
		if prefix.Addr().Unmap() == ip || subnetBroadcast(prefix, ip) {
			return true
		}
	}
	return false
}

// BSD also accepts subnet network/broadcast destinations on wildcard host
// sockets. They must not turn a unicast-only host check into a UDP bypass.
func subnetBroadcast(prefix netip.Prefix, ip netip.Addr) bool {
	if !prefix.Addr().Is4() || prefix.Bits() >= 31 || !prefix.Contains(ip) {
		return false
	}
	a := ip.As4()
	hostmask := uint32(0xffffffff) >> prefix.Bits()
	host := binary.BigEndian.Uint32(a[:]) & hostmask
	return host == 0 || host == hostmask
}

func (f *packetFilter) allowARP(arp []byte) bool {
	if len(arp) < 28 || binary.BigEndian.Uint16(arp[:2]) != 1 || binary.BigEndian.Uint16(arp[2:4]) != 0x0800 ||
		arp[4] != 6 || arp[5] != 4 || !bytes.Equal(arp[8:14], f.mac) {
		return false
	}
	op := binary.BigEndian.Uint16(arp[6:8])
	src, dst := netip.AddrFrom4([4]byte(arp[14:18])), netip.AddrFrom4([4]byte(arp[24:28]))
	if (op != 1 && op != 2) || !f.prefix.Contains(dst) {
		return false
	}
	if src.IsUnspecified() {
		return op == 1
	} // ARP probe, never a claim
	addrs, ok := f.localAddresses()
	return ok && f.guestAddress(src) && !hostAddress(addrs, src)
}

// receive records the reverse flow only for a host-initiated TCP SYN or UDP
// datagram. Guest packets cannot create an exception to the host deny rule.
func (f *packetFilter) receive(frame []byte) {
	p, ok := parseIPv4(frame)
	if !ok || !f.guestAddress(p.dst) {
		return
	}
	flow, ok := p.flow()
	if !ok {
		return
	}
	addrs, known := f.localAddresses()
	if !known || !hostAddress(addrs, p.src) || p.src == f.gateway && (p.proto == 6 || p.proto == 17) &&
		(flow.sport == 53 || p.proto == 17 && flow.sport == 67) {
		return
	}
	reverse := packetFlow{flow.dst, flow.src, flow.dport, flow.sport, flow.proto}
	create := p.proto == 17 || p.proto == 6 && p.body[13]&0x12 == 0x02 || p.proto == 1 && p.body[0] == 8
	if p.proto == 1 {
		// Echo replies keep the request's identifier and sequence number.
		reverse.sport, reverse.dport = flow.sport, flow.dport
	}
	f.reply(reverse, create, p.closing())
}

func (p ipv4Packet) closing() bool { return p.proto == 6 && len(p.body) >= 20 && p.body[13]&0x05 != 0 }

// Reply state is bounded, expires after inactivity, and lives only as long
// as this VM's packet bridge. It never survives a pause or network reuse.
func (f *packetFilter) reply(flow packetFlow, create, closing bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	state := f.replies[flow]
	if now.Before(state.until) {
		state.closing = closing || state.closing && !create
		state.until = now.Add(replyLifetime(flow.proto, state.closing))
		f.replies[flow] = state
		return true
	}
	if !create {
		return false
	}
	for key, state := range f.replies {
		if !now.Before(state.until) {
			delete(f.replies, key)
		}
	}
	if len(f.replies) < 4096 {
		f.replies[flow] = replyState{now.Add(replyLifetime(flow.proto, closing)), closing}
	}
	return false
}

func replyLifetime(proto byte, closing bool) time.Duration {
	if proto == 6 && !closing {
		return 24 * time.Hour
	}
	return time.Minute
}
