import { LoreRestClient } from "./client.ts";

const DEFAULT_BASE_URL = "http://localhost:8080";

/** Configuration for the stdio server, read from the environment. */
export interface StdioConfig {
  /** The bearer API key (never logged). */
  apiKey: string;
  /** Base URL of the Lore server. */
  baseUrl: string;
  /** Optional per-request timeout in milliseconds. */
  timeoutMs: number | undefined;
}

/**
 * Read the server configuration from the environment:
 *   LORE_API_KEY   (required) — the bearer key from `lore provision` / `lore keys create`.
 *   LORE_BASE_URL  (optional) — the Lore server URL; defaults to http://localhost:8080.
 *   LORE_TIMEOUT_MS(optional) — per-request timeout in milliseconds.
 * Throws a descriptive Error (never echoing the key) when a required value is missing or malformed.
 */
export function configFromEnv(env: NodeJS.ProcessEnv): StdioConfig {
  const apiKey = env["LORE_API_KEY"];
  if (!apiKey) {
    throw new Error(
      "LORE_API_KEY is not set. Provision a key with `lore provision` (or `lore keys create`) and pass it to the MCP server as the LORE_API_KEY environment variable.",
    );
  }
  const baseUrl = env["LORE_BASE_URL"] || DEFAULT_BASE_URL;
  const timeoutMs = parseTimeout(env["LORE_TIMEOUT_MS"]);
  return { apiKey, baseUrl, timeoutMs };
}

function parseTimeout(raw: string | undefined): number | undefined {
  if (raw === undefined || raw === "") return undefined;
  const n = Number(raw);
  if (!Number.isFinite(n) || n <= 0) {
    throw new Error(`LORE_TIMEOUT_MS must be a positive number of milliseconds, got ${JSON.stringify(raw)}`);
  }
  return n;
}

/** Construct a {@link LoreRestClient} from a resolved {@link StdioConfig}. */
export function clientFromConfig(config: StdioConfig): LoreRestClient {
  return new LoreRestClient({
    apiKey: config.apiKey,
    baseUrl: config.baseUrl,
    ...(config.timeoutMs !== undefined ? { timeoutMs: config.timeoutMs } : {}),
  });
}
