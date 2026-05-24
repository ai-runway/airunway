#!/usr/bin/env bun
/**
 * Generate shared/types/versions.generated.ts from /versions.env at repo root.
 *
 * Run via `bun run generate-versions` from the shared/ package, or via the root
 * `make verify-versions` target which calls this and diff-checks the output.
 */

import { readFileSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const repoRoot = join(here, '..', '..');
const versionsPath = join(repoRoot, 'versions.env');

// Optional `--out <path>` overrides the default destination. Used by the
// Makefile `verify-versions` target so it can generate to a temp file and
// compare against the working-tree copy without mutating it.
const argv = process.argv.slice(2);
const outIdx = argv.indexOf('--out');
const outPath =
  outIdx !== -1 && argv[outIdx + 1]
    ? argv[outIdx + 1]
    : join(here, '..', 'types', 'versions.generated.ts');

const env = readFileSync(versionsPath, 'utf8');

const entries: Array<[string, string]> = [];
for (const rawLine of env.split('\n')) {
  const line = rawLine.trim();
  if (!line || line.startsWith('#')) continue;
  const eq = line.indexOf('=');
  if (eq === -1) continue;
  const key = line.slice(0, eq).trim();
  const value = line.slice(eq + 1).trim();
  if (!/^[A-Z][A-Z0-9_]*$/.test(key)) {
    throw new Error(`Invalid key in versions.env: ${key}`);
  }
  entries.push([key, value]);
}

const body = entries
  .map(([k, v]) => `export const ${k} = ${JSON.stringify(v)};`)
  .join('\n');

const header = `// Auto-generated from /versions.env. Do not edit by hand.
// After editing /versions.env, regenerate with \`cd shared && bun run generate-versions\`,
// then run \`make verify-versions\` from the repo root to confirm it is in sync.
`;

writeFileSync(outPath, `${header}\n${body}\n`);

console.log(`Wrote ${outPath}`);
for (const [k, v] of entries) {
  console.log(`  ${k}=${v}`);
}
