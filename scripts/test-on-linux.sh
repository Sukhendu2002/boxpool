#!/usr/bin/env bash
# Run the Linux checks on your own Linux+KVM machine over SSH, using the
# working tree as it is now (uncommitted changes included). Nothing on
# GitHub can reach that machine; only you, over SSH.
#
# Usage: BOXPOOL_LINUX_HOST=<ssh host> scripts/test-on-linux.sh
# The remote needs rsync, Go (on PATH or in ~/.local/go) and access to /dev/kvm.
set -euo pipefail

HOST=${BOXPOOL_LINUX_HOST:?set BOXPOOL_LINUX_HOST to the SSH host of your Linux machine with KVM}
DIR=.cache/boxpool-linux-test # relative to the remote home

cd "$(git rev-parse --show-toplevel)"
rsync -az --delete --exclude .git --exclude /bin "./" "$HOST:$DIR/"
# shellcheck disable=SC2087 # expand $DIR locally on purpose
ssh "$HOST" bash -se <<REMOTE
set -euo pipefail
export PATH=\$HOME/.local/go/bin:\$PATH
cd "$DIR"
echo "== \$(hostname): \$(uname -srm), \$(go version)"
go vet ./...
go test ./...
go build -o bin/boxpool ./cmd/boxpool
out=\$(./bin/boxpool doctor)
echo "\$out"
grep -Fxq 'Backend: firecracker' <<<"\$out"
echo "== linux checks passed"
REMOTE
