// Package docker is the fallback backend for Linux hosts without KVM. Each
// sandbox is a container of the base guest image on a rootful Docker
// Engine, reached through the Engine API on its unix socket (DOCKER_HOST
// if that is a unix:// URL, else /var/run/docker.sock). The client is
// plain net/http and negotiates API version 1.41 (Docker 20.10) to 1.47.
//
// Containers share the host's kernel, so isolation is weaker than a
// Firecracker microVM: a kernel exploit escapes. Container root is the
// host's uid 0 unless the engine remaps user namespaces. Warning says so:
// doctor prints it, the daemon logs it at start and serves it in
// /v1/info, for the commands that will create sandboxes (#15) to print.
//
// This is not Docker Sandboxes (https://docs.docker.com/ai/sandboxes/),
// which runs each sandbox in a microVM and needs KVM locally
// (https://docs.docker.com/ai/sandboxes/install/), so it cannot stand in
// on hosts without it.
//
// # Resources
//
// Sandbox ID x in state directory S has a container, a bridge network and a
// volume, all called boxpool-<hash of S/docker>-x and labelled
// run.boxpool.owner=S/docker and run.boxpool.sandbox=x, and the directory
// S/docker/x (0700) with agent/ (0777) in it. The driver lists only
// containers with its owner label, and before starting, stopping or
// removing anything checks the name and both labels: a name prefix is not
// proof of ownership. It removes nothing in New, so running sandboxes
// survive a daemon restart and a new Driver uses them as they are.
//
// # Hardening
//
//   - All capabilities dropped and none added; no-new-privileges; Docker's
//     default seccomp and AppArmor profiles; not privileged; no host PID,
//     IPC or network namespace; no devices; no published ports.
//   - A read-only root filesystem. /root, the volume, is HOME, the working
//     directory and the only path that persists across Stop and Start; it
//     starts as the image's /root. /tmp and /var/tmp (each half the memory
//     limit) and /run (16 MiB) are tmpfs, emptied by Stop. Installing system
//     packages fails; that needs a writable root, which this backend does not
//     offer.
//   - Spec.CPUs and Spec.MemoryMB become CPU and memory limits, with no swap;
//     at most 4096 processes. A cpuset also makes nproc report Spec.CPUs.
//     Spec.DiskGB is advisory: a plain named volume has no portable quota,
//     so Docker disk growth is not limited or reserved. Monitor the Engine's
//     data filesystem. /proc/meminfo and free report host memory; inspect
//     the memory cgroup for the enforced limit.
//   - Each sandbox has its own bridge network, and Docker's own rules drop
//     traffic between bridges, so sandboxes cannot reach each other. The
//     engine manages all of it; boxpool changes no host firewall rules.
//
// The only host path in the container is the sandbox's agent directory, on
// /run/boxpool. No Docker socket, /dev/kvm, home or other sandbox is
// mounted. The image's agent listens on agent.sock there as root without
// capabilities, so it cannot write the daemon's 0700 directory or chown its
// socket; the container's command makes the socket 0666 and the 0777 agent
// directory lets it create it. The 0700 sandbox directory keeps every other
// host user out.
//
// The guest owns everything in the agent directory, so the daemon never
// follows what is there: Dial opens agent.sock with O_PATH|O_NOFOLLOW,
// refuses anything but a socket (a symlink to another sandbox's socket,
// say) and connects through /proc/self/fd to the inode it opened, so a
// later swap cannot redirect it. That needs /proc on the host.
//
// # Limits
//
//   - Linux hosts only, rootful Docker only. Rootless Docker, Podman and
//     gVisor are out of scope; Docker Desktop's VM cannot connect a
//     bind-mounted unix socket to the host.
//   - Sandboxes reach the host's own addresses (the bridge gateway) and its
//     network like any container does; #53 reviews that.
//   - Each sandbox's network takes a subnet from the engine's address pools,
//     about 30 by default; raise default-address-pools in daemon.json for
//     more.
//   - SELinux hosts would need the agent directory relabelled; not handled.
//   - Pause and Resume are not supported; the daemon stops and starts
//     the sandbox instead, losing its memory.
//   - Snapshot saves only /root, as a tar from the Engine's archive API,
//     and Create with Spec.From extracts it there. The root file system is
//     read-only, so docker commit would save nothing the guest changed.
//
// Shared resource admission (internal/resources) keeps one CPU and the
// larger of 1 GiB or 10% of memory for the host. Defaults are 2 CPUs,
// 2048 MiB and 10 GiB. S/docker/x/resources.json retains the reservation
// until Remove, including after Stop or a failed Create whose rollback
// could not finish. New records the limits of older verified containers
// without changing their settings; old containers keep their old affinity.
package docker
