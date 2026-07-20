# Installation and releases

## Homebrew

Homebrew is the recommended installation path on macOS and Linux. The public
tap consumes the same checksummed archives published on GitHub:

```sh
brew install jokull/tap/onwardpg
onwardpg version
```

Upgrade with `brew upgrade onwardpg`.

## Go install

With the Go toolchain declared in `go.mod` available, install a tagged version
without cloning the repository:

```sh
go install github.com/jokull/onwardpg/cmd/onwardpg@v0.1.0-preview.1
onwardpg version
```

Tagged module builds discover their module version through Go build metadata,
so `onwardpg version` reports the release tag even without release-workflow
linker flags.

## Build from source

To work from an unpublished checkout, clone the source and install the local
command:

```sh
git clone https://github.com/jokull/onwardpg.git
cd onwardpg
go install ./cmd/onwardpg
onwardpg version
```

To build an explicit local binary instead:

```sh
go build -trimpath -o ./bin/onwardpg ./cmd/onwardpg
./bin/onwardpg version
```

A source build reports
`{"protocol_version":"onwardpg.version/v1","status":"ok","build":{"version":"dev","commit":"...","dirty":true,"go_version":"...","supported_postgres_majors":[15,16,17,18]}}`
unless the build supplies `-X main.buildVersion=...`.

## Preview archives

Each preview tag publishes `.tar.gz` archives for:

- Darwin amd64 and arm64;
- Linux amd64 and arm64; and
- Windows amd64 and arm64.

Download the archive and `checksums.txt` from the [GitHub
release](https://github.com/jokull/onwardpg/releases), then verify before
extracting:

```sh
sha256sum --check checksums.txt
tar -xzf onwardpg_VERSION_OS_ARCH.tar.gz
./onwardpg_VERSION_OS_ARCH/onwardpg version
```

On macOS, use `shasum -a 256 --check checksums.txt`.

GitHub also records build-provenance attestations for the release assets. With
the GitHub CLI installed, verify an archive with:

```sh
gh attestation verify onwardpg_0.1.0-preview.1_darwin_arm64.tar.gz \
  --repo jokull/onwardpg
```

The archives include README, changelog, third-party notices, and the project
license required by the release workflow. The embedded version must equal the
release tag.

## Reproduce a release build

The release workflow calls the same repository script available locally:

```sh
scripts/build-release.sh v0.1.0-preview.1 ./dist
```

The builder uses `CGO_ENABLED=0`, `-trimpath`, stable archive metadata, and
`gzip -n`. Running it twice from the same source and Go toolchain must produce
the same `checksums.txt` and Homebrew Formula. GNU tar or BSD tar is supported.

## PostgreSQL requirements

onwardpg supports PostgreSQL 15–18. The PostgreSQL major is discovered from the
configured scratch server and receipted automatically. The development URL may
be read-only. The scratch URL is an administrator for a dedicated local or CI
cluster and must be able to create and force-drop disposable databases and
short-lived login roles. Project DDL runs as the random database owner, which
has no cluster-global authority. Do not use a production or shared application
cluster. Scratch creation copies the connected control database's encoding and
locale environment and rejects a collation-version mismatch. Referenced roles,
languages, and extension packages must already be
available; extensions requiring superuser execution cannot be materialized by
the restricted owner.

See [PostgreSQL version policy](postgresql-version-policy.md) and
[Schema inputs](schema-inputs.md).

## Release process

Pushing a SemVer-shaped `v*` tag triggers
[the release workflow](../.github/workflows/release.yml). It runs quality
checks, builds deterministic archives and a tap Formula, verifies checksums and
embedded version metadata, records provenance attestations, then creates the
GitHub release. The generated `onwardpg.rb` is published to
[`jokull/homebrew-tap`](https://github.com/jokull/homebrew-tap) after the
release assets exist. Release application or database deployment is not part
of this workflow.

The source and release artifacts are available under the [MIT
License](../LICENSE).
