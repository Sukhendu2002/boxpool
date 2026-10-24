//go:build darwin && cgo

package vz

/*
#cgo CFLAGS: -x objective-c -fno-objc-arc -mmacosx-version-min=14
#cgo LDFLAGS: -framework Foundation -framework Virtualization -framework vmnet
#import <Virtualization/Virtualization.h>
#import <vmnet/vmnet.h>

// An SDK older than macOS 26's builds a binary that always shares the NAT.
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

// attach replaces the network device's attachment with one on network.
static int attach(void *nic, void *network) {
#ifdef HAVE_VMNET_NETWORK
	if (@available(macOS 26, *)) {
		if (![(id)nic isKindOfClass:[VZVirtioNetworkDeviceConfiguration class]]) {
			return 0;
		}
		VZVmnetNetworkDeviceAttachment *a = [[VZVmnetNetworkDeviceAttachment alloc] initWithNetwork:(vmnet_network_ref)network];
		[(VZVirtioNetworkDeviceConfiguration *)nic setAttachment:a];
		[a release];
		return 1;
	}
#endif
	return 0;
}
*/
import "C"

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"

	virt "github.com/Code-Hex/vz/v3"
)

// SharedNetworkWarning is the daemon's warning when sandboxes share the
// framework's NAT (see doc.go).
const SharedNetworkWarning = "sandboxes share one NAT network before macOS 26: root in one can send packets to another (see docs/security.md)"

// IsolatedNetworks reports whether each VM gets a network of its own, which
// needs macOS 26 and a binary built with its SDK.
func IsolatedNetworks() bool { return C.haveNetworks() == 1 }

// networks are the process's idle vmnet networks. A network holds its /24
// for as long as it exists and a process can hold only 32
// (192.168.64.0/24 to 192.168.95.0/24), so networks are reused once their
// VM has stopped instead of waiting for the framework's objects to be
// garbage collected.
var networks struct {
	sync.Mutex
	idle []unsafe.Pointer
}

// takeNetwork returns an idle vmnet network or a new one, or nil before
// macOS 26.
func takeNetwork() (unsafe.Pointer, error) {
	networks.Lock()
	defer networks.Unlock()
	if n := len(networks.idle); n > 0 {
		net := networks.idle[n-1]
		networks.idle = networks.idle[:n-1]
		return net, nil
	}
	var status C.int
	net := C.newNetwork(&status)
	if net == nil && status != 0 {
		return nil, fmt.Errorf("create a vmnet network: status %d (at most 32 sandboxes can run at once)", status)
	}
	return net, nil
}

// putNetwork makes a network whose VM has stopped, or never started,
// available to another VM.
func putNetwork(net unsafe.Pointer) {
	if net == nil {
		return
	}
	networks.Lock()
	networks.idle = append(networks.idle, net)
	networks.Unlock()
}

// attachNetwork moves nic onto the vmnet network.
// shortcut: reads the Objective-C object from vz's unexported pointer field
// (the attach checks its class), as the vz fork does not wrap
// VZVmnetNetworkDeviceAttachment; use its wrapper once it has one.
func attachNetwork(nic *virt.VirtioNetworkDeviceConfiguration, net unsafe.Pointer) error {
	obj := **(**unsafe.Pointer)(unsafe.Pointer(nic))
	if C.attach(obj, net) == 0 {
		return errors.New("attach the network device to its vmnet network")
	}
	return nil
}
