#!/bin/bash
# The coverage of the code (specs.md §27): what share of each package's statements the unit tests run, what share the
# conformance scenarios run, and what share either of them runs. For a CI's log and, on GitHub Actions, the job's
# summary: a row per package, sorted by its path, then the total, and how long each part took. A measure, never a gate.
# Run from the root of the repository, with Go, as make cover does (GO names the go command, go by default):
#
#   bash test/ci/cover.sh
#
# Into bin/cover/, emptied first:
#   cover.out       the unit tests' profile: one of the whole module, each package's tests counting toward every package
#                   they reach (go test -coverpkg=./...), as make cover wrote it before the scenarios counted;
#   scenarios/      the counters of each run of a conformance build made with -cover -coverpkg=./..., which the runner
#                   gives each run as GOCOVERDIR (its --cover-dir), and scenarios.out, the profile go tool covdata
#                   textfmt makes of them;
#   merged.out      a block run when either ran it;
#   cover.html, merged.html   the code coloured by what ran, of the unit tests alone and of either.
#
# A block is the same in both builds, as each comes from its own file. The statements counted are those of the unit
# tests' profile, the code the delivered build holds, so all three columns share them and the unit tests' column stays
# what it was: a file only the conformance build holds (internal/disks/observer_conformance.go, the control file) is
# left out, and said; a file only the delivered build holds (internal/disks/observer_delivered.go) counts with what the
# unit tests ran, as no scenario can reach it. A block two test binaries both list counts once, run when either ran it,
# as go tool cover counts it. A package the conformance build does not hold has no scenarios' share ("–").
#
# The exit code is 0 when the unit tests and the scenarios all passed, 1 otherwise, after the table of what ran.
set -u

go=${GO:-go}
out=bin/cover
module=$("$go" list -m) || exit 1
rm -rf "$out" && mkdir -p "$out" || exit 1
work=$(mktemp -d) || exit 1
trap 'rm -rf "$work"' EXIT

failed=()
start=$SECONDS
echo "==== the unit tests"
"$go" test -coverpkg=./... -coverprofile="$out/cover.out" ./... || failed+=("the unit tests")
unit_time=$((SECONDS - start))

start=$SECONDS
echo "==== the scenarios, against a conformance build with -cover"
if "$go" build -tags conformance -cover -coverpkg=./... -o "$out/atoll-conformance" ./cmd/atoll; then
	"$go" run ./conformance/runner --bin "$out/atoll-conformance" --scenarios conformance/scenarios \
		--cover-dir "$out/scenarios" || failed+=("the scenarios")
	if [ -n "$(ls -A "$out/scenarios" 2>/dev/null)" ]; then
		"$go" tool covdata textfmt -i="$out/scenarios" -o "$out/scenarios.out" || failed+=("go tool covdata")
	fi
else
	failed+=("the conformance build")
fi
scenarios_time=$((SECONDS - start))

[ -s "$out/cover.out" ] || {
	echo "no profile of the unit tests at $out/cover.out" >&2
	exit 1
}
scenarios=$out/scenarios.out
[ -s "$scenarios" ] || scenarios=/dev/null

# A line per block, sorted: the block, its statements, whether the unit tests ran it (- for a block of the
# conformance build alone), whether the conformance build holds it, whether the scenarios ran it. A profile's line is
# github.com/…/internal/glob/glob.go:12.34,15.2 3 1: a block, its statements, how often it ran.
awk -v unitfile="$out/cover.out" '
	/^mode:/ { next }
	FILENAME == unitfile {
		statements[$1] = $2
		if ($3 > 0) unit[$1] = 1
		next
	}
	{
		held[$1] = $2
		if ($3 > 0) ran[$1] = 1
	}
	END {
		for (block in statements)
			printf "%s %d %d %d %d\n", block, statements[block], (block in unit), (block in held), (block in ran)
		for (block in held)
			if (!(block in statements)) printf "%s %d - 1 %d\n", block, held[block], (block in ran)
	}' "$out/cover.out" "$scenarios" | LC_ALL=C sort >"$work/blocks" || exit 1

{
	echo "mode: set"
	awk '$3 != "-" { print $1, $2, (($3 || $5) ? 1 : 0) }' "$work/blocks"
} >"$out/merged.out" || exit 1

