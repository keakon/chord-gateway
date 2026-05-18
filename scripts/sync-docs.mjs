#!/usr/bin/env node
// Sync user-facing Markdown into the Starlight content collection.
//
// Source of truth stays in the repository root and docs/. The generated files
// under website/src/content/docs are gitignored; do not edit them by hand.

import { mkdir, readdir, readFile, rm, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);

const repoRoot = path.resolve(__dirname, '..');
const docsDir = path.join(repoRoot, 'docs');
const websiteContentDir = path.join(repoRoot, 'website', 'src', 'content', 'docs');
const enDir = websiteContentDir;
const zhDir = path.join(websiteContentDir, 'zh');
const repo = 'https://github.com/keakon/chord-gateway';
const SITE_BASE = '/chord-gateway';

// Pages rendered as hand-written Starlight landing pages.
const SKIP_DOC_FILES = new Set(['index.md', 'index_CN.md']);

const ROOT_DOCS = [
  { filename: 'QUICKSTART.md', lang: 'en', slug: 'quickstart' },
  { filename: 'QUICKSTART_CN.md', lang: 'zh', slug: 'quickstart' },
];

function deriveSlug(basename) {
  if (basename === 'QUICKSTART') return 'quickstart';
  return basename.replace(/_CN$/, '');
}

function detectLanguage(filename) {
  return filename.endsWith('_CN.md') ? 'zh' : 'en';
}

function extractTitleAndBody(markdown) {
  const lines = markdown.split('\n');
  let title = '';
  let bodyStart = 0;
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    if (line.startsWith('# ')) {
      title = line.slice(2).trim();
      bodyStart = i + 1;
      while (bodyStart < lines.length && lines[bodyStart].trim() === '') {
        bodyStart++;
      }
      break;
    }
  }
  const body = lines.slice(bodyStart).join('\n');
  return { title, body };
}

function deriveDescription(body) {
  const blocks = body.split(/\n{2,}/);
  for (const block of blocks) {
    const trimmed = block.trim();
    if (!trimmed) continue;
    if (trimmed.startsWith('#')) continue;
    if (trimmed.startsWith('```')) continue;
    if (trimmed.startsWith(':::')) continue;
    if (trimmed.startsWith('|')) continue;
    if (trimmed.startsWith('>')) continue;
    return trimmed
      .replace(/\n+/g, ' ')
      .replace(/\[([^\]]+)\]\(([^)]+)\)/g, '$1')
      .replace(/`([^`]+)`/g, '$1')
      .replace(/\s+/g, ' ')
      .slice(0, 180)
      .trim();
  }
  return '';
}

function sitePath(lang, slug, anchor = '') {
  const langPrefix = lang === 'zh' ? '/zh' : '';
  return `${SITE_BASE}${langPrefix}/${slug}/${anchor}`;
}

function repoBlobPath(filePath, anchor = '') {
  return `${repo}/blob/main/${filePath}${anchor}`;
}

function linkLangFromSuffix(cn) {
  return cn ? 'zh' : 'en';
}

function rewriteRelativeMarkdownLink(target, rest = '') {
  const anchor = rest.startsWith('#') ? rest : '';

  const quickstart = target.match(/^(?:\.\.\/|\.\/)QUICKSTART(_CN)?\.md$/);
  if (quickstart) {
    return sitePath(linkLangFromSuffix(quickstart[1]), 'quickstart', anchor);
  }

  const docsFromRoot = target.match(/^\.\/docs\/([\w-]+?)(_CN)?\.md$/);
  if (docsFromRoot) {
    return sitePath(linkLangFromSuffix(docsFromRoot[2]), docsFromRoot[1], anchor);
  }

  const sibling = target.match(/^\.\/([\w-]+?)(_CN)?\.md$/);
  if (sibling) {
    const slug = sibling[1] === 'index' ? '' : sibling[1];
    return slug ? sitePath(linkLangFromSuffix(sibling[2]), slug, anchor) : `${SITE_BASE}${sibling[2] ? '/zh' : ''}/`;
  }

  const parent = target.match(/^\.\.\/([\w-]+?)(_CN)?\.md$/);
  if (parent) {
    const file = `${parent[1]}${parent[2] || ''}.md`;
    return repoBlobPath(file, anchor);
  }

  const parentPlain = target.match(/^\.\.\/([\w.-]+)$/);
  if (parentPlain) {
    return repoBlobPath(parentPlain[1], anchor);
  }

  return null;
}

function rewriteLinks(body) {
  return body.replace(/\((\.?\.\/[^)#]+?)(#[^)]*)?\)/g, (match, target, rest = '') => {
    const rewritten = rewriteRelativeMarkdownLink(target, rest);
    return rewritten ? `(${rewritten})` : match;
  });
}

function rewriteAdmonitions(body) {
  return body.replace(
    /(^|\n)>\s+\*\*(Note|Important|Warning|Tip|Caution|Danger)(?::|\*\*:)\*\*\s*([^\n]+(?:\n>\s+[^\n]+)*)/g,
    (_m, prefix, kind, content) => {
      const flavour = {
        Note: 'note',
        Tip: 'tip',
        Important: 'caution',
        Warning: 'caution',
        Caution: 'caution',
        Danger: 'danger',
      }[kind] || 'note';
      const inner = content.replace(/\n>\s?/g, '\n').trim();
      return `${prefix}:::${flavour}\n${inner}\n:::`;
    },
  );
}

function escapeYaml(value) {
  return value.replace(/\\/g, '\\\\').replace(/"/g, '\\"');
}

function buildFrontmatter({ title, description }) {
  const lines = ['---'];
  if (title) lines.push(`title: "${escapeYaml(title)}"`);
  if (description) lines.push(`description: "${escapeYaml(description)}"`);
  lines.push('---', '');
  return lines.join('\n');
}

async function syncOne(srcPath, lang, targetSlug) {
  const raw = await readFile(srcPath, 'utf8');
  const { title, body } = extractTitleAndBody(raw);
  const description = deriveDescription(body);
  let processed = rewriteLinks(body);
  processed = rewriteAdmonitions(processed);
  const out = buildFrontmatter({ title, description }) + processed.trimStart() + '\n';
  const targetDir = lang === 'zh' ? zhDir : enDir;
  const targetPath = path.join(targetDir, `${targetSlug}.md`);
  await mkdir(path.dirname(targetPath), { recursive: true });
  await writeFile(targetPath, out, 'utf8');
}

async function clean() {
  for (const dir of [enDir, zhDir]) {
    await mkdir(dir, { recursive: true });
    const entries = await readdir(dir, { withFileTypes: true });
    await Promise.all(
      entries
        .filter((entry) => entry.isFile() && entry.name.endsWith('.md'))
        .map((entry) => rm(path.join(dir, entry.name), { force: true })),
    );
  }
}

async function main() {
  await clean();

  for (const rootDoc of ROOT_DOCS) {
    await syncOne(path.join(repoRoot, rootDoc.filename), rootDoc.lang, rootDoc.slug);
  }

  const entries = await readdir(docsDir, { withFileTypes: true });
  for (const entry of entries) {
    if (!entry.isFile()) continue;
    if (!entry.name.endsWith('.md')) continue;
    if (SKIP_DOC_FILES.has(entry.name)) continue;
    const srcPath = path.join(docsDir, entry.name);
    const lang = detectLanguage(entry.name);
    const baseName = entry.name.replace(/\.md$/, '');
    const slug = deriveSlug(baseName);
    await syncOne(srcPath, lang, slug);
  }

  console.log('Synced user docs to website/src/content/docs/.');
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
