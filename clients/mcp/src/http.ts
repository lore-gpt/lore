import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import { LoreRestClient } from "./client.ts";
import type { HttpConfig } from "./config.ts";
import { buildServer } from "./server.ts";

const MCP_PATH = "/mcp";
// Socket-level guards against slow-client resource exhaustion (independent of the per-request Lore timeout).
const REQUEST_TIMEOUT_MS = 60_000;
const HEADERS_TIMEOUT_MS = 15_000;

class BodyTooLargeError extends Error {}

/**
 * Build a node:http request handler for the STATELESS streamable-HTTP MCP transport. Each request extracts a
 * bearer key from its OWN Authorization header, builds a per-request REST client + MCP server + transport
 * (`sessionIdGenerator: undefined` — no session state is retained), and disposes them once handling completes.
 * So concurrent clients never share state or credentials, and the process stays stateless. `fetchImpl`
 * overrides the REST client's fetch — a test seam.
 */
export function createHttpHandler(
  config: HttpConfig,
  fetchImpl?: typeof globalThis.fetch,
): (req: IncomingMessage, res: ServerResponse) => void {
  return (req, res) => {
    handle(req, res, config, fetchImpl).catch((err: unknown) => {
      // Backstop: handle() is written not to reject, but never leak an unhandled rejection from the listener.
      process.stderr.write(`lore-mcp: unhandled http error: ${errorMessage(err)}\n`);
      if (!res.headersSent) writeJson(res, 500, { error: "internal", message: "an internal error occurred" });
    });
  };
}

/** Start the stateless streamable-HTTP MCP server and resolve once it is listening. */
export function serveHttp(config: HttpConfig, fetchImpl?: typeof globalThis.fetch): Promise<Server> {
  const server = createServer(createHttpHandler(config, fetchImpl));
  server.requestTimeout = REQUEST_TIMEOUT_MS;
  server.headersTimeout = HEADERS_TIMEOUT_MS;
  return new Promise((resolve) => {
    server.listen(config.port, config.host, () => resolve(server));
  });
}

async function handle(
  req: IncomingMessage,
  res: ServerResponse,
  config: HttpConfig,
  fetchImpl: typeof globalThis.fetch | undefined,
): Promise<void> {
  const url = new URL(req.url ?? "/", "http://localhost");
  if (url.pathname !== MCP_PATH) {
    writeJson(res, 404, { error: "not_found", message: `unknown path ${url.pathname}` });
    return;
  }

  // Per-request auth: the caller's own bearer key is passed through to the Lore server. A missing or
  // malformed header is rejected here, before any MCP processing — the server never holds a key of its own.
  const apiKey = bearerToken(req.headers.authorization);
  if (apiKey === undefined) {
    writeJson(res, 401, { error: "unauthorized", message: "missing or malformed Authorization: Bearer <key> header" });
    return;
  }

  let body: unknown;
  if (req.method === "POST") {
    try {
      body = await readJsonBody(req, config.maxBodyBytes);
    } catch (err) {
      if (err instanceof BodyTooLargeError) {
        writeJson(res, 413, { error: "payload_too_large", message: "request body exceeds the size limit" });
        return;
      }
      if (err instanceof SyntaxError) {
        writeJson(res, 400, { error: "invalid_json", message: "request body is not valid JSON" });
        return;
      }
      throw err;
    }
  }

  const client = new LoreRestClient({
    apiKey,
    baseUrl: config.baseUrl,
    ...(config.timeoutMs !== undefined ? { timeoutMs: config.timeoutMs } : {}),
    ...(fetchImpl !== undefined ? { fetch: fetchImpl } : {}),
  });
  const server = buildServer(client);
  const transport = new StreamableHTTPServerTransport({
    sessionIdGenerator: undefined,
    enableJsonResponse: true,
    // DNS-rebinding protection is enabled only when an allowlist is configured (see HttpConfig.allowedHosts).
    ...(config.allowedHosts !== undefined
      ? { enableDnsRebindingProtection: true, allowedHosts: config.allowedHosts }
      : {}),
  });
  try {
    await server.connect(transport);
    await transport.handleRequest(req, res, body);
  } catch (err) {
    // Log the detail for the operator; return a generic message so internals never leak to the HTTP client.
    process.stderr.write(`lore-mcp: http handler error: ${errorMessage(err)}\n`);
    if (!res.headersSent) writeJson(res, 500, { error: "internal", message: "an internal error occurred" });
  } finally {
    // Dispose AFTER handling completes (not on res 'close'), so cleanup never races an in-flight response.
    await Promise.allSettled([transport.close(), server.close()]);
  }
}

function bearerToken(header: string | undefined): string | undefined {
  if (header === undefined) return undefined;
  const match = /^bearer\s+(.+)$/i.exec(header.trim());
  const token = match?.[1]?.trim();
  return token !== undefined && token.length > 0 ? token : undefined;
}

async function readJsonBody(req: IncomingMessage, maxBytes: number): Promise<unknown> {
  const chunks: Buffer[] = [];
  let total = 0;
  for await (const chunk of req) {
    const buf = chunk as Buffer;
    total += buf.length;
    if (total > maxBytes) throw new BodyTooLargeError();
    chunks.push(buf);
  }
  const raw = Buffer.concat(chunks).toString("utf8");
  if (raw.length === 0) return undefined;
  return JSON.parse(raw) as unknown;
}

function writeJson(res: ServerResponse, status: number, body: unknown): void {
  res.writeHead(status, { "content-type": "application/json" });
  res.end(JSON.stringify(body));
}

function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
