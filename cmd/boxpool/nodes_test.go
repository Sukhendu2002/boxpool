//go:build linux || darwin

package main

import (
	"bytes"
	"testing"
	"time"

	"boxpool.run/boxpool/internal/daemon"
)

func TestNodesTable(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	list := []daemon.Node{
		{Name: "head", Role: "head", OS: "linux", Arch: "amd64", State: "ready", CPUs: 4, TotalCPUs: 8, MemoryMB: 4096, TotalMemoryMB: 8192},
		{Name: "mac", Role: "node", OS: "darwin", Arch: "arm64", State: "ready", CPUs: 3, TotalCPUs: 6, MemoryMB: 2048, TotalMemoryMB: 8192, JoinedAt: now.Add(-time.Hour), Labels: []string{"gpu"}, Warning: "on battery: connect AC power for reliable long-running sandboxes"},
		{Name: "offline", Role: "node", OS: "linux", Arch: "arm64", State: "unreachable", JoinedAt: now.Add(-72 * time.Hour)},
	}
	var b bytes.Buffer
	writeNodes(&b, list, now)
	golden(t, "ls-nodes.txt", b.Bytes())
}

func TestCapacityBar(t *testing.T) {
	for _, tc := range []struct {
		free, total int64
		want        string
	}{
		{0, 0, "[??????????]"}, {1, -1, "[??????????]"},
		{0, 10, "[----------]"}, {5, 10, "[#####-----]"},
		{20, 10, "[##########]"}, {-1, 10, "[----------]"},
	} {
		if got := capacityBar(tc.free, tc.total); got != tc.want {
			t.Errorf("bar(%d, %d) = %s, want %s", tc.free, tc.total, got, tc.want)
		}
	}
}

func TestAllowSleepPlist(t *testing.T) {
	b := agentPlist(launchAgent{allowSleep: true})
	if !bytes.Contains(b, []byte("<string>--allow-sleep</string>")) {
		t.Fatal("launch agent dropped --allow-sleep")
	}
	if bytes.Contains(agentPlist(launchAgent{}), []byte("--allow-sleep")) {
		t.Fatal("default launch agent allows sleep")
	}
}
