# Installation and releases

## Build from source

With the Go toolchain declared in `go.mod` installed, clone the preview source
and install the local command:

```sh
git clone https://github.com/jokull/onwardpg.git
cd onwardpg
go install ./cmd/onwardpg
onwardpg version
```

Before the first public tag, this is the only supported installation path.
To build an explicit local binary instead:

```sh
go build -trimpath -o ./bin/onwardpg ./cmd/onwardpg
./bin/onwardpg version
```

A source build reports
`{"protocol_version":"onwardpg.version/v1","status":"ok","version":"dev"}`
unless the build supplies `-X main.buildVersion=...`.

## Preview archives (after the first tag)

When a preview tag is published, it will publish `.tar.gz` archives for:

- Darwin amd64 and arm64;
- Linux amd64 and arm64; and
- Windows amd64 and arm64.

After that release exists, download the archive and `checksums.txt` from it,
then verify before extracting:

```sh
sha256sum --check checksums.txt
tar -xzf onwardpg_VERSION_OS_ARCH.tar.gz
./onwardpg_VERSION_OS_ARCH/onwardpg version
```

On macOS, use `shasum -a 256 --check checksums.txt`.

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
the same `checksums.txt`. GNU tar or BSD tar is supported.

## PostgreSQL requirements

onwardpg supports PostgreSQL 15–18. The PostgreSQL major is discovered from the
configured scratch server and receipted automatically. The development URL may
be read-only; the scratch URL must be able to create and force-drop disposable
databases.

See [PostgreSQL version policy](postgresql-version-policy.md) and
[Schema inputs](schema-inputs.md).

## Release process

Pushing a SemVer-shaped `v*` tag triggers
[the release workflow](../.github/workflows/release.yml). It runs quality
checks, builds deterministic archives, verifies checksums and embedded version
metadata, then creates the GitHub release. Release application or database
deployment is not part of this workflow.

No preview tag has been published yet. The source is available under the
[MIT License](../LICENSE); until a tag exists, installation from a clean
checkout is the preview evaluation path.
