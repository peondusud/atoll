#!/bin/bash
# Atoll's fuzz targets (specs.md §27), each for a while: go test runs one target of one package at a time, from its
# seeds and what earlier runs kept in the build cache, and writes an input that fails to the package's
# testdata/fuzz/<target>/, which this copies to bin/fuzz/ for a CI's artifact; committed there, it becomes a case of go
# test ./... For a CI's log and, on GitHub Actions, the job's summary. Run from the root of the repository, with Go:
#
#   bash test/ci/fuzz.sh --list               # every target, as the JSON of a matrix: {"include": [{"package", "target"}]}
#   bash test/ci/fuzz.sh [PACKAGE TARGET]     # that target, or every one in turn
#   make fuzz FUZZTIME=10s
#
# FUZZTIME is each target's time, as -fuzztime takes it, 1m by default. The exit code is 0 when no target found a
# failing input, 1 when one did or could not run, 2 for a wrong argument.
set -u

# Each target, "package target" a line, sorted: the fuzz functions of the test files of the module's packages.
targets() {
	grep -rE --include='*_test.go' '^func Fuzz[A-Za-z0-9_]*\(f \*testing\.F\)' cmd conformance internal 2>/dev/null |
		sed -E 's#^(.*)/[^/]*_test\.go:func (Fuzz[A-Za-z0-9_]*)\(.*#./\1 \2#' | LC_ALL=C sort -u
}

case ${1:-} in
--list)
	printf '{"include":['
	sep=""
	while read -r pkg target; do
		printf '%s{"package":"%s","target":"%s"}' "$sep" "$pkg" "$target"
		sep=,
	done < <(targets)
	printf ']}\n'
	exit 0
	;;
-*)
	echo "unknown option $1" >&2
	exit 2
	;;
esac
if [ $# -ne 0 ] && [ $# -ne 2 ]; then
	echo "usage: $0 [--list | PACKAGE TARGET]" >&2
	exit 2
fi
if [ $# -eq 2 ]; then
	targets | grep -qxF "$1 $2" || {
		echo "no fuzz target $2 in $1; the targets are:" >&2
		targets >&2
		exit 2
	}
	list="$1 $2"
else
	list=$(targets)
fi

fuzztime=${FUZZTIME:-1m}
mkdir -p bin/fuzz || exit 1
worst=0 rows=""
while read -r pkg target; do
	corpus=$pkg/testdata/fuzz/$target
	before=$(ls "$corpus" 2>/dev/null)
	echo "==== $pkg $target, $fuzztime"
	go test -run '^$' -fuzz "^$target\$" -fuzztime "$fuzztime" "$pkg"
	status=$?
	row="| \`$pkg\` | \`$target\` | $fuzztime |"
	if [ "$status" = 0 ]; then
		rows+="$row nothing found |"$'\n'
		continue
	fi
	worst=1
	found=$(comm -13 <(echo "$before") <(ls "$corpus" 2>/dev/null) | sed '/^$/d')
	if [ -z "$found" ]; then
		rows+="$row **failed**, exit $status, no input kept: see the log |"$'\n'
		continue
	fi
	keep=bin/fuzz/$(echo "${pkg#./}" | tr / -)/$target
	mkdir -p "$keep"
	while read -r name; do
		cp "$corpus/$name" "$keep/"
	done <<<"$found"
	# Markdown's backticks around each name, no command.
	# shellcheck disable=SC2016
	rows+="$row **failing input** $(echo "$found" | sed 's/.*/`&`/' | paste -sd' ') |"$'\n'
done <<<"$list"

if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
	{
		echo "### Fuzzing"
		echo
		echo "A failing input is in the run's artifact, and in the log with the command that runs it again; copied to"
		echo "the package's \`testdata/fuzz/<target>/\` and committed, it becomes a case of \`go test ./...\`."
		echo
		echo '| Package | Target | Time | Result |'
		echo '|---|---|---|---|'
		printf '%s' "$rows"
	} >>"$GITHUB_STEP_SUMMARY"
fi
exit "$worst"
