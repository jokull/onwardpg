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

const generatedRequiredContract = await readFile(
  path.join(repositoryRoot, 'docs/receipts/required-column/contract.generated.sql'),
  'utf8',
);
const editedRequiredContract = await readFile(
  path.join(repositoryRoot, 'docs/receipts/required-column/contract.edited.sql'),
  'utf8',
);
const requiredExpand = await readFile(
  path.join(repositoryRoot, 'docs/receipts/required-column/expand.sql'),
  'utf8',
);
const requiredVerify = await readFile(
  path.join(repositoryRoot, 'docs/receipts/required-column/verify.sql'),
  'utf8',
);
const requiredQuestionsProjection = await readFile(
  path.join(repositoryRoot, 'docs/receipts/required-column/questions.projection.json'),
  'utf8',
);
const requiredPlanProjection = await readFile(
  path.join(repositoryRoot, 'docs/receipts/required-column/plan.projection.json'),
  'utf8',
);
const draftNeedsSQLEdits = await readFile(
  path.join(repositoryRoot, 'docs/receipts/required-column/draft-needs-sql-edits.json'),
  'utf8',
);
const expandContractPath = path.join(
  repositoryRoot,
  'website/src/content/docs/concepts/expand-contract.md',
);
const expandContract = documents.get(expandContractPath);
const planCommand = documents.get(
  path.join(repositoryRoot, 'website/src/content/docs/concepts/plan-command.md'),
);
const comparison = documents.get(
  path.join(repositoryRoot, 'website/src/content/docs/start/comparison.mdx'),
);
const introduction = documents.get(
  path.join(repositoryRoot, 'website/src/content/docs/start/introduction.mdx'),
);
const protocolDoc = documents.get(path.join(repositoryRoot, 'docs/protocol.md'));
const compatibilityDoc = documents.get(path.join(repositoryRoot, 'docs/compatibility.md'));
const runATest = await readFile(
  path.join(repositoryRoot, 'internal/contractcheck/blind_gauntlet_run_a_integration_test.go'),
  'utf8',
);
const runBTest = await readFile(
  path.join(repositoryRoot, 'internal/graphplan/blind_gauntlet_b_integration_test.go'),
  'utf8',
);

function markedFence(markdown, marker) {
  const markerText = `<!-- onwardpg-receipt: ${marker} -->`;
  const markerOffset = markdown.indexOf(markerText);
  if (markerOffset < 0) fail(`missing documentation receipt marker ${marker}`);
  const fenceStart = markdown.indexOf('```', markerOffset + markerText.length);
  const contentStart = markdown.indexOf('\n', fenceStart) + 1;
  const fenceEnd = markdown.indexOf('\n```', contentStart);
  if (fenceStart < 0 || contentStart === 0 || fenceEnd < 0) {
    fail(`malformed documentation receipt fence ${marker}`);
  }
  return markdown.slice(contentStart, fenceEnd);
}

function hasSQLFence(markdown, body) {
  return markdown.includes(`\`\`\`sql\n${body.trimEnd()}\n\`\`\``);
}

const expandStatement = requiredExpand.match(/ALTER TABLE[^\n]+;/)?.[0];
if (!expandStatement) fail('required-column expand receipt has no ALTER TABLE statement');
if (!hasSQLFence(expandContract, expandStatement)) {
  fail('required-column expand documentation differs from actual onwardpg output');
}

const generatedPocketStart = generatedRequiredContract.indexOf('-- onwardpg:edit begin ');
const generatedEnforcementEnd = generatedRequiredContract.indexOf(' SET NOT NULL;') + ' SET NOT NULL;'.length;
const generatedPocket = `-- contract.sql\n${generatedRequiredContract.slice(generatedPocketStart, generatedEnforcementEnd)}`;
if (!hasSQLFence(expandContract, generatedPocket)) {
  fail('required-column generated contract documentation differs from actual onwardpg output');
}

function editPocket(body) {
  const begin = body.indexOf('\n', body.indexOf('-- onwardpg:edit begin ')) + 1;
  const end = body.indexOf('-- onwardpg:edit end ', begin);
  return body.slice(begin, end).trimEnd();
}

