//go:build !linux

package resources

import "fmt"

func hostCapacity(string) (Capacity, error) {
	return Capacity{}, fmt.Errorf("host resource admission requires Linux")
}
