//go:build linux || darwin

package main

import (
	"context"
	"strings"
	"testing"

	"boxpool.run/boxpool/internal/client"
)

func TestNodeLabels(t *testing.T) {
	dir := testDaemon(t)
	for _, tc := range []struct {
		labels string
		code   int
		want   string
	}{
		{"gpu,always-on,gpu", 0, "always-on,gpu"},
		{"bad label", 1, "always-on,gpu"},
		{"", 0, ""},
	} {
		if code, out, errOut := cli(t, dir, "", "node", "label", tc.labels); code != tc.code {
			t.Fatalf("node label %q = %d %q %q", tc.labels, code, out, errOut)
		}
		nodes, err := client.New(dir).ListNodes(context.Background())
		if err != nil || len(nodes) != 1 || strings.Join(nodes[0].Labels, ",") != tc.want {
			t.Fatalf("node labels = %+v, %v; want %q", nodes, err, tc.want)
		}
	}
	if code, _, errOut := cli(t, dir, "", "node", "label"); code != 2 || !strings.Contains(errOut, "Usage:") {
		t.Errorf("node label without a list = %d %q", code, errOut)
	}
}
