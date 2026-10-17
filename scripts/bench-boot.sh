#!/usr/bin/env bash
# Cold boot benchmark: boots the upstream Firecracker quickstart kernel and
# rootfs RUNS times (default 10) and reports, from Firecracker process start:
#   api     the InstanceStart API call returning
#   first   the first byte from the guest on the serial console
#   prompt  the root shell prompt on the serial console (autologin getty)
# plus two guest-side splits: Firecracker's boot timer (InstanceStart to the
# guest init writing the boot-timer MMIO address) and the kernel's printk
# time when it runs init.
#
# Usage: scripts/bench-boot.sh [RUNS]
#
# Needs Linux, read/write access to /dev/kvm, bash 5+, curl, tar, sha256sum,
# unsquashfs (squashfs-tools), mkfs.ext4 (e2fsprogs), and sudo once to turn
# the rootfs squashfs into ext4 as the getting-started guide does. Downloads
# are cached in $CACHE_DIR. Set LOG_DIR to keep each run's serial console and
# Firecracker log. See docs/benchmarks/m1-boot.md.
set -euo pipefail
export LC_ALL=C # EPOCHREALTIME uses the locale's decimal point

RUNS=${1:-10}
CACHE_DIR=${CACHE_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/boxpool-bench-boot}
LOG_DIR=${LOG_DIR:-}

# Pinned artifacts. The Firecracker tarball sha256 matches the .sha256.txt
# published with the release. The CI kernel and rootfs have no published
# checksum; these are the sha256 values we observed on 2026-10-18 for the CI
# prefix that docs/getting-started.md's "latest" lookup selected that day.
FC_VERSION=v1.17.0
CI_URL=https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/20261016-5732b8331e1e-0
KERNEL_NAME=vmlinux-6.18.54
ROOTFS_NAME=ubuntu-24.04
VCPUS=2
MEM_MIB=1024
# The guide's boot args, plus the rootfs's boot-timer init, which signals
# Firecracker and then execs /sbin/init (systemd).
BOOT_ARGS="console=ttyS0 reboot=k panic=1 init=/usr/local/bin/init"
PROMPT='root@ubuntu-fc-uvm:'
RUN_TIMEOUT_S=60

die() {
	echo "bench-boot: $*" >&2
	exit 1
}

[[ $RUNS =~ ^[1-9][0-9]*$ ]] || die "RUNS must be a positive integer, got '$RUNS'"
[ "$(uname -s)" = Linux ] || die "needs Linux with KVM; this host runs $(uname -s). Firecracker cannot run here: record 'no KVM' for this machine."
((BASH_VERSINFO[0] >= 5)) || die "needs bash 5 or newer (for EPOCHREALTIME)"

ARCH=$(uname -m)
case $ARCH in
x86_64)
	FC_SHA=06094a1108ae9e82aa4c23a775aa92758f53f1175d422270d9d6162cb9ade558
	KERNEL_SHA=445b3d0ed9650869e854656308d60e6cca773190526b037b45941cbf61910f0d
	ROOTFS_SHA=40eaeb2ccd067314d75977784835884c22f19190b757456662df13134f58956e
	;;
aarch64)
	FC_SHA=e351ebe4f7a16b5873bbd51005d2e6767103cff4d5ebc829df2d3f95a93e2256
	KERNEL_SHA=cfbfd2028adfd76e8ede9e56f18034c40f206c3afb2a529c04755bd17c144ba1
	ROOTFS_SHA=8ce1b81cf68f8340f79e08c2e6947e3bd8269c5324b1d8d9089f2e017b0d8698
	BOOT_ARGS="keep_bootcon $BOOT_ARGS"
	;;
*) die "unsupported architecture $ARCH (Firecracker supports x86_64 and aarch64)" ;;
esac

if [ ! -e /dev/kvm ]; then
	die "/dev/kvm does not exist, so KVM is not available on this host.
  - Bare metal x86_64: enable Intel VT-x / AMD-V in firmware, then
    'sudo modprobe kvm_intel' or 'sudo modprobe kvm_amd'.
  - Inside a VM: enable nested virtualization on the outer hypervisor.
  - If the CPU has no virtualization support, record 'no KVM' for this machine.
  See docs/development.md (Troubleshooting doctor)."
fi
if [ ! -r /dev/kvm ] || [ ! -w /dev/kvm ]; then
	die "/dev/kvm exists but $(id -un) cannot read and write it. Run
  'sudo usermod -aG kvm $(id -un)' and log in again (see docs/development.md)."
fi
for tool in curl tar sha256sum unsquashfs mkfs.ext4 sudo; do
	command -v "$tool" >/dev/null ||
		die "missing '$tool'. On Debian/Ubuntu: sudo apt install curl tar coreutils squashfs-tools e2fsprogs sudo"
done

work=$(mktemp -d "${TMPDIR:-/tmp}/bench-boot.XXXXXX")
fc_pid=

