import { defineConfig } from "blume";

const previousPages: Record<string, string> = {
  "/start/comparison": "/#quick-start",
  "/start/first-plan": "/#quick-start",
  "/start/introduction": "/#quick-start",
  "/start/installation": "/#quick-start",
  "/agents/agent-assisted-planning": "/#work-with-a-coding-agent",
  "/guides/sqlalchemy": "/#use-your-frameworks-schema",
  "/guides/drizzle": "/#use-your-frameworks-schema",
  "/guides/django": "/#use-your-frameworks-schema",
  "/guides/prisma": "/#use-your-frameworks-schema",
  "/concepts/expand-contract": "/#required-columns",
  "/concepts/plan-command": "/#day-to-day-use",
  "/concepts/verification": "/#review-and-verify",
  "/concepts/contract-readiness": "/#deployment",
  "/concepts/decisions": "/#required-columns",
  "/delivery/production-runbook": "/#deployment",
  "/delivery/github-actions": "/#deployment",
  "/reference/cli": "/#reference",
  "/reference/safety": "/#reference",
  "/reference/generated-cli-help": "/#reference",
};

export default defineConfig({
  title: "onwardpg",
  description:
    "Plan and verify PostgreSQL schema migrations for rolling deployments.",
  logo: {
    href: "/",
    image: "/favicon.svg",
    text: "onwardpg",
  },
  github: {
    owner: "jokull",
    repo: "onwardpg",
    dir: "website",
  },
  content: {
    root: "src/content/docs",
    pages: "src/pages",
  },
  navigation: {
    sidebar: ["/"],
  },
  redirects: Object.entries(previousPages).flatMap(([from, to]) => [
    { from, to },
    { from: `${from}.md`, to: "/index.md" },
  ]),
  theme: {
    accent: {
      light: "#62715c",
      dark: "#c8b979",
    },
    background: {
      light: "#fbf7ed",
      dark: "#131915",
    },
    fonts: {
      body: "inter",
      display: "source-serif-4",
      mono: "ibm-plex-mono",
    },
    mode: "system",
    radius: "sm",
  },
  seo: {
    og: {
      enabled: true,
      // The renderer rejects the unquoted numeric family name in Source Serif 4.
      fonts: ["Inter"],
      logo: "/favicon.svg",
      palette: {
        accent: "#c8b979",
        background: "#29342e",
        foreground: "#f8f3e7",
        muted: "#bfc5ba",
        border: "#536253",
      },
      titles: {
        "/": "PostgreSQL migrations for rolling deployments",
      },
    },
    robots: true,
    sitemap: true,
    structuredData: true,
  },
  agents: {
    llmsTxt: true,
  },
  deployment: {
    site: "https://onwardpg.solberg.is",
  },
});
