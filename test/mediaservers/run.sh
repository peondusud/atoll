#!/bin/bash
# Tries Atoll's notifications to media servers (specs.md §21.5) on real servers: Plex, Jellyfin and
# Emby, each in a container of its vendor's latest image, with only git and Docker on the host.
#
#   test/mediaservers/run.sh [--keep] [plex] [jellyfin] [emby] [REV]
#
# With no server named, all three; REV is HEAD by default, and the test runs on the commit as git
# holds it. Each run has a network and a volume of its own, and publishes no port on the host. The
# servers read the library on the volume at /data, and the test writes it at /pool, as the merged
# view holds it: path_map /pool = /data takes one to the other. Each server is set up from scratch.
# Plex is left unclaimed, in no account, and gets no token: unclaimed, it answers 401 to every other
# host, allowedNetworks or not, but takes its own host's requests, so the test runs in Plex's
# network namespace and reaches it at 127.0.0.1 (Plex 1.43.5, measured on 2026-11-20). With
# PLEX_CLAIM set in the environment, a claim token from https://plex.tv/claim valid for 4 minutes,
# the server is claimed by that account instead, where it stays listed until removed there: the test
# then reaches it by name with its token, which it reads from a volume of its own, and once without
# it from an allowed network. Plex's image talks to plex.tv on its own, claimed or not. Jellyfin and
# Emby go through their first-run wizard, with an account and an API key made for the run. No token,
# key or password is printed. The three images take about 4.4 GB. --keep leaves the servers
# running, and prints how to remove them.
set -u
# Git Bash on Windows rewrites what looks like a path, /src included, into a Windows one.
export MSYS_NO_PATHCONV=1

# The image of .github/workflows/ci.yml, by its digest, its tag after it. The servers' images stay at latest, unpinned:
# what this test watches is the vendors' images changing.
image=golangci/golangci-lint@sha256:fbfcfd12a6f8d7f1b97b3362ca80fc2a244d46863148c73cb88e7793af96c481 # v2.15.0
keep=0 servers="" rev=HEAD
for a in "$@"; do
	case $a in
	--keep) keep=1 ;;
	plex | jellyfin | emby) servers="$servers $a" ;;
	-h | --help)
		sed -n '2,20p' "$0"
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
		echo "kept: docker rm -f -v$names; docker volume rm $run $run-src $run-secret; docker network rm $run"
		return
	fi
	# -v takes the volumes the images declare, which no name holds. One name a word, split on purpose.
	# shellcheck disable=SC2086
	[ -n "$names" ] && docker rm -f -v $names >/dev/null 2>&1
	docker volume rm -f "$run" "$run-src" "$run-secret" >/dev/null 2>&1
	docker network rm "$run" >/dev/null 2>&1
}
trap cleanup EXIT
trap 'echo "stopped" >&2; exit 130' INT TERM

docker network create "$run" >/dev/null && docker volume create "$run" >/dev/null &&
	docker volume create "$run-src" >/dev/null && docker volume create "$run-secret" >/dev/null || exit 2
# The commit, in a volume: a bind mount of the working tree would show what is not committed, and a
# path of the host means something else to Docker Desktop.
if ! git archive --format=tar "$sha" | docker run -i --rm -v "$run-src:/src" "$image" tar -x -C /src; then
	echo "cannot copy $sha into a volume" >&2
	exit 2
fi

env=() net=$run
for s in $servers; do
	name=$run-$s
	case $s in
	plex)
		if [ -n "${PLEX_CLAIM:-}" ]; then
			# -e PLEX_CLAIM with no value passes the environment's, so the claim is on no command line.
			set -- -e TZ=UTC -e ALLOWED_NETWORKS=10.0.0.0/8,172.16.0.0/12,192.168.0.0/16 -e PLEX_CLAIM \
				plexinc/pms-docker:latest
			env+=(-e "ATOLL_PLEX_URL=http://$name:32400")
		else
			# The test shares Plex's network namespace, which is on the run's network, so it still
			# reaches the other servers by name.
			set -- -e TZ=UTC plexinc/pms-docker:latest
			env+=(-e ATOLL_PLEX_URL=http://127.0.0.1:32400)
			net=container:$name
		fi
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

# A claimed Plex writes its token in its preferences: it goes, through a pipe, to a volume only the
# test reads.
if [ -n "${PLEX_CLAIM:-}" ] && [ "${servers#*plex}" != "$servers" ]; then
	prefs="/config/Library/Application Support/Plex Media Server/Preferences.xml"
	for _ in $(seq 60); do
		docker exec "$run-plex" grep -q 'PlexOnlineToken="' "$prefs" 2>/dev/null && break
		sleep 2
	done
	docker exec "$run-plex" sed -nE 's/.*PlexOnlineToken="([^"]+)".*/\1/p' "$prefs" |
		docker run -i --rm -v "$run-secret:/secret" "$image" sh -c 'umask 077 && cat > /secret/plex.token && test -s /secret/plex.token' || {
		echo "Plex was not claimed: its claim token may have expired (4 minutes)" >&2
		exit 1
	}
	env+=(-e ATOLL_PLEX_TOKEN_FILE=/secret/plex.token)
fi

docker run --rm --name "$run-test" --network "$net" -v "$run:/pool" -v "$run-src:/src:ro" -v "$run-secret:/secret:ro" \
	-v atoll-ci-gomod:/go/pkg/mod "${env[@]}" "$image" \
	bash -c "cp -r /src /w && cd /w && go test -count=1 -tags mediaservers -v ./test/mediaservers"
