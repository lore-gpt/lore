import type {
  LoreClientOptions,
  PackArgs,
  PackResult,
  RunResult,
  Scopes,
  WriteArgs,
  WriteResult,
  WriteStateArgs,
} from "./types.ts";
import type {
  WireCreateEventRequest,
  WireCreateEventResponse,
  WireCreateRunResponse,
  WireError,
  WirePackRequest,
  WirePackResponse,
} from "./wire.ts";
import { fromResponse, LoreConnectionError, LoreParseError } from "./errors.ts";

const DEFAULT_BASE_URL = "http://localhost:8080";
const DEFAULT_TIMEOUT_MS = 30_000;

/**
 * A client for the Lore coordination-memory API. Create a run, write events to it, and pack read-your-writes
 * context from it. The project is fixed by the API key. Requests are not retried (a POST is not idempotent);
 * failures throw a typed {@link LoreError}.
 */
export class LoreClient {
  readonly #apiKey: string;
  readonly #baseUrl: string;
  readonly #fetch: typeof globalThis.fetch;
  readonly #headers: Record<string, string>;
  readonly #timeoutMs: number;

  constructor(options: LoreClientOptions) {
    if (!options.apiKey) throw new TypeError("LoreClient: apiKey is required");
    this.#apiKey = options.apiKey;
    this.#baseUrl = (options.baseUrl ?? DEFAULT_BASE_URL).replace(/\/+$/, "");
    this.#fetch = options.fetch ?? globalThis.fetch;
    this.#headers = options.headers ?? {};
    this.#timeoutMs = options.timeoutMs ?? DEFAULT_TIMEOUT_MS;
  }

  /** Create a run in the API key's project. */
  async createRun(): Promise<RunResult> {
    const body = await this.#request<WireCreateRunResponse>("/v1/runs", {});
    return { runId: body.run_id, createdAt: body.created_at };
  }

  /**
   * Append an event to a run. Supply EITHER `content` (a string, wrapped into the event payload as
   * `{ content }`) OR `payload` (an opaque object sent verbatim) — not both.
   */
  async write(args: WriteArgs): Promise<WriteResult> {
    const payload: Record<string, unknown> =
      "content" in args ? { content: args.content } : args.payload;
    const body: WireCreateEventRequest = {
      run_id: args.runId,
      agent_id: args.agentId,
      payload,
    };
    return this.#writeEvent(body);
  }

  /**
   * Write one working-memory fact (the `kind:"state"` convention): it is written through to the low-latency
   * stripe so a same-run reader sees it immediately. The server validates the fact and rejects a malformed one
   * with a typed {@link InvalidBodyError}-family error.
   */
  async writeState(args: WriteStateArgs): Promise<WriteResult> {
    const body: WireCreateEventRequest = {
      run_id: args.runId,
      agent_id: args.agentId,
      payload: { kind: "state", entity: args.entity, predicate: args.predicate, value: args.value },
    };
    return this.#writeEvent(body);
  }

  /** Retrieve a context pack for a run. */
  async pack(args: PackArgs): Promise<PackResult> {
    const body: WirePackRequest = {
      run_id: args.runId,
      query: args.query,
      min_seq: args.minSeq ?? 0,
      ...(args.scopes !== undefined ? { scopes: normalizeScopes(args.scopes) } : {}),
      ...(args.limit !== undefined ? { limit: args.limit } : {}),
      ...(args.tokenBudget !== undefined ? { token_budget: args.tokenBudget } : {}),
    };
    const b = await this.#request<WirePackResponse>("/v1/pack", body);
    return {
      text: b.text,
      sources: b.sources.map((s) => ({ id: s.id, kind: s.kind, score: s.score, section: s.section })),
      coveredSeq: b.covered_seq,
      freshnessLagMs: b.freshness_lag_ms,
      savedTokens: b.saved_tokens,
      workingSource: b.working_source,
      truncated: b.truncated,
    };
  }

  async #writeEvent(body: WireCreateEventRequest): Promise<WriteResult> {
    const b = await this.#request<WireCreateEventResponse>("/v1/events", body);
    return { eventId: b.event_id, seq: b.seq };
  }

  // Single request path. No retry (POST /v1/events is not idempotent): a failure throws, the caller decides.
  async #request<T>(path: string, body: unknown): Promise<T> {
    let res: Response;
    try {
      res = await this.#fetch(this.#baseUrl + path, {
        method: "POST",
        headers: {
          // User headers first, so the SDK's content-type and auth always win (never accidentally overridden).
          ...this.#headers,
          "content-type": "application/json",
          authorization: `Bearer ${this.#apiKey}`,
        },
        body: JSON.stringify(body),
        signal: AbortSignal.timeout(this.#timeoutMs),
      });
    } catch (cause) {
      throw new LoreConnectionError("request did not reach a response", { cause });
    }

    const text = await res.text();
    let parsed: unknown;
    if (text.length > 0) {
      try {
        parsed = JSON.parse(text) as unknown;
      } catch (cause) {
        if (!res.ok) throw fromResponse(res.status, { message: res.statusText });
        throw new LoreParseError(`response body was not JSON (status ${res.status})`, { cause });
      }
    }

    if (!res.ok) throw fromResponse(res.status, toWireError(parsed, res.statusText));
    return parsed as T;
  }
}

function toWireError(parsed: unknown, statusText: string): WireError {
  if (parsed !== null && typeof parsed === "object") {
    const o = parsed as Record<string, unknown>;
    return {
      message: typeof o["message"] === "string" ? o["message"] : statusText,
      ...(typeof o["code"] === "string" ? { code: o["code"] } : {}),
    };
  }
  return { message: statusText };
}

function normalizeScopes(scopes: Scopes): string[] {
  return Array.isArray(scopes) ? scopes : Object.entries(scopes).map(([k, v]) => `${k}:${v}`);
}
