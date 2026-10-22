#!/usr/bin/env bash
# One-time setup of the self-hosted GitHub Actions runner that runs
# .github/workflows/hardware.yml on a bare-metal Linux machine with KVM.
#
# From the Mac (the registration token is valid for one hour):
#   scp scripts/setup-hardware-runner.sh <host>:/tmp/
#   ssh -t <host> sudo bash /tmp/setup-hardware-runner.sh \
#     "$(gh api -X POST repos/Sukhendu2002/boxpool/actions/runners/registration-token --jq .token)"
#
# Jobs run as a dedicated user that is only in the kvm group: no sudo, no
# docker, and no access to other users' home directories.
set -euo pipefail

TOKEN=${1:?usage: sudo bash $0 <registration-token>}
REPO_URL=https://github.com/Sukhendu2002/boxpool
VERSION=2.338.1
SHA256=08e9ccb3604d2462c24b5e33604dc30810296ca15cc65f23cffd4317eb144c37 # linux-x64, from the release notes
RUNNER_USER=boxpool-ci
DIR=/opt/boxpool-runner

[ "$(id -u)" = 0 ] || { echo "run with sudo" >&2; exit 1; }
[ "$(uname -m)" = x86_64 ] || { echo "only the x86_64 runner is pinned here" >&2; exit 1; }
[ -c /dev/kvm ] || { echo "/dev/kvm missing; this machine cannot run the hardware jobs" >&2; exit 1; }

id "$RUNNER_USER" >/dev/null 2>&1 || useradd --create-home --shell /bin/bash "$RUNNER_USER"
usermod -aG kvm "$RUNNER_USER"

tgz=$(mktemp)
trap 'rm -f "$tgz"' EXIT
curl -fsSL -o "$tgz" "https://github.com/actions/runner/releases/download/v$VERSION/actions-runner-linux-x64-$VERSION.tar.gz"
echo "$SHA256  $tgz" | sha256sum -c --quiet

mkdir -p "$DIR"
tar -xzf "$tgz" -C "$DIR"
chown -R "$RUNNER_USER:" "$DIR"
"$DIR/bin/installdependencies.sh" >/dev/null

cd "$DIR"
sudo -u "$RUNNER_USER" ./config.sh --unattended --replace \
	--url "$REPO_URL" --token "$TOKEN" \
	--name "$(hostname)" --labels kvm,bare-metal
./svc.sh install "$RUNNER_USER"
./svc.sh start
echo "runner '$(hostname)' installed; check $REPO_URL/settings/actions/runners"
