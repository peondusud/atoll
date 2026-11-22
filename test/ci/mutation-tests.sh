#!/bin/bash
# The map of which tests of a package reach each of its lines, for test/ci/mutation-go.sh: each test of the package run
# alone, with one CPU and the coverage of the package, its profile read block by block. One line of the map a line of
# code that some test reaches: "<file>:<line> <test>|<test>...", the file from the root of the module. Beside it,
# <map>.times holds each test, its exit code and how long it took, in milliseconds. Run from the root of the module:
#
#   bash test/ci/mutation-tests.sh ./internal/jobs bin/mutation/internal-jobs.tests [parallel]
#
# The exit code is 0 when every test passed alone, 1 otherwise, as a test that fails alone would kill every mutant it
# is chosen for; 2 for a wrong argument.
set -u

[ $# -ge 2 ] || {
	echo "usage: $0 <package, such as ./internal/jobs> <map to write> [tests at once]" >&2
	exit 2
}
pkg=${1#./}
map=$2
parallel=${3:-$(nproc)}
[ -d "$pkg" ] || {
	echo "$pkg: no such folder" >&2
	exit 2
}
module=$(go list -m) || exit 1
work=$(mktemp -d) || exit 1
trap 'rm -rf "$work"' EXIT
# Built alone: the flags of a run, -count among them, are no build's (test/ci/mutation.sh sets them in GOFLAGS).
GOFLAGS='' go test -c -cover -covermode=set -o "$work/pkg.test" "./$pkg" || exit 1
# The tests as go test runs them: from the package's folder. A listing writes no coverage, and says so on stderr.
(cd "$pkg" && "$work/pkg.test" -test.list '.*' 2>/dev/null) | grep -E '^(Test|Fuzz|Example)' >"$work/tests" || exit 1
mkdir -p "$work/cov"
export work pkg
# One test alone: its profile, and a line of times. xargs runs it, through bash -c.
# shellcheck disable=SC2329
one() {
	start=$(date +%s%N)
	(cd "$pkg" && "$work/pkg.test" -test.run "^$1\$" -test.count=1 -test.cpu=1 -test.coverprofile="$work/cov/$1" >"$work/cov/$1.out" 2>&1)
	status=$?
	echo "$1 $status $((($(date +%s%N) - start) / 1000000))"
}
export -f one
xargs -P "$parallel" -I{} bash -c 'one {}' <"$work/tests" | sort >"$map.times"
# Each block a test ran, line by line; then the tests of each line together.
while read -r name; do
	awk -v name="$name" -v module="$module/" 'NR > 1 && $NF > 0 {
		split($1, place, ":"); file = place[1]; sub("^" module, "", file)
		split(place[2], span, ","); split(span[1], from, "."); split(span[2], to, ".")
		for (l = from[1]; l <= to[1]; l++) print file ":" l, name
	}' "$work/cov/$name"
done <"$work/tests" | LC_ALL=C sort -u |
	awk '$1 == key { list = list "|" $2; next } { if (key != "") print key, list; key = $1; list = $2 } END { if (key != "") print key, list }' >"$map"
failed=$(awk '$2 != 0 { print $1 }' "$map.times")
echo "$(wc -l <"$work/tests") tests, $(wc -l <"$map") lines reached; $(awk '{ ms += $3 } END { printf "%d", ms / 1000 }' "$map.times") s of tests in all"
if [ -n "$failed" ]; then
	echo "failed alone: $failed" >&2
	exit 1
fi
exit 0
