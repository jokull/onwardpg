# onwardpg documentation site

The site is built with [Blume](https://useblume.dev/). Markdown and MDX remain
repository-owned; Blume provides the docs shell, search, raw Markdown mirrors,
agent-readable discovery, and generated 1200×630 Open Graph images.

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
`reference/generated-cli-help.md` page. `dev` refreshes it automatically;
`check` and `build` fail if it is stale so published flags and defaults cannot
drift from the executable.

`prepare:agent-docs` runs automatically before development, checks, and builds.
It publishes the canonical skill and its references from `../skills/onwardpg`,
plus the curated `llms.txt` surfaces. Do not edit their generated copies under
`public/`.

CI also runs `pnpm audit --audit-level high`. Blume bundles optional server-side
AI integrations, but onwardpg keeps those disabled and ships only static files.

Deploy the static `dist/` directory to Cloudflare Workers Assets. The configured
Worker Custom Domain owns `onwardpg.solberg.is` and its DNS record:

```sh
pnpm exec wrangler deploy
```

The `onwardpg-docs` Pages project is also available as a fallback preview; it is
not responsible for the custom domain.
