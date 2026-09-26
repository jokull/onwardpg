import { readFile, readdir } from 'node:fs/promises';
import path from 'node:path';
import process from 'node:process';

const repositoryRoot = process.cwd();

async function collect(directory, predicate, output = []) {
  for (const entry of await readdir(directory, { withFileTypes: true })) {
    const resolved = path.join(directory, entry.name);
    if (entry.isDirectory()) await collect(resolved, predicate, output);
    if (entry.isFile() && predicate(entry.name)) output.push(resolved);
  }
  return output;
}

const documentationPaths = [
  path.join(repositoryRoot, 'README.md'),
  ...(await collect(path.join(repositoryRoot, 'docs'), (name) => name.endsWith('.md'))),
  ...(await collect(path.join(repositoryRoot, 'examples'), (name) => name.endsWith('.md'))),
  ...(await collect(path.join(repositoryRoot, 'skills'), (name) => name.endsWith('.md'))),
  ...(await collect(path.join(repositoryRoot, 'website/src/content/docs'), (name) => /\.mdx?$/.test(name))),
];
const documents = new Map();
for (const documentPath of documentationPaths) {
  documents.set(documentPath, await readFile(documentPath, 'utf8'));
}

function fail(message) {
  throw new Error(message);
}

for (const [documentPath, body] of documents) {
  const relative = path.relative(repositoryRoot, documentPath);
  if (
    relative.startsWith(path.join('website', 'src', 'content', 'docs')) &&
    documentPath.endsWith('.md') &&
    /^:::[a-z]/m.test(body)
  ) {
    fail(`${relative} uses an MDX-only Blume directive but has a .md extension`);
  }
  if (/onwardpg verify \[(?:name|NAME)\]/.test(body)) {
    fail(`${relative} documents the rejected positional verify syntax`);
  }
  if (/onwardpg verify add-[a-z0-9-]+/.test(body)) {
    fail(`${relative} passes a positional bundle name to verify`);
  }
  if (body.includes('protocol_version') || /onwardpg\.[a-z-]+\/v\d+/.test(body)) {
    fail(`${relative} references a speculative protocol version`);
  }
}

const guide = documents.get(path.join(repositoryRoot, 'website/src/content/docs/index.md'));
function hasSQLFence(markdown, body) {
  return markdown.includes(`\`\`\`sql\n${body.trimEnd()}\n\`\`\``);
}

// Keep the examples tied to CLI receipts, without pinning editorial wording.
const receipts = path.join(repositoryRoot, 'docs/receipts/required-column');
const base = await readFile(path.join(receipts, 'base.sql'), 'utf8');
const expand = await readFile(path.join(receipts, 'expand.sql'), 'utf8');
const contract = await readFile(path.join(receipts, 'contract.edited.sql'), 'utf8');
const editStart = contract.indexOf('\n', contract.indexOf('-- onwardpg:edit begin ')) + 1;
const editEnd = contract.indexOf('-- onwardpg:edit end ', editStart);
const examples = [
  base.trim(),
  expand.split('\n').find((line) => line.startsWith('ALTER TABLE')),
  contract.slice(editStart, editEnd).trim(),
  contract.split('\n').find((line) => line.includes('ALTER COLUMN "status" SET NOT NULL;')),
];
for (const example of examples) {
  if (!example || !hasSQLFence(guide, example)) {
    fail('quick start SQL differs from its required-column receipt');
  }
}

const protocolDoc = documents.get(path.join(repositoryRoot, 'docs/protocol.md'));
const draftReceipt = await readFile(path.join(receipts, 'draft-needs-sql-edits.json'), 'utf8');
const marker = '<!-- onwardpg-receipt: draft-needs-sql-edits -->';
const markerOffset = protocolDoc.indexOf(marker);
const fenceStart = protocolDoc.indexOf('```', markerOffset + marker.length);
const contentStart = protocolDoc.indexOf('\n', fenceStart) + 1;
const fenceEnd = protocolDoc.indexOf('\n```', contentStart);
if (markerOffset < 0 || fenceStart < 0 || fenceEnd < 0 ||
    protocolDoc.slice(contentStart, fenceEnd) !== draftReceipt.trimEnd()) {
  fail('draft needs_sql_edits documentation differs from actual onwardpg output');
}

const supportedFeatures = documents.get(path.join(repositoryRoot, 'docs/supported-features.md'));
for (const choice of ['`assert_only`', '`manual_sql`', '`split_plan`']) {
  if (!supportedFeatures.includes(choice)) fail(`supported-features omits required-column choice ${choice}`);
}
const bundles = documents.get(path.join(repositoryRoot, 'docs/bundles.md'));
for (const artifact of [
  'contract-gates.json',
  'contract-gate-overrides.json',
  'expand-checkpoint.json',
  'verify.sql',
]) {
  if (!bundles.includes(artifact)) fail(`bundle inventory omits ${artifact}`);
}
const decisionProtocol = documents.get(
  path.join(repositoryRoot, 'skills/onwardpg/references/decision-protocol.md'),
);
if (!decisionProtocol.includes('required production readiness assertion') ||
    !decisionProtocol.includes('They do not authorize production contract')) {
  fail('agent decision protocol conflates contract gates with optional verify.sql assertions');
}
const cliReference = documents.get(path.join(repositoryRoot, 'docs/cli.md'));
for (const expected of ['--statement-timeout 30s', 'PlanID']) {
  if (!cliReference.includes(expected)) fail(`CLI reference omits ${expected}`);
}

const goRoots = ['acceptance', 'cmd', 'internal', 'pgschema', 'scripts'].map((name) =>
  path.join(repositoryRoot, name),
);
const testPaths = [];
for (const goRoot of goRoots) {
  testPaths.push(...(await collect(goRoot, (name) => name.endsWith('_test.go'))));
}
const actualTests = new Set();
for (const testPath of testPaths) {
  const source = await readFile(testPath, 'utf8');
  for (const match of source.matchAll(/\bfunc (Test[A-Za-z0-9_]+)\s*\(/g)) actualTests.add(match[1]);
}
for (const [documentPath, body] of documents) {
  for (const match of body.matchAll(/\b(Test[A-Z][A-Za-z0-9_]+)\b/g)) {
    if (!actualTests.has(match[1])) {
      fail(`${path.relative(repositoryRoot, documentPath)} references missing Go test ${match[1]}`);
    }
  }
}

console.log(
  `documentation verified: ${documentationPaths.length} Markdown files, ` +
    'quick start SQL and protocol receipts, current verify interface, and all cited Go tests',
);
