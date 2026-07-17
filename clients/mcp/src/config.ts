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

/**
 * Configuration for the streamable-HTTP server. Unlike stdio, NO API key lives here: each HTTP request
 * carries its own bearer key (Authorization header), passed through to the Lore server per request.
 */
export interface HttpConfig {
  /** Base URL of the Lore server. */
  baseUrl: string;
  /** Optional per-request timeout in milliseconds. */
  timeoutMs: number | undefined;
  /** Host to bind. Defaults to loopback (127.0.0.1); set to 0.0.0.0 only behind auth/TLS you control. */
  host: string;
  /** Port to listen on. */
  port: number;
  /**
   * Host allowlist for DNS-rebinding protection. When set, the transport validates the request Host header
   * against it. Undefined leaves protection off — safe for the loopback default (and per-request auth already
   * blocks any unauthenticated caller); set it when binding beyond loopback.
   */
  allowedHosts: string[] | undefined;
  /** Maximum accepted request body size in bytes (a memory-exhaustion guard). */
  maxBodyBytes: number;
}

const DEFAULT_HOST = "127.0.0.1";
const DEFAULT_PORT = 3000;
const DEFAULT_MAX_BODY_BYTES = 4 * 1024 * 1024;

/**
 * Read the streamable-HTTP server configuration from the environment (LORE_BASE_URL, LORE_TIMEOUT_MS,
 * LORE_MCP_HOST, LORE_MCP_PORT, LORE_MCP_ALLOWED_HOSTS, LORE_MAX_BODY_BYTES), with optional CLI overrides. No
 * LORE_API_KEY is read — the key is per request. Throws a descriptive Error on a malformed value.
 */
export function httpConfigFromEnv(env: NodeJS.ProcessEnv, overrides?: { host?: string; port?: string }): HttpConfig {
  const baseUrl = env["LORE_BASE_URL"] || DEFAULT_BASE_URL;
  const timeoutMs = parseTimeout(env["LORE_TIMEOUT_MS"]);
  const host = overrides?.host || env["LORE_MCP_HOST"] || DEFAULT_HOST;
  const port = parsePort(overrides?.port ?? env["LORE_MCP_PORT"]) ?? DEFAULT_PORT;
  const allowedHosts = parseAllowedHosts(env["LORE_MCP_ALLOWED_HOSTS"]);
  const maxBodyBytes = parseMaxBodyBytes(env["LORE_MAX_BODY_BYTES"]);
  return { baseUrl, timeoutMs, host, port, allowedHosts, maxBodyBytes };
}

function parsePort(raw: string | undefined): number | undefined {
  if (raw === undefined || raw === "") return undefined;
  const n = Number(raw);
  if (!Number.isInteger(n) || n < 1 || n > 65535) {
    throw new Error(`port must be an integer in 1-65535, got ${JSON.stringify(raw)}`);
  }
  return n;
}

function parseAllowedHosts(raw: string | undefined): string[] | undefined {
  if (raw === undefined || raw.trim() === "") return undefined;
  const hosts = raw
    .split(",")
    .map((h) => h.trim())
    .filter((h) => h.length > 0);
  return hosts.length > 0 ? hosts : undefined;
}

function parseMaxBodyBytes(raw: string | undefined): number {
  if (raw === undefined || raw === "") return DEFAULT_MAX_BODY_BYTES;
  const n = Number(raw);
  if (!Number.isInteger(n) || n < 1) {
    throw new Error(`LORE_MAX_BODY_BYTES must be a positive integer, got ${JSON.stringify(raw)}`);
  }
  return n;
}
