#!/bin/bash
# host-network.sh HOST_TCP_PORT HOST_UDP_PORT HOST_IP_OR_GATEWAY...
set -eu -o pipefail
tcp=$1
udp=$2
shift 2
command -v perl >/dev/null
command -v timeout >/dev/null
test "$#" -gt 0
for ip in "$@"; do
  # IO::Socket errors and timeout are denials; tool failures must fail the probe.
  if timeout 3 perl -MIO::Socket::INET -e '
    my $s = IO::Socket::INET->new(PeerAddr=>$ARGV[0], PeerPort=>$ARGV[1], Timeout=>1);
    exit($s ? 0 : 42)' "$ip" "$tcp"; then
    echo "FAIL: host TCP reachable at $ip:$tcp"; exit 1
  else
    code=$?; test "$code" -eq 42 -o "$code" -eq 124
  fi
  # UDP connect/send success is not evidence of reachability: the host harness
  # must check that no packet arrives at its listener.
  perl -MIO::Socket::INET -e '
    my $s = IO::Socket::INET->new(PeerAddr=>$ARGV[0], PeerPort=>$ARGV[1], Proto=>"udp") or die $!;
    $s->send("boxpool-escape-probe") or die $!;' "$ip" "$udp"
done
echo 'blocked: guest-initiated host TCP (UDP checked by host harness)'
