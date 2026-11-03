# Guest escape probes

The shell scripts run as guest root. `probes.go` embeds and runs them through
an agent connection, sets up real host TCP/UDP listeners and a private host
marker, and checks that no traffic or filesystem access crosses the boundary.
It also requires working guest DNS and outbound HTTP. Missing tools fail the
probe rather than counting as a blocked escape.

Run the harness on a disposable Linux host from the repository root:

```sh
go test -c -tags integration -o /tmp/docker-security.test ./internal/backend/docker
sudo env BOXPOOL_TEST_REQUIRE_DOCKER=1 /tmp/docker-security.test -test.v -test.run '^TestIntegration$'
go test -c -tags integration -o /tmp/fc-security.test ./internal/backend/firecracker
sudo /tmp/fc-security.test -test.v -test.run '^TestIntegrationNetwork$'
```

Docker requires a local rootful Engine, `ip`, `nft` and enough free CPU/RAM for
two sandboxes. Firecracker additionally requires accessible `/dev/kvm` and
fetches the pinned kernel/rootfs. CI requires both integration tests to pass;
Docker's runner requires the Engine and checks for skipped tests, and the KVM
runner requires an explicit `TestIntegrationNetwork` PASS.

For manual probes inside a guest, copy the scripts in and supply values from
the host:

```sh
bash namespaces.sh docker HOST_KERNEL_BOOT_ID HOST_PRIVATE_MARKER_PATH
bash host-network.sh HOST_TCP_PORT HOST_UDP_PORT GATEWAY HOST_LAN_IP HOST_VPN_IP
```

Use `firecracker` instead of `docker` for a VM. Both host listeners must be
bound and independently verified reachable from the host first. Verify no UDP
datagrams arrive: UDP send success alone cannot prove isolation. The host
harness does this automatically. Expected results start with `blocked:`;
`FAIL:` or a nonzero exit is a failure.

The Docker syscall probe attempts a privileged mount and raw socket, checks
capability dropping, no-new-privileges, seccomp and masked/read-only proc/sys.
VM root may use privileged syscalls inside its own kernel, so VM probes check
kernel separation and host-marker/control-socket inaccessibility instead.
Guest devices such as `/dev/mem` refer to the VM kernel and are permitted;
the same devices are forbidden in Docker, which shares the host kernel.

File transfer fuzzing runs without a backend:

```sh
go test ./internal/agent -run '^$' -fuzz '^FuzzFileUploadPaths$' -fuzztime=30s -parallel=2
```

See [the threat model and residual limits](../../docs/security.md).
