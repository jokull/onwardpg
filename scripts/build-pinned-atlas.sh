#!/usr/bin/env bash
set -euo pipefail

commit="a5e0aecc2bb64143bf522734f8ad88e04885fca6"
source_dir="${ATLAS_SOURCE_DIR:-/tmp/atlas}"
output="${1:-.tools/atlas-pinned}"

if [[ ! -f "$source_dir/cmd/atlas/go.mod" ]]; then
  echo "Atlas source checkout not found at $source_dir" >&2
  exit 1
fi
if [[ "$(git -C "$source_dir" rev-parse HEAD)" != "$commit" ]]; then
  echo "Atlas checkout must be pinned to $commit" >&2
  exit 1
fi

mkdir -p "$(dirname "$output")"
output="$(cd "$(dirname "$output")" && pwd)/$(basename "$output")"
(cd "$source_dir/cmd/atlas" && go build -o "$output" .)
echo "built pinned Atlas at $output"
