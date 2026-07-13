#!/usr/bin/env bash
set -euo pipefail

version=${1:-}
output=${2:-dist}
if [[ ! $version =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]]; then
  echo "usage: scripts/build-release.sh vX.Y.Z[-preview.N] [OUTPUT_DIR]" >&2
  exit 2
fi

rm -rf "$output"
mkdir -p "$output"
output=$(cd "$output" && pwd)
stage=$(mktemp -d)
cleanup() { rm -rf "$stage"; }
trap cleanup EXIT

targets=(
  darwin/amd64
  darwin/arm64
  linux/amd64
  linux/arm64
  windows/amd64
  windows/arm64
)

for target in "${targets[@]}"; do
  os=${target%/*}
  arch=${target#*/}
  name="onwardpg_${version#v}_${os}_${arch}"
  directory="$stage/$name"
  mkdir -p "$directory"
  binary=onwardpg
  if [[ $os == windows ]]; then
    binary=onwardpg.exe
  fi
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build \
    -trimpath \
    -ldflags "-s -w -X main.buildVersion=$version" \
    -o "$directory/$binary" ./cmd/onwardpg
  cp README.md CHANGELOG.md THIRD_PARTY_NOTICES.md "$directory/"
  if [[ -f LICENSE ]]; then
    cp LICENSE "$directory/"
  fi
  find "$directory" -exec touch -t 197001010000 {} +
  if tar --version 2>/dev/null | grep -q 'GNU tar'; then
    (
      cd "$stage"
      find "$name" -print | LC_ALL=C sort | \
        tar --format=ustar --no-recursion --mtime=@0 --owner=0 --group=0 \
          --numeric-owner -cf - -T -
    ) | gzip -n > "$output/$name.tar.gz"
  else
    (
      cd "$stage"
      find "$name" -print | LC_ALL=C sort | \
        tar --format=ustar --no-recursion --uid 0 --gid 0 --uname root \
          --gname root -cf - -T -
    ) | gzip -n > "$output/$name.tar.gz"
  fi
done

(
  cd "$output"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum ./*.tar.gz > checksums.txt
    sha256sum --check checksums.txt
  else
    shasum -a 256 ./*.tar.gz > checksums.txt
    shasum -a 256 --check checksums.txt
  fi
)
