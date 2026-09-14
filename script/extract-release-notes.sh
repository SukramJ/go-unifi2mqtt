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

# Assemble the full payload: the section body, then the compare link.
# The first release has no predecessor — that's fine, just skip the link.
payload="$body"

# Changelog headers carry a bare version ("# Version 1.2.0"); the tags
# they correspond to are v-prefixed ("v1.2.0"). Comparing the bare form
# 404s, which is what every release before v1.2.1 shipped. Resolve each
# side to a ref that actually exists, preferring the v-prefixed tag.
tag_ref() {
	if git rev-parse -q --verify "refs/tags/v$1" >/dev/null 2>&1; then
		printf 'v%s' "$1"
	elif git rev-parse -q --verify "refs/tags/$1" >/dev/null 2>&1; then
		printf '%s' "$1"
	else
		# Not fetched (shallow clone, or the tag for the release being
		# built is created by the push that triggers us). Assume the
		# convention every tag in this repo follows.
		printf 'v%s' "$1"
	fi
}

if [ -n "$prev_version" ]; then
	repo="${GITHUB_REPOSITORY:-SukramJ/go-unifi2mqtt}"
	from_ref=$(tag_ref "$prev_version")
	to_ref="${GITHUB_REF_NAME:-$(tag_ref "$VERSION")}"
	link=$(printf '\n**Full Changelog**: https://github.com/%s/compare/%s...%s' \
		"$repo" "$from_ref" "$to_ref")
	payload="${payload}
${link}"
fi

# GitHub rejects a release body over 125,000 characters with a 422, and
# by then the tag is already on the remote — the release step is the
# last thing that runs. Trim to a safe margin with a pointer to the full
# entry instead of letting the upload fail after the fact.
MAX_BYTES="${MAX_BYTES:-118000}"
size=$(printf '%s\n' "$payload" | LC_ALL=C wc -c | tr -d '[:space:]')

if [ "$size" -le "$MAX_BYTES" ]; then
	printf '%s\n' "$payload"
	exit 0
fi

repo="${GITHUB_REPOSITORY:-SukramJ/go-unifi2mqtt}"
ref="${GITHUB_REF_NAME:-v$VERSION}"
# Built with a double-quoted format argument so the ${...} expansions
# below actually expand — a single-quoted printf format would emit them
# literally.
notice=$(printf '\n---\n\n*Release note trimmed to fit GitHub'\''s 125,000-character\nrelease-body limit (full section: %s bytes). The complete entry is in\n[changelog.md](https://github.com/%s/blob/%s/changelog.md).*\n' \
	"$size" "$repo" "$ref")
notice_bytes=$(printf '%s\n' "$notice" | LC_ALL=C wc -c | tr -d '[:space:]')
keep=$((MAX_BYTES - notice_bytes))

# Trim on a line boundary, counting bytes (LC_ALL=C), so the cut never
# lands inside a multi-byte character or mid-sentence.
printf '%s\n' "$payload" | LC_ALL=C awk -v max="$keep" '
	{ n += length($0) + 1; if (n > max) exit; print }
'
printf '%s\n' "$notice"

echo "warning: release notes for $VERSION trimmed from $size to ~$MAX_BYTES bytes" >&2
