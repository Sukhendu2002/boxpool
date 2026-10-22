#!/usr/bin/env bash
# Boot this build's actual image, then check its agent via vsock. No networking,
# daemon, driver or general integration harness. Requires Linux, KVM and Go.
set -euo pipefail
die() { echo "image/smoke: $*" >&2; exit 1; }
[[ $# == 2 ]] || die 'usage: image/smoke.sh <artifact-directory> <expected-version>'
[[ $(uname -s) == Linux ]] || die 'requires Linux with accessible KVM'
[[ -r /dev/kvm && -w /dev/kvm ]] || die '/dev/kvm must be readable and writable'
for tool in curl tar sha256sum zstd go timeout; do
    command -v "$tool" >/dev/null || die "missing $tool"
done
# Resolve current once so a concurrent build cannot mix generations mid-check.
dir=$(cd "$1" && pwd -P)
version=$2
case $(uname -m) in
x86_64)
    arch=amd64; machine=x86_64
    sha=06094a1108ae9e82aa4c23a775aa92758f53f1175d422270d9d6162cb9ade558
    ;;
aarch64)
    arch=arm64; machine=aarch64
    sha=e351ebe4f7a16b5873bbd51005d2e6767103cff4d5ebc829df2d3f95a93e2256
    ;;
*) die 'unsupported host architecture' ;;
esac
repo=$(cd "$(dirname "$0")/.." && pwd)
(cd "$dir" && sha256sum -c "SHA256SUMS-$arch")
cache=${CACHE_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/boxpool-bench-boot}
mkdir -p "$cache"
# Same release, upstream release checksum and cache path as bench-boot.sh.
fc_version=v1.17.0
archive=$cache/firecracker-$fc_version-$machine.tgz
if ! echo "$sha  $archive" | sha256sum -c --status 2>/dev/null; then
    part=$(mktemp "$cache/.firecracker.XXXXXX")
    trap 'rm -f "$part"' EXIT
    curl -fL --retry 3 --connect-timeout 30 --max-time 300 \
        "https://github.com/firecracker-microvm/firecracker/releases/download/$fc_version/firecracker-$fc_version-$machine.tgz" -o "$part"
    echo "$sha  $part" | sha256sum -c -
    mv "$part" "$archive"
fi
work=$(mktemp -d "${TMPDIR:-/tmp}/boxpool-image-smoke.XXXXXX")
pid=
cleanup() {
    if [[ -n $pid ]]; then
        kill -TERM "$pid" 2>/dev/null || true
        # The outer timeout bounds shutdown too; only this child can be killed.
        wait "$pid" 2>/dev/null || true
    fi
    rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 130' INT TERM
on_error() {
    tail -n 60 "$work/console.log" >&2 || true
    tail -n 20 "$work/firecracker.log" >&2 || true
}
trap on_error ERR
tar -xzf "$archive" -C "$work"
fc=$work/release-$fc_version-$machine/firecracker-$fc_version-$machine
zstd -d "$dir/rootfs-$arch.ext4.zst" -o "$work/rootfs.ext4"
cd "$repo"
go build -o "$work/probe" ./image/smoke
# Python only serializes paths safely into Firecracker's JSON config.
command -v python3 >/dev/null || die 'missing python3'
python3 - "$dir/vmlinux-$arch" "$work" <<'PY'
import json, sys
kernel, work = sys.argv[1:]
args = "console=ttyS0 reboot=k panic=1 root=/dev/vda rw init=/sbin/init i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd"
with open(work + "/config.json", "w") as f:
    json.dump({"boot-source": {"kernel_image_path": kernel, "boot_args": args},
               "drives": [{"drive_id": "rootfs", "path_on_host": work + "/rootfs.ext4", "is_root_device": True, "is_read_only": False}],
               "machine-config": {"vcpu_count": 2, "mem_size_mib": 256},
               "vsock": {"guest_cid": 3, "uds_path": work + "/vsock.sock"}}, f)
PY
timeout --signal=TERM --kill-after=2 45 "$fc" --no-api --enable-pci --config-file "$work/config.json" \
    --log-path "$work/firecracker.log" --level Info </dev/null >"$work/console.log" 2>&1 &
pid=$!
"$work/probe" -socket "$work/vsock.sock" -version "$version"
echo "Firecracker $fc_version: built $arch image smoke passed"
