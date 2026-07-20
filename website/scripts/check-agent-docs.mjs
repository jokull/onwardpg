import { createHash } from 'node:crypto';
import { readFile, readdir } from 'node:fs/promises';
import path from 'node:path';

const websiteRoot = process.cwd();
const repositoryRoot = path.resolve(websiteRoot, '..');
const distRoot = path.join(websiteRoot, 'dist');

const skillSource = await readFile(path.join(repositoryRoot, 'skills/onwardpg/SKILL.md'), 'utf8');
const builtSkill = await readFile(path.join(distRoot, 'skill.md'), 'utf8');
if (builtSkill !== skillSource) {
  throw new Error('dist/skill.md does not match the canonical skill source');
}

const discovery = JSON.parse(
  await readFile(path.join(distRoot, '.well-known/agent-skills/index.json'), 'utf8'),
);
const expectedDigest = `sha256:${createHash('sha256').update(skillSource).digest('hex')}`;
if (discovery.skills?.[0]?.digest !== expectedDigest) {
  throw new Error('agent skill discovery digest does not match skill.md');
}

const llms = await readFile(path.join(distRoot, 'llms.txt'), 'utf8');
const docsRoot = path.join(websiteRoot, 'src/content/docs');
const markdownDocs = [];

async function collect(directory) {
  for (const entry of await readdir(directory, { withFileTypes: true })) {
    const resolved = path.join(directory, entry.name);
    if (entry.isDirectory()) await collect(resolved);
    if (entry.isFile() && entry.name.endsWith('.md')) markdownDocs.push(resolved);
  }
}

await collect(docsRoot);
for (const sourcePath of markdownDocs) {
  const slug = path.relative(docsRoot, sourcePath).replaceAll(path.sep, '/').replace(/\.md$/, '');
  const outputPath = path.join(distRoot, `${slug}.md`);
  const built = await readFile(outputPath, 'utf8');
  const source = await readFile(sourcePath, 'utf8');
  if (built !== source) throw new Error(`${slug}.md does not match its source page`);
  if (!llms.includes(`/${slug}.md`)) throw new Error(`llms.txt does not index ${slug}.md`);
}

for (const required of [
  'onwardpg plan',
  'onwardpg verify',
  'Never run generated phase SQL',
  'Do not describe the migration as safe',
]) {
  if (!skillSource.includes(required)) throw new Error(`skill is missing required contract: ${required}`);
}

console.log(`agent docs verified: ${markdownDocs.length} pages, one skill, matching discovery digest`);
