#!/bin/sh
# Checks that GitHub serves each file of a release under the name SHA256SUMS gives it (specs.md §26). On upload, GitHub
# renames a file whose name holds anything but letters, digits, '.', '-' and '_', or begins or ends with '.': the '~'
# of a prerelease's .deb, atoll_0.1.0~rc.1-1_amd64.deb, became '.' on v0.1.0-rc.1's draft. The check the notes give,
# `sha256sum --ignore-missing -c SHA256SUMS`, then passes over that file without a word. .github/workflows/release.yml's
# build runs it on dist/ after GoReleaser, as make release-snapshot does; run from the root of the repository:
#
#   sh test/ci/release-names.sh dist
#
# The exit code is 0 when every name of SHA256SUMS stays as it is, 1 when one would change or when SHA256SUMS lists
# nothing or cannot be read.
set -eu
export LC_ALL=C

cd "${1:-dist}"
[ -r SHA256SUMS ] || {
	echo "release-names: no SHA256SUMS in ${1:-dist}" >&2
	exit 1
}
names=0
bad=0
while read -r sum name; do
	names=$((names + 1))
	case $name in
	'' | .* | *. | *[!A-Za-z0-9._-]*)
		echo "release-names: GitHub would rename $name on upload, and SHA256SUMS would no longer check it" >&2
		bad=1
		;;
	esac
done <SHA256SUMS
[ "$names" -gt 0 ] || {
	echo "release-names: SHA256SUMS lists nothing" >&2
	exit 1
}
[ "$bad" = 0 ] || exit 1
echo "release-names: GitHub serves the $names files of SHA256SUMS under their names"
