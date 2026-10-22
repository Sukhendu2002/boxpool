package agent

import (
	"net"
	"testing"
)

func TestPorts(t *testing.T) {
	for _, network := range []string{"tcp4", "tcp6"} {
		t.Run(network, func(t *testing.T) {
			l, err := net.Listen(network, "localhost:0")
			if err != nil {
				if network == "tcp6" {
					t.Skipf("IPv6 unavailable: %v", err)
				}
				t.Fatal(err)
			}
			defer l.Close()
			ports, err := serve(t).Ports(testContext(t))
			if err != nil {
				t.Fatal(err)
			}
			addr := l.Addr().(*net.TCPAddr)
			for _, p := range ports {
				if p.Proto == "tcp" && p.Address == addr.IP.String() && p.Port == addr.Port {
					return
				}
			}
			t.Fatalf("listener %s absent from %+v", addr, ports)
		})
	}
}
