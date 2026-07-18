// Test harness: wire an in-memory MCP client to a server built over a stubbed fetch, so tools are exercised
// through a real MCP round-trip (client.callTool -> transport -> server -> tool handler -> REST client).
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js";
import { buildServer, LoreRestClient } from "../src/index.ts";

/** One recorded outbound REST request. */
export interface RecordedCall {
  path: string;
  method: string;
  authorization: string | undefined;
  contentType: string | undefined;
  body: unknown;
}

export interface FetchStub {
  fetch: typeof globalThis.fetch;
  calls: RecordedCall[];
}

interface Route {
  status: number;
  body?: unknown;
  raw?: string;
}

/** A fetch stub that records each request and returns a canned JSON response keyed by URL path. */
export function stubFetch(routes: Record<string, Route>): FetchStub {
  const calls: RecordedCall[] = [];
  const fetch: typeof globalThis.fetch = async (input, init) => {
    const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
    const path = new URL(url).pathname;
    const headers = new Headers(init?.headers);
    const rawBody = typeof init?.body === "string" ? init.body : undefined;
    calls.push({
      path,
      method: init?.method ?? "GET",
      authorization: headers.get("authorization") ?? undefined,
      contentType: headers.get("content-type") ?? undefined,
      body: rawBody !== undefined ? (JSON.parse(rawBody) as unknown) : undefined,
    });
    const route = routes[path];
    if (route === undefined) throw new Error(`stubFetch: no route registered for ${path}`);
    const responseBody = route.raw ?? JSON.stringify(route.body ?? {});
    return new Response(responseBody, { status: route.status, headers: { "content-type": "application/json" } });
  };
  return { fetch, calls };
}

/** A fetch that always throws — models a transport failure (connection refused, timeout, abort). */
export function throwingFetch(err: Error): typeof globalThis.fetch {
  return () => Promise.reject(err);
}

export interface Connected {
  client: Client;
  close: () => Promise<void>;
}

/** Connect an in-memory MCP client to a Lore MCP server built over the given fetch. */
export async function connect(fetchImpl: typeof globalThis.fetch): Promise<Connected> {
  const rest = new LoreRestClient({ apiKey: "lore_sk_test", fetch: fetchImpl });
  const server = buildServer(rest);
  const [clientTransport, serverTransport] = InMemoryTransport.createLinkedPair();
  const client = new Client({ name: "lore-mcp-test", version: "0.0.0" });
  await Promise.all([server.connect(serverTransport), client.connect(clientTransport)]);
  return {
    client,
    close: async () => {
      await client.close();
      await server.close();
    },
  };
}

/** Concatenate the text of a tool result's text content blocks. Accepts the callTool result union defensively
 * (the SDK's return type unions the modern content shape with a legacy one). */
export function textOf(res: unknown): string {
  const content = (res as { content?: ReadonlyArray<{ type: string; text?: string }> }).content;
  if (!Array.isArray(content)) return "";
  return content.map((c) => (c.type === "text" && typeof c.text === "string" ? c.text : "")).join("");
}
