import { createHash } from 'node:crypto';
import { mkdir, readFile, readdir, writeFile } from 'node:fs/promises';
import path from 'node:path';

const websiteRoot = process.cwd();
const repositoryRoot = path.resolve(websiteRoot, '..');
const docsRoot = path.join(websiteRoot, 'src/content/docs');
const publicRoot = path.join(websiteRoot, 'public');
const site = 'https://onwardpg.solberg.is';

const sectionLabels = {
  start: 'Start here',
  concepts: 'Core concepts',
  guides: 'Framework guides',
  agents: 'Coding agents',
  delivery: 'Delivery',
  reference: 'Reference',
};

const docOrder = [
  'start/introduction',
  'start/installation',
  'start/first-plan',
  'concepts/plan-command',
  'concepts/expand-contract',
  'concepts/contract-readiness',
  'concepts/decisions',
  'concepts/verification',
  'guides/drizzle',
  'guides/django',
  'guides/prisma',
  'guides/sqlalchemy',
  'agents/agent-assisted-planning',
  'delivery/github-actions',
  'delivery/production-runbook',
  'reference/cli',
  'reference/generated-cli-help',
  'reference/safety',
];

async function collectMarkdown(directory, output = []) {
  for (const entry of await readdir(directory, { withFileTypes: true })) {
    const resolved = path.join(directory, entry.name);
    if (entry.isDirectory()) await collectMarkdown(resolved, output);
    if (entry.isFile() && /\.mdx?$/.test(entry.name)) output.push(resolved);
  }
  return output;
}

function unquote(value) {
  const trimmed = value.trim();
  if (
    (trimmed.startsWith('"') && trimmed.endsWith('"')) ||
    (trimmed.startsWith("'") && trimmed.endsWith("'"))
  ) {
    return trimmed.slice(1, -1);
  }
  return trimmed;
}

function parseDocument(sourcePath, raw) {
  const frontmatterMatch = raw.match(/^---\r?\n([\s\S]*?)\r?\n---\r?\n/);
  const frontmatter = frontmatterMatch?.[1] ?? '';
  const readField = (name) => {
    const match = frontmatter.match(new RegExp(`^${name}:\\s*(.+)$`, 'm'));
    return match ? unquote(match[1]) : '';
  };
  const slug = path
    .relative(docsRoot, sourcePath)
    .replaceAll(path.sep, '/')
    .replace(/\.mdx?$/, '');

  return {
    body: frontmatterMatch ? raw.slice(frontmatterMatch[0].length).trim() : raw.trim(),
    description: readField('description'),
    raw,
    section: slug.split('/')[0],
    slug,
    title: readField('title') || slug,
  };
}

const documents = await Promise.all(
  (await collectMarkdown(docsRoot)).map(async (sourcePath) =>
    parseDocument(sourcePath, await readFile(sourcePath, 'utf8')),
  ),
);
documents.sort((left, right) => docOrder.indexOf(left.slug) - docOrder.indexOf(right.slug));

const skillPath = path.join(repositoryRoot, 'skills/onwardpg/SKILL.md');
const skill = await readFile(skillPath, 'utf8');
const skillDigest = `sha256:${createHash('sha256').update(skill).digest('hex')}`;
const referenceNames = ['decision-protocol', 'production-evidence', 'schema-states'];

const llmsLines = [
  '# onwardpg',
  '',
  '> PostgreSQL migration planning around one evolving application deployment, with compatibility-aware expand/contract SQL, captured semantic decisions, and disposable verification.',
  '',
  `Start with [the onwardpg agent skill](${site}/skill.md) when operating onwardpg. It contains the workflow, constraints, and required evidence handoff. This file is the documentation directory, not the operating procedure.`,
  '',
];

for (const section of Object.keys(sectionLabels)) {
  const sectionDocuments = documents.filter((document) => document.section === section);
  if (sectionDocuments.length === 0) continue;
  llmsLines.push(`## ${sectionLabels[section]}`, '');
  for (const document of sectionDocuments) {
    llmsLines.push(
      `- [${document.title}](${site}/${document.slug}): ${document.description} ([raw Markdown](${site}/${document.slug}.md))`,
    );
  }
  llmsLines.push('');
}

llmsLines.push(
  '## Optional',
  '',
  `- [Complete documentation corpus](${site}/llms-full.txt): Every public documentation page in one file; use only when bulk context is appropriate.`,
  '',
);

const llmsFullSections = [
  '# onwardpg complete documentation',
  '',
  '> Bulk Markdown context for onwardpg. Prefer /skill.md plus targeted page Markdown when operating the CLI.',
  '',
  '## Agent operating skill',
  '',
  `Source: ${site}/skill.md`,
  '',
  skill.trim(),
  '',
];

for (const document of documents) {
  llmsFullSections.push(
    `## Document: ${document.title}`,
    '',
    `Source: ${site}/${document.slug}.md`,
    '',
    document.body,
    '',
  );
}

const discovery = {
  $schema: 'https://schemas.agentskills.io/discovery/0.2.0/schema.json',
  skills: [
    {
      name: 'onwardpg',
      type: 'skill-md',
      description:
        'Plan, revise, restack, and verify compatibility-aware PostgreSQL migrations with onwardpg.',
      url: '/.well-known/agent-skills/onwardpg/SKILL.md',
      digest: skillDigest,
    },
  ],
};

await Promise.all([
  mkdir(path.join(publicRoot, '.well-known/agent-skills/onwardpg'), { recursive: true }),
  mkdir(path.join(publicRoot, 'references'), { recursive: true }),
]);

await Promise.all([
  writeFile(path.join(publicRoot, 'skill.md'), skill),
  writeFile(path.join(publicRoot, '.well-known/agent-skills/onwardpg/SKILL.md'), skill),
  writeFile(
    path.join(publicRoot, '.well-known/agent-skills/index.json'),
    `${JSON.stringify(discovery, null, 2)}\n`,
  ),
  writeFile(path.join(publicRoot, 'llms.txt'), llmsLines.join('\n')),
  writeFile(path.join(publicRoot, 'llms-full.txt'), `${llmsFullSections.join('\n')}\n`),
  ...referenceNames.map(async (name) => {
    const source = await readFile(
      path.join(repositoryRoot, `skills/onwardpg/references/${name}.md`),
      'utf8',
    );
    await writeFile(path.join(publicRoot, `references/${name}.md`), source);
  }),
]);

console.log(
  `prepared agent docs: ${documents.length} pages, one skill, ${referenceNames.length} references`,
);
