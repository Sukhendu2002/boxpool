//go:build darwin && cgo

// Package power manages macOS idle-sleep assertions and power sources.
package power

/*
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation
#include <CoreFoundation/CoreFoundation.h>
#include <IOKit/pwr_mgt/IOPMLib.h>
#include <IOKit/ps/IOPowerSources.h>
#include <IOKit/ps/IOPSKeys.h>

static IOReturn preventSleep(IOPMAssertionID *id) {
	return IOPMAssertionCreateWithName(kIOPMAssertionTypePreventUserIdleSystemSleep,
		kIOPMAssertionLevelOn, CFSTR("boxpool: running sandboxes"), id);
}
static int onBattery(void) {
	CFTypeRef info = IOPSCopyPowerSourcesInfo();
	if (!info) return -1;
	CFStringRef source = IOPSGetProvidingPowerSourceType(info);
	int result = source ? CFEqual(source, CFSTR(kIOPSBatteryPowerValue)) : -1;
	CFRelease(info);
	return result;
}
*/
import "C"

import "fmt"

// PreventSleep prevents idle system sleep until the returned function runs.
// It does not prevent lid-close sleep or explicit sleep.
func PreventSleep() (func() error, error) {
	var id C.IOPMAssertionID
	if code := C.preventSleep(&id); code != 0 {
		return nil, fmt.Errorf("create idle-sleep assertion: IOKit %d", code)
	}
	return func() error {
		if code := C.IOPMAssertionRelease(id); code != 0 {
			return fmt.Errorf("release idle-sleep assertion: IOKit %d", code)
		}
		return nil
	}, nil
}

// BatteryWarning reads the current power source, not a startup snapshot.
func BatteryWarning() string {
	switch C.onBattery() {
	case 1:
		return "on battery: connect AC power for reliable long-running sandboxes"
	case -1:
		return "could not read the Mac's power source"
	}
	return ""
}
