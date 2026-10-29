package backend

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"boxpool.run/boxpool/internal/agent"
)

// GuestNetwork replaces the network identity captured in a warm snapshot.
// Empty fields leave the kernel's cold-boot configuration alone.
type GuestNetwork struct {
	Address, Mask, Gateway, Resolvers string
}

// ConfigureGuest runs before exposing a new VM. Fresh restores must replace
// cloned identity and reseed the kernel RNG before any user code runs.
func ConfigureGuest(ctx context.Context, c *agent.Client, id, name string, fresh, restore bool, network GuestNetwork) error {
	if name == "" {
		name = id
	}
	machineID := sha256.Sum256([]byte("boxpool:" + id))
	seed := make([]byte, 32)
	rand.Read(seed)
	now := time.Now()
	flag := func(b bool) string {
		if b {
			return "1"
		}
		return "0"
	}
	args := []string{"sh", "-eu", "-c", configureGuest, "sh", name, hex.EncodeToString(machineID[:16]), flag(fresh), flag(restore),
		hex.EncodeToString(seed), fmt.Sprintf("%d.%09d", now.Unix(), now.Nanosecond()), network.Address, network.Mask, network.Gateway, network.Resolvers}
	var out bytes.Buffer
	s, err := c.Exec(ctx, agent.ExecRequest{Argv: args}, &out, &out)
	if err != nil {
		return err
	}
	if code, err := s.Wait(); err != nil || code != 0 {
		return fmt.Errorf("initialize guest: exit status %d, %v: %s", code, err, bytes.TrimSpace(out.Bytes()))
	}
	return nil
}

const configureGuest = `
if [ "$4" = 1 ]; then
    date -u -s "@$6" >/dev/null
    perl -e 'open(my $r, "+<", "/dev/random") or die $!;
        ioctl($r, 0x40085203, pack("iiH*", 256, 32, $ARGV[0])) or die "RNDADDENTROPY: $!";
        ioctl($r, 0x5207, 0) or die "RNDRESEEDCRNG: $!"' "$5"
fi
if [ "$3" = 1 ]; then
    printf '%s\n' "$1" > /proc/sys/kernel/hostname
    printf '%s\n' "$1" > /etc/hostname
    printf '%s\n' "$2" > /etc/machine-id
    if [ -e /var/lib/dbus/machine-id ]; then printf '%s\n' "$2" > /var/lib/dbus/machine-id; fi
    printf '127.0.0.1 localhost\n127.0.1.1 %s\n::1 localhost ip6-localhost ip6-loopback\n' "$1" > /etc/hosts
fi
if [ -n "$7" ]; then
    perl -MSocket -e '
        socket(my $s, AF_INET, SOCK_DGRAM, 0) or die $!;
        sub address { pack("S", AF_INET) . pack("n", 0) . inet_aton($_[0]) . ("\0" x 8) }
        # The image has Perl but no iproute2; ifreq and rtentry are Linux 64-bit ABIs.
        my $req = pack("a16", "eth0") . address($ARGV[0]) . ("\0" x 8);
        ioctl($s, 0x8916, $req) or die "SIOCSIFADDR: $!";
        $req = pack("a16", "eth0") . address($ARGV[1]) . ("\0" x 8);
        ioctl($s, 0x891c, $req) or die "SIOCSIFNETMASK: $!";
        sub route {
            my $r = "\0" x 120;
            substr($r, 8, 16) = address("0.0.0.0");
            substr($r, 24, 16) = address($_[0]);
            substr($r, 40, 16) = address("0.0.0.0");
            substr($r, 56, 2) = pack("S", 3);
            return $r;
        }
        open(my $routes, "<", "/proc/net/route") or die $!;
        while (<$routes>) {
            my @f = split;
            next unless $f[0] eq "eth0" && $f[1] eq "00000000";
            my $old = inet_ntoa(pack("V", hex($f[2])));
            ioctl($s, 0x890c, route($old)) or die "SIOCDELRT: $!";
        }
        ioctl($s, 0x890b, route($ARGV[2])) or die "SIOCADDRT: $!";
    ' "$7" "$8" "$9"
fi
if [ -n "${10}" ]; then printf '%s' "${10}" > /etc/resolv.conf; fi
`
