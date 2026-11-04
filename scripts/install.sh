#!/bin/sh
# Served verbatim at https://boxpool.run/install. No sudo or daemon startup.
set -eu

main() {
    case "$(uname -s)/$(uname -m)" in
        Linux/x86_64|Linux/amd64) platform=linux-amd64 ;;
        Linux/aarch64|Linux/arm64) platform=linux-arm64 ;;
        Darwin/arm64) platform=darwin-arm64 ;;
        *) echo 'boxpool: supported hosts are Linux amd64/arm64 and Apple silicon macOS' >&2; exit 1 ;;
    esac
    command -v curl >/dev/null 2>&1 || { echo 'boxpool: curl is required' >&2; exit 1; }
    if command -v sha256sum >/dev/null 2>&1; then
        hash_command=sha256sum
    elif command -v shasum >/dev/null 2>&1; then
        hash_command='shasum -a 256'
    else
        echo 'boxpool: sha256sum or shasum is required' >&2
        exit 1
    fi

    version=${BOXPOOL_VERSION:-latest}
    if [ "$version" != latest ]; then
        printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9_.-]+)?$' || {
            echo 'boxpool: BOXPOOL_VERSION must be a release tag such as v0.1.0' >&2; exit 1;
        }
    fi
    work=$(mktemp -d)
    staging=
    trap 'rm -rf "$work"; if [ -n "$staging" ]; then rm -f "$staging"; fi' EXIT
    trap 'exit 1' HUP INT TERM
    releases=https://github.com/Sukhendu2002/boxpool/releases
    if [ "$version" = latest ]; then
        manifest_url=$releases/latest/download/checksums.txt
    else
        manifest_url=$releases/download/$version/checksums.txt
    fi
    # Resolve latest once so a concurrent release cannot mix versions.
    effective_url=$(download "$manifest_url" "$work/checksums.txt" -w '%{url_effective}')
    if [ "$version" = latest ]; then
        # GitHub redirects the asset to its CDN. Resolve the release tag from
        # the manifest's redirect chain separately, without selecting latest again.
        version=$(sed -n 's|^[Ll]ocation: https://github.com/Sukhendu2002/boxpool/releases/download/\([^/]*\)/checksums.txt.*|\1|p' "$work/headers" | tr -d '\r')
        printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9_.-]+)?$' || {
            echo "boxpool: cannot resolve latest release from $effective_url; set BOXPOOL_VERSION" >&2; exit 1;
        }
    fi
    asset=boxpool-$platform
    expected=$(awk -v name="$asset" '$2 == name { print $1 }' "$work/checksums.txt")
    if [ "${#expected}" -ne 64 ] || ! printf '%s\n' "$expected" | grep -Eq '^[a-f0-9]{64}$'; then
        echo "boxpool: missing or invalid checksum for $asset" >&2; exit 1
    fi
    download "$releases/download/$version/$asset" "$work/boxpool"
    # Intentional splitting: the only values are sha256sum and shasum -a 256.
    actual=$($hash_command "$work/boxpool" | awk '{print $1}')
    [ "$actual" = "$expected" ] || { echo 'boxpool: checksum mismatch; nothing installed' >&2; exit 1; }

    if [ -n "${BOXPOOL_INSTALL_DIR:-}" ]; then
        install_dir=$BOXPOOL_INSTALL_DIR
    elif [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
        install_dir=/usr/local/bin
    else
        install_dir=$HOME/.local/bin
    fi
    mkdir -p "$install_dir"
    [ ! -d "$install_dir/boxpool" ] || { echo 'boxpool: destination is a directory' >&2; exit 1; }
    staging=$(mktemp "$install_dir/.boxpool.XXXXXX")
    cp "$work/boxpool" "$staging"
    chmod 755 "$staging"
    mv -f "$staging" "$install_dir/boxpool"
    staging=
    printf 'Installed boxpool %s to %s/boxpool\n' "$version" "$install_dir"
    case ":$PATH:" in
        *":$install_dir:"*) ;;
        *) printf 'Add %s to your PATH.\n' "$install_dir" ;;
    esac
    echo 'Run boxpool doctor to check host prerequisites. See https://boxpool.run for startup instructions.'
}

download() {
    url=$1 dest=$2
    shift 2
    curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' \
        --retry 3 --connect-timeout 15 --max-time 180 --dump-header "$work/headers" \
        "$@" --output "$dest" "$url"
}

# Keep execution at the end so a truncated piped download does not install.
main "$@"
