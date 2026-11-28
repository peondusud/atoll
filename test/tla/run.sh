#!/bin/bash
# Checks the TLA+ model of conformance/models/ with TLC (specs.md §27, §29.4): each instance there, a .cfg file,
# against what its first line expects, "\* expect: pass", or "\* expect: <invariant>" for a flaw of specs.md the model
# found, whose remedy is the maintainer's to decide (conformance/models/README.md). An instance checks MC.tla, or the
# module a line "\* module: <name>.tla" of its comments names. Java runs only in a container:
# with only Docker, this script runs itself in the image below; with --inside, it runs TLC where it is, in that image,
# as the CI's job does.
#
#   test/tla/run.sh [--inside] [INSTANCE.cfg ...]
#
# Every instance by default. tla2tools.jar is downloaded once into TLA_CACHE, bin/tla/ by default (git-ignored), its
# SHA-256 checked, and never committed. TLC runs with 2 workers and a heap of 2 GiB, in a container of 2 CPUs and
# 3 GiB, so that it leaves the machine to whatever else runs there. Each instance's log goes to bin/tla/<name>.log. The
# exit code is 0 when every instance gave what it expects, 1 otherwise, and 2 when nothing could run.
#
# TLA_PART=K/N runs the Kth of N parts of every instance: by name, one in N from the Kth, as the CI's two halves run
# them at once. With instances named, it changes nothing.
set -u
# Git Bash on Windows rewrites what looks like a path, /src included, into a Windows one.
export MSYS_NO_PATHCONV=1

# .github/workflows/ci.yml and test/ci/run.sh name the same image, by its digest, its tag after it.
image=eclipse-temurin@sha256:2ec98aa522c472deadfd2b7c5b39bca680e33ef522a53a72f10789bdf89a6bb5 # 25.0.5.1_2-jre-noble
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
	# Made here, by the host's account: on Docker Desktop for Windows, the container's account may not make a folder in
	# the bind mount (found on a fresh checkout, 2026-11-19).
	mkdir -p bin/tla || exit 2
	exec docker run --rm --cpus 2 --memory 3g --user "$(id -u):$(id -g)" -v "$host:/src" -w /src \
		-e TLA_PART="${TLA_PART:-}" "$image" bash test/tla/run.sh --inside "$@"
fi

command -v java >/dev/null || {
	echo "no java here: run test/tla/run.sh without --inside, which needs only Docker" >&2
	exit 2
}

# fetch URL FILE downloads URL into FILE: with curl where there is one; otherwise, as in Temurin's images of Java 25,
# which have neither curl nor wget, by HTTP/1.0 over openssl s_client, its certificate and host name verified and its
# redirects followed, the response's body cut from its head by perl. Either way, the SHA-256 checked after it is what
# vouches for the bytes.
fetch() {
	if command -v curl >/dev/null; then
		curl -fsSL --retry 3 -o "$2" "$1"
		return
	fi
	local url=$1 hop rest host path headers status
	for hop in 1 2 3 4 5; do
		case $url in
		https://*) rest=${url#https://} ;;
		*) break ;;
		esac
		host=${rest%%/*} path=/${rest#*/}
		[ "$rest" = "$host" ] && path=/
		printf 'GET %s HTTP/1.0\r\nHost: %s\r\nUser-Agent: atoll-test-tla\r\n\r\n' "$path" "$host" |
			openssl s_client -quiet -verify_return_error -verify_hostname "$host" -servername "$host" \
				-connect "$host:443" >"$2.http" 2>/dev/null || break
		headers=$(perl -0777 -ne 'print $1 if /\A(.*?)\r\n\r\n/s' "$2.http")
		status=$(sed -n '1s/^HTTP\/[0-9.]* \([0-9]*\).*/\1/p' <<<"$headers")
		case $status in
		200)
			perl -0777 -pe 's/\A.*?\r\n\r\n//s' "$2.http" >"$2" && rm -f "$2.http"
			return
			;;
		301 | 302 | 303 | 307 | 308) url=$(sed -n 's/^[Ll]ocation: *//p' <<<"$headers" | tr -d '\r' | head -n 1) ;;
		*) break ;;
		esac
	done
	rm -f "$2.http"
	return 1
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
	fetch "$url" "$jar.part" || {
		echo "cannot download $url" >&2
		rm -f "$jar.part"
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
	if [ -n "${TLA_PART:-}" ]; then
		part=${TLA_PART%/*} parts=${TLA_PART#*/}
		case "$part/$parts" in
		*[!0-9/]* | /* | */ | 0/* | */0)
			echo "TLA_PART=$TLA_PART: give K/N, 1 <= K <= N" >&2
			exit 2
			;;
		esac
		[ "$part" -le "$parts" ] || {
			echo "TLA_PART=$TLA_PART: give K/N, 1 <= K <= N" >&2
			exit 2
		}
		i=0 mine=()
		for cfg in "$@"; do
			[ $((i % parts + 1)) -eq "$part" ] && mine+=("$cfg")
			i=$((i + 1))
		done
		set -- "${mine[@]}"
		echo "part $part of $parts: $# instances"
	fi
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
	# The module it checks: MC.tla, or the one a line "\* module: <name>.tla" of its comments names.
	module=$(sed -n 's/^\\\* module: \([A-Za-z0-9]*\.tla\)$/\1/p' "$cfg" | head -n 1)
	start=$(date +%s)
	java -XX:+UseParallelGC -Xmx2g -cp "$jar" tlc2.TLC -workers 2 -metadir "$meta/$name" -config "$cfg" "${module:-MC.tla}" >"$log" 2>&1
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
