# onwardpg documentation site

The site is one page, authored in `src/content/docs/index.md` and built with
[Blume](https://useblume.dev/). Old documentation URLs redirect to sections of
that page. Detailed engineering references live in the repository’s `docs/`.
Blume provides search, a Markdown mirror, and the Open Graph image.

```sh
pnpm install
pnpm dev
pnpm check
pnpm build
pnpm validate
pnpm audit:site
pnpm check:agent-docs
```

`generate:cli-docs` builds the current onwardpg binary and refreshes the tracked
`../docs/generated-cli-help.md` reference. `dev` refreshes it automatically;
`check` and `build` fail if it is stale so published flags and defaults cannot
drift from the executable.

`prepare:agent-docs` runs automatically before development, checks, and builds.
It publishes the canonical skill and its references from `../skills/onwardpg`,
plus the curated `llms.txt` surfaces. Do not edit their generated copies under
`public/`.

A scheduled workflow (`.github/workflows/website-audit.yml`) runs
`pnpm audit --audit-level high` each day and on changes to this directory. It is
not part of pull request CI or of a release: the site is not in the binary, and
a new advisory in a site dependency must not stop a CLI change. Blume bundles
optional server-side AI integrations, but onwardpg keeps those disabled and
ships only static files.

Deploy the static `dist/` directory to Cloudflare Workers Assets. The configured
Worker Custom Domain owns `onwardpg.solberg.is` and its DNS record:

```sh
pnpm exec wrangler deploy
```

The `onwardpg-docs` Pages project is also available as a fallback preview; it is
not responsible for the custom domain.
