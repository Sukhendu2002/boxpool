//go:build linux || (darwin && !cgo)

package diskspace

func importantCapacity(string) (int64, error) {
	return -1, nil // no native estimate; Probe retains the filesystem value
}
