#!/bin/sh
# Print the body of one version's section of CHANGELOG.md, used as the GitHub
# release notes. Exits 1 if the section is missing or empty, so a tag pushed
# before its CHANGELOG entry fails the release before anything is published.
#
# Usage: scripts/release-notes.sh <version> [changelog]
#   <version> without the "v", e.g. 0.1.0. A pre-release such as 0.1.0-rc.1
#   with no section of its own uses the section of the version it precedes.
set -eu

version=$1
changelog=${2:-CHANGELOG.md}

section() {
	# From "## [<v>]" up to the next "## [" heading or the link definitions.
	awk -v v="$1" '
		/^## \[/ { if (on) exit; on = index($0, "## [" v "]") == 1; next }
		/^\[[^]]*\]: / { if (on) exit }
		on
	' "$changelog"
}

notes=$(section "$version")
case $version in
*-*) [ -n "$(printf '%s' "$notes" | tr -d '[:space:]')" ] || notes=$(section "${version%%-*}") ;;
esac

if [ -z "$(printf '%s' "$notes" | tr -d '[:space:]')" ]; then
	echo "release-notes: no CHANGELOG section for $version in $changelog" >&2
	exit 1
fi
printf '%s\n' "$notes"
