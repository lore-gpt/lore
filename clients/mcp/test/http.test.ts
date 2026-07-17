import { strict as assert } from "node:assert";
import type { Server } from "node:http";
import type { AddressInfo } from "node:net";
import { test } from "node:test";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";
import type { HttpConfig } from "../src/config.ts";
import { serveHttp } from "../src/http.ts";
import { type FetchStub, stubFetch, textOf } from "./harness.ts";

const RUN = { run_id: "run-9", created_at: "2026-07-17T00:00:00Z" };

function config(overrides?: Partial<HttpConfig>): HttpConfig {
  return {
    baseUrl: "http://lore.internal:8080",
    timeoutMs: undefined,
    host: "127.0.0.1",
    port: 0,
    allowedHosts: undefined,
    maxBodyBytes: 4 * 1024 * 1024,
    ...overrides,
  };
}

async function start(
  stub: FetchStub,
  overrides?: Partial<HttpConfig>,
): Promise<{ server: Server; url: URL; base: string }> {
  const server = await serveHttp(config(overrides), stub.fetch);
  const port = (server.address() as AddressInfo).port;
  return { server, url: new URL(`http://127.0.0.1:${port}/mcp`), base: `http://127.0.0.1:${port}` };
}

function stop(server: Server): Promise<void> {
  return new Promise((resolve) => server.close(() => resolve()));
}

async function connectClient(url: URL, apiKey: string): Promise<Client> {
  const transport = new StreamableHTTPClientTransport(url, {
    requestInit: { headers: { authorization: `Bearer ${apiKey}` } },
  });
  const client = new Client({ name: "lore-mcp-http-test", version: "0.0.0" });
  await client.connect(transport);
  return client;
}

test("create_run round-trips over HTTP and passes the request's bearer key through to Lore", async () => {
  const stub = stubFetch({ "/v1/runs": { status: 201, body: RUN } });
  const { server, url } = await start(stub);
  try {
    const client = await connectClient(url, "lore_sk_alice");
    const res = await client.callTool({ name: "create_run", arguments: {} });
    assert.deepEqual(res.structuredContent, { run_id: "run-9", created_at: "2026-07-17T00:00:00Z" });
    assert.equal(stub.calls[0]?.authorization, "Bearer lore_sk_alice");
    await client.close();
  } finally {
    await stop(server);
  }
});

test("each HTTP request carries its own key — per-request auth passthrough, no shared credential", async () => {
  const stub = stubFetch({ "/v1/runs": { status: 201, body: RUN } });
  const { server, url } = await start(stub);
  try {
    const alice = await connectClient(url, "lore_sk_alice");
    await alice.callTool({ name: "create_run", arguments: {} });
    await alice.close();

    const bob = await connectClient(url, "lore_sk_bob");
    await bob.callTool({ name: "create_run", arguments: {} });
    await bob.close();

    assert.deepEqual(
      stub.calls.map((c) => c.authorization),
      ["Bearer lore_sk_alice", "Bearer lore_sk_bob"],
    );
  } finally {
    await stop(server);
  }
});

test("a request with no bearer key is rejected 401 before any MCP processing or Lore call", async () => {
  const stub = stubFetch({});
  const { server, url } = await start(stub);
  try {
    const res = await fetch(url, {
      method: "POST",
      headers: { "content-type": "application/json", accept: "application/json, text/event-stream" },
      body: "{}",
    });
    assert.equal(res.status, 401);
    assert.equal(stub.calls.length, 0);
  } finally {
    await stop(server);
  }
});

test("an unknown path is 404", async () => {
  const stub = stubFetch({});
  const { server, base } = await start(stub);
  try {
    const res = await fetch(`${base}/nope`, {
      method: "POST",
      headers: { authorization: "Bearer k", "content-type": "application/json" },
      body: "{}",
    });
    assert.equal(res.status, 404);
  } finally {
    await stop(server);
  }
});

test("a body over the size cap is rejected 413 before any MCP processing or Lore call", async () => {
  const stub = stubFetch({});
  const { server, url } = await start(stub, { maxBodyBytes: 64 });
  try {
    const res = await fetch(url, {
      method: "POST",
      headers: { authorization: "Bearer k", "content-type": "application/json" },
      body: JSON.stringify({ data: "x".repeat(256) }),
    });
    assert.equal(res.status, 413);
    assert.equal(((await res.json()) as { error: string }).error, "payload_too_large");
    assert.equal(stub.calls.length, 0);
  } finally {
    await stop(server);
  }
});

test("a malformed JSON body is rejected 400 before any MCP processing or Lore call", async () => {
  const stub = stubFetch({});
  const { server, url } = await start(stub);
  try {
    const res = await fetch(url, {
      method: "POST",
      headers: { authorization: "Bearer k", "content-type": "application/json" },
      body: "{not valid json",
    });
    assert.equal(res.status, 400);
    assert.equal(((await res.json()) as { error: string }).error, "invalid_json");
    assert.equal(stub.calls.length, 0);
  } finally {
    await stop(server);
  }
});

test("a Lore 4xx error over HTTP surfaces as an isError tool result carrying the machine code", async () => {
  const stub = stubFetch({ "/v1/runs": { status: 400, body: { code: "invalid_run_id", message: "bad run" } } });
  const { server, url } = await start(stub);
  try {
    const client = await connectClient(url, "lore_sk_test");
    const res = await client.callTool({ name: "create_run", arguments: {} });
    assert.equal(res.isError, true);
    assert.match(textOf(res), /^invalid_run_id: /);
    await client.close();
  } finally {
    await stop(server);
  }
});
