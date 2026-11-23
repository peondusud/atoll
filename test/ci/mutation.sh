#!/bin/bash
# Mutation testing of Atoll's pure core with gremlins (specs.md §27): gremlins changes the code of a package one
# mutant at a time (a condition negated, a boundary moved, an operator swapped, a loop's break and continue swapped)
# and runs that package's own tests against each mutant the tests reach. A mutant they catch is killed; one that lives
# says that a test is missing, or that the change makes no difference, which test/ci/mutation-equivalents.txt then
# names and argues. Each package's score goes to the log and, on GitHub Actions, to the job's summary, with the
# mutants that lived and are not named; gremlins' log and JSON go to bin/mutation/. Then the gate
# (test/ci/mutation-gate.sh): a package of test/ci/gates.txt whose efficacy or mutator coverage falls under its gate
# fails the run. Run from the root of the repository, with Go:
#
#   bash test/ci/mutation.sh [PACKAGE ...]     # the pure core when no package is named
#   make mutation PKG=./internal/glob
#
# WORKERS mutants are tested at once, the CPUs by default, each test run with one CPU. A package of by_test runs, for
# each mutant, only the tests that reach the line it changed (test/ci/mutation-go.sh), from the coverage of each test
# alone, which test/ci/mutation-tests.sh reads first: gremlins runs a package's every test against each mutant, and
# jobs' take half an hour. MUTATION_FILES, an extended regexp, mutates only the files whose names, .go left out, it
# matches whole, the others left out as generated code is, or after a !, those it does not match: a part of a package too
# large for one run, the last part of which takes every file no other names, a new one included. MUTATION_LINES,
# "<from>-<to>" or "<from>-", leaves a package of by_test the mutants of those lines alone, those of the others said not
# viable at once: a part of a file too large for one job, test/ci/mutation-merge.sh making one score of the parts
# (.github/workflows/mutation-weekly.yml). The exit code is 0 when gremlins ran on every package and each met its gate, a
# package with no gate only measured; 1 when gremlins failed on one or one fell under its gate; 2 for a wrong argument.
set -u

# Released 2025-12-05 (Apache-2.0); a development tool, never a dependency of the module.
gremlins_version=v0.6.0
pure_core="./internal/glob ./internal/value ./internal/names ./internal/rules ./internal/config ./internal/integrity ./internal/solver"
# Every operator gremlins has, those it leaves off by default included.
operators="--invert-assignments --invert-bitwise --invert-bwassign --invert-logical --invert-loopctrl --remove-self-assignments"
# gremlins stops a mutant's go test past this many times its first run of the package's tests, and counts it timed
# out, in neither score. That first run takes its build from go's cache, where each mutant builds its package and the
# test binary again: at gremlins' own 3, a fast package on a loaded machine had a third of its mutants timed out,
# among them mutants the tests let live, so hidden from the score (glob: 59 timed out and 15 lived at 3, 15 and 22
# at 20, on 2026-11-21). At 20 a timed-out mutant is a loop the mutant made endless.
timeout_coefficient=20
# The packages whose mutants run only the tests that reach them, MUTATION_BY_TEST in place of the list. gremlins' first
# run is then the whole of their tests, and a mutant's a part of them: for jobs, whose tests take most of an hour with
# one CPU, twice that run is room enough, where 20 would leave a mutant that loops for good a worker for half a day.
# A package whose tests take less than building it needs more, MUTATION_BY_TEST_COEFFICIENT: glob, whose tests run in a
# second, had 110 of its 170 mutants timed out at 2, as a mutant's run builds the package again (2026-11-22).
by_test=${MUTATION_BY_TEST:-./internal/jobs}
by_test_coefficient=${MUTATION_BY_TEST_COEFFICIENT:-2}

