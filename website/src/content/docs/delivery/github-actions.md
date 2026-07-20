---
title: GitHub Actions
description: Scaffold a deployment pipeline that sandwiches the app rollout between expand and contract.
---

The deployment graph should encode the compatibility contract directly:

```text
verify -> expand -> deploy app -> prove drain -> contract
```

The following workflow is a scaffold. Adapt authentication, artifact lookup, deployment, health checks, and drain evidence to your platform.

```yaml
name: Deploy with onwardpg

on:
  workflow_dispatch:
    inputs:
      bundle:
        description: Bundle directory name
        required: true

permissions:
  contents: read
  id-token: write

env:
  BUNDLE_DIR: migrations/onward/app/${{ inputs.bundle }}

jobs:
  verify:
    runs-on: ubuntu-latest
    services:
      postgres:
        image: postgres:18
        env:
          POSTGRES_PASSWORD: postgres
        ports: ["5432:5432"]
        options: >-
          --health-cmd "pg_isready -U postgres"
          --health-interval 2s
          --health-timeout 5s
          --health-retries 20
    env:
      ONWARDPG_SCRATCH_DATABASE_URL: postgres://postgres:postgres@localhost:5432/postgres
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: stable
      - run: go install github.com/jokull/onwardpg/cmd/onwardpg@v0.1.0-preview.1
      - run: onwardpg verify --bundle "${{ inputs.bundle }}" --check

  expand:
    needs: verify
    runs-on: ubuntu-latest
    environment: production
    steps:
      - uses: actions/checkout@v4
      - name: Authenticate to production
        run: ./scripts/auth-production
      - name: Apply expand
        env:
          DATABASE_URL: ${{ secrets.PRODUCTION_DATABASE_URL }}
        run: psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f "$BUNDLE_DIR/phases/expand.sql"

  deploy_app:
    needs: expand
    runs-on: ubuntu-latest
    environment: production
    steps:
      - uses: actions/checkout@v4
      - run: ./scripts/deploy-app

  drain:
    needs: deploy_app
    runs-on: ubuntu-latest
    environment: production
    steps:
      - uses: actions/checkout@v4
      - name: Prove old writers are gone
        run: ./scripts/wait-for-old-release-to-drain

  contract:
    needs: drain
    runs-on: ubuntu-latest
    environment: production
    steps:
      - uses: actions/checkout@v4
      - name: Authenticate to production
        run: ./scripts/auth-production
      - name: Apply contract
        env:
          DATABASE_URL: ${{ secrets.PRODUCTION_DATABASE_URL }}
        run: psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f "$BUNDLE_DIR/phases/contract.sql"
```

## Make drain evidence real

A fixed sleep is not proof. A useful drain gate checks platform state: zero old-version instances, no old workers consuming queues, connection age below the rollout boundary, and no incompatible writer telemetry. Keep contract behind a protected GitHub environment when a human must confirm those facts.

## Production hardening

- Download a reviewed, immutable bundle artifact rather than trusting a mutable workspace path.
- Use workload identity instead of long-lived database credentials where possible.
- Set appropriate `lock_timeout` and `statement_timeout` for each generated batch.
- Record the exact bundle digest and phase outcome in deployment receipts.
- Stop before contract if deployment or drain evidence is ambiguous; expand is designed to leave a compatibility window open.
