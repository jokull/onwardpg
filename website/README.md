# onwardpg documentation site

The site is built with Astro and Starlight.

```sh
pnpm install
pnpm dev
pnpm check
pnpm build
```

Deploy the static `dist/` directory to Cloudflare Workers Assets. The configured
Worker Custom Domain owns `onwardpg.solberg.is` and its DNS record:

```sh
pnpm exec wrangler deploy
```

The `onwardpg-docs` Pages project is also available as a fallback preview; it is
not responsible for the custom domain.
