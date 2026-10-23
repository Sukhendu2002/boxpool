//go:build linux && integration

package firecracker

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// Capture complete output: a formatting pipeline can hide the command's
// failure or cause a successful producer to fail with SIGPIPE.
type networkProbe struct {
	command string
	check   func(string) bool
}

var httpsProbe = networkProbe{
	command: "curl --fail --silent --show-error --head --connect-timeout 10 --max-time 20 https://example.com",
	check: func(out string) bool {
		status := ""
		for _, line := range strings.Split(out, "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && strings.HasPrefix(fields[0], "HTTP/") {
				status = fields[1]
			}
		}
		return status == "200" || status == "301"
	},
}

var pingProbe = networkProbe{
	command: "ping -n -c1 -W2 1.1.1.1",
	check: func(out string) bool {
		return strings.Contains(out, "bytes from 1.1.1.1:") &&
			regexp.MustCompile(`(?m)1 packets transmitted, 1 (?:packets )?received, 0% packet loss`).MatchString(out)
	},
}

func (p networkProbe) validate(out string, code int) error {
	if code != 0 || !p.check(out) {
		return fmt.Errorf("%s: exit %d, missing successful network response: %s", p.command, code, out)
	}
	return nil
}

func TestNetworkProbe(t *testing.T) {
	for _, tc := range []struct {
		name, stub, good, bad string
		probe                 networkProbe
	}{
		{"HTTPS", "curl", "HTTP/2 200\r\nserver: test\r\n", "HTTP/2 503\r\n", httpsProbe},
		{"ping", "ping", "64 bytes from 1.1.1.1: icmp_seq=1 ttl=57 time=10 ms\n1 packets transmitted, 1 received, 0% packet loss", "1 packets transmitted, 0 received, 100% packet loss", pingProbe},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, test := range []struct {
				name, out string
				code      int
				wantOK    bool
			}{
				{"success", tc.good, 0, true},
				{"empty success", "", 0, false},
				{"bad response", tc.bad, 0, false},
				{"failed command", "", 60, false},
				{"failure with plausible output", tc.good, 60, false},
			} {
				t.Run(test.name, func(t *testing.T) {
					// Run the exact guest command with a stub, so restoring a masking
					// pipeline would make the plausible-output negative control fail.
					script := fmt.Sprintf("%s(){ printf '%%s' \"$PROBE_OUTPUT\"; return %d; }; %s", tc.stub, test.code, tc.probe.command)
					cmd := exec.Command("bash", "-c", script)
					cmd.Env = append(cmd.Environ(), "PROBE_OUTPUT="+test.out)
					out, err := cmd.CombinedOutput()
					code := 0
					if err != nil {
						if exit, ok := err.(*exec.ExitError); ok {
							code = exit.ExitCode()
						} else {
							t.Fatal(err)
						}
					}
					if got := tc.probe.validate(string(out), code); (got == nil) != test.wantOK {
						t.Fatalf("validate: %v; want success=%v", got, test.wantOK)
					}
				})
			}
		})
	}
}
