// The public, camelCase surface of the SDK. The wire is snake_case; the client maps between them so callers
// only ever see these types.

/** Scopes filter for a pack: either a list of scope strings, or an object flattened to `"key:value"` strings. */
export type Scopes = string[] | Record<string, string>;

// XOR<T, U> makes exactly one of two shapes required: giving both fails to compile (the other's keys are
// forced to `never`), and giving neither fails (the required keys are missing). Load-bearing on
// `exactOptionalPropertyTypes` in tsconfig.
type Without<T, U> = { [P in Exclude<keyof T, keyof U>]?: never };
type XOR<T, U> = (Without<T, U> & U) | (Without<U, T> & T);

interface WriteBase {
  runId: string;
  agentId: string;
}

/**
 * Arguments for {@link LoreClient.write}: a base plus EITHER `content` (a string, wrapped into the event
 * payload as `{ content }`) OR `payload` (an opaque object sent as-is). Supplying both — or neither — is a
 * compile error.
 */
export type WriteArgs = XOR<WriteBase & { content: string }, WriteBase & { payload: Record<string, unknown> }>;

/** Arguments for {@link LoreClient.writeState}: one working-memory fact (the `kind:"state"` convention). */
export interface WriteStateArgs {
  runId: string;
  agentId: string;
  entity: string;
  predicate: string;
  value: unknown;
}

/** Arguments for {@link LoreClient.pack}. */
export interface PackArgs {
  runId: string;
  query: string;
  /** Read-your-writes barrier: the run seq the pack must reflect. Omitted asserts nothing (sent as 0). */
  minSeq?: number;
  scopes?: Scopes;
  limit?: number;
  tokenBudget?: number;
}

/** Options for constructing a {@link LoreClient}. */
export interface LoreClientOptions {
  /** The bearer API key. Required. */
  apiKey: string;
  /** Base URL of the Lore server. Default `http://localhost:8080`. */
  baseUrl?: string;
  /** A custom fetch implementation (transport override or test seam). Default the global `fetch`. */
  fetch?: typeof globalThis.fetch;
  /** Extra headers sent on every request. */
  headers?: Record<string, string>;
  /** Per-request timeout in milliseconds. Default 30000. */
  timeoutMs?: number;
}

/** Result of {@link LoreClient.createRun}. */
export interface RunResult {
  runId: string;
  createdAt: string;
}

/** Result of {@link LoreClient.write} and {@link LoreClient.writeState}. */
export interface WriteResult {
  eventId: string;
  seq: number;
}

/** One distilled memory that composed a pack, in pack order. */
export interface PackSourceResult {
  id: string;
  kind: string;
  score: number;
  section: string;
}

/** Result of {@link LoreClient.pack}: the assembled context pack and its provenance. */
export interface PackResult {
  text: string;
  sources: PackSourceResult[];
  coveredSeq: number;
  freshnessLagMs: number;
  savedTokens: number;
  workingSource: "live" | "durable" | "skipped";
  truncated: boolean;
}
