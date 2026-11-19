#!/bin/bash
# Checks the TLA+ model of conformance/models/ with TLC (specs.md §27, §29.4): each instance there, a .cfg file,
# against what its first line expects, "\* expect: pass", or "\* expect: <invariant>" for a flaw of specs.md the model
# found, whose remedy is the maintainer's to decide (conformance/models/README.md). Java runs only in a container:
# with only Docker, this script runs itself in the image below; with --inside, it runs TLC where it is, in that image,
# as the CI's job does.
#
#   test/tla/run.sh [--inside] [INSTANCE.cfg ...]
#
# Every instance by default. tla2tools.jar is downloaded once into TLA_CACHE, bin/tla/ by default (git-ignored), its
# SHA-256 checked, and never committed. TLC runs with 2 workers and a heap of 2 GiB, in a container of 2 CPUs and
# 3 GiB, so that it leaves the machine to whatever else runs there. Each instance's log goes to bin/tla/<name>.log. The
# exit code is 0 when every instance gave what it expects, 1 otherwise, and 2 when nothing could run.
set -u
# Git Bash on Windows rewrites what looks like a path, /src included, into a Windows one.
export MSYS_NO_PATHCONV=1

# .github/workflows/ci.yml and test/ci/run.sh name the same image.
image=eclipse-temurin:21.0.12_8-jre-noble
version=1.7.4
sum=936a262061c914694dfd669a543be24573c45d5aa0ff20a8b96b23d01e050e88
url=https://github.com/tlaplus/tlaplus/releases/download/v$version/tla2tools.jar

inside=0
if [ "${1:-}" = --inside ]; then
	inside=1
	shift
fi
case "${1:-}" in
-h | --help)
	sed -n '2,13p' "$0"
	exit 0
	;;
-*)
	echo "unknown option $1" >&2
	exit 2
	;;
esac

top=$(cd "$(dirname "$0")/../.." && pwd) || exit 2
cd "$top" || exit 2

if [ "$inside" = 0 ]; then
	docker info >/dev/null 2>&1 || {
		echo "Docker does not answer" >&2
		exit 2
	}
	# The folder as Docker Desktop reads it on Windows, and as it is elsewhere. What the container writes, the jar and
	# the logs below bin/tla/, belongs to whoever runs this script.
	host=$(pwd -W 2>/dev/null || pwd)
	exec docker run --rm --cpus 2 --memory 3g --user "$(id -u):$(id -g)" -v "$host:/src" -w /src "$image" \
		bash test/tla/run.sh --inside "$@"
fi

command -v java >/dev/null || {
	echo "no java here: run test/tla/run.sh without --inside, which needs only Docker" >&2
	exit 2
}
cache=${TLA_CACHE:-bin/tla}
case $cache in
/*) ;;
*) cache=$top/$cache ;;
esac
jar=$cache/tla2tools-$version.jar
mkdir -p "$cache" "$top/bin/tla" || exit 2
if ! echo "$sum  $jar" | sha256sum -c --status 2>/dev/null; then
	echo "downloading tla2tools.jar $version"
	rm -f "$jar.part"
	curl -fsSL --retry 3 -o "$jar.part" "$url" || {
		echo "cannot download $url" >&2
		exit 2
	}
	if ! echo "$sum  $jar.part" | sha256sum -c --status; then
		echo "tla2tools.jar $version is not the one pinned: SHA-256 $(sha256sum "$jar.part" | cut -d ' ' -f 1)" >&2
		rm -f "$jar.part"
		exit 2
	fi
	mv "$jar.part" "$jar" || exit 2
fi

meta=$(mktemp -d) || exit 2
trap 'rm -rf "$meta"' EXIT
cd conformance/models || exit 2
if [ $# -eq 0 ]; then
	# By name, in byte order.
	LC_COLLATE=C
	set -- *.cfg
fi

took() { printf '%dm%02ds' $(($1 / 60)) $(($1 % 60)); }

worst=0
for cfg in "$@"; do
	name=${cfg%.cfg}
	log=$top/bin/tla/$name.log
	expect=$(sed -n '1s/^\\\* expect: //p' "$cfg")
	if [ -z "$expect" ]; then
		echo "$cfg: its first line does not say what it expects (\\* expect: pass, or an invariant)" >&2
		worst=1
		continue
	fi
	start=$(date +%s)
	java -XX:+UseParallelGC -Xmx2g -cp "$jar" tlc2.TLC -workers 2 -metadir "$meta/$name" -config "$cfg" MC.tla >"$log" 2>&1
	spent=$(($(date +%s) - start))
	got=error
	if grep -q '^Model checking completed. No error has been found.' "$log"; then
		got=pass
	elif grep -q '^Error: Temporal properties were violated' "$log"; then
		got=liveness
	else
		violated=$(sed -n 's/^Error: Invariant \(.*\) is violated\.$/\1/p' "$log" | head -n 1)
		[ -n "$violated" ] && got=$violated
	fi
	states=$(sed -n 's/^.* \([0-9]*\) distinct states found.*$/\1/p' "$log" | tail -n 1)
	if [ "$got" = "$expect" ]; then
		printf '  %-22s %-30s %9s states  %s\n' "$name" "$got" "${states:-?}" "$(took "$spent")"
		continue
	fi
	printf '  %-22s %-30s %9s states  %s  FAILED: expected %s\n' "$name" "$got" "${states:-?}" "$(took "$spent")" "$expect"
	worst=1
	echo "==== $name.log, from its first error"
	sed -n '/^Error:/,$p' "$log" | head -n 400
done
exit "$worst"
