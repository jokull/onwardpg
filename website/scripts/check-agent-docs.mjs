import { createHash } from 'node:crypto';
import { readFile, readdir, stat } from 'node:fs/promises';
import path from 'node:path';

const websiteRoot = process.cwd();
const repositoryRoot = path.resolve(websiteRoot, '..');
const distRoot = path.join(websiteRoot, 'dist');
const docsRoot = path.join(websiteRoot, 'src/content/docs');
const site = 'https://onwardpg.solberg.is';

const skillSource = await readFile(path.join(repositoryRoot, 'skills/onwardpg/SKILL.md'), 'utf8');
for (const builtPath of [
  'skill.md',
  '.well-known/agent-skills/onwardpg/SKILL.md',
]) {
  const builtSkill = await readFile(path.join(distRoot, builtPath), 'utf8');
  if (builtSkill !== skillSource) throw new Error(`${builtPath} does not match the canonical skill`);
}

const discovery = JSON.parse(
  await readFile(path.join(distRoot, '.well-known/agent-skills/index.json'), 'utf8'),
);
const expectedDigest = `sha256:${createHash('sha256').update(skillSource).digest('hex')}`;
if (discovery.skills?.[0]?.digest !== expectedDigest) {
  throw new Error('agent skill discovery digest does not match skill.md');
}

for (const name of ['decision-protocol', 'production-evidence', 'schema-states']) {
  const source = await readFile(
    path.join(repositoryRoot, `skills/onwardpg/references/${name}.md`),
    'utf8',
  );
  const built = await readFile(path.join(distRoot, `references/${name}.md`), 'utf8');
  if (built !== source) throw new Error(`references/${name}.md does not match its source`);
}

const llms = await readFile(path.join(distRoot, 'llms.txt'), 'utf8');
const llmsFull = await readFile(path.join(distRoot, 'llms-full.txt'), 'utf8');
if (!llms.includes(`${site}/skill.md`)) throw new Error('llms.txt does not point agents to skill.md');
if (!llmsFull.includes(skillSource.trim())) throw new Error('llms-full.txt does not embed the operating skill');

const markdownDocs = [];
async function collect(directory) {
  for (const entry of await readdir(directory, { withFileTypes: true })) {
    const resolved = path.join(directory, entry.name);
    if (entry.isDirectory()) await collect(resolved);
    if (entry.isFile() && /\.mdx?$/.test(entry.name)) markdownDocs.push(resolved);
  }
}
await collect(docsRoot);

function pngDimensions(buffer) {
  const signature = '89504e470d0a1a0a';
  if (buffer.subarray(0, 8).toString('hex') !== signature) throw new Error('invalid PNG signature');
  return { width: buffer.readUInt32BE(16), height: buffer.readUInt32BE(20) };
}

const sitemap = await readFile(path.join(distRoot, 'sitemap.xml'), 'utf8');
for (const sourcePath of markdownDocs) {
  const slug = path
    .relative(docsRoot, sourcePath)
    .replaceAll(path.sep, '/')
    .replace(/\.mdx?$/, '');
  const source = await readFile(sourcePath, 'utf8');
  const rawMarkdown = await readFile(path.join(distRoot, `${slug}.md`), 'utf8');
  if (rawMarkdown !== source) throw new Error(`${slug}.md does not match its canonical source page`);
  if (!llms.includes(`${site}/${slug}.md`)) throw new Error(`llms.txt does not index ${slug}.md`);

  const pageHtml = await readFile(path.join(distRoot, slug, 'index.html'), 'utf8');
  const ogUrl = `${site}/og/${slug}.png`;
  if (!pageHtml.includes('property="og:image"') || !pageHtml.includes(ogUrl)) {
    throw new Error(`${slug} does not advertise its generated Open Graph image`);
  }
  if (!pageHtml.includes('1200') || !pageHtml.includes('630')) {
    throw new Error(`${slug} does not advertise Open Graph image dimensions`);
  }
  if (!sitemap.includes(`${site}/${slug}`)) throw new Error(`sitemap does not index ${slug}`);

  const ogPath = path.join(distRoot, 'og', `${slug}.png`);
  const dimensions = pngDimensions(await readFile(ogPath));
  if (dimensions.width !== 1200 || dimensions.height !== 630) {
    throw new Error(`${slug} Open Graph image is ${dimensions.width}x${dimensions.height}`);
  }
  if ((await stat(ogPath)).size < 1_000) throw new Error(`${slug} Open Graph image is unexpectedly small`);
}

const homepage = await readFile(path.join(distRoot, 'index.html'), 'utf8');
const homeOgUrl = `${site}/og/index.png`;
if (!homepage.includes('property="og:image"') || !homepage.includes(homeOgUrl)) {
  throw new Error('homepage does not advertise its generated Open Graph image');
}
const homeDimensions = pngDimensions(await readFile(path.join(distRoot, 'og/index.png')));
if (homeDimensions.width !== 1200 || homeDimensions.height !== 630) {
  throw new Error(`homepage Open Graph image is ${homeDimensions.width}x${homeDimensions.height}`);
}

const readability = JSON.parse(await readFile(path.join(distRoot, 'agent-readability.json'), 'utf8'));
if (readability.generator !== 'blume@1.1.2') throw new Error('agent-readability.json has the wrong generator');
if (readability.artifacts?.markdown?.pattern !== `${site}/{route}.md`) {
  throw new Error('agent-readability.json does not advertise raw Markdown routes');
}
if (readability.artifacts?.llmsTxt !== `${site}/llms.txt`) {
  throw new Error('agent-readability.json does not advertise llms.txt');
}

for (const required of [
  'onwardpg plan',
  'onwardpg verify',
  'Never run generated phase SQL',
  'Do not describe the migration as safe',
]) {
  if (!skillSource.includes(required)) throw new Error(`skill is missing required contract: ${required}`);
}

console.log(
  `Blume docs verified: ${markdownDocs.length} pages, ${markdownDocs.length + 1} generated 1200x630 OG images, one canonical skill, and matching agent discovery`,
);
