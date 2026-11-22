#!/bin/bash
# Stands for go in a mutation run of a package whose tests are many and long (test/ci/mutation.sh, MUTATION_BY_TEST):
# gremlins runs `go test` of the package for each mutant, in its own copy of the module, and this runs, of that
# package's tests, only those that reach the line the mutant changed, as the coverage of each test alone says
# (bin/mutation/<package>.tests, which mutation.sh writes first). A test that does not run that line cannot see what
# the mutant changed. Anything else goes to go as it is: the run that gathers the coverage, go mod download, and a
# mutant on a line no test is known to reach, which runs every test.
#
# The line is found by comparing each file of the package with the checkout's own, spaces aside: gremlins writes a
# mutated file through go/printer, which may align things otherwise. With MUTATION_LOG set, each choice is appended to
# it, one line a run: the line, then how many tests ran, or "all".
set -u
real=${MUTATION_GO:?the go to run}
src=${MUTATION_SRC:?the checkout}
pkg=${MUTATION_PKG:?the package folder, from the root of the module}
tests=${MUTATION_MAP:?the map of the lines each test reaches}

if [ "${1:-}" != test ]; then
	exec "$real" "$@"
fi
for arg in "$@"; do
	case $arg in
	-cover | -coverprofile | -coverprofile=*) exec "$real" "$@" ;;
	esac
done

line=
for f in ./*.go; do
	[ -e "$f" ] || continue
	base=${f#./}
	cmp -s "$f" "$src/$pkg/$base" && continue
	at=$(diff -w "$src/$pkg/$base" "$f" | sed -n '1s/^\([0-9]*\).*/\1/p')
	if [ -n "$at" ]; then
		line=$pkg/$base:$at
		break
	fi
done
run=
if [ -n "$line" ]; then
	run=$(awk -v key="$line" '$1 == key { print $2; exit }' "$tests")
fi
if [ -n "${MUTATION_LOG:-}" ]; then
	if [ -n "$run" ]; then
		echo "${line:-?} $(echo "$run" | tr '|' '\n' | wc -l)" >>"$MUTATION_LOG"
	else
		echo "${line:-?} all" >>"$MUTATION_LOG"
	fi
fi
if [ -z "$run" ]; then
	exec "$real" "$@"
fi
# The package is the last argument gremlins gives; -run goes before it.
last=${!#}
set -- "${@:1:$#-1}" -run "^($run)\$" "$last"
exec "$real" "$@"
