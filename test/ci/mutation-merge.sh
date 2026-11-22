#!/bin/bash
# The score of a package measured in parts (specs.md §27): jobs, whose weekly run is a matrix of jobs, each mutating a
# part of its files (.github/workflows/mutation-weekly.yml, test/ci/mutation.sh with MUTATION_FILES). Reads gremlins'
# log of each part, keeps each mutant once, as the part that tried it found it, writes one log of them all with
# gremlins' last lines, and gates that log as the log of one run (test/ci/mutation-gate.sh). Run from the root of the
# repository:
#
#   bash test/ci/mutation-merge.sh ./internal/jobs bin/mutation/internal-jobs.log part/*/internal-jobs.log
#
# A mutant two parts list is the one a part tried, killed, lived or timed out, before one it did not reach, which
# before one it did not try (not viable, skipped); two parts that tried it and disagree fail the merge, as one
# mutant has one fate. It prints test/ci/mutation-gate.sh's line of figures, and its words on stderr; the exit code is
# mutation-gate.sh's, 1 when a part's log cannot be read or two parts disagree, 2 for a wrong argument.
set -u

[ $# -ge 3 ] || {
	echo "usage: $0 <package, such as ./internal/jobs> <merged log to write> <part log>..." >&2
	exit 2
}
pkg=$1
merged=$2
shift 2
for log in "$@"; do
	grep -q '^Killed: ' "$log" 2>/dev/null || {
		echo "$log: no whole log of gremlins to read" >&2
		exit 1
	}
done

LC_ALL=C awk '
	function rank(s) { return (s == "KILLED" || s == "LIVED" || s == "TIMED OUT") ? 3 : (s == "NOT COVERED") ? 2 : 1 }
	match($0, /^ *(KILLED|LIVED|NOT COVERED|TIMED OUT|NOT VIABLE|SKIPPED) [A-Z_]+ at /) {
		line = $0
		sub(/^ +/, "", line)
		status = line
		sub(/ [A-Z_]+ at .*$/, "", status)
		key = substr(line, length(status) + 2)
		if (!(key in fate)) {
			order[++n] = key
		} else if (rank(fate[key]) == 3 && rank(status) == 3 && fate[key] != status) {
			printf "%s: %s in one part, %s in another\n", key, fate[key], status > "/dev/stderr"
			bad = 1
		}
		if (!(key in fate) || rank(status) > rank(fate[key])) fate[key] = status
	}
	END {
		for (i = 1; i <= n; i++) {
			s = fate[order[i]]
			printf "%12s %s\n", s, order[i]
			count[s]++
		}
		printf "\nKilled: %d, Lived: %d, Not covered: %d\n", count["KILLED"], count["LIVED"], count["NOT COVERED"]
		printf "Timed out: %d, Not viable: %d, Skipped: %d\n", count["TIMED OUT"], count["NOT VIABLE"], count["SKIPPED"]
		exit bad
	}' "$@" >"$merged" || exit 1
figures=$(bash "$(dirname "$0")/mutation-gate.sh" "$pkg" "$merged")
status=$?
echo "$figures"

# On GitHub Actions, the score of all the parts, as test/ci/mutation.sh gives that of one, and the mutants that lived.
summary=${GITHUB_STEP_SUMMARY:-}
if [ -n "$summary" ]; then
	read -r killed lived uncovered timed named efficacy coverage gate <<<"$figures"
	verdict=passes
	[ "$status" = 0 ] || verdict=FAILS
	case ${gate:-?}:$verdict in
	-:passes) cell=– ;;
	-:*) cell="–, $verdict" ;;
	*) cell="${gate:-?}, $verdict" ;;
	esac
	named_list=$(grep -v -e '^#' -e '^$' "$(dirname "$0")/mutation-equivalents.txt" | cut -f1,2 | tr '\t' ' ')
	{
		echo "### Mutation testing of \`$pkg\`, its $# parts together"
		echo
		echo '| Package | Killed | Lived | Named | Not covered | Timed out | Not viable | Efficacy | Coverage | Gate |'
		echo '|---|---:|---:|---:|---:|---:|---:|---:|---:|---|'
		echo "| \`$pkg\` | ${killed:-?} | ${lived:-?} | ${named:-?} | ${uncovered:-?} | ${timed:-?} | $(sed -n 's/.*Not viable: \([0-9]*\).*/\1/p' "$merged") | ${efficacy:-?}% | ${coverage:-?}% | $cell |"
		echo
		echo "#### The mutants that lived, not named equivalent"
		echo
		echo '```'
		sed -n 's/^ *LIVED \([A-Z_]*\) at \(.*\)$/\2 \1/p' "$merged" | sed "s|^|${pkg#./}/|" | grep -v -x -F -e "${named_list:-none}" |
			sort -t: -k1,1 -k2,2n -k3,3n
		echo '```'
	} >>"$summary"
fi
exit "$status"
