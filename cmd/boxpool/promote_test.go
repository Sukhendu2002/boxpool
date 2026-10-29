//go:build linux || darwin

package main

import (
	"strings"
	"testing"
)

func TestPromoteCmd(t *testing.T) {
	dir := testDaemon(t)
	if code, _, errOut := cli(t, dir, "no\n", "promote"); code != 1 || !strings.Contains(errOut, "the pool\nsplits in two") || !strings.HasSuffix(errOut, "boxpool promote: cancelled\n") {
		t.Errorf("promote without yes = %d %q", code, errOut)
	}
	if code, _, errOut := cli(t, dir, "yes\n", "promote"); code != 1 || !strings.Contains(errOut, "no replica of the pool's state to promote") {
		t.Errorf("promote on a lone machine = %d %q", code, errOut)
	}
	if code, _, _ := cli(t, dir, "", "promote", "now"); code != 2 {
		t.Errorf("promote with an argument = %d, want 2", code)
	}
}
