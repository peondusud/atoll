#!/bin/bash
# The coverage of the code (specs.md §27): what share of each package's statements the unit tests run, what share the
# conformance scenarios run, and what share either of them runs. For a CI's log and, on GitHub Actions, the job's
# summary: a row per package, sorted by its path, then the total, and how long each part took. Then the floor: the
# total share of the unit tests and the merged one, as the table prints them, each at least its gate in
# test/ci/gates.txt ("cover unit" and "cover merged"). Run from the root of the repository, with Go, as make cover does
# (GO names the go command, go by default):
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
#   cover.html, merged.html   the code coloured by what ran, of the unit tests alone and of either;
#   qemu.out, all.out, all.html   with QEMU_COVERDIR (below): the loop-device tests' profile, and a block run when any
#                   of the three ran it.
#
# A block is the same in both builds, as each comes from its own file. The statements counted are those of the unit
# tests' profile, the code the delivered build holds, so all three columns share them and the unit tests' column stays
# what it was: a file only the conformance build holds (internal/disks/observer_conformance.go, the control file) is
# left out, and said; a file only the delivered build holds (internal/disks/observer_delivered.go) counts with what the
# unit tests ran, as no scenario can reach it. A block two test binaries both list counts once, run when either ran it,
# as go tool cover counts it. A package the conformance build does not hold has no scenarios' share ("–").
#
# With QEMU_COVERDIR, a folder of the counters test/loopdev's run in qemu gathered (make test-qemu-cover, into
# bin/qemu-cover/, a folder for each kernel), as make cover-qemu gives it: their profile too, qemu.out, which go tool
# covdata textfmt makes of every folder below it that holds counters, those of the loop-device tests' own process, of
# the children they start, and of each atoll the units start under systemd; a column of what they ran ("qemu"), and one
# of what any of the three ran ("all"), with all.out and all.html. The floors stay those of the unit tests and of the
# merged share, which the loop-device tests do not count toward: without QEMU_COVERDIR the table is as it was.
#
# The exit code is 0 when the unit tests and the scenarios all passed and both totals meet their floor, 1 otherwise,
# after the table of what ran.
set -u

go=${GO:-go}
out=bin/cover
qemu_dir=${QEMU_COVERDIR:-}
if [ -n "$qemu_dir" ] && [ -z "$(find "$qemu_dir" -name 'covmeta.*' -print -quit 2>/dev/null)" ]; then
	echo "no counters below $qemu_dir: make test-qemu-cover gathers them" >&2
	exit 1
fi
module=$("$go" list -m) || exit 1
rm -rf "$out" && mkdir -p "$out" || exit 1
work=$(mktemp -d) || exit 1
trap 'rm -rf "$work"' EXIT

failed=()
start=$SECONDS
echo "==== the unit tests"
# 45 minutes, as the race detector has: jobs' tests run past go test's 10 under -coverpkg on a loaded machine.
"$go" test -timeout 45m -coverpkg=./... -coverprofile="$out/cover.out" ./... || failed+=("the unit tests")
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
# The loop-device tests' counters, from every folder below QEMU_COVERDIR that holds some; their profile, or none.
qemu=/dev/null
if [ -n "$qemu_dir" ]; then
	echo "==== the loop-device tests in qemu, from $qemu_dir"
	dirs=$(find "$qemu_dir" -name 'covmeta.*' -exec dirname {} \; | LC_ALL=C sort -u | paste -sd, -)
	if "$go" tool covdata textfmt -i="$dirs" -o "$out/qemu.out" && [ -s "$out/qemu.out" ]; then
		qemu=$out/qemu.out
	else
		failed+=("go tool covdata of the loop-device tests")
	fi
fi

# A line per block, sorted: the block, its statements, whether the unit tests ran it (- for a block of the
# conformance build or of qemu's alone), whether the conformance build holds it, whether the scenarios ran it, whether
# the builds of qemu hold it, whether the loop-device tests ran it. A profile's line is
# github.com/…/internal/glob/glob.go:12.34,15.2 3 1: a block, its statements, how often it ran.
awk -v unitfile="$out/cover.out" -v qemufile="$qemu" '
	/^mode:/ { next }
	FILENAME == unitfile {
		statements[$1] = $2
		if ($3 > 0) unit[$1] = 1
		next
	}
	FILENAME == qemufile {
		qheld[$1] = $2
		if ($3 > 0) qran[$1] = 1
		next
	}
	{
		held[$1] = $2
		if ($3 > 0) ran[$1] = 1
	}
	END {
		for (block in statements)
			printf "%s %d %d %d %d %d %d\n", block, statements[block], (block in unit), (block in held), (block in ran),
				(block in qheld), (block in qran)
		for (block in held)
			if (!(block in statements)) printf "%s %d - 1 %d %d %d\n", block, held[block], (block in ran), (block in qheld), (block in qran)
		for (block in qheld)
			if (!(block in statements) && !(block in held)) printf "%s %d - 0 0 1 %d\n", block, qheld[block], (block in qran)
	}' "$out/cover.out" "$scenarios" "$qemu" | LC_ALL=C sort >"$work/blocks" || exit 1

