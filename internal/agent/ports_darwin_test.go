package agent

import (
	"strings"
	"testing"
)

func TestPortsUnsupported(t *testing.T) {
	if _, err := serve(t).Ports(testContext(t)); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Errorf("Ports = %v, want unsupported", err)
	}
}
