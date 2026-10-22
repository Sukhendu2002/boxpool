#!/usr/bin/env bash
# Called inside the pinned tools stage; compiles natively or with a cross GCC.
set -euo pipefail
arch=${1:?expected amd64 or arm64}
jobs=${2:-4}
version=6.18.54
sha=9df30b02dd8102bbd0be52556288ef6889ddbe7f1ddb96fbf847d0becf3eacac
case $arch in
amd64) kernel_arch=x86; cross=x86_64-linux-gnu-; target=bzImage ;;
arm64) kernel_arch=arm64; cross=aarch64-linux-gnu-; target=Image ;;
*) echo "unsupported architecture: $arch (use amd64 or arm64)" >&2; exit 1 ;;
esac
[[ $jobs =~ ^[1-8]$ ]] || { echo 'JOBS must be between 1 and 8' >&2; exit 1; }
curl -fL --retry 3 --connect-timeout 30 --max-time 600 \
    "https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-$version.tar.xz" -o linux.tar.xz
echo "$sha  linux.tar.xz" | sha256sum -c -
tar -xf linux.tar.xz
rm linux.tar.xz
cd "linux-$version"
cp "../config/$arch.config" .config
# Upstream Firecracker configs target Amazon's microvm kernel. Resolve against
# this pinned kernel.org source and require the boot-critical built-ins below.
scripts/config --disable MODULES --disable LOCALVERSION_AUTO --set-str LOCALVERSION '' \
    --disable DEBUG_INFO --enable DEBUG_INFO_NONE --disable SERIO_I8042 \
    --enable DEVTMPFS --enable DEVTMPFS_MOUNT --enable TMPFS \
    --enable UNIX98_PTYS --enable PROC_FS --enable SYSFS \
    --enable EXT4_FS --enable VSOCKETS --enable VIRTIO_VSOCKETS \
    --enable VIRTIO_BLK --enable VIRTIO_NET --enable VIRTIO_CONSOLE \
    --enable VIRTIO_MMIO --enable VIRTIO_PCI --enable HW_RANDOM_VIRTIO \
    --set-str SYSTEM_TRUSTED_KEYS '' --set-str SYSTEM_REVOCATION_KEYS ''
export ARCH=$kernel_arch CROSS_COMPILE=$cross
export KBUILD_BUILD_USER=boxpool KBUILD_BUILD_HOST=builder
export KBUILD_BUILD_TIMESTAMP='2026-10-20 00:00:00 UTC' KBUILD_BUILD_VERSION=1
make olddefconfig
for option in DEVTMPFS DEVTMPFS_MOUNT TMPFS UNIX98_PTYS PROC_FS SYSFS EXT4_FS VSOCKETS \
    VIRTIO_VSOCKETS VIRTIO_BLK VIRTIO_NET VIRTIO_CONSOLE VIRTIO_MMIO VIRTIO_PCI; do
    grep -qx "CONFIG_$option=y" .config || { echo "missing built-in $option" >&2; exit 1; }
done
make -j"$jobs" "$target"
mkdir /out
case $arch in
amd64)
    "${cross}strip" -o /out/vmlinux-amd64 vmlinux
    cp arch/x86/boot/bzImage /out/bzImage-amd64
    ;;
arm64) cp arch/arm64/boot/Image /out/vmlinux-arm64 ;;
esac
cp .config "/out/kernel-config-$arch"
printf 'kernel=%s\nsource=https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-%s.tar.xz\nsource_sha256=%s\n' \
    "$version" "$version" "$sha" > "/out/kernel-$arch.txt"
