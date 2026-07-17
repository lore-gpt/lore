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

/** Options for constructing a {@link LoreRestClient}. */
export interface LoreRestClientOptions {
  /** The bearer API key. Required. */
  apiKey: string;
  /** Base URL of the Lore server. Default `http://localhost:8080`. */
  baseUrl?: string;
  /** A custom fetch implementation (transport override or test seam). Default the global `fetch`. */
  fetch?: typeof globalThis.fetch;
  /** Per-request timeout in milliseconds. Default 30000. */
  timeoutMs?: number;
}

/**
 * A thin REST client over the Lore API for the MCP server: create runs, write events, and pack context. It
 * returns the wire (snake_case) shapes verbatim — the MCP tools mirror the API vocabulary, so there is no
 * camelCase surface to translate. Requests are not retried (a POST is not idempotent); failures throw a typed
 * {@link LoreError}. The wire types and error mapping are shared with the TypeScript SDK's client to keep the
 * two in lockstep.
 */
export class LoreRestClient {
  readonly #apiKey: string;
  readonly #baseUrl: string;
  readonly #fetch: typeof globalThis.fetch;
  readonly #timeoutMs: number;

  constructor(options: LoreRestClientOptions) {
    if (!options.apiKey) throw new TypeError("LoreRestClient: apiKey is required");
    this.#apiKey = options.apiKey;
    this.#baseUrl = (options.baseUrl ?? DEFAULT_BASE_URL).replace(/\/+$/, "");
    this.#fetch = options.fetch ?? globalThis.fetch;
    this.#timeoutMs = options.timeoutMs ?? DEFAULT_TIMEOUT_MS;
  }

  /** Create a run in the API key's project. */
  createRun(): Promise<WireCreateRunResponse> {
    return this.#request<WireCreateRunResponse>("/v1/runs", {});
  }

  /** Append an event to a run. */
  writeEvent(body: WireCreateEventRequest): Promise<WireCreateEventResponse> {
    return this.#request<WireCreateEventResponse>("/v1/events", body);
  }

  /** Retrieve a context pack for a run. */
  pack(body: WirePackRequest): Promise<WirePackResponse> {
    return this.#request<WirePackResponse>("/v1/pack", body);
  }

  // Single request path. No retry (POST is not idempotent): a failure throws, the caller decides.
  async #request<T>(path: string, body: unknown): Promise<T> {
    let res: Response;
    try {
      res = await this.#fetch(this.#baseUrl + path, {
        method: "POST",
        headers: {
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
