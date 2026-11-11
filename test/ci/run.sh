#!/bin/bash
# What a CI runs on a commit, on this machine, with only git and Docker: the tests with the race
# detector, the integration tests, the conformance scenarios, lint, and test/loopdev in qemu on two
# kernels. Each job runs in a container of its own, all at once, on the commit as git holds it, never
# on the working tree, which may hold what no commit does.
#
#   test/ci/run.sh [--report] [--jobs "race lint ..."] [REV]
#
# REV is HEAD by default. Each job writes its log to bin/ci/<commit>/<job>.log. The exit code is 0
# when every job passed, 1 when one failed, 2 when nothing could run. --report also posts each job to
# GitHub as a commit status, local/<job>, which a pull request on that commit lists among its checks:
# the commit must be pushed, and gh signed in. KERNELS, MAINLINE, RUN, COUNT and VERBOSE reach the qemu
# job as they reach make test-qemu (test/qemu/README.md).
set -u
# Git Bash on Windows rewrites what looks like a path, /src included, into a Windows one.
export MSYS_NO_PATHCONV=1

image=golangci/golangci-lint:v2.15.0
all_jobs="race integration conformance lint qemu"
jobs=$all_jobs report=0 rev=HEAD
while [ $# -gt 0 ]; do
	case $1 in
	--report) report=1 ;;
	--jobs)
		[ $# -ge 2 ] || { echo "--jobs needs a list" >&2; exit 2; }
		jobs=$2
		shift
		;;
	-h | --help)
		sed -n '2,14p' "$0"
		exit 0
		;;
	-*)
		echo "unknown option $1" >&2
		exit 2
		;;
	*) rev=$1 ;;
	esac
	shift
done
for job in $jobs; do
	case " $all_jobs " in
	*" $job "*) ;;
	*)
		echo "no job $job; the jobs are: $all_jobs" >&2
		exit 2
		;;
	esac
done

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
if [ "$report" = 1 ]; then
	gh auth status >/dev/null 2>&1 || {
		echo "--report needs gh signed in" >&2
		exit 2
	}
	gh api --silent "repos/{owner}/{repo}/commits/$sha" 2>/dev/null || {
		echo "GitHub has no commit $sha: push it first" >&2
		exit 2
	}
fi

logs=$top/bin/ci/${sha:0:12}
rm -rf "$logs" && mkdir -p "$logs" || exit 2

# The commit, in a volume: a bind mount of the working tree would show what is not committed, and a
# path of the host means something else to Docker Desktop. The module cache outlives the run.
id=atoll-ci-${sha:0:12}-$$
src=$id-src built=$id-qemu gomod=atoll-ci-gomod
cleanup() {
	local left
	left=$(docker ps -aq --filter "name=^$id-")
	# One id a line, split on purpose.
	# shellcheck disable=SC2086
	[ -n "$left" ] && docker rm -f $left >/dev/null 2>&1
	docker volume rm -f "$src" "$built" >/dev/null 2>&1
}
trap cleanup EXIT
trap 'echo "stopped" >&2; exit 130' INT TERM

docker volume create "$src" >/dev/null && docker volume create "$built" >/dev/null || exit 2
if ! git archive --format=tar "$sha" | docker run -i --rm -v "$src:/src" "$image" tar -x -C /src; then
	echo "cannot copy $sha into a volume" >&2
	exit 2
fi

# go_job runs a shell command in a copy of the commit of its own, as tests and builds write beside
# the sources. What follows the command goes to docker run.
go_job() {
	local name=$1 cmd=$2
	shift 2
	docker run --rm --name "$id-$name" -v "$src:/src:ro" -v "$gomod:/go/pkg/mod" ${1+"$@"} "$image" \
		bash -c "cp -r /src /w && cd /w && $cmd"
}

job_race() { go_job race 'make test-race'; }
job_integration() { go_job integration 'make test-integration'; }
job_conformance() { go_job conformance 'make conformance'; }
# make lint covers the delivered build and the conformance build; the loopdev tests are a third.
job_lint() { go_job lint 'make lint && golangci-lint run --build-tags loopdev ./test/loopdev/...'; }
job_qemu() {
	go_job qemu-build 'make qemu-build && cp -r bin/qemu/. /qemu/' -v "$built:/qemu" || return 2
	local kvm=()
	[ -e /dev/kvm ] && kvm=(--device /dev/kvm)
	docker run --rm --name "$id-qemu" ${kvm[@]+"${kvm[@]}"} -e KERNELS -e MAINLINE -e RUN -e COUNT -e VERBOSE \
		-v "$src:/src:ro" -v "$built:/qemu:ro" debian:13 bash /src/test/qemu/run.sh
}

took() { printf '%dm%02ds' $(($1 / 60)) $(($1 % 60)); }

post() { # job, state, description
	[ "$report" = 1 ] || return 0
	gh api --silent -X POST "repos/{owner}/{repo}/statuses/$sha" \
		-f state="$2" -f context="local/$1" -f description="$3" >/dev/null 2>&1 ||
		echo "could not post the status of $1" >&2
}

echo "commit $sha, jobs: $jobs"
echo "logs: $logs"
for job in $jobs; do
	post "$job" pending "running on a local machine"
	(
		start=$(date +%s)
		"job_$job" >"$logs/$job.log" 2>&1
		status=$?
		spent=$(($(date +%s) - start))
		echo "$status $spent" >"$logs/$job.result"
		if [ "$status" = 0 ]; then
			post "$job" success "passed in $(took "$spent"), on a local machine"
			echo "$job: passed in $(took "$spent")"
		else
			post "$job" failure "failed in $(took "$spent"), exit $status: see bin/ci/${sha:0:12}/$job.log"
			echo "$job: FAILED in $(took "$spent"), exit $status"
		fi
	) &
done
wait

worst=0
echo
for job in $jobs; do
	read -r status spent 2>/dev/null <"$logs/$job.result" || status=2 spent=0
	if [ "$status" = 0 ]; then
		printf '  %-12s passed  %s\n' "$job" "$(took "$spent")"
	else
		printf '  %-12s FAILED  %s, exit %s\n' "$job" "$(took "$spent")" "$status"
		worst=1
	fi
done
for job in $jobs; do
	read -r status _ 2>/dev/null <"$logs/$job.result" || status=2
	[ "$status" = 0 ] && continue
	echo
	echo "==== the end of $job.log"
	tail -n 25 "$logs/$job.log"
done
exit "$worst"
