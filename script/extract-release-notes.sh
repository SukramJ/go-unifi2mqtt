#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
# Copyright (C) 2026 SukramJ
#
# Extract the changelog.md section for the given version and emit it
# as a self-contained release-notes payload on stdout. Single source
# of truth shared by `make release-notes` (local dry-run) and the
# .github/workflows/release-on-tag.yml workflow.
#
# Uses only POSIX-compatible awk + sed so it runs the same on macOS
# (BSD awk) and Ubuntu (gawk) — no `match($0, regex, array)` tricks.
#
# Usage: script/extract-release-notes.sh <version>
#
# Exits non-zero when no matching section is found, so `make release`
# fails fast instead of producing an empty release body.

set -euo pipefail

if [ $# -lt 1 ]; then
	echo "usage: $0 <version>" >&2
	exit 2
fi

VERSION="$1"
CHANGELOG="${CHANGELOG:-changelog.md}"

if [ ! -f "$CHANGELOG" ]; then
	echo "error: $CHANGELOG not found at $(pwd)" >&2
	exit 1
fi

# Body: skip the header line itself, print everything until the next
# "# Version " header (or EOF).
body=$(awk -v ver="$VERSION" '
	/^# Version / {
		if (insec) exit
		if ($0 ~ "^# Version " ver " ") { insec=1; next }
	}
	insec { print }
' "$CHANGELOG")

if [ -z "$body" ]; then
	echo "error: no '# Version $VERSION ' section found in $CHANGELOG" >&2
	exit 1
fi

# Previous version: the next "# Version <tag> ..." header that appears
# after our section. Splitting the regex/extraction into awk+sed keeps
# us off the gawk-only match-with-array form.
prev_header=$(awk -v ver="$VERSION" '
	$0 ~ "^# Version " ver " " { insec=1; next }
	insec && /^# Version / { print; exit }
' "$CHANGELOG")

prev_version=""
if [ -n "$prev_header" ]; then
	prev_version=$(printf '%s\n' "$prev_header" | sed -E 's/^# Version ([^ ]+).*$/\1/')
fi

# Emit the body, then optionally the compare link. The first release
# has no predecessor — that's fine, just skip the link.
printf '%s\n' "$body"

if [ -n "$prev_version" ]; then
	repo="${GITHUB_REPOSITORY:-SukramJ/go-unifi2mqtt}"
	printf '\n**Full Changelog**: https://github.com/%s/compare/%s...%s\n' \
		"$repo" "$prev_version" "$VERSION"
fi
