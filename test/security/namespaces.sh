#!/bin/bash
# Run as guest root: namespaces.sh docker|firecracker HOST_BOOT_ID HOST_MARKER
set -eu -o pipefail
backend=$1
host_boot=$2
marker=$3
for tool in cat grep mount timeout perl; do command -v "$tool" >/dev/null; done
for p in /var/run/docker.sock /run/docker.sock /dev/kvm /dev/mem /dev/sda; do
  if test -e "$p"; then echo "FAIL: host resource visible: $p"; exit 1; fi
done
test ! -e "$marker"
test ! -e "/proc/1/root$marker"
if test "$backend" = docker; then
  grep -Eq '^Cap(Eff|Prm|Bnd):[[:space:]]+0+$' /proc/self/status
  for field in CapEff CapPrm CapBnd; do grep -Eq "^$field:[[:space:]]+0+$" /proc/self/status; done
  grep -Eq '^NoNewPrivs:[[:space:]]+1$' /proc/self/status
  grep -Eq '^Seccomp:[[:space:]]+2$' /proc/self/status
  # Mount is a real privileged syscall attempt. A nonexistent binary is fatal.
  mkdir -p /tmp/security-mount
  if mount -t tmpfs none /tmp/security-mount; then
    umount /tmp/security-mount; echo 'FAIL: privileged mount succeeded'; exit 1
  fi
  test ! -w /proc/sys/kernel/core_pattern
  test ! -w /sys/kernel/uevent_helper
  test ! -s /proc/kcore
  # Perl's core Socket module is present; raw sockets need CAP_NET_RAW.
  perl -MSocket -e 'socket(my $s, PF_INET, SOCK_RAW, 1) and die "raw socket succeeded"; $!{EPERM} or die "unexpected raw socket failure: $!"'
else
  # VM root may mount and use raw sockets in its own kernel. A distinct
  # boot ID plus the inaccessible host marker checks the actual boundary.
  test "$(cat /proc/sys/kernel/random/boot_id)" != "$host_boot"
fi
echo 'blocked: host socket, devices, /proc and /sys escape attempts'
