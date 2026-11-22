#!/bin/bash
# The throughput of Atoll's hashing on this machine, for a CI's log (internal/integrity/bench_test.go): which code
# zeebo/blake3 takes on this CPU, what the CPU has of AVX-512 or SVE2, then three runs of each benchmark. On GitHub
# Actions, the same goes to the job's summary. Run from the root of the repository, with Go:
#
#   bash test/ci/blake3.sh
set -u

case $(uname -m) in
x86_64) feature=avx512f ;;
aarch64) feature=sve2 ;;
*) feature="" ;;
esac
has="this CPU has no $feature"
[ -n "$feature" ] && grep -q -m1 -w "$feature" /proc/cpuinfo && has="this CPU has $feature"
model=$(lscpu 2>/dev/null | sed -n 's/^Model name: *//p' | head -1)
[ -n "$model" ] || model=$(sed -n 's/^model name[[:space:]]*: //p' /proc/cpuinfo | head -1)
echo "$(uname -m), ${model:-a CPU with no name}, $(nproc) CPUs; ${feature:+$has}"

out=$(go test -run '^$' -bench . -benchtime 2s -count 3 ./internal/integrity 2>&1)
status=$?
echo "$out"

if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
	{
		echo "### BLAKE3 on $(uname -m)"
		echo
		echo "${model:-A CPU with no name}, $(nproc) CPUs${feature:+; $has}."
		grep -m1 -o 'BLAKE3 backend: .*' <<<"$out" | sed 's/^/zeebo\/blake3 takes /; s/BLAKE3 backend: //; s/$/./'
		echo
		echo '| Benchmark | MB/s, each run |'
		echo '|---|---|'
		awk '/^Benchmark/ { name = $1; sub(/-[0-9]+$/, "", name); for (i = 1; i < NF; i++) if ($(i + 1) == "MB/s") runs[name] = runs[name] (runs[name] ? ", " : "") $i }
			END { for (n in runs) print "| " n " | " runs[n] " |" }' <<<"$out" | sort
	} >>"$GITHUB_STEP_SUMMARY"
fi
exit "$status"
