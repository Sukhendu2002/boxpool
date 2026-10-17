//go:build !linux && !darwin

package preflight

func diskCheck() Check {
	return Check{Name: "free disk", Required: true, Detail: "unsupported OS"}
}