if (!hasSQLFence(expandContract, editPocket(editedRequiredContract))) {
  fail('required-column application edit documentation differs from its verified fixture');
}
if (!hasSQLFence(expandContract, requiredVerify)) {
  fail('required-column verification documentation differs from its verified fixture');
}
if (!hasSQLFence(comparison, expandStatement)) {
  fail('comparison required-column expand differs from actual onwardpg output');
}
if (!hasSQLFence(comparison, editPocket(editedRequiredContract))) {
  fail('comparison required-column cleanup differs from its verified fixture');
}
const requiredEnforcement = generatedRequiredContract
  .split('\n')
  .find((line) => line.includes('ALTER COLUMN "status" SET NOT NULL;'));
if (!hasSQLFence(comparison, requiredEnforcement)) {
  fail('comparison required-column enforcement differs from actual onwardpg output');
}
for (const fragment of [
  'Reviewed 21 July 2026',
  'APPLY EXPAND — before the pull request merges',
  'temporarily makes the database **more permissive than both the old and',
  'Why this almost never breaks locally',
  'one branch, one merge, and one application deploy',
  'Online DDL is not application compatibility',
  'Compatibility lands before code',
  'What onwardpg plans—and when it asks for help',
  'Required columns',
  'Check constraints',
  'Column and table renames',
  'onwardpg is deliberately designed for a developer or coding agent',
  'Unresolved TODOs fail verification',
  'it knows what old code writes, but it cannot know what those rows mean to the',
  '`assert_only` if application code or an earlier operation will fill every row',
  '`manual_sql` if a developer or agent can supply the rule now',
  '`split_plan` if the honest answer is “not in this release.”',
  'onwardpg then stops with `needs_sql_edits`',
  'An agent can also attach the reviewed SQL and',
  'One production operation, one DDL executor',
  ':::warning[One production operation, one DDL executor]',
  'Their SQL output tells onwardpg',
  'These tools write **SQL that moves the database from A to B**',
  'https://orm.drizzle.team/docs/kit-overview',
  'https://docs.djangoproject.com/en/6.0/topics/migrations/',
  'https://alembic.sqlalchemy.org/en/latest/autogenerate.html',
  'https://www.prisma.io/docs/orm/prisma-migrate/workflows/development-and-production',
  'https://atlasgo.io/versioned/lint',
  'https://atlasgo.io/declarative/diff',
  'https://github.com/stripe/pg-schema-diff',
  'https://github.com/djrobstep/migra',
  'https://github.com/xataio/pgroll#how-pgroll-works',
  'https://github.com/xataio/pgroll/blob/main/docs/guides/orms.mdx',
  'https://github.com/xataio/pgroll/blob/main/docs/guides/clientapps.mdx',
]) {
  if (!comparison.includes(fragment)) {
    fail(`comparison page is missing maintained evidence: ${fragment}`);
  }
}
for (const fragment of [
  'how can the code\nrunning now and the code about to deploy both use the database',
  'Two jobs, one plan',
  'more permissive than both the old and new\nschema',
  'Either half on its own is incomplete',
  'APPLY EXPAND — before the pull request merges',
  'one feature one branch, one merge, and one application deployment',
  'onwardpg does **not** invent what those legacy `NULL` rows mean',
  'treats it as temporarily nullable when reading',
  'The plan command is the core product',
  'Rebase the branch; restack the migration',
  'git rebase origin/main',
  'rebuilds the **same PlanID** from that new base',
  'Generated work that is now supplied upstream disappears',
  'fast-forward SQL needed locally',
  'restacking at the schema level',
  'The durable migration is always accepted history → working DDL',
  'Developers and agents supply the meaning PostgreSQL lacks',
  'An unresolved TODO cannot pass verification',
]) {
  if (!introduction.includes(fragment)) {
    fail(`introduction is missing its core product explanation: ${fragment}`);
  }
}
for (const fragment of [
  'Advanced: one rollout, several compatibility problems',
  'adds three dependency-scoped pockets',
  'CREATE OR REPLACE VIEW app.account_directory AS',
  "SET account_status = 'active'",
  "SET delivery_tier = 'push'",
  'same prepared statements afterward',
  'Only after it closes and its\nbackend disappears',
  'age_text text` to `age integer',
  'exactly two edit pockets',
]) {
  if (!planCommand.includes(fragment)) {
    fail(`plan-command advanced walkthrough is missing grounded evidence: ${fragment}`);
  }
}
for (const fragment of [
  'TestBlindGauntletRunADependentViewRenameOnPostgreSQL',
  'legacy_account_insert',
  'legacy_directory_select',
  'CREATE OR REPLACE VIEW app.account_directory AS',
  "UPDATE app.accounts SET account_status = 'active' WHERE account_status IS NULL;",
  "UPDATE app.accounts SET delivery_tier = 'push' WHERE delivery_tier = 'email';",
  'waitForBlindGauntletBackendDrain(t, ctx, deploy, legacyPID)',
]) {
  if (!runATest.includes(fragment)) {
    fail(`run-A PostgreSQL receipt no longer proves documented behavior: ${fragment}`);
  }
}
for (const fragment of [
  'TestBlindGauntletCrossNameTypeTransitionKeepsLegacySQLAliveOnPostgreSQL',
  'compound transition must produce exactly two editable SQL pockets',
  'prepared legacy INSERT failed after expand',
  'prepared legacy view query failed after overlap view replacement',
  'materialized overlap freshness boundary',
]) {
  if (!runBTest.includes(fragment)) {
    fail(`run-B PostgreSQL receipt no longer proves documented behavior: ${fragment}`);
  }
}
for (const fragment of [
  'Same-type column rename',
  'Same-name column type change',
  'Cross-name/type column transition',
  'Ordinary view inside a column transition',
  'Materialized view inside a column transition',
]) {
  if (!compatibilityDoc.includes(fragment)) {
    fail(`compatibility matrix omits transition support class: ${fragment}`);
  }
}
if (
  markedFence(protocolDoc, 'draft-needs-sql-edits') !== draftNeedsSQLEdits.trimEnd()
) {
  fail('draft needs_sql_edits documentation differs from actual onwardpg output');
}

