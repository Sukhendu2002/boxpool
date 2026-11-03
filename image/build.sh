#!/usr/bin/env bash
# Build the OCI image and VM artifacts through a Linux Docker/BuildKit engine.
set -euo pipefail
die() { echo "image/build: $*" >&2; exit 1; }
[[ $# == 1 ]] || die 'usage: image/build.sh <amd64|arm64>'
arch=$1
case $arch in amd64|arm64) ;; *) die "unsupported architecture '$arch'; use amd64 or arm64" ;; esac
variant=${IMAGE_VARIANT:-base}
case $variant in base) suffix=; target=userland; image_name=boxpool-image ;; desktop) suffix=-desktop; target=desktop; image_name=boxpool-desktop ;; *) die 'IMAGE_VARIANT must be base or desktop' ;; esac
repo=$(cd "$(dirname "$0")/.." && pwd)
version=${IMAGE_VERSION:-}
# Reserve six characters for the architecture suffix in the local Docker tag.
[[ -z $version || $version =~ ^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,121}$ ]] || die 'IMAGE_VERSION must be 1-122 letters, digits, _, . or -; no leading . or - (the Docker tag also includes -<arch>)'
jobs=${JOBS:-4}
[[ $jobs =~ ^[1-8]$ ]] || die 'JOBS must be between 1 and 8'
revision=${IMAGE_REVISION:-}
if [[ -z $revision ]]; then
    # Do not accidentally label a source copy with an enclosing repo's HEAD.
    checkout=$(git -C "$repo" rev-parse --show-toplevel 2>/dev/null) || die 'cannot determine source revision; use a Git checkout or set IMAGE_REVISION to the full source commit SHA'
    [[ $(cd "$checkout" && pwd) == "$repo" ]] || die 'source directory is not a Git checkout root; set IMAGE_REVISION to the full source commit SHA'
    revision=$(git -C "$repo" rev-parse --verify HEAD 2>/dev/null) || die 'cannot resolve HEAD; set IMAGE_REVISION to the full source commit SHA'
fi
[[ $revision =~ ^([0-9a-f]{40}|[0-9a-f]{64})$ ]] || die 'IMAGE_REVISION must be a full lowercase hexadecimal Git commit SHA (40 or 64 characters)'
version=${version:-0.0.0-dev.${revision:0:12}}
command -v docker >/dev/null || die 'Docker with the buildx plugin is required'
docker buildx version >/dev/null || die 'Docker buildx is required'
docker info >/dev/null 2>&1 || die 'cannot reach the Docker engine; select an accessible engine with DOCKER_HOST or DOCKER_CONTEXT'
default_out=$repo/dist/image/$arch
if [[ $variant == desktop ]]; then default_out=$repo/dist/image/desktop/$arch; fi
out=${OUTPUT_DIR:-$default_out}
mkdir -p "$out"
out=$(cd "$out" && pwd -P)
[[ ! -e $out/current || -L $out/current ]] || die "$out/current must be absent or a symlink; move this existing entry aside explicitly"
# Rename must replace the symlink itself, even when it points to a directory.
# GNU and BSD mv spell this differently; both perform a same-filesystem rename.
case $(uname -s) in
    Linux) switch=(-Tf) ;;
    Darwin) switch=(-fh) ;;
    *) die 'artifact promotion requires Linux or macOS mv' ;;
esac
work=$(mktemp -d "$out/.generation.XXXXXX")
pointer=$work.current
keep=false
cleanup() {
    rm -f "$pointer"
    if [[ $keep == false ]]; then rm -rf "$work"; fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
image=$image_name:$version-$arch
args=(--platform "linux/$arch" --file "$repo/image/Dockerfile"
    --build-arg "IMAGE_VERSION=$version" --build-arg "IMAGE_REVISION=$revision"
    --build-arg "JOBS=$jobs" --build-arg "IMAGE_VARIANT=$variant")
docker buildx build "${args[@]}" --target "$target" --tag "$image" --load "$repo"
docker buildx build "${args[@]}" --target artifacts --output "type=local,dest=$work" "$repo"
# Save the exact image tested locally; the release job loads it, never rebuilds.
docker image save "$image" -o "$work/oci$suffix-$arch.tar"
if command -v sha256sum >/dev/null; then
    (cd "$work" && sha256sum "oci$suffix-$arch.tar" > "oci$suffix-$arch.tar.sha256")
else
    (cd "$work" && shasum -a 256 "oci$suffix-$arch.tar" > "oci$suffix-$arch.tar.sha256")
fi
wc -c "$work/rootfs$suffix-$arch.ext4.zst"
ln -s "${work##*/}" "$pointer"
# Keep this complete generation before the switch: a signal immediately after
# rename must never let EXIT remove the newly selected set. Old generations
# and legacy flat outputs are retained too; only current selects final assets.
keep=true
mv "${switch[@]}" "$pointer" "$out/current" || die "cannot promote artifacts; prior output retained, complete candidate: $work"
printf 'image=%s\nversion=%s\nartifacts=%s\n' "$image" "$version" "$work"
