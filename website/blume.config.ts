import { defineConfig } from "blume";

export default defineConfig({
  title: "onwardpg",
  description:
    "Plan one evolving PostgreSQL migration around one compatibility-safe application deployment.",
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
    sidebar: [
      {
        label: "Start here",
        items: [
          "/start/introduction",
          "/start/installation",
          "/start/first-plan",
        ],
      },
      {
        label: "Core concepts",
        items: [
          "/concepts/plan-command",
          "/concepts/expand-contract",
          "/concepts/contract-readiness",
          "/concepts/decisions",
          "/concepts/verification",
        ],
      },
      {
        label: "Framework guides",
        items: [
          "/guides/drizzle",
          "/guides/django",
          "/guides/prisma",
          "/guides/sqlalchemy",
        ],
      },
      {
        label: "Coding agents",
        items: ["/agents/agent-assisted-planning"],
      },
      {
        label: "Delivery",
        items: [
          "/delivery/github-actions",
          "/delivery/production-runbook",
        ],
      },
      {
        label: "Reference",
        items: ["/reference/cli", "/reference/generated-cli-help", "/reference/safety"],
      },
    ],
  },
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
      logo: "/favicon.svg",
      palette: {
        accent: "#c8b979",
        background: "#29342e",
        foreground: "#f8f3e7",
        muted: "#bfc5ba",
        border: "#536253",
      },
      titles: {
        "/": "One evolving PostgreSQL migration",
      },
    },
    robots: true,
    sitemap: true,
    structuredData: true,
  },
  ai: {
    llmsTxt: true,
  },
  deployment: {
    output: "static",
    site: "https://onwardpg.solberg.is",
  },
});