function assertInOrder(body, fragments, label) {
  let offset = -1;
  for (const fragment of fragments) {
    const next = body.indexOf(fragment, offset + 1);
    if (next < 0) fail(`${label} is missing: ${fragment}`);
    offset = next;
  }
}

assertInOrder(planCommand, [
  expandStatement,
  '-- PRODUCT-SPECIFIC SQL: Provide reviewed reconcile_contract_sql SQL for app.bookings.status',
  'onwardpg contract gate failed: data:c6703912502bd497',
  'ALTER TABLE "app"."bookings" ALTER COLUMN "status" SET NOT NULL;',
], 'plan-command easy/medium ladder');

const typeExpand = await readFile(
  path.join(repositoryRoot, 'docs/receipts/type-change/expand.generated.sql'),
  'utf8',
);
const typeContract = await readFile(
  path.join(repositoryRoot, 'docs/receipts/type-change/contract.generated.sql'),
  'utf8',
);
for (const line of [...typeExpand.split('\n'), ...typeContract.split('\n')].filter((line) =>
  line.startsWith('-- ONWARDPG TODO:') ||
  line.startsWith('-- Establish both old and new interfaces') ||
  line.startsWith('-- Do not use a direct ALTER TYPE') ||
  line.startsWith('-- After pre-deployment writers drain')
)) {
  if (![documents.get(path.join(repositoryRoot, 'README.md')), documents.get(path.join(repositoryRoot, 'website/src/content/docs/concepts/decisions.md'))].some((body) => body.includes(line))) {
    fail(`actual type-change output is not represented in public documentation: ${line}`);
  }
}

