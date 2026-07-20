import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

export default defineConfig({
  site: 'https://onwardpg.solberg.is',
  integrations: [
    starlight({
      title: 'onwardpg',
      description: 'PostgreSQL schema changes designed around the application deployment.',
      favicon: '/favicon.svg',
      logo: {
        light: './src/assets/wordmark-dark.svg',
        dark: './src/assets/wordmark-light.svg',
        replacesTitle: true,
      },
      social: [
        {
          icon: 'github',
          label: 'GitHub',
          href: 'https://github.com/jokull/onwardpg',
        },
      ],
      editLink: {
        baseUrl: 'https://github.com/jokull/onwardpg/edit/main/website/',
      },
      customCss: ['./src/styles/starlight.css'],
      sidebar: [
        {
          label: 'Start here',
          items: [
            { label: 'Introduction', slug: 'start/introduction' },
            { label: 'Install & configure', slug: 'start/installation' },
            { label: 'Your first plan', slug: 'start/first-plan' },
          ],
        },
        {
          label: 'Core concepts',
          items: [
            { label: 'The plan command', slug: 'concepts/plan-command' },
            { label: 'Expand → deploy → contract', slug: 'concepts/expand-contract' },
            { label: 'Contract readiness', slug: 'concepts/contract-readiness' },
            { label: 'Decisions and editable SQL', slug: 'concepts/decisions' },
            { label: 'What verification proves', slug: 'concepts/verification' },
          ],
        },
        {
          label: 'Framework guides',
          items: [
            { label: 'Drizzle', slug: 'guides/drizzle' },
            { label: 'Django', slug: 'guides/django' },
            { label: 'Prisma', slug: 'guides/prisma' },
            { label: 'SQLAlchemy & Alembic', slug: 'guides/sqlalchemy' },
          ],
        },
        {
          label: 'Coding agents',
          items: [
            { label: 'Agent-assisted planning', slug: 'agents/agent-assisted-planning' },
          ],
        },
        {
          label: 'Delivery',
          items: [
            { label: 'GitHub Actions', slug: 'delivery/github-actions' },
            { label: 'Production runbook', slug: 'delivery/production-runbook' },
          ],
        },
        {
          label: 'Reference',
          items: [
            { label: 'CLI', slug: 'reference/cli' },
            { label: 'Safety boundaries', slug: 'reference/safety' },
          ],
        },
      ],
    }),
  ],
});
