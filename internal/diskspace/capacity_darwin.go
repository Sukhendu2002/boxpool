//go:build darwin && cgo

package diskspace

/*
#cgo LDFLAGS: -framework Foundation
#include <stdlib.h>
#include "capacity_darwin.h"
*/
import "C"

import (
	"errors"
	"unsafe"
)

func importantCapacity(path string) (int64, error) {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	var available C.int64_t
	if C.boxpool_important_capacity(cpath, &available) == 0 {
		return 0, errors.New("macOS important-usage capacity unavailable")
	}
	return int64(available), nil
}
