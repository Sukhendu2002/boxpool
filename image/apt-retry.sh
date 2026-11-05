#!/bin/sh
# Build helper: callers bound the whole operation with GNU timeout.
set -eu
retry() {
    attempt=1
    while :; do
        if "$@"; then return 0; else status=$?; fi
        if [ "$attempt" -ge 3 ]; then return "$status"; fi
        echo "APT download attempt $attempt failed; retrying cached downloads" >&2
        sleep "$((attempt * 10))"
        attempt=$((attempt + 1))
    done
}
operation=${1:-}
case "$operation" in
    update)
        shift
        test "$#" -eq 0
        retry apt-get -o APT::Update::Error-Mode=any update
        ;;
    install)
        shift
        test "$#" -gt 0
        # Keep successful archives between attempts. Do not retry dpkg or any
        # package maintainer scripts; install exactly once after every verified
        # archive has arrived, with network downloads disabled.
        retry apt-get install --download-only -y --no-install-recommends "$@"
        exec apt-get install --no-download -y --no-install-recommends "$@"
        ;;
    *) echo 'usage: apt-retry.sh update | install <packages...>' >&2; exit 2 ;;
esac