const renameExpand = await readFile(
  path.join(repositoryRoot, 'docs/receipts/rename/expand.generated.sql'),
  'utf8',
);
const renameContract = await readFile(
  path.join(repositoryRoot, 'docs/receipts/rename/contract.generated.sql'),
  'utf8',
);
const renameFragments = [
  'ALTER TABLE "app"."accounts" ADD COLUMN "full_name" text;',
  'CREATE TRIGGER "onwardpg_sync_column_4cff936be08db67c" BEFORE INSERT OR UPDATE OF "display_name", "full_name" ON "app"."accounts" FOR EACH ROW EXECUTE FUNCTION "app"."onwardpg_sync_column_4cff936be08db67c"();',
  'UPDATE "app"."accounts" SET "full_name" = "display_name" WHERE "full_name" IS DISTINCT FROM "display_name";',
];
assertInOrder(renameExpand, renameFragments, 'rename expand receipt');
for (const fragment of renameFragments) {
  if (!planCommand.includes(fragment)) fail(`plan-command hard ladder is missing: ${fragment}`);
}
for (const fragment of [
  'DROP TRIGGER "onwardpg_sync_column_4cff936be08db67c" ON "app"."accounts";',
  'DROP FUNCTION "app"."onwardpg_sync_column_4cff936be08db67c"();',
  'ALTER TABLE "app"."accounts" DROP COLUMN "full_name";',
  'ALTER TABLE "app"."accounts" RENAME COLUMN "display_name" TO "full_name";',
]) {
  if (!renameContract.includes(fragment) || !planCommand.includes(fragment)) {
    fail(`plan-command hard ladder differs from rename receipt: ${fragment}`);
  }
}

const dependencyContract = await readFile(
  path.join(repositoryRoot, 'docs/receipts/dependency-type-change/contract.generated.sql'),
  'utf8',
);
const dependencyFragments = [
  'DROP MATERIALIZED VIEW "app"."fact_cache";',
  'DROP VIEW "app"."fact_view";',
  '-- ONWARDPG TODO: replace this comment with reviewed CONTRACT SQL for column:app:facts:val (integer -> bigint).',
  'ANALYZE "app"."facts" ("val");',
  'CREATE VIEW "app"."fact_view" AS SELECT val',
  'CREATE MATERIALIZED VIEW "app"."fact_cache" AS SELECT val',
  '-- onwardpg:batch nontransactional',
  'CREATE UNIQUE INDEX CONCURRENTLY "fact_cache_val_idx" ON "app"."fact_cache" USING "btree" ("val" NULLS LAST);',
];
assertInOrder(dependencyContract, dependencyFragments, 'dependency type-change receipt');
assertInOrder(planCommand, dependencyFragments, 'plan-command nightmare ladder');

const homepage = await readFile(path.join(repositoryRoot, 'website/src/pages/index.astro'), 'utf8');
if (homepage.includes('protocol_version') || /onwardpg\.[a-z-]+\/v\d+/.test(homepage)) {
  fail('homepage references a speculative protocol version');
}
const requiredCleanup = generatedRequiredContract
  .split('\n')
  .find((line) => line.includes('PRODUCT-SPECIFIC SQL: Provide reviewed reconcile_contract_sql SQL'));
const requiredGate = generatedRequiredContract
  .split('\n')
  .find((line) => line.includes('onwardpg contract gate failed'));
if (!requiredCleanup || !requiredGate) fail('required-column receipt is missing its cleanup or contract gate');

function escapedCompactJSON(document) {
  return JSON.stringify(JSON.parse(document))
    .replaceAll('{', '&#123;')
    .replaceAll('}', '&#125;');
}

assertInOrder(homepage, [
  escapedCompactJSON(requiredQuestionsProjection),
  '"kind":"reconcile","object":"column","name":["app","bookings","status"],"strategy":"manual_sql"',
  '"kind":"manual_sql","action":"reconcile_contract_sql","object":"column","name":["app","bookings","status"]',
  escapedCompactJSON(requiredPlanProjection),
  expandStatement,
  requiredCleanup.trim(),
  requiredGate.trim(),
  'ALTER COLUMN "status" SET NOT NULL;',
], 'homepage required-column receipt');

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

const goRoots = ['cmd', 'internal', 'pgschema', 'scripts'].map((name) =>
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
    'four CLI receipt scenarios, current verify interface, and all cited Go tests',
);
