#!/usr/bin/env bash
set -euo pipefail

readonly expected_commit="6208f8f3ceccae8ca634055dc47907a6a864cb76"
readonly checkout="${1:-.stripe-pg-schema-diff}"
readonly output="${2:-.tools/pg-schema-diff-v1.0.7}"

actual_commit="$(git -C "$checkout" rev-parse HEAD)"
if [[ "$actual_commit" != "$expected_commit" ]]; then
  echo "Stripe reference checkout is $actual_commit; expected $expected_commit" >&2
  exit 1
fi

mkdir -p "$(dirname "$output")"
(
  cd "$checkout/cmd/pg-schema-diff"
  go build -trimpath -o "$OLDPWD/$output" .
)
