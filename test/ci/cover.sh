#!/bin/bash
# The coverage of the unit tests (specs.md §27): what share of each package's statements some test of the module runs,
# from one profile where each package's tests count toward every package they reach (go test -coverpkg=./...). For a
# CI's log and, on GitHub Actions, the job's summary: a row per package, sorted by its path, then the total. A
# measure, never a gate. Run from the root of the repository, with Go, on a profile make cover wrote:
#
#   bash test/ci/cover.sh bin/cover/cover.out
#
# It also writes bin/cover/cover.html, the profile's code coloured by what ran, beside the profile. A block that two
# test binaries both list counts once, covered when either ran it, as go tool cover counts it.
set -u

profile=${1:?a coverage profile, such as bin/cover/cover.out}
[ -s "$profile" ] || {
	echo "no profile at $profile" >&2
	exit 2
}
module=$(go list -m) || exit 1

table=$(awk -v module="$module/" '
	NR == 1 { next }
	{
		# github.com/…/internal/glob/glob.go:12.34,15.2 3 1: a block, its statements, how often it ran.
		statements[$1] = $2
		if ($3 > 0) ran[$1] = 1
	}
	END {
		for (block in statements) {
			pkg = block
			sub(/:.*/, "", pkg)
			sub(/\/[^\/]*$/, "", pkg)
			sub("^" module, "", pkg)
			all[pkg] += statements[block]
			if (block in ran) hit[pkg] += statements[block]
		}
		for (pkg in all) printf "%s %d %d\n", pkg, all[pkg], hit[pkg]
	}' "$profile" | LC_ALL=C sort) || exit 1

total=$(awk '{ all += $2; hit += $3 } END { printf "%d %d", all, hit }' <<<"$table")
share() { awk -v all="$1" -v hit="$2" 'BEGIN { if (all == 0) print "–"; else printf "%.1f%%", 100 * hit / all }'; }

echo "Coverage of the unit tests, by package (statements run of all):"
while read -r pkg all hit; do
	printf '  %-36s %6d of %6d  %s\n' "$pkg" "$hit" "$all" "$(share "$all" "$hit")"
done <<<"$table"
read -r all hit <<<"$total"
printf '  %-36s %6d of %6d  %s\n' "total" "$hit" "$all" "$(share "$all" "$hit")"

go tool cover -html="$profile" -o "$(dirname "$profile")/cover.html" || exit 1

if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
	{
		echo "### Coverage of the unit tests"
		echo
		echo "Statements that some test of the module runs (\`go test -coverpkg=./...\`), by package; the integration"
		echo "tests, the scenarios and the loop-device tests are not counted. The profile and its HTML are the run's artifact."
		echo
		echo '| Package | Statements | Run | Share |'
		echo '|---|---:|---:|---:|'
		while read -r pkg all hit; do
			echo "| \`$pkg\` | $all | $hit | $(share "$all" "$hit") |"
		done <<<"$table"
		read -r all hit <<<"$total"
		echo "| **Total** | **$all** | **$hit** | **$(share "$all" "$hit")** |"
	} >>"$GITHUB_STEP_SUMMARY"
fi
