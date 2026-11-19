#!/bin/bash
# Tries Atoll's notifications to media servers (specs.md §21.5) on real servers: Plex, Jellyfin and
# Emby, each in a container of its vendor's latest image, with only git and Docker on the host.
#
#   test/mediaservers/run.sh [--keep] [plex] [jellyfin] [emby] [REV]
#
# With no server named, all three; REV is HEAD by default, and the test runs on the commit as git
# holds it. Each run has a network and a volume of its own, and publishes no port on the host. The
# servers read the library on the volume at /data, and the test writes it at /pool, as the merged
# view holds it: path_map /pool = /data takes one to the other. Each server is set up from scratch:
# Plex unclaimed, which takes its network's requests without a token; Jellyfin and Emby through their
# first-run wizard, with an account and an API key made for the run and never printed. The three
# images take about 4.4 GB. --keep leaves the servers running, and prints how to remove them.
set -u
# Git Bash on Windows rewrites what looks like a path, /src included, into a Windows one.
export MSYS_NO_PATHCONV=1

image=golangci/golangci-lint:v2.15.0
keep=0 servers="" rev=HEAD
for a in "$@"; do
	case $a in
	--keep) keep=1 ;;
	plex | jellyfin | emby) servers="$servers $a" ;;
	-h | --help)
		sed -n '2,13p' "$0"
		exit 0
		;;
	-*)
		echo "unknown option $a" >&2
		exit 2
		;;
	*) rev=$a ;;
	esac
done
[ -n "$servers" ] || servers="plex jellyfin emby"

top=$(git rev-parse --show-toplevel) || exit 2
cd "$top" || exit 2
sha=$(git rev-parse --verify --quiet "$rev^{commit}") || {
	echo "no commit $rev" >&2
	exit 2
}
docker info >/dev/null 2>&1 || {
	echo "Docker does not answer" >&2
	exit 2
}

run=atoll-media-${sha:0:12}-$$
names=""
cleanup() {
	if [ $keep = 1 ]; then
		echo "kept: docker rm -f$names; docker volume rm $run $run-src; docker network rm $run"
		return
	fi
	# One name a word, split on purpose.
	# shellcheck disable=SC2086
	[ -n "$names" ] && docker rm -f $names >/dev/null 2>&1
	docker volume rm -f "$run" "$run-src" >/dev/null 2>&1
	docker network rm "$run" >/dev/null 2>&1
}
trap cleanup EXIT
trap 'echo "stopped" >&2; exit 130' INT TERM

docker network create "$run" >/dev/null && docker volume create "$run" >/dev/null &&
	docker volume create "$run-src" >/dev/null || exit 2
# The commit, in a volume: a bind mount of the working tree would show what is not committed, and a
# path of the host means something else to Docker Desktop.
if ! git archive --format=tar "$sha" | docker run -i --rm -v "$run-src:/src" "$image" tar -x -C /src; then
	echo "cannot copy $sha into a volume" >&2
	exit 2
fi

env=()
for s in $servers; do
	name=$run-$s
	case $s in
	plex)
		set -- -e TZ=UTC -e ALLOWED_NETWORKS=10.0.0.0/8,172.16.0.0/12,192.168.0.0/16 plexinc/pms-docker:latest
		env+=(-e "ATOLL_PLEX_URL=http://$name:32400")
		;;
	jellyfin)
		set -- jellyfin/jellyfin:latest
		env+=(-e "ATOLL_JELLYFIN_URL=http://$name:8096")
		;;
	emby)
		set -- emby/embyserver:latest
		env+=(-e "ATOLL_EMBY_URL=http://$name:8096")
		;;
	esac
	docker run -d --name "$name" --network "$run" -v "$run:/data" "$@" >/dev/null || exit 2
	names="$names $name"
done

docker run --rm --name "$run-test" --network "$run" -v "$run:/pool" -v "$run-src:/src:ro" \
	-v atoll-ci-gomod:/go/pkg/mod "${env[@]}" "$image" \
	bash -c "cp -r /src /w && cd /w && go test -count=1 -tags mediaservers -v ./test/mediaservers"
