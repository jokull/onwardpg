#!/usr/bin/env bash
set -euo pipefail

# Work from the tracked Go sources, but tolerate a local worktree with deleted
# files. GitHub Actions runs this against a clean checkout; the existence check
# makes the same gate useful while a refactor is in progress locally.
files=()
while IFS= read -r -d '' file; do
  [[ -f "$file" ]] && files+=("$file")
done < <(git ls-files -z -- '*.go')

if ((${#files[@]} == 0)); then
  exit 0
fi

unformatted="$(gofmt -l "${files[@]}")"
if [[ -n "$unformatted" ]]; then
  echo "Go files that must be formatted with gofmt:" >&2
  printf '%s\n' "$unformatted" >&2
  exit 1
fi
