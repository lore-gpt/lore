// A runnable, type-checked version of the README hero. The block between the markers is kept byte-identical
// to the fenced `ts` hero in the repo README (scripts/check-readme-hero.mjs enforces it), so the snippet we
// show the world always compiles against the real client. This file is type-checked, not executed.
import { LoreClient } from "../src/index.ts";

const apiKey = process.env["LORE_API_KEY"] ?? "";

// >>> readme-hero
const lore = new LoreClient({ apiKey });

const { runId } = await lore.createRun();
const { seq } = await lore.write({
  runId,
  agentId: "researcher",
  content: "Auth flow moved to v2 — PR #42 merged",
});

const pack = await lore.pack({
  runId,
  query: "current state of auth work",
  scopes: { team: "platform" },
  minSeq: seq,
  tokenBudget: 2000,
});

pack.coveredSeq; // ≥ seq once distilled; until then the write is in the raw tail — either way, reflected
pack.savedTokens; // token savings vs raw history — a coarse estimate; small packs may round to 0
// <<< readme-hero
