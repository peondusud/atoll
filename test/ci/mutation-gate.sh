#!/bin/bash
# The gate of a mutation run (specs.md §27): reads gremlins' log of one package, leaves out the mutants that
# test/ci/mutation-equivalents.txt names, and checks the package's efficacy and mutator coverage against its gate in
# test/ci/gates.txt. test/ci/mutation.sh runs it after each package; run from the root of the repository:
#
#   bash test/ci/mutation-gate.sh ./internal/glob bin/mutation/internal-glob.log
#
# Efficacy: the mutants killed among those the tests reach, killed or lived, the named ones left out: a mutant that
# changes nothing a program can see cannot be killed. Mutator coverage: the mutants the tests reach among all, as
# gremlins counts it, the named ones in: the tests do reach them. A timed-out mutant counts in neither, as for gremlins.
# Each figure is compared as gremlins prints it, rounded to two decimals.
#
# A named mutant must be in the log, at its place and with its mutator, and its line of code must still read as the
# list says: a line moved or changed fails the gate, until the list is brought up to date and the mutant argued again.
# One gremlins counted killed is said, as a kill may come from outside, a machine too loaded, and left out all the same.
#
# It prints a line of figures, "<killed> <lived> <not covered> <timed out> <named> <efficacy> <coverage> <gate>", with
# its words on stderr; the gate is "-" for a package with none. The exit code is 0 when the package meets its gate or
# has none, 1 when it does not, or when the log or the list cannot be read as they must, 2 for a wrong argument.
set -u

[ $# -eq 2 ] || {
	echo "usage: $0 <package, such as ./internal/glob> <gremlins' log of it>" >&2
	exit 2
}
pkg=$1
log=$2
case $pkg in
./*) ;;
*)
	echo "$pkg: name a package by its folder, such as ./internal/glob" >&2
	exit 2
	;;
esac
[ -r "$log" ] || {
	echo "$log: no log to read" >&2
	exit 2
}
here=$(dirname "$0")
gates=$here/gates.txt
equivalents=$here/mutation-equivalents.txt
for f in "$gates" "$equivalents"; do
	[ -r "$f" ] || {
		echo "$f: cannot be read" >&2
		exit 1
	}
done

# The list is sorted as lived.txt is, by file, then line, then column, so that a change shows where it belongs.
if ! grep -v -e '^#' -e '^$' "$equivalents" | cut -f1 | LC_ALL=C sort -c -t: -k1,1 -k2,2n -k3,3n 2>/dev/null; then
	echo "$equivalents: not sorted by file, line and column" >&2
	exit 1
fi

dir=${pkg#./}
# The source line of each named mutant of this package, trimmed, for awk to compare: "<place>\t<line>".
lines=$(mktemp) || exit 1
trap 'rm -f "$lines"' EXIT
grep -v -e '^#' -e '^$' "$equivalents" | while IFS=$'\t' read -r place _ _ _; do
	case $place in
	"$dir"/*) ;;
	*) continue ;;
	esac
	file=${place%%:*}
	rest=${place#*:}
	line=${rest%%:*}
	if [ -r "$file" ]; then
		printf '%s\t%s\n' "$place" "$(sed -n "${line}p" "$file" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')"
	else
		printf '%s\t%s\n' "$place" "<no such file>"
	fi
done >"$lines"

gate=$(awk -v pkg="$pkg" '$1 == "mutation" && $2 == pkg { print $3, $4 }' "$gates")

awk -v dir="$dir" -v gate="$gate" -v lines="$lines" -v equivalents="$equivalents" -F'\t' '
	BEGIN {
		while ((getline l < lines) > 0) {
			split(l, f, "\t")
			source[f[1]] = f[2]
		}
		while ((getline l < equivalents) > 0) {
			if (l ~ /^#/ || l == "") continue
			n = split(l, f, "\t")
			if (n != 4 || f[2] !~ /^[A-Z_]+$/ || f[4] == "") {
				printf "%s: want a place, a mutator, a line of code and a reason, separated by tabs: %s\n", equivalents, l > "/dev/stderr"
				bad = 1
				continue
			}
			if (index(f[1], dir "/") != 1) continue
			key = f[1] " " f[2]
			if (key in named) {
				printf "%s: %s named twice\n", equivalents, key > "/dev/stderr"
				bad = 1
			}
			named[key] = 1
			if (source[f[1]] != f[3]) {
				printf "%s: %s no longer reads %s, but %s: argue it again\n", equivalents, f[1], f[3], source[f[1]] > "/dev/stderr"
				bad = 1
			}
		}
	}
	# A mutant of the log: "      KILLED CONDITIONALS_BOUNDARY at glob.go:94:14".
	match($0, /^ *(KILLED|LIVED|NOT COVERED|TIMED OUT|NOT VIABLE|SKIPPED) [A-Z_]+ at /) {
		line = $0
		sub(/^ +/, "", line)
		status = line
		sub(/ [A-Z_]+ at .*$/, "", status)
		rest = substr(line, length(status) + 2)
		mutator = rest
		sub(/ .*$/, "", mutator)
		place = rest
		sub(/^[A-Z_]+ at /, "", place)
		key = dir "/" place " " mutator
		count[status]++
		if (key in named) {
			seen[key] = 1
			if (status == "LIVED") left["LIVED"]++
			if (status == "KILLED") {
				left["KILLED"]++
				printf "named equivalent, yet gremlins counted it killed, left out all the same: %s\n", key > "/dev/stderr"
			}
		}
		next
	}
	/^Killed: / {
		gsub(/[^0-9]+/, " ")
		split($0, said, " ")
		summed = 1
	}
	END {
		for (key in named)
			if (!(key in seen)) {
				printf "%s: %s is not in the log: brought up to date?\n", equivalents, key > "/dev/stderr"
				bad = 1
			}
		k = count["KILLED"] + 0; l = count["LIVED"] + 0; nc = count["NOT COVERED"] + 0; to = count["TIMED OUT"] + 0
		if (!summed || said[1] != k || said[2] != l || said[3] != nc) {
			printf "the log lists %d killed, %d lived, %d not covered, which its last lines do not say\n", k, l, nc > "/dev/stderr"
			exit 1
		}
		ek = k - left["KILLED"]; el = l - left["LIVED"]
		efficacy = (ek + el > 0) ? 100 * ek / (ek + el) : 0
		coverage = (k + l + nc > 0) ? 100 * (k + l) / (k + l + nc) : 0
		e = sprintf("%.2f", efficacy); c = sprintf("%.2f", coverage)
		named_here = left["KILLED"] + left["LIVED"]
		if (gate == "") {
			printf "%d %d %d %d %d %s %s -\n", k, l, nc, to, named_here, e, c
			printf "efficacy %s%%, mutator coverage %s%%, %d named equivalent left out; no gate\n", e, c, named_here > "/dev/stderr"
			exit bad
		}
		split(gate, g, " ")
		pass = (e + 0 >= g[1] + 0) && (c + 0 >= g[2] + 0)
		printf "%d %d %d %d %d %s %s %s/%s\n", k, l, nc, to, named_here, e, c, g[1], g[2]
		printf "efficacy %s%% (gate %s%%), mutator coverage %s%% (gate %s%%), %d named equivalent left out: %s\n",
			e, g[1], c, g[2], named_here, pass ? "passes" : "FAILS" > "/dev/stderr"
		exit (bad || !pass) ? 1 : 0
	}' "$log"
