#!/usr/bin/env bash
# M1 gate (#19) timings on a Linux box with KVM, systemd and passwordless
# sudo. See docs/benchmarks/m1-gate.md.
#
# Usage: scripts/bench-gate.sh install <binary>  # sudo <binary> up, then the first shell prompt
#        scripts/bench-gate.sh boot [runs]        # cold boots: a new sandbox per run (default 10)
#        scripts/bench-gate.sh selftest           # check the duration parser
#
# boxpool sh has no one-shot command and needs a terminal, so each shell runs
# under script(1). The script waits for the shell's prompt, then types "exit"
# and removes the sandbox. --timing measures from sh's start to the shell's
# first output. Each run's transcript is kept in $LOG_DIR (default: a new
# temp dir, printed at the start), so you can see what that output was.
#
# Sandboxes are named gate-<random>-<n>, new for every invocation. The script
# stops if a name already exists, and removes only the sandboxes it created,
# also when it fails.
set -euo pipefail

PROMPT_RE=${PROMPT_RE:-'[#$] $'} # the end of bash's prompt, e.g. "root@host:/# "
PROMPT_TIMEOUT_S=60

# ms DURATION prints a Go time.Duration string (437ms, 1.2s, 1m2.5s) in whole
# ms, rounded. Anything else is an error.
ms() {
	local d=$1 expr=0 mul re='^([0-9]+(\.[0-9]+)?)(h|ms|m|s|us|µs|ns)([0-9].*)?$'
	while :; do
		[[ $d =~ $re ]] || {
			echo "bad duration: '$1'" >&2
			return 1
		}
		case ${BASH_REMATCH[3]} in
		h) mul=3600000 ;;
		m) mul=60000 ;;
		s) mul=1000 ;;
		ms) mul=1 ;;
		us | µs) mul=0.001 ;;
		ns) mul=0.000001 ;;
		esac
		expr+="+${BASH_REMATCH[1]}*$mul"
		d=${BASH_REMATCH[4]}
		[[ -n $d ]] || break
	done
	awk "BEGIN { printf \"%d\n\", $expr + 0.5 }"
}

selftest() {
	local t got
	for t in 437ms=437 1.2s=1200 1m2s=62000 2m=120000 1m2.5s=62500 1h0m0s=3600000 1.5ms=2 0s=0; do
		got=$(ms "${t%=*}") || got=error
		[[ $got == "${t#*=}" ]] || {
			echo "FAIL: ms ${t%=*} = $got, want ${t#*=}" >&2
			return 1
		}
		echo "ok: ${t%=*} = $got ms"
	done
	for t in '' 1 1x ms 1m2 1.s abc; do
		if ms "$t" >/dev/null 2>&1; then
			echo "FAIL: ms '$t' accepted" >&2
			return 1
		fi
		echo "ok: '$t' rejected"
	done
}

current="" # the sandbox this invocation created and has not removed yet
pid=""     # the running script(1) session, if any

cleanup() {
	[[ -z $pid ]] || kill "$pid" 2>/dev/null || true
	[[ -z $current ]] || sudo boxpool rm --force "$current" >/dev/null ||
		echo "warning: could not remove sandbox $current" >&2
}

# session NAME runs `sudo boxpool sh --timing --name NAME` for a sandbox
# that must not exist yet, types exit at its prompt, removes the sandbox, and
# sets ms_to_prompt.
session() {
	local name=$1 log=$LOG_DIR/$1.log fifo=$LOG_DIR/$1.in list rc=0 i v
	list=$(sudo boxpool ls --json) || {
		echo "boxpool ls failed" >&2
		return 1
	}
	if grep -qF "\"name\": \"$name\"" <<<"$list"; then
		echo "sandbox $name already exists; not touching it" >&2
		return 1
	fi
	current=$name
	mkfifo "$fifo"
	script -qfec "sudo boxpool sh --timing --name $name" "$log" <"$fifo" >/dev/null &
	pid=$!
	exec 3>"$fifo"
	rm "$fifo"
	for ((i = 0; i < PROMPT_TIMEOUT_S * 10; i++)); do
		tail -n 1 "$log" 2>/dev/null | grep -qE "$PROMPT_RE" && break
		kill -0 "$pid" 2>/dev/null || break
		sleep 0.1
	done
	if ! tail -n 1 "$log" 2>/dev/null | grep -qE "$PROMPT_RE"; then
		exec 3>&-
		echo "$name: no prompt matching '$PROMPT_RE' (sh ended, or ${PROMPT_TIMEOUT_S}s passed); transcript: $log" >&2
		return 1
	fi
	printf 'exit\n' >&3
	exec 3>&-
	wait "$pid" || rc=$?
	pid=""
	if ((rc != 0)); then
		echo "$name: boxpool sh exited $rc; transcript: $log" >&2
		return 1
	fi
	sudo boxpool rm --force "$name" >/dev/null || {
		echo "$name: boxpool rm --force failed" >&2
		return 1
	}
	current=""
	v=$(grep -ao 'time to prompt: [^[:space:]]*' "$log" | head -n 1 | cut -d' ' -f4)
	[[ -n $v ]] || {
		echo "$name: no timing in $log" >&2
		return 1
	}
	ms_to_prompt=$(ms "$v")
}

start_logs() {
	LOG_DIR=${LOG_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/bench-gate.XXXXXX")}
	mkdir -p "$LOG_DIR"
	echo "transcripts: $LOG_DIR" >&2
	prefix=gate-$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')
	trap cleanup EXIT
}

case ${1:-} in
install)
	start_logs
	t0=$(date +%s%N)
	sudo "${2:?usage: $0 install <binary>}" up
	t1=$(date +%s%N)
	session "$prefix-install"
	up=$(((t1 - t0) / 1000000))
	echo "up: $up ms, sh to prompt: $ms_to_prompt ms, total: $((up + ms_to_prompt)) ms"
	;;
boot)
	start_logs
	vals=()
	for i in $(seq "${2:-10}"); do
		session "$prefix-$i"
		vals+=("$ms_to_prompt")
		echo "$i $ms_to_prompt"
	done
	mapfile -t s < <(printf '%s\n' "${vals[@]}" | sort -n)
	n=${#s[@]}
	echo "median: $(((s[(n - 1) / 2] + s[n / 2]) / 2)) ms, p90: ${s[(9 * n + 9) / 10 - 1]} ms (nearest rank)"
	;;
selftest)
	selftest
	;;
*)
	sed -n '5,7p' "$0" >&2
	exit 2
	;;
esac
