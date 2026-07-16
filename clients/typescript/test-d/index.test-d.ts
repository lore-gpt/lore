// Compile-time assertions, checked by `tsc -p tsconfig.check.json` (NOT run by node — `declare const` has no
// runtime). An @ts-expect-error whose next line stops erroring is itself a compile error, so a regression in
// the write() XOR union or a relaxed required field turns the typecheck step red.
import { LoreClient } from "../src/index.ts";
import type { PackResult, WriteResult, RunResult } from "../src/index.ts";

declare const lore: LoreClient;

// --- write(): exactly one of content | payload ---
// content alone compiles.
void lore.write({ runId: "r", agentId: "a", content: "x" });
// payload alone compiles.
void lore.write({ runId: "r", agentId: "a", payload: { k: 1 } });
// @ts-expect-error both content and payload must NOT compile.
void lore.write({ runId: "r", agentId: "a", content: "x", payload: { k: 1 } });
// @ts-expect-error neither content nor payload must NOT compile.
void lore.write({ runId: "r", agentId: "a" });

// --- pack(): runId and query are required ---
void lore.pack({ runId: "r", query: "q" });
// @ts-expect-error runId is required.
void lore.pack({ query: "q" });
// @ts-expect-error query is required.
void lore.pack({ runId: "r" });

// --- results are camelCase with the expected field types ---
declare const run: RunResult;
const _runId: string = run.runId;
const _createdAt: string = run.createdAt;

declare const wr: WriteResult;
const _eventId: string = wr.eventId;
const _seq: number = wr.seq;

declare const pack: PackResult;
const _coveredSeq: number = pack.coveredSeq;
const _savedTokens: number = pack.savedTokens;
const _workingSource: "live" | "durable" | "skipped" = pack.workingSource;

// Reference the bindings so noUnusedLocals-style checks (if ever enabled) and readers see intent.
void [_runId, _createdAt, _eventId, _seq, _coveredSeq, _savedTokens, _workingSource];
