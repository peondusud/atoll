#!/bin/bash
# Mutation testing of Atoll's pure core with gremlins (specs.md §27): gremlins changes the code of a package one
# mutant at a time (a condition negated, a boundary moved, an operator swapped, a loop's break and continue swapped)
# and runs that package's own tests against each mutant the tests reach. A mutant they catch is killed; one that lives
# says that a test is missing, or that the change makes no difference. Each package's score goes to the log and, on
# GitHub Actions, to the job's summary, with the mutants that lived; gremlins' log and JSON go to bin/mutation/. No
# threshold yet: the score is measured, not enforced. Run from the root of the repository, with Go:
#
#   bash test/ci/mutation.sh [PACKAGE ...]     # the pure core when no package is named
#   make mutation PKG=./internal/glob
#
# WORKERS mutants are tested at once, the CPUs by default, each test run with one CPU. The exit code is 0 when
# gremlins ran on every package, whatever the mutants that lived, 1 when it failed on one, 2 for a wrong argument.
set -u

# Released 2025-12-05 (Apache-2.0); a development tool, never a dependency of the module.
gremlins_version=v0.6.0
pure_core="./internal/glob ./internal/value ./internal/names ./internal/rules ./internal/config ./internal/integrity ./internal/solver"
# Every operator gremlins has, those it leaves off by default included.
operators="--invert-assignments --invert-bitwise --invert-bwassign --invert-logical --invert-loopctrl --remove-self-assignments"

if [ $# -eq 0 ]; then
	# One package a word, split on purpose.
	# shellcheck disable=SC2086
	set -- $pure_core
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
# killed.
export GOFLAGS="${GOFLAGS:+$GOFLAGS }-cpu=1"

summary=${GITHUB_STEP_SUMMARY:-/dev/null}
{
	echo "### Mutation testing, gremlins $gremlins_version"
	echo
	echo "Efficacy: the mutants killed among those the tests reach. Coverage: the mutants the tests reach among all."
	echo
	echo '| Package | Killed | Lived | Not covered | Timed out | Not viable | Efficacy | Coverage | Time |'
	echo '|---|---:|---:|---:|---:|---:|---:|---:|---:|'
} >>"$summary"

# A count and a share of gremlins' last lines, in the log of the package.
count() { sed -n "s/.*$1: \([0-9]*\).*/\1/p" "$log" | head -1; }
share() { sed -n "s/^$1: \([0-9.]*%\).*/\1/p" "$log" | head -1; }

worst=0
: >"$out/lived.txt"
for pkg in "$@"; do
	name=$(echo "${pkg#./}" | tr / -)
	log=$out/$name.log
	# Generated code (a table, a generator behind //go:build ignore) is not what the tests are for.
	excludes=()
	for f in "$pkg"/*.go; do
		if grep -q -m1 -E '^// Code generated .* DO NOT EDIT\.$|^//go:build ignore$' "$f"; then
			excludes+=(-E "$(basename "$f" | sed 's/\./\\./g')\$")
		fi
	done
	echo "==== $pkg"
	start=$(date +%s)
	# One operator a word, split on purpose.
	# shellcheck disable=SC2086
	"$out/gremlins" unleash --workers "$workers" $operators ${excludes[@]+"${excludes[@]}"} \
		-o "$out/$name.json" "$pkg" 2>&1 | sed 's/\x1b\[[0-9;]*m//g' | tee "$log"
	status=${PIPESTATUS[0]}
	spent=$(($(date +%s) - start))
	if [ "$status" != 0 ] || ! grep -q '^Killed: ' "$log"; then
		echo "| \`$pkg\` | gremlins failed, exit $status | | | | | | | ${spent}s |" >>"$summary"
		worst=1
		continue
	fi
	echo "| \`$pkg\` | $(count Killed) | $(count Lived) | $(count 'Not covered') | $(count 'Timed out') | $(count 'Not viable') | $(share 'Test efficacy') | $(share 'Mutator coverage') | ${spent}s |" >>"$summary"
	sed -n 's/^ *LIVED \([A-Z_]*\) at \(.*\)$/\2 \1/p' "$log" | sort -t: -k1,1 -k2,2n -k3,3n | sed "s|^|${pkg#./}/|" >>"$out/lived.txt"
done

if [ -s "$out/lived.txt" ]; then
	{
		echo
		echo "#### The mutants that lived"
		echo
		echo "Each a test to write, or a change that makes no difference, to say so."
		echo
		echo '```'
		cat "$out/lived.txt"
		echo '```'
	} >>"$summary"
fi
exit "$worst"
