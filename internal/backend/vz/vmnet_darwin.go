//go:build darwin && cgo

package vz

/*
#cgo CFLAGS: -x objective-c -fno-objc-arc -mmacosx-version-min=14
#cgo LDFLAGS: -framework Foundation -framework Virtualization -framework vmnet
#import <Virtualization/Virtualization.h>
#import <vmnet/vmnet.h>

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
	"time"
	"unsafe"

	virt "github.com/Code-Hex/vz/v3"
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

// attachNetwork moves nic onto the vmnet network.
// shortcut: reads the Objective-C object from vz's unexported pointer field
// (the attach checks its class), as the vz fork does not wrap
// VZVmnetNetworkDeviceAttachment; use its wrapper once it has one. A change
// to the fork's layout would crash here before the class check, so do not
// change the fork without rerunning the vz integration suite.
func attachNetwork(nic *virt.VirtioNetworkDeviceConfiguration, net unsafe.Pointer) error {
	obj := **(**unsafe.Pointer)(unsafe.Pointer(nic))
	if C.attach(obj, net) == 0 {
		return errors.New("attach the network device to its vmnet network")
	}
	return nil
}
