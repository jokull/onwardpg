import { createHash } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import path from 'node:path';

const websiteRoot = process.cwd();
const repositoryRoot = path.resolve(websiteRoot, '..');
const docsRoot = path.join(websiteRoot, 'src/content/docs');
const publicRoot = path.join(websiteRoot, 'public');
const site = 'https://onwardpg.solberg.is';

const guide = await readFile(path.join(docsRoot, 'index.md'), 'utf8');
const guideBody = guide.replace(/^---\r?\n[\s\S]*?\r?\n---\r?\n/, '').trim();

const skillPath = path.join(repositoryRoot, 'skills/onwardpg/SKILL.md');
const skill = await readFile(skillPath, 'utf8');
const skillDigest = `sha256:${createHash('sha256').update(skill).digest('hex')}`;
const referenceNames = ['decision-protocol', 'production-evidence', 'schema-states'];

const llmsLines = [
  '# onwardpg',
  '',
  '> Plan and verify PostgreSQL schema migrations for rolling deployments.',
  '',
  `- [Quick start and guide](${site}/): Setup, first migration, required columns, and deployment. [Markdown](${site}/index.md).`,
  `- [Agent skill](${site}/skill.md): Instructions for agents operating the CLI.`,
  `- [Full text](${site}/llms-full.txt): The guide and agent skill in one file.`,
  '',
];

const llmsFullSections = [
  '# onwardpg documentation',
  '',
  `Source: ${site}/index.md`,
  '',
  guideBody,
  '',
  '## Agent skill',
  '',
  `Source: ${site}/skill.md`,
  '',
  skill.trim(),
  '',
];

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
  `prepared agent docs: one page, one skill, ${referenceNames.length} references`,
);
