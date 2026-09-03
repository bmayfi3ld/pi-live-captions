#!/bin/sh
# Print the release version: the first `## X.Y.Z` heading in CHANGELOG.md.
# Single source of truth for the justfile and CI, so the two cannot drift.
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
version=$(sed -n 's/^##[[:space:]]*\([0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\)[[:space:]]*$/\1/p' \
	"$root/CHANGELOG.md" | head -1)
if [ -z "$version" ]; then
	echo "no '## X.Y.Z' heading found in CHANGELOG.md" >&2
	exit 1
fi
echo "$version"
