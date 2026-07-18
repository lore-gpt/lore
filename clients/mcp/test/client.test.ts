import { strict as assert } from "node:assert";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import { test } from "node:test";
import { LoreConnectionError } from "../src/errors.ts";
import { LoreRestClient } from "../src/index.ts";
import { stubFetch } from "./harness.ts";

test("normalizes a trailing-slash base URL and wires the given key into the bearer header", async () => {
  const stub = stubFetch({ "/v1/runs": { status: 201, body: { run_id: "r", created_at: "t" } } });
  const client = new LoreRestClient({
    apiKey: "lore_sk_other",
    baseUrl: "http://example.test:9000/",
    fetch: stub.fetch,
  });
  await client.createRun();
  const call = stub.calls[0];
  assert.equal(call?.path, "/v1/runs");
  assert.equal(call?.authorization, "Bearer lore_sk_other");
});

test("aborts a request that exceeds the configured timeout, surfaced as a connection error", async () => {
  const server = createServer((_req, res) => {
    // Respond well after the client's timeout so the abort fires first.
    setTimeout(() => {
      res.writeHead(201, { "content-type": "application/json" });
      res.end("{}");
    }, 2000);
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", () => resolve()));
  const port = (server.address() as AddressInfo).port;
  const client = new LoreRestClient({ apiKey: "k", baseUrl: `http://127.0.0.1:${port}`, timeoutMs: 200 });
  try {
    await assert.rejects(() => client.createRun(), LoreConnectionError);
  } finally {
    await new Promise<void>((resolve) => server.close(() => resolve()));
  }
});

test("rejects construction without an api key", () => {
  assert.throws(() => new LoreRestClient({ apiKey: "" }), TypeError);
});