{
	echo "mode: set"
	awk '$3 != "-" { print $1, $2, (($3 || $5) ? 1 : 0) }' "$work/blocks"
} >"$out/merged.out" || exit 1
if [ "$qemu" != /dev/null ]; then
	{
		echo "mode: set"
		awk '$3 != "-" { print $1, $2, (($3 || $5 || $7) ? 1 : 0) }' "$work/blocks"
	} >"$out/all.out" || exit 1
fi

# A line per package, sorted by its path: statements, those the unit tests ran, whether the conformance build holds
# the package, those the scenarios ran, those either ran; whether the builds of qemu hold it, those the loop-device
# tests ran, those any of the three ran.
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
		if ($6) qheld[pkg] = 1
		if ($7) qran[pkg] += $2
		if ($3 || $5 || $7) any[pkg] += $2
	}
	END {
		for (pkg in all)
			printf "%s %d %d %d %d %d %d %d %d\n", pkg, all[pkg], unit[pkg], (pkg in held), ran[pkg], either[pkg],
				(pkg in qheld), qran[pkg], any[pkg]
	}' "$work/blocks" | LC_ALL=C sort) || exit 1
total=$(awk '{ all += $2; unit += $3; held += $4; ran += $5; either += $6; qheld += $7; qran += $8; any += $9 }
	END { printf "%d %d %d %d %d %d %d %d", all, unit, held, ran, either, qheld, qran, any }' <<<"$table")
# The statements of the conformance build alone, or of qemu's, and their files.
left=$(awk '$3 == "-" { all += $2 } END { printf "%d", all }' "$work/blocks")
left_files=$(awk -v module="$module/" '$3 == "-" { f = $1; sub(/:.*/, "", f); sub("^" module, "", f); print f }' "$work/blocks" |
	LC_ALL=C sort -u | paste -sd, - | sed 's/,/, /g')

share() { awk -v all="$1" -v hit="$2" 'BEGIN { if (all == 0) print "–"; else printf "%.1f%%", 100 * hit / all }'; }
# The scenarios' share of a package the conformance build holds, or the loop-device tests' of one qemu's builds hold;
# – otherwise.
held_share() { if [ "$3" -eq 0 ]; then echo "–"; else share "$1" "$2"; fi; }
duration() { printf '%dm%02ds' $(($1 / 60)) $(($1 % 60)); }

echo
if [ "$qemu" = /dev/null ]; then
	echo "Coverage by package: the share of its statements the unit tests run, the scenarios, and either (merged):"
	printf '  %-36s %10s  %6s  %9s  %6s\n' "package" "statements" "unit" "scenarios" "merged"
	while read -r pkg all unit held ran either qheld qran any; do
		printf '  %-36s %10d  %6s  %9s  %6s\n' "$pkg" "$all" "$(share "$all" "$unit")" "$(held_share "$all" "$ran" "$held")" "$(share "$all" "$either")"
	done <<<"$table"
	read -r all unit held ran either qheld qran any <<<"$total"
	printf '  %-36s %10d  %6s  %9s  %6s\n' "total" "$all" "$(share "$all" "$unit")" "$(held_share "$all" "$ran" "$held")" "$(share "$all" "$either")"
else
	echo "Coverage by package: the share of its statements the unit tests run, the scenarios, either (merged), the"
	echo "loop-device tests in qemu, and any of the three (all):"
	printf '  %-36s %10s  %6s  %9s  %6s  %6s  %6s\n' "package" "statements" "unit" "scenarios" "merged" "qemu" "all"
	while read -r pkg all unit held ran either qheld qran any; do
		printf '  %-36s %10d  %6s  %9s  %6s  %6s  %6s\n' "$pkg" "$all" "$(share "$all" "$unit")" "$(held_share "$all" "$ran" "$held")" \
			"$(share "$all" "$either")" "$(held_share "$all" "$qran" "$qheld")" "$(share "$all" "$any")"
	done <<<"$table"
	read -r all unit held ran either qheld qran any <<<"$total"
	printf '  %-36s %10d  %6s  %9s  %6s  %6s  %6s\n' "total" "$all" "$(share "$all" "$unit")" "$(held_share "$all" "$ran" "$held")" \
		"$(share "$all" "$either")" "$(held_share "$all" "$qran" "$qheld")" "$(share "$all" "$any")"
fi
if [ "$left" -gt 0 ] && [ "$qemu" = /dev/null ]; then
	echo "Left out: $left statements only the conformance build holds, in $left_files."
elif [ "$left" -gt 0 ]; then
	echo "Left out: $left statements only the conformance build or the builds of qemu hold, in $left_files."
fi
echo "The unit tests took $(duration "$unit_time"), the scenarios $(duration "$scenarios_time"), their build included."

# The floor, each total as the table prints it, to a tenth of a percent.
floors=()
under=()
for part in unit merged; do
	gate=$(awk -v part="$part" '$1 == "cover" && $2 == part { print $3 }' "$(dirname "$0")/gates.txt")
	if [ "$part" = unit ]; then hit=$unit; else hit=$either; fi
	got=$(share "$all" "$hit")
	if [ -z "$gate" ]; then
		under+=("no floor for the $part share in test/ci/gates.txt")
	elif awk -v got="${got%\%}" -v gate="$gate" 'BEGIN { exit !(got + 0 >= gate + 0) }'; then
		floors+=("the $part share $got, its floor $gate%: passes")
	else
		floors+=("the $part share $got, its floor $gate%: FAILS")
		under+=("the $part share, $got, under its floor of $gate%")
	fi
done
for f in "${floors[@]}"; do
	echo "Floor: $f."
done
for part in "${failed[@]}"; do
	echo "FAILED: $part; the table counts what ran." >&2
done
for part in "${under[@]}"; do
	echo "FAILED: $part." >&2
done

"$go" tool cover -html="$out/cover.out" -o "$out/cover.html" || exit 1
"$go" tool cover -html="$out/merged.out" -o "$out/merged.html" || exit 1
if [ "$qemu" != /dev/null ]; then
	"$go" tool cover -html="$out/all.out" -o "$out/all.html" || exit 1
fi

if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
	{
		echo "### Coverage"
		echo
		echo "The share of each package's statements that the unit tests run (\`go test -coverpkg=./...\`, the measure"
		echo "before the scenarios counted), that the conformance scenarios run (a conformance build with \`-cover\`), and"
		echo "that either runs. The statements are those of the delivered build, which the unit tests build."
		if [ "$left" -gt 0 ] && [ "$qemu" = /dev/null ]; then
			echo "The $left only the conformance build holds are left out: $left_files."
		elif [ "$left" -gt 0 ]; then
			echo "The $left only the conformance build or the builds of qemu hold are left out: $left_files."
		fi
		if [ "$qemu" = /dev/null ]; then
			echo "A package the conformance build does not hold has no scenarios' share (–). The integration and loop-device"
			echo "tests are not counted. The unit tests took $(duration "$unit_time"), the scenarios $(duration "$scenarios_time"),"
			echo "their build included. The profiles and their HTML are the run's artifact."
		else
			echo "A package the conformance build does not hold has no scenarios' share (–), nor one qemu's builds do not hold"
			echo "a loop-device tests' share. The loop-device tests in qemu count apart (\`qemu\`), and with the others in"
			echo "\`all\`, which has no floor; the integration tests are not counted. The unit tests took"
			echo "$(duration "$unit_time"), the scenarios $(duration "$scenarios_time"), their build included."
		fi
		echo
		for part in "${failed[@]}"; do
			echo "**Failed: $part**; the table counts what ran."
			echo
		done
		if [ "$qemu" = /dev/null ]; then
			echo '| Package | Statements | Unit tests | Scenarios | Merged |'
			echo '|---|---:|---:|---:|---:|'
		else
			echo '| Package | Statements | Unit tests | Scenarios | Merged | qemu | All |'
			echo '|---|---:|---:|---:|---:|---:|---:|'
		fi
		while read -r pkg all unit held ran either qheld qran any; do
			row="| \`$pkg\` | $all | $(share "$all" "$unit") | $(held_share "$all" "$ran" "$held") | $(share "$all" "$either") |"
			[ "$qemu" = /dev/null ] || row="$row $(held_share "$all" "$qran" "$qheld") | $(share "$all" "$any") |"
			echo "$row"
		done <<<"$table"
		read -r all unit held ran either qheld qran any <<<"$total"
		row="| **Total** | **$all** | **$(share "$all" "$unit")** | **$(held_share "$all" "$ran" "$held")** | **$(share "$all" "$either")** |"
		[ "$qemu" = /dev/null ] || row="$row **$(held_share "$all" "$qran" "$qheld")** | **$(share "$all" "$any")** |"
		echo "$row"
		echo
		echo "The floor (\`test/ci/gates.txt\`), each total as the table prints it:"
		echo
		for f in "${floors[@]}"; do
			echo "- $f"
		done
		for part in "${under[@]}"; do
			echo "- **Failed: $part**"
		done
	} >>"$GITHUB_STEP_SUMMARY"
fi

[ ${#failed[@]} -eq 0 ] && [ ${#under[@]} -eq 0 ]
