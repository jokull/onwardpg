# Installation and release status

## Development installation

With Go installed, build a local binary from this checkout:

```sh
go install ./cmd/onwardpg
onwardpg plan --help
```

For integration and differential tests, provision PostgreSQL 14–18 as
appropriate and build the exact pinned Atlas reference with
`scripts/build-pinned-atlas.sh`.

## Release status

There is currently no published release artifact, package-manager formula,
container image, checksum manifest, signed provenance, or release automation.
The current target is a **developer preview** for local installation from a
clean checkout. It must not be presented as production-released until those
artifacts exist and are tested from a clean checkout.

The intended release contract is a versioned binary archive with SHA-256
checksums, a changelog, supported-version policy, reproducible build
instructions, and automated release verification. These are tracked as open
production-readiness work in `PLAN.md`.

The current preview history is recorded in the
[changelog](../CHANGELOG.md).
