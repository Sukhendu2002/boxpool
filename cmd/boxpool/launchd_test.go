//go:build linux || darwin

package main

import (
	"encoding/xml"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

var testAgent = launchAgent{
	label: agentLabel,
	plist: "/Users/me/Library/LaunchAgents/run.boxpool.daemon.plist",
	bin:   "/Users/me/Library/Application Support/boxpool/bin/boxpool",
	log:   "/Users/me/Library/Logs/boxpool/daemon.log",
}

func TestAgentPlist(t *testing.T) {
	golden(t, "run.boxpool.daemon.plist", agentPlist(testAgent))

	a := testAgent
	a.home = "/tmp/a&b<c>"
	p := agentPlist(a)
	if !strings.Contains(string(p), "<key>BOXPOOL_HOME</key>\n\t\t<string>/tmp/a&amp;b&lt;c&gt;</string>") {
		t.Errorf("BOXPOOL_HOME is missing or not escaped:\n%s", p)
	}
	for _, p := range [][]byte{agentPlist(testAgent), p} {
		d := xml.NewDecoder(strings.NewReader(string(p)))
		for {
			_, err := d.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("plist is not well-formed XML: %v\n%s", err, p)
			}
		}
		if runtime.GOOS == "darwin" {
			cmd := exec.Command("plutil", "-lint", "-")
			cmd.Stdin = strings.NewReader(string(p))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("plutil -lint: %v: %s", err, out)
			}
		}
	}
}

func TestAgentSteps(t *testing.T) {
	a := testAgent
	var (
		enable    = "enable " + a.target() + "; "
		bootout   = "bootout " + a.target() + "; "
		install   = "install; "
		bootstrap = "bootstrap " + a.domain() + " " + a.plist
		kickstart = "kickstart -k " + a.target()
	)
	for _, c := range []struct {
		name string
		f    agentFacts
		want string
	}{
		{"first install", agentFacts{plistStale: true}, install + bootstrap},
		{"nothing to do", agentFacts{loaded: true, pid: 42}, ""}, // keeps its PID
		{"plist changed", agentFacts{loaded: true, pid: 42, plistStale: true}, bootout + install + bootstrap},
		// The plist is booted out with it, so the new one starts fresh.
		{"plist and binary changed", agentFacts{loaded: true, pid: 42, plistStale: true, staleExe: true}, bootout + install + bootstrap},
		{"binary changed", agentFacts{loaded: true, pid: 42, staleExe: true}, kickstart},
		// It exited cleanly, or crashed and waits for its throttle.
		{"loaded, not running", agentFacts{loaded: true}, kickstart},
		{"after down", agentFacts{plistStale: true}, install + bootstrap},
		// Logged in again before an earlier up bootstrapped it, or a
		// bootstrap failed after the plist was written.
		{"plist current, not loaded", agentFacts{}, bootstrap},
		{"disabled", agentFacts{disabled: true, plistStale: true}, enable + install + bootstrap},
		{"disabled while loaded", agentFacts{disabled: true, loaded: true, pid: 42}, strings.TrimSuffix(enable, "; ")},
	} {
		var got []string
		for _, s := range agentSteps(c.f, a) {
			got = append(got, strings.Join(s, " "))
		}
		if g := strings.Join(got, "; "); g != c.want {
			t.Errorf("%s: agentSteps(%+v) = %q; want %q", c.name, c.f, g, c.want)
		}
	}
}

func TestParseService(t *testing.T) {
	const running = `gui/501/run.boxpool.daemon = {
	active count = 1
	path = /Users/me/Library/LaunchAgents/run.boxpool.daemon.plist
	state = running

	program = /Users/me/Library/Application Support/boxpool/bin/boxpool
	arguments = {
		/Users/me/Library/Application Support/boxpool/bin/boxpool
		daemon
	}

	resource coalition = {
		ID = 152177
		state = active
	}

	runs = 1
	pid = 63736
	last exit code = (never exited)
}
`
	s, err := parseService(running)
	if want := (service{state: "running", pid: 63736, lastExit: "(never exited)"}); err != nil || s != want {
		t.Errorf("parseService(running) = %+v, %v; want %+v", s, err, want)
	}
	waiting := "gui/501/x = {\n\tstate = spawn scheduled\n\tlast exit code = 1\n}\n"
	if s, err := parseService(waiting); err != nil || s != (service{state: "spawn scheduled", lastExit: "1"}) {
		t.Errorf("parseService(waiting) = %+v, %v", s, err)
	}
	for _, bad := range []string{"", "Bad request.\n", "x = {\n\tstate = running\n\tpid = -1\n}", "x = {\n\tstate = running\n\tpid = abc\n}"} {
		if s, err := parseService(bad); err == nil {
			t.Errorf("parseService(%q) = %+v, want an error", bad, s)
		}
	}
}

func TestParseDisabled(t *testing.T) {
	out := "disabled services = {\n\t\t\"com.example\" => disabled\n\t\t\"run.boxpool.daemon.test\" => disabled\n\t\t\"run.boxpool.daemon\" => enabled\n\t}\n"
	if parseDisabled(out, agentLabel) {
		t.Error("enabled label reported disabled")
	}
	if !parseDisabled(out, "run.boxpool.daemon.test") {
		t.Error("disabled label reported enabled")
	}
	if parseDisabled(out, "missing") {
		t.Error("missing label reported disabled")
	}
	if !parseDisabled("\t\"run.boxpool.daemon\" => true\n", agentLabel) {
		t.Error("older macOS true not read as disabled")
	}
}

func TestListsPID(t *testing.T) {
	for _, c := range []struct {
		out  string
		want bool
	}{{"42\n", true}, {"7\n42\n", true}, {"420\n", false}, {"", false}} {
		if got := listsPID(c.out, 42); got != c.want {
			t.Errorf("listsPID(%q, 42) = %v", c.out, got)
		}
	}
}

func TestHasVirtualization(t *testing.T) {
	for _, c := range []struct {
		xml  string
		want bool
	}{
		// codesign -d --entitlements - --xml prints it on one line.
		{`<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "https://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>com.apple.security.virtualization</key><true/></dict></plist>`, true},
		{"<dict>\n\t<key>com.apple.security.virtualization</key>\n\t<true/>\n</dict>", true},
		{"<dict><key>com.apple.security.virtualization</key><false/></dict>", false},
		{"<dict><key>com.apple.security.hypervisor</key><true/></dict>", false},
		{"", false}, // linker-signed or unsigned: no entitlements
	} {
		if got := hasVirtualization([]byte(c.xml)); got != c.want {
			t.Errorf("hasVirtualization(%q) = %v", c.xml, got)
		}
	}
}
