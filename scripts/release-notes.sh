#!/bin/sh
set -eu

version="${1#v}"
notes=$(awk -v v="$version" '
  index($0, "## [" v "]") == 1 { found = 1; next }
  found && /^## \[/ { exit }
  found { print }
' "$(dirname "$0")/../CHANGELOG.md" | sed -e '/./,$!d' | awk '{ lines[NR] = $0 } END { while (NR > 0 && lines[NR] == "") NR--; for (i = 1; i <= NR; i++) print lines[i] }')

if [ -z "$notes" ]; then
  echo "CHANGELOG.md has no entry for $version" >&2
  exit 1
fi
printf '%s\n' "$notes"
