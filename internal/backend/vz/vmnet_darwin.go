//go:build darwin && cgo

package vz

/*
#cgo CFLAGS: -x objective-c -fno-objc-arc -mmacosx-version-min=14
#cgo LDFLAGS: -framework Foundation -framework Virtualization -framework vmnet
#import <Virtualization/Virtualization.h>
#import <vmnet/vmnet.h>
#import <arpa/inet.h>
#import <sys/socket.h>
#import <sys/uio.h>
#import <unistd.h>
#import <fcntl.h>
#import <errno.h>

// An SDK older than macOS 26's builds a binary whose sandboxes have no network.
#if __MAC_OS_X_VERSION_MAX_ALLOWED >= 260000
#define HAVE_VMNET_NETWORK 1
#endif

static int haveNetworks(void) {
#ifdef HAVE_VMNET_NETWORK
	if (@available(macOS 26, *)) {
		return 1;
	}
#endif
	return 0;
}

// newNetwork creates a vmnet shared (NAT) network on a /24 of its own. It
// returns NULL with *status 0 before macOS 26, which has no such networks.
static void *newNetwork(int *status) {
	*status = 0;
#ifdef HAVE_VMNET_NETWORK
	if (@available(macOS 26, *)) {
		vmnet_return_t st = 0;
		vmnet_network_configuration_ref cfg = vmnet_network_configuration_create(VMNET_SHARED_MODE, &st);
		if (cfg == NULL) {
			*status = st;
			return NULL;
		}
		// IPv6 is not forwarded by the packet filter. Do not advertise it.
		vmnet_network_configuration_disable_nat66(cfg);
		vmnet_network_configuration_disable_router_advertisement(cfg);
		vmnet_network_ref net = vmnet_network_create(cfg, &st);
		CFRelease(cfg);
		if (net == NULL) {
			*status = st;
		}
		return net;
	}
#endif
	return NULL;
}

static int networkIPv4(void *network, uint32_t *subnet, uint32_t *mask) {
#ifdef HAVE_VMNET_NETWORK
	if (@available(macOS 26, *)) {
		struct in_addr s, m;
		vmnet_network_get_ipv4_subnet((vmnet_network_ref)network, &s, &m);
		*subnet = ntohl(s.s_addr);
		*mask = ntohl(m.s_addr);
		return 1;
	}
#endif
	return 0;
}

// The callback owns only C memory and a C-owned descriptor. No Go pointers
// or goroutines are retained by vmnet. All vmnet I/O uses a serial queue.
typedef struct {
    interface_ref iface;
    dispatch_queue_t queue;
    int fd;
} packetInterface;

static int stopPacketInterface(packetInterface *p) {
    vmnet_return_t disabled = vmnet_interface_set_event_callback(p->iface, VMNET_INTERFACE_PACKETS_AVAILABLE, NULL, NULL);
    dispatch_sync(p->queue, ^{});
    dispatch_semaphore_t done = dispatch_semaphore_create(0);
    __block int result = 0;
    vmnet_return_t st = vmnet_stop_interface(p->iface, p->queue, ^(vmnet_return_t status) {
        result = status;
        dispatch_semaphore_signal(done);
    });
    if (st == VMNET_SUCCESS) dispatch_semaphore_wait(done, DISPATCH_TIME_FOREVER);
    dispatch_release(done);
    // If neither operation succeeded, callbacks may still use p and its fd.
    // Retain both on this exceptional path rather than reuse a live fd.
    if (disabled != VMNET_SUCCESS && (st != VMNET_SUCCESS || result != VMNET_SUCCESS)) {
        return st != VMNET_SUCCESS ? st : result;
    }
    dispatch_sync(p->queue, ^{});
    close(p->fd);
    dispatch_release(p->queue);
    free(p);
    return st != VMNET_SUCCESS ? st : result;
}

static packetInterface *startPacketInterface(void *network, int fd, int *status) {
    *status = 0;
#ifdef HAVE_VMNET_NETWORK
    if (@available(macOS 26, *)) {
        packetInterface *p = calloc(1, sizeof(*p));
        if (p == NULL) return NULL;
        p->fd = fcntl(fd, F_DUPFD_CLOEXEC, 0);
        if (p->fd < 0) { *status = -errno; free(p); return NULL; }
        p->queue = dispatch_queue_create("run.boxpool.vmnet", DISPATCH_QUEUE_SERIAL);
        xpc_object_t desc = xpc_dictionary_create(NULL, NULL, 0);
        xpc_dictionary_set_bool(desc, vmnet_allocate_mac_address_key, false);
        xpc_dictionary_set_bool(desc, vmnet_enable_isolation_key, true);
        xpc_dictionary_set_bool(desc, vmnet_enable_tso_key, false);
        xpc_dictionary_set_bool(desc, vmnet_enable_checksum_offload_key, false);
        dispatch_semaphore_t ready = dispatch_semaphore_create(0);
        __block int result = 0;
        p->iface = vmnet_interface_start_with_network((vmnet_network_ref)network, desc, p->queue, ^(vmnet_return_t st, xpc_object_t params) {
            result = st;
            dispatch_semaphore_signal(ready);
        });
        xpc_release(desc);
        if (p->iface != NULL) dispatch_semaphore_wait(ready, DISPATCH_TIME_FOREVER);
        dispatch_release(ready);
        *status = result;
        if (p->iface == NULL) {
            close(p->fd);
            dispatch_release(p->queue);
            free(p);
            return NULL;
        }
        if (result != VMNET_SUCCESS) {
            stopPacketInterface(p);
            return NULL;
        }
        vmnet_return_t st = vmnet_interface_set_event_callback(p->iface, VMNET_INTERFACE_PACKETS_AVAILABLE, p->queue, ^(interface_event_t event, xpc_object_t params) {
            // A nonblocking notification coalesces when its socket is full.
            // Go drains packets one at a time, so continuous receive traffic
            // cannot monopolize this queue and block writes or shutdown.
            char ready = 1;
            send(p->fd, &ready, 1, MSG_DONTWAIT);
        });
        if (st != VMNET_SUCCESS) {
            *status = st;
            stopPacketInterface(p);
            return NULL;
        }
        return p;
    }
#endif
    return NULL;
}

static int readPacket(packetInterface *p, void *buf, size_t capacity, size_t *size) {
    __block int result;
    __block size_t received = 0;
    dispatch_sync(p->queue, ^{
        struct iovec iov = { .iov_base = buf, .iov_len = capacity };
        struct vmpktdesc packet = { .vm_pkt_size = capacity, .vm_pkt_iov = &iov, .vm_pkt_iovcnt = 1 };
        int count = 1;
        result = vmnet_read(p->iface, &packet, &count);
        if (result == VMNET_SUCCESS && count == 1) received = packet.vm_pkt_size;
    });
    *size = received;
    return result;
}

static int writePacket(packetInterface *p, void *buf, size_t size) {
    __block int result;
    dispatch_sync(p->queue, ^{
        struct iovec iov = { .iov_base = buf, .iov_len = size };
        struct vmpktdesc packet = { .vm_pkt_size = size, .vm_pkt_iov = &iov, .vm_pkt_iovcnt = 1 };
        int count = 1;
        result = vmnet_write(p->iface, &packet, &count);
        if (result == VMNET_SUCCESS && count != 1) result = 0;
    });
    return result;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"time"
	"unsafe"

	"boxpool.run/boxpool/internal/backend"
)

// NoNetworkWarning is the daemon's warning when sandboxes start without a
// network because isolated ones are unavailable (see doc.go).
const NoNetworkWarning = "sandboxes have no network: networking needs macOS 26 or later for isolated sandbox networks (see docs/security.md)"

// IsolatedNetworks reports whether each VM can get a network of its own,
// which needs macOS 26 and a binary built with its SDK.
func IsolatedNetworks() bool { return C.haveNetworks() == 1 }

// maxNetworks is how many vmnet networks a process can hold: each holds a
// /24 for as long as it exists.
const maxNetworks = 32

// quarantine is how long a removed sandbox's network waits before a new
// sandbox may take it: late replies to the removed sandbox's address expire
// first. A variable so tests can shorten it.
var quarantine = 2 * time.Minute

var errTooManyNetworks = errors.New("at most 32 sandboxes can have networking at once on macOS, and a removed sandbox's network is reusable 2 minutes after removal; remove one or wait")

// networks are the process's vmnet networks. A network a VM has used keeps
// its /24 until the process exits, even once released, so networks are
// never released: a removed sandbox's network waits in idle, oldest first,
// for a new sandbox.
var networks struct {
	sync.Mutex
	held int
	idle []idleNetwork
}

type idleNetwork struct {
	net     unsafe.Pointer
	removed time.Time
}

// takeNetwork returns an idle network whose quarantine is over, or a new
// one; it fails before macOS 26.
func takeNetwork() (unsafe.Pointer, error) {
	networks.Lock()
	defer networks.Unlock()
	if len(networks.idle) > 0 && time.Since(networks.idle[0].removed) >= quarantine {
		net := networks.idle[0].net
		networks.idle = networks.idle[1:]
		return net, nil
	}
	if networks.held >= maxNetworks {
		return nil, errTooManyNetworks
	}
	var status C.int
	net := C.newNetwork(&status)
	if net == nil {
		return nil, fmt.Errorf("create a vmnet network: status %d: %w", status, errTooManyNetworks)
	}
	networks.held++
	return net, nil
}

// putNetwork hands a removed sandbox's network to a new sandbox once its
// quarantine is over.
func putNetwork(net unsafe.Pointer) {
	networks.Lock()
	networks.idle = append(networks.idle, idleNetwork{net, time.Now()})
	networks.Unlock()
}

// networkConfig assigns the sole guest .2 on its private /24; vmnet's NAT
// gateway and DNS proxy are .1. No DHCP lease from the template is reused.
func networkConfig(network unsafe.Pointer) (backend.GuestNetwork, error) {
	var subnet, mask C.uint32_t
	if C.networkIPv4(network, &subnet, &mask) == 0 || uint32(mask) != 0xffffff00 {
		return backend.GuestNetwork{}, errors.New("warm restore needs a vmnet IPv4 /24")
	}
	addr := func(n uint32) string {
		return netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}).String()
	}
	base := uint32(subnet) & uint32(mask)
	gateway := addr(base + 1)
	return backend.GuestNetwork{
		Address: addr(base + 2), Mask: "255.255.255.0", Gateway: gateway,
		Resolvers: "nameserver " + gateway + "\n",
	}, nil
}

// nativeInterface is accessed only while the bridge's Go loops are running.
// Close waits for both loops before stopping it and releasing its C storage.
type nativeInterface struct{ ptr *C.packetInterface }

func startNativeInterface(network unsafe.Pointer, fd uintptr) (*nativeInterface, error) {
	var status C.int
	p := C.startPacketInterface(network, C.int(fd), &status)
	if p == nil {
		return nil, fmt.Errorf("start filtered vmnet interface: status %d", status)
	}
	return &nativeInterface{p}, nil
}

func (n *nativeInterface) read(packet []byte) (int, error) {
	var size C.size_t
	if st := C.readPacket(n.ptr, unsafe.Pointer(&packet[0]), C.size_t(len(packet)), &size); st != C.VMNET_SUCCESS {
		return 0, fmt.Errorf("read vmnet packet: status %d", st)
	}
	return int(size), nil
}

func (n *nativeInterface) write(packet []byte) error {
	if st := C.writePacket(n.ptr, unsafe.Pointer(&packet[0]), C.size_t(len(packet))); st != C.VMNET_SUCCESS {
		return fmt.Errorf("write vmnet packet: status %d", st)
	}
	return nil
}

func (n *nativeInterface) close() error {
	if st := C.stopPacketInterface(n.ptr); st != C.VMNET_SUCCESS {
		return fmt.Errorf("stop vmnet packet interface: status %d", st)
	}
	return nil
}