stop_vm() {
	if [ -n "$fc_pid" ]; then
		kill -KILL "$fc_pid" 2>/dev/null || true
		wait "$fc_pid" 2>/dev/null || true
		fc_pid=
	fi
}
cleanup() {
	stop_vm
	# squashfs-root is root-owned if we die while converting the rootfs.
	rm -rf "$work" 2>/dev/null || sudo rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# fetch URL SHA256 DEST: download once into the cache and verify.
fetch() {
	[ -f "$3" ] && echo "$2  $3" | sha256sum -c --status && return
	echo "downloading $1" >&2
	curl -fsSL --retry 3 -o "$3.part" "$1" ||
		die "download failed: $1 (if upstream removed it, update the pins in $0)"
	echo "$2  $3.part" | sha256sum -c --status || die "sha256 mismatch for $1"
	mv "$3.part" "$3"
}

mkdir -p "$CACHE_DIR"
fc_tgz=$CACHE_DIR/firecracker-$FC_VERSION-$ARCH.tgz
fetch "https://github.com/firecracker-microvm/firecracker/releases/download/$FC_VERSION/firecracker-$FC_VERSION-$ARCH.tgz" "$FC_SHA" "$fc_tgz"
tar -xzf "$fc_tgz" -C "$work"
fc=$work/release-$FC_VERSION-$ARCH/firecracker-$FC_VERSION-$ARCH

kernel=$CACHE_DIR/$KERNEL_NAME-$ARCH
fetch "$CI_URL/$ARCH/$KERNEL_NAME" "$KERNEL_SHA" "$kernel"
squashfs=$CACHE_DIR/$ROOTFS_NAME-$ARCH.squashfs
fetch "$CI_URL/$ARCH/$ROOTFS_NAME.squashfs" "$ROOTFS_SHA" "$squashfs"

# As in the guide: unpack as root to keep ownership, then build a 1 GiB ext4.
# We skip the guide's SSH key step; this benchmark only uses the serial console.
rootfs=$CACHE_DIR/$ROOTFS_NAME-$ARCH-$ROOTFS_SHA.ext4
if [ ! -f "$rootfs" ]; then
	echo "converting $ROOTFS_NAME.squashfs to ext4 (needs sudo)" >&2
	sudo unsquashfs -q -d "$work/squashfs-root" "$squashfs" >/dev/null
	truncate -s 1G "$work/rootfs.ext4"
	sudo mkfs.ext4 -q -F -d "$work/squashfs-root" "$work/rootfs.ext4"
	sudo rm -rf "$work/squashfs-root"
	mv "$work/rootfs.ext4" "$rootfs"
fi

cpu_model=$( (lscpu 2>/dev/null || true) | sed -n 's/^Model name: *//p' | head -n 1)
virt=$(systemd-detect-virt 2>/dev/null || true)
hv_flag=no
if grep -qw hypervisor /proc/cpuinfo; then hv_flag=yes; fi
cat <<EOF
== host
cpu:          ${cpu_model:-unknown}
nproc:        $(nproc)
memory:       $(awk '/^MemTotal/ { printf "%.1f GiB", $2 / 1048576 }' /proc/meminfo)
os:           $(sed -n 's/^PRETTY_NAME="\{0,1\}\([^"]*\)"\{0,1\}$/\1/p' /etc/os-release)
host kernel:  $(uname -r) $ARCH
virtualized:  ${virt:-unknown} (systemd-detect-virt); hypervisor cpu flag: $hv_flag
== guest
firecracker:  $("$fc" --version | head -n 1)
kernel:       $KERNEL_NAME ($(stat -c %s "$kernel") bytes) $KERNEL_SHA
rootfs:       $ROOTFS_NAME.squashfs ($(stat -c %s "$squashfs") bytes) $ROOTFS_SHA
rootfs ext4:  $(stat -c %s "$rootfs") bytes apparent, $(du -k "$rootfs" | cut -f1) KiB used
machine:      $VCPUS vCPU, $MEM_MIB MiB, --enable-pci --boot-timer
boot args:    $BOOT_ARGS
runs:         $RUNS (ms from Firecracker process start, host page cache warm after run 1)

EOF

# us: EPOCHREALTIME (seconds.micros) as integer microseconds.
us() { echo $((10#${1/./})); }

api() {
	curl -sS --fail-with-body --unix-socket "$sock" -X PUT \
		-H 'Content-Type: application/json' -d "$2" "http://localhost/$1" >&2 ||
		fail_run "PUT /$1 failed"
}

fail_run() {
	echo "bench-boot: run $run: $*" >&2
	for f in "$con" "$work/fc.stderr" "$log"; do
		[ -s "$f" ] && { echo "--- tail of ${f##*/}" >&2; tail -n 20 "$f" >&2; }
	done
	exit 1
}

sock=$work/fc.sock con=$work/console.log log=$work/fc.log disk=$work/rootfs.ext4
api_ms=() first_ms=() prompt_ms=() timer_ms=() kinit_ms=()
printf '%-4s %8s %8s %8s %10s %10s\n' run api first prompt boot-timer kernel-init
for ((run = 1; run <= RUNS; run++)); do
	rm -f "$sock"
	: >"$con"
	: >"$log"
	cp --sparse=always "$rootfs" "$disk" # fresh disk: no journal replay from the last kill

	t0=$EPOCHREALTIME
	"$fc" --api-sock "$sock" --enable-pci --boot-timer --log-path "$log" --level Info \
		</dev/null >"$con" 2>"$work/fc.stderr" &
	fc_pid=$!
	until [ -S "$sock" ]; do
		kill -0 "$fc_pid" 2>/dev/null || fail_run "firecracker exited before opening its API socket"
		sleep 0.001
	done
	api machine-config "{\"vcpu_count\": $VCPUS, \"mem_size_mib\": $MEM_MIB}"
	api boot-source "{\"kernel_image_path\": \"$kernel\", \"boot_args\": \"$BOOT_ARGS\"}"
	api drives/rootfs "{\"drive_id\": \"rootfs\", \"path_on_host\": \"$disk\", \"is_root_device\": true, \"is_read_only\": false}"
	api actions '{"action_type": "InstanceStart"}'
	t_api=$EPOCHREALTIME

	# shortcut: 5 ms polling, so first/prompt are late by up to ~5 ms plus a
	# grep; fine for second-scale boots, use an event-driven reader if
	# sub-10 ms precision ever matters.
	t_first='' t_prompt='' deadline=$(($(us "$t0") + RUN_TIMEOUT_S * 1000000))
	while [ -z "$t_prompt" ]; do
		now=$EPOCHREALTIME
		if [ -z "$t_first" ] && [ -s "$con" ]; then t_first=$now; fi
		if [ -n "$t_first" ] && grep -qF "$PROMPT" "$con"; then t_prompt=$now; fi
		kill -0 "$fc_pid" 2>/dev/null || fail_run "firecracker exited before the shell prompt"
		((10#${now/./} < deadline)) ||
			fail_run "no shell prompt within ${RUN_TIMEOUT_S}s"
		[ -n "$t_prompt" ] || sleep 0.005
	done
	stop_vm

	timer=$(sed -n 's/.*Guest-boot-time = *\([0-9]*\) us.*/\1/p' "$log" | head -n 1)
	kinit=$(sed -n 's/^\[ *\([0-9]*\.[0-9]*\)\] Run .* as init process.*/\1/p' "$con" | head -n 1)
	api_ms+=($((($(us "$t_api") - $(us "$t0")) / 1000)))
	first_ms+=($((($(us "$t_first") - $(us "$t0")) / 1000)))
	prompt_ms+=($((($(us "$t_prompt") - $(us "$t0")) / 1000)))
	timer_ms+=("${timer:+$((timer / 1000))}")
	kinit_ms+=("${kinit:+$(awk -v s="$kinit" 'BEGIN { printf "%d", s * 1000 }')}")
	printf '%-4s %8s %8s %8s %10s %10s\n' "$run" "${api_ms[-1]}" "${first_ms[-1]}" \
		"${prompt_ms[-1]}" "${timer_ms[-1]:--}" "${kinit_ms[-1]:--}"

	if [ -n "$LOG_DIR" ]; then
		mkdir -p "$LOG_DIR"
		cp "$con" "$LOG_DIR/run-$run-console.log"
		cp "$log" "$LOG_DIR/run-$run-firecracker.log"
	fi
	rm -f "$disk" "$sock"
done

# stats NAME VALUES...: median and nearest-rank p90 of the numeric values.
stats() {
	local name=$1
	shift
	printf '%s\n' "$@" | awk '/^[0-9]+$/' | sort -n | awk -v name="$name" '
		{ v[NR] = $1 }
		END {
			if (NR == 0) { printf "%-44s n/a\n", name; exit }
			med = NR % 2 ? v[(NR + 1) / 2] : (v[NR / 2] + v[NR / 2 + 1]) / 2
			p = int(0.9 * NR); if (p < 0.9 * NR) p++
			printf "%-44s median %7.1f  p90 %6d  min %6d  max %6d  (n=%d)\n", name, med, v[p], v[1], v[NR], NR
		}'
}
echo
echo "== summary (ms)"
stats "api: process start -> InstanceStart returned" "${api_ms[@]}"
stats "first: process start -> first console byte" "${first_ms[@]}"
stats "prompt: process start -> shell prompt" "${prompt_ms[@]}"
stats "boot-timer: InstanceStart -> guest init" "${timer_ms[@]}"
stats "kernel-init: guest printk time at init" "${kinit_ms[@]}"
