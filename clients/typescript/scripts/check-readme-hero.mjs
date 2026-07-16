// Enforces that the repo README's "hero" TypeScript snippet is byte-identical to the type-checked example in
// examples/hero.ts — so the snippet we show the world always compiles against the real client. The example is
// type-checked by `pnpm typecheck`; this script proves the README matches it. Run via `pnpm example`.
import { readFile } from "node:fs/promises";

const README = new URL("../../../README.md", import.meta.url); // repo root README
const HERO = new URL("../examples/hero.ts", import.meta.url);

const START = "// >>> readme-hero";
const END = "// <<< readme-hero";

function extractRegion(src) {
  const s = src.indexOf(START);
  const e = src.indexOf(END, s + START.length);
  if (s === -1 || e === -1) {
    throw new Error("examples/hero.ts is missing the readme-hero markers");
  }
  return src.slice(s + START.length, e).trim();
}

function extractTsBlocks(md) {
  const blocks = [];
  const re = /```ts\r?\n([\s\S]*?)```/g;
  let m;
  while ((m = re.exec(md)) !== null) blocks.push(m[1].trim());
  return blocks;
}

const [md, hero] = await Promise.all([readFile(README, "utf8"), readFile(HERO, "utf8")]);
const want = extractRegion(hero);
const blocks = extractTsBlocks(md);

if (blocks.some((b) => b === want)) {
  console.log("ok: the README hero snippet matches examples/hero.ts");
  process.exit(0);
}

console.error(
  "The README hero snippet does not match examples/hero.ts.\n" +
    "Update the fenced ```ts block in README.md to exactly:\n\n" +
    want +
    "\n",
);
process.exit(1);
