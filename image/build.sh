#!/usr/bin/env bash
# Build the OCI image and VM artifacts through a Linux Docker/BuildKit engine.
set -euo pipefail
die() { echo "image/build: $*" >&2; exit 1; }
[[ $# == 1 ]] || die 'usage: image/build.sh <amd64|arm64>'
arch=$1
case $arch in amd64|arm64) ;; *) die "unsupported architecture '$arch'; use amd64 or arm64" ;; esac
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
out=${OUTPUT_DIR:-$repo/dist/image/$arch}
mkdir -p "$out"
out=$(cd "$out" && pwd)
# Stage outputs in a unique directory: failed builds never leave partial assets
# under the final names, and cleanup touches only this invocation's temp path.
work=$(mktemp -d "$out/.build.XXXXXX")
trap 'rm -rf "$work"' EXIT
image=boxpool-image:$version-$arch
args=(--platform "linux/$arch" --file "$repo/image/Dockerfile"
    --build-arg "IMAGE_VERSION=$version" --build-arg "IMAGE_REVISION=$revision"
    --build-arg "JOBS=$jobs")
docker buildx build "${args[@]}" --target userland --tag "$image" --load "$repo"
docker buildx build "${args[@]}" --target artifacts --output "type=local,dest=$work" "$repo"
# Save the exact image tested locally; the release job loads it, never rebuilds.
docker image save "$image" -o "$work/oci-$arch.tar"
if command -v sha256sum >/dev/null; then
    (cd "$work" && sha256sum "oci-$arch.tar" > "oci-$arch.tar.sha256")
else
    (cd "$work" && shasum -a 256 "oci-$arch.tar" > "oci-$arch.tar.sha256")
fi
cp "$work/"* "$out/"
printf 'image=%s\nversion=%s\nartifacts=%s\n' "$image" "$version" "$out"
wc -c "$out/rootfs-$arch.ext4.zst"
