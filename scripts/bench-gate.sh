#!/usr/bin/env bash
# M1 gate (#19) timings on a Linux box with KVM, systemd and passwordless
# sudo. See docs/benchmarks/m1-gate.md.
#
# Usage: scripts/bench-gate.sh install <binary>  # sudo <binary> up, then the first shell prompt
#        scripts/bench-gate.sh boot [runs]        # cold boots: a new sandbox per run (default 10)
#
# boxpool sh has no one-shot command and needs a terminal, so each shell runs
# under script(1), is fed "exit", and its sandbox is removed afterwards.
# --timing measures from sh's start to the shell's first output.
set -euo pipefail

# prompt NAME prints the ms from `boxpool sh` starting to the new sandbox's
# first shell output, then removes the sandbox.
prompt() {
	local out
	out=$(printf 'exit\n' | script -qec "sudo boxpool sh --timing --name $1" /dev/null)
	sudo boxpool rm --force "$1" >/dev/null
	# Go durations: 873ms or 1.234s.
	awk '/time to prompt:/ { v = $4; sub(/\r$/, "", v)
		if (v ~ /ms$/) { sub(/ms$/, "", v); print int(v + 0.5) } else { sub(/s$/, "", v); print int(v * 1000 + 0.5) }
		found = 1 } END { exit !found }' <<<"$out" || { echo "no timing in: $out" >&2; return 1; }
}

case ${1:-} in
install)
	t0=$(date +%s%N)
	sudo "${2:?usage: $0 install <binary>}" up
	t1=$(date +%s%N)
	p=$(prompt gate-install)
	echo "up: $(((t1 - t0) / 1000000)) ms, sh to prompt: $p ms, total: $(((t1 - t0) / 1000000 + p)) ms"
	;;
boot)
	vals=()
	for i in $(seq "${2:-10}"); do
		vals+=("$(prompt "gate-$i")")
		echo "$i ${vals[-1]}"
	done
	mapfile -t s < <(printf '%s\n' "${vals[@]}" | sort -n)
	n=${#s[@]}
	echo "median: $(((s[(n - 1) / 2] + s[n / 2]) / 2)) ms, p90: ${s[(9 * n + 9) / 10 - 1]} ms (nearest rank)"
	;;
*)
	sed -n '5,6p' "$0" >&2
	exit 2
	;;
esac