# A line per package, sorted by its path: statements, those the unit tests ran, whether the conformance build holds
# the package, those the scenarios ran, those either ran.
table=$(awk -v module="$module/" '
	$3 == "-" { next }
	{
		pkg = $1
		sub(/:.*/, "", pkg)
		sub(/\/[^\/]*$/, "", pkg)
		sub("^" module, "", pkg)
		all[pkg] += $2
		if ($3) unit[pkg] += $2
		if ($4) held[pkg] = 1
		if ($5) ran[pkg] += $2
		if ($3 || $5) either[pkg] += $2
	}
	END {
		for (pkg in all) printf "%s %d %d %d %d %d\n", pkg, all[pkg], unit[pkg], (pkg in held), ran[pkg], either[pkg]
	}' "$work/blocks" | LC_ALL=C sort) || exit 1
total=$(awk '{ all += $2; unit += $3; held += $4; ran += $5; either += $6 } END { printf "%d %d %d %d %d", all, unit, held, ran, either }' <<<"$table")
# The statements of the conformance build alone, and their files.
left=$(awk '$3 == "-" { all += $2 } END { printf "%d", all }' "$work/blocks")
left_files=$(awk -v module="$module/" '$3 == "-" { f = $1; sub(/:.*/, "", f); sub("^" module, "", f); print f }' "$work/blocks" |
	LC_ALL=C sort -u | paste -sd, - | sed 's/,/, /g')

share() { awk -v all="$1" -v hit="$2" 'BEGIN { if (all == 0) print "–"; else printf "%.1f%%", 100 * hit / all }'; }
# The scenarios' share of a package the conformance build holds; – otherwise.
held_share() { if [ "$3" -eq 0 ]; then echo "–"; else share "$1" "$2"; fi; }
duration() { printf '%dm%02ds' $(($1 / 60)) $(($1 % 60)); }

echo
echo "Coverage by package: the share of its statements the unit tests run, the scenarios, and either (merged):"
printf '  %-36s %10s  %6s  %9s  %6s\n' "package" "statements" "unit" "scenarios" "merged"
while read -r pkg all unit held ran either; do
	printf '  %-36s %10d  %6s  %9s  %6s\n' "$pkg" "$all" "$(share "$all" "$unit")" "$(held_share "$all" "$ran" "$held")" "$(share "$all" "$either")"
done <<<"$table"
read -r all unit held ran either <<<"$total"
printf '  %-36s %10d  %6s  %9s  %6s\n' "total" "$all" "$(share "$all" "$unit")" "$(held_share "$all" "$ran" "$held")" "$(share "$all" "$either")"
if [ "$left" -gt 0 ]; then
	echo "Left out: $left statements only the conformance build holds, in $left_files."
fi
echo "The unit tests took $(duration "$unit_time"), the scenarios $(duration "$scenarios_time"), their build included."
for part in "${failed[@]}"; do
	echo "FAILED: $part; the table counts what ran." >&2
done

"$go" tool cover -html="$out/cover.out" -o "$out/cover.html" || exit 1
"$go" tool cover -html="$out/merged.out" -o "$out/merged.html" || exit 1

if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
	{
		echo "### Coverage"
		echo
		echo "The share of each package's statements that the unit tests run (\`go test -coverpkg=./...\`, the measure"
		echo "before the scenarios counted), that the conformance scenarios run (a conformance build with \`-cover\`), and"
		echo "that either runs. The statements are those of the delivered build, which the unit tests build."
		if [ "$left" -gt 0 ]; then
			echo "The $left only the conformance build holds are left out: $left_files."
		fi
		echo "A package the conformance build does not hold has no scenarios' share (–). The integration and loop-device"
		echo "tests are not counted. The unit tests took $(duration "$unit_time"), the scenarios $(duration "$scenarios_time"),"
		echo "their build included. The profiles and their HTML are the run's artifact."
		echo
		for part in "${failed[@]}"; do
			echo "**Failed: $part**; the table counts what ran."
			echo
		done
		echo '| Package | Statements | Unit tests | Scenarios | Merged |'
		echo '|---|---:|---:|---:|---:|'
		while read -r pkg all unit held ran either; do
			echo "| \`$pkg\` | $all | $(share "$all" "$unit") | $(held_share "$all" "$ran" "$held") | $(share "$all" "$either") |"
		done <<<"$table"
		read -r all unit held ran either <<<"$total"
		echo "| **Total** | **$all** | **$(share "$all" "$unit")** | **$(held_share "$all" "$ran" "$held")** | **$(share "$all" "$either")** |"
	} >>"$GITHUB_STEP_SUMMARY"
fi

[ ${#failed[@]} -eq 0 ]