if [ $# -eq 0 ]; then
	# One package a word, split on purpose.
	# shellcheck disable=SC2086
	set -- $pure_core
fi
if [ -n "${MUTATION_LINES:-}" ] && ! echo "$MUTATION_LINES" | grep -qxE '[1-9][0-9]*-([1-9][0-9]*)?'; then
	echo "MUTATION_LINES: want <from>-<to> or <from>-, such as 645-965, not $MUTATION_LINES" >&2
	exit 2
fi
for pkg in "$@"; do
	case $pkg in
	./*) ;;
	*)
		echo "$pkg: name a package by its folder, such as ./internal/glob" >&2
		exit 2
		;;
	esac
	[ -d "$pkg" ] || {
		echo "$pkg: no such folder" >&2
		exit 2
	}
done

out=bin/mutation
mkdir -p "$out" || exit 1
workers=${WORKERS:-$(nproc)}
GOBIN=$(pwd)/$out go install "github.com/go-gremlins/gremlins/cmd/gremlins@$gremlins_version" || exit 1
# Each test run with one CPU, through GOFLAGS, never gremlins' --test-cpu: v0.6.0 hands that to go test as the single
# argument "-cpu N", a flag go test does not know, which it passes with the package after it to a test binary of the
# folder it runs in, the module's root. Every run then failed before a test started, and every mutant was counted
# killed. -count=1 too: gremlins gives each mutant its timeout from its first run of the tests, and a result go test
# took from its cache, as for a package whose code and tests are those of a run before, timed out mutants the tests
# kill.
export GOFLAGS="${GOFLAGS:+$GOFLAGS }-cpu=1 -count=1"
# MUTATION_SKIP, a regexp of test names with no space, as go test's -skip reads it, leaves those tests out of every run,
# the map of a package of by_test included: jobs' sweeps of a whole pass, which reach most of its mutants and take more
# than half an hour of its tests, so that its weekly parts fit in a job each (.github/workflows/mutation-weekly.yml).
if [ -n "${MUTATION_SKIP:-}" ]; then
	GOFLAGS="$GOFLAGS -skip=$MUTATION_SKIP"
	export MUTATION_SKIP
fi

summary=${GITHUB_STEP_SUMMARY:-/dev/null}
{
	echo "### Mutation testing, gremlins $gremlins_version"
	echo
	echo "Efficacy: the mutants killed among those the tests reach, the mutants named equivalent"
	echo "(\`test/ci/mutation-equivalents.txt\`) left out. Coverage: the mutants the tests reach among all. Gate: the least"
	echo "of each (\`test/ci/gates.txt\`), – for a package with none."
	echo
	echo '| Package | Killed | Lived | Named | Not covered | Timed out | Not viable | Efficacy | Coverage | Gate | Time |'
	echo '|---|---:|---:|---:|---:|---:|---:|---:|---:|---|---:|'
} >>"$summary"

# A count of gremlins' last lines, in the log of the package.
count() { sed -n "s/.*$1: \([0-9]*\).*/\1/p" "$log" | head -1; }

worst=0
failed=()
: >"$out/lived.txt"
# The places and mutators named equivalent, as lived.txt writes them.
named=$(grep -v -e '^#' -e '^$' test/ci/mutation-equivalents.txt | cut -f1,2 | tr '\t' ' ')
for pkg in "$@"; do
	name=$(echo "${pkg#./}" | tr / -)
	log=$out/$name.log
	# Generated code (a table, a generator behind //go:build ignore) is not what the tests are for.
	excludes=()
	files=${MUTATION_FILES:-} others=
	case $files in
	'!'*) files=${files#!} others=1 ;;
	esac
	for f in "$pkg"/*.go; do
		base=$(basename "$f" .go)
		matched=
		if [ -n "$files" ] && echo "$base" | grep -qxE "$files"; then
			matched=1
		fi
		if grep -q -m1 -E '^// Code generated .* DO NOT EDIT\.$|^//go:build ignore$' "$f" ||
			{ [ -n "$files" ] && [ "$matched" = "$others" ]; }; then
			excludes+=(-E "$(basename "$f" | sed 's/\./\\./g')\$")
		fi
	done
	echo "==== $pkg"
	start=$(date +%s)
	coefficient=$timeout_coefficient
	shim=()
	case " $by_test " in
	*" $pkg "*)
		if ! bash test/ci/mutation-tests.sh "$pkg" "$out/$name.tests" "$workers"; then
			echo "| \`$pkg\` | the tests alone failed | | | | | | | | | |" >>"$summary"
			worst=1
			failed+=("$pkg: a test fails alone, which would kill every mutant it is run for")
			continue
		fi
		mkdir -p "$out/shim" || exit 1
		printf '#!/bin/bash\nexec bash %q "$@"\n' "$(pwd)/test/ci/mutation-go.sh" >"$out/shim/go" && chmod +x "$out/shim/go" || exit 1
		shim=(env "MUTATION_GO=$(command -v go)" "MUTATION_SRC=$(pwd)" "MUTATION_PKG=${pkg#./}" "MUTATION_MAP=$(pwd)/$out/$name.tests"
			"MUTATION_LOG=$(pwd)/$out/$name.runs" "PATH=$(pwd)/$out/shim:$PATH")
		: >"$out/$name.runs"
		coefficient=$by_test_coefficient
		;;
	esac
	# One operator a word, split on purpose.
	# shellcheck disable=SC2086
	${shim[@]+"${shim[@]}"} "$out/gremlins" unleash --workers "$workers" --timeout-coefficient "$coefficient" $operators \
		${excludes[@]+"${excludes[@]}"} -o "$out/$name.json" "$pkg" 2>&1 | sed 's/\x1b\[[0-9;]*m//g' | tee "$log"
	status=${PIPESTATUS[0]}
	spent=$(($(date +%s) - start))
	if [ "$status" != 0 ] || ! grep -q '^Killed: ' "$log"; then
		echo "| \`$pkg\` | gremlins failed, exit $status | | | | | | | | | ${spent}s |" >>"$summary"
		worst=1
		failed+=("$pkg: gremlins failed")
		continue
	fi
	# killed lived not-covered timed-out named efficacy coverage gate
	if figures=$(bash test/ci/mutation-gate.sh "$pkg" "$log"); then
		verdict=passes
	else
		verdict=FAILS
		worst=1
		failed+=("$pkg: under its gate, or its list of equivalents out of date")
	fi
	read -r killed lived uncovered timed named_here efficacy coverage gate <<<"$figures"
	if [ -z "${gate:-}" ]; then
		# The gate could not read the log: gremlins' own figures.
		killed=$(count Killed) lived=$(count Lived) uncovered=$(count 'Not covered') timed=$(count 'Timed out')
		named_here=? efficacy=? coverage=? gate=?
	fi
	case $gate:$verdict in
	-:passes) cell=– ;;
	-:*) cell="–, $verdict" ;;
	*) cell="$gate, $verdict" ;;
	esac
	# A part of a package says which: a | in a cell would split it.
	part=${MUTATION_FILES:+, $(echo "$MUTATION_FILES" | sed 's/^!/all but /; s/|/, /g')}${MUTATION_LINES:+, lines $MUTATION_LINES}
	echo "| \`$pkg\`$part | $killed | $lived | $named_here | $uncovered | $timed | $(count 'Not viable') | $efficacy% | $coverage% | $cell | ${spent}s |" >>"$summary"
	sed -n 's/^ *LIVED \([A-Z_]*\) at \(.*\)$/\2 \1/p' "$log" | sed "s|^|${pkg#./}/|" | grep -v -x -F -e "${named:-none}" |
		sort -t: -k1,1 -k2,2n -k3,3n >>"$out/lived.txt"
done

if [ -s "$out/lived.txt" ]; then
	{
		echo
		echo "#### The mutants that lived, not named equivalent"
		echo
		echo "Each a test to write, or a change that makes no difference, to name and argue in"
		echo "\`test/ci/mutation-equivalents.txt\`."
		echo
		echo '```'
		cat "$out/lived.txt"
		echo '```'
	} >>"$summary"
fi
for f in "${failed[@]}"; do
	echo "FAILED: $f" >&2
	echo "**Failed: $f**" >>"$summary"
done
exit "$worst"
