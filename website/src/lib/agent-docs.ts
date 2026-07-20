import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import path from 'node:path';

const site = 'https://onwardpg.solberg.is';
const rawDocs = import.meta.glob('../content/docs/**/*.md', {
  eager: true,
  import: 'default',
  query: '?raw',
}) as Record<string, string>;

const repositoryRoot = path.resolve(process.cwd(), '..');
const skillPath = path.join(repositoryRoot, 'skills/onwardpg/SKILL.md');
const referenceBasePath = path.join(repositoryRoot, 'skills/onwardpg/references');

const sectionLabels: Record<string, string> = {
  start: 'Start here',
  concepts: 'Core concepts',
  agents: 'Coding agents',
  guides: 'Framework guides',
  delivery: 'Delivery',
  reference: 'Reference',
};

const docOrder = [
  'start/introduction',
  'start/installation',
  'start/first-plan',
  'concepts/plan-command',
  'concepts/expand-contract',
  'concepts/decisions',
  'concepts/verification',
  'agents/agent-assisted-planning',
  'guides/drizzle',
  'guides/django',
  'guides/prisma',
  'guides/sqlalchemy',
  'delivery/github-actions',
  'delivery/production-runbook',
  'reference/cli',
  'reference/safety',
];

export interface AgentDoc {
  body: string;
  description: string;
  raw: string;
  section: string;
  slug: string;
  title: string;
}

function unquote(value: string): string {
  const trimmed = value.trim();
  if (
    (trimmed.startsWith('"') && trimmed.endsWith('"')) ||
    (trimmed.startsWith("'") && trimmed.endsWith("'"))
  ) {
    return trimmed.slice(1, -1);
  }
  return trimmed;
}

function parseMarkdown(path: string, raw: string): AgentDoc {
  const match = raw.match(/^---\r?\n([\s\S]*?)\r?\n---\r?\n/);
  const frontmatter = match?.[1] ?? '';
  const readField = (name: string) => {
    const field = frontmatter.match(new RegExp(`^${name}:\\s*(.+)$`, 'm'));
    return field ? unquote(field[1]) : '';
  };
  const slug = path.replace('../content/docs/', '').replace(/\.md$/, '');

  return {
    body: match ? raw.slice(match[0].length).trim() : raw.trim(),
    description: readField('description'),
    raw,
    section: slug.split('/')[0],
    slug,
    title: readField('title') || slug,
  };
}

export function getAgentDocs(): AgentDoc[] {
  return Object.entries(rawDocs)
    .map(([path, raw]) => parseMarkdown(path, raw))
    .sort((left, right) => {
      const leftIndex = docOrder.indexOf(left.slug);
      const rightIndex = docOrder.indexOf(right.slug);
      if (leftIndex === -1 && rightIndex === -1) return left.slug.localeCompare(right.slug);
      if (leftIndex === -1) return 1;
      if (rightIndex === -1) return -1;
      return leftIndex - rightIndex;
    });
}

export function getSkill(): string {
  return readFileSync(skillPath, 'utf8');
}

export function getSkillDigest(): string {
  return `sha256:${createHash('sha256').update(getSkill()).digest('hex')}`;
}

export function getSkillReferences(): Array<{ name: string; source: string }> {
  return ['decision-protocol', 'production-evidence', 'schema-states'].map((name) => ({
    name,
    source: readFileSync(path.join(referenceBasePath, `${name}.md`), 'utf8'),
  }));
}

export function renderLlmsTxt(): string {
  const docs = getAgentDocs();
  const sections = [...new Set(docs.map((doc) => doc.section))];
  const lines = [
    '# onwardpg',
    '',
    '> PostgreSQL migration planning around one evolving application deployment, with compatibility-aware expand/contract SQL, captured semantic decisions, and disposable verification.',
    '',
    `Start with [the onwardpg agent skill](${site}/skill.md) when operating onwardpg. It contains the workflow, constraints, and required evidence handoff. This file is the documentation directory, not the operating procedure.`,
    '',
  ];

  for (const section of sections) {
    lines.push(`## ${sectionLabels[section] ?? section}`);
    lines.push('');
    for (const doc of docs.filter((candidate) => candidate.section === section)) {
      lines.push(`- [${doc.title}](${site}/${doc.slug}.md): ${doc.description}`);
    }
    lines.push('');
  }

  lines.push('## Optional');
  lines.push('');
  lines.push(`- [Complete documentation corpus](${site}/llms-full.txt): Every public documentation page in one file; use only when bulk context is appropriate.`);
  lines.push('');
  return lines.join('\n');
}

export function renderLlmsFullTxt(): string {
  const sections = [
    '# onwardpg complete documentation',
    '',
    '> Bulk Markdown context for onwardpg. Prefer /skill.md plus targeted page Markdown when operating the CLI.',
    '',
    '## Agent operating skill',
    '',
    `Source: ${site}/skill.md`,
    '',
    getSkill().trim(),
    '',
  ];

  for (const doc of getAgentDocs()) {
    sections.push(`## Document: ${doc.title}`);
    sections.push('');
    sections.push(`Source: ${site}/${doc.slug}.md`);
    sections.push('');
    sections.push(doc.body);
    sections.push('');
  }

  return `${sections.join('\n')}\n`;
}

export function markdownResponse(source: string): Response {
  return new Response(source.endsWith('\n') ? source : `${source}\n`, {
    headers: {
      'Cache-Control': 'public, max-age=300',
      'Content-Type': 'text/markdown; charset=utf-8',
    },
  });
}
