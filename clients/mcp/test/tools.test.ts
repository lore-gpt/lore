import { strict as assert } from "node:assert";
import { test } from "node:test";
import { connect, stubFetch, textOf, throwingFetch } from "./harness.ts";

const RUN = { run_id: "run-1", created_at: "2026-07-16T00:00:00Z" };
const EVENT = { event_id: "ev-1", seq: 7 };
const PACK = {
  text: "PACK BODY",
  sources: [{ id: "m1", kind: "semantic", score: 0.9, section: "distilled" }],
  covered_seq: 5,
  freshness_lag_ms: 12,
  saved_tokens: 100,
  working_source: "live",
  truncated: false,
};

test("advertises exactly the three live tools — no stub tools", async () => {
  const { client, close } = await connect(stubFetch({}).fetch);
  try {
    const { tools } = await client.listTools();
    assert.deepEqual(
      tools.map((t) => t.name).sort(),
      ["create_run", "memory_pack", "memory_write"],
    );
  } finally {
    await close();
  }
});

test("create_run hits POST /v1/runs with bearer auth and returns a structured run", async () => {
  const stub = stubFetch({ "/v1/runs": { status: 201, body: RUN } });
  const { client, close } = await connect(stub.fetch);
  try {
    const res = await client.callTool({ name: "create_run", arguments: {} });
    assert.equal(res.isError ?? false, false);
    assert.deepEqual(res.structuredContent, { run_id: "run-1", created_at: "2026-07-16T00:00:00Z" });
    assert.match(textOf(res), /run-1/);
    const call = stub.calls[0];
    assert.equal(call?.path, "/v1/runs");
    assert.equal(call?.method, "POST");
    assert.equal(call?.authorization, "Bearer lore_sk_test");
    assert.equal(call?.contentType, "application/json");
  } finally {
    await close();
  }
});

test("memory_write with content wraps the payload and surfaces the seq for read-your-writes", async () => {
  const stub = stubFetch({ "/v1/events": { status: 202, body: EVENT } });
  const { client, close } = await connect(stub.fetch);
  try {
    const res = await client.callTool({
      name: "memory_write",
      arguments: { run_id: "r", agent_id: "researcher", content: "hello" },
    });
    assert.equal(res.isError ?? false, false);
    assert.deepEqual(res.structuredContent, { event_id: "ev-1", seq: 7 });
    assert.match(textOf(res), /min_seq=7/);
    assert.deepEqual(stub.calls[0]?.body, { run_id: "r", agent_id: "researcher", payload: { content: "hello" } });
  } finally {
    await close();
  }
});

test("memory_write with payload sends it verbatim", async () => {
  const stub = stubFetch({ "/v1/events": { status: 202, body: EVENT } });
  const { client, close } = await connect(stub.fetch);
  try {
    await client.callTool({
      name: "memory_write",
      arguments: { run_id: "r", agent_id: "a", payload: { note: "x", n: 3 } },
    });
    assert.deepEqual(stub.calls[0]?.body, { run_id: "r", agent_id: "a", payload: { note: "x", n: 3 } });
  } finally {
    await close();
  }
});

test("memory_write with state builds the kind:state working-memory fact", async () => {
  const stub = stubFetch({ "/v1/events": { status: 202, body: EVENT } });
  const { client, close } = await connect(stub.fetch);
  try {
    await client.callTool({
      name: "memory_write",
      arguments: { run_id: "r", agent_id: "a", state: { entity: "auth-service", predicate: "status", value: "up" } },
    });
    assert.deepEqual(stub.calls[0]?.body, {
      run_id: "r",
      agent_id: "a",
      payload: { kind: "state", entity: "auth-service", predicate: "status", value: "up" },
    });
  } finally {
    await close();
  }
});

test("memory_write rejects both and neither of content/payload/state without calling the API", async () => {
  const stub = stubFetch({ "/v1/events": { status: 202, body: EVENT } });
  const { client, close } = await connect(stub.fetch);
  try {
    const both = await client.callTool({
      name: "memory_write",
      arguments: { run_id: "r", agent_id: "a", content: "x", payload: { a: 1 } },
    });
    assert.equal(both.isError, true);
    // The error names which fields were supplied, so a calling model can self-correct on the next turn.
    assert.match(textOf(both), /expected exactly one of content\|payload\|state; got: content, payload/);

    const neither = await client.callTool({ name: "memory_write", arguments: { run_id: "r", agent_id: "a" } });
    assert.equal(neither.isError, true);
    assert.match(textOf(neither), /got: none/);

    assert.equal(stub.calls.length, 0);
  } finally {
    await close();
  }
});

test("memory_pack always sends min_seq, returns the pack text verbatim, and structures the metadata", async () => {
  const stub = stubFetch({ "/v1/pack": { status: 200, body: PACK } });
  const { client, close } = await connect(stub.fetch);
  try {
    const res = await client.callTool({
      name: "memory_pack",
      arguments: { run_id: "r", query: "auth", token_budget: 2000 },
    });
    assert.equal(res.isError ?? false, false);
    // The pack text is returned as-is, never reformatted.
    assert.equal(textOf(res), "PACK BODY");
    assert.deepEqual(res.structuredContent, {
      covered_seq: 5,
      freshness_lag_ms: 12,
      saved_tokens: 100,
      working_source: "live",
      truncated: false,
      sources: [{ id: "m1", kind: "semantic", score: 0.9, section: "distilled" }],
    });
    // min_seq is always sent (0 when omitted); scopes/limit omitted are absent.
    assert.deepEqual(stub.calls[0]?.body, { run_id: "r", query: "auth", min_seq: 0, token_budget: 2000 });
  } finally {
    await close();
  }
});

test("memory_pack forwards min_seq, scopes, and limit when given", async () => {
  const stub = stubFetch({ "/v1/pack": { status: 200, body: PACK } });
  const { client, close } = await connect(stub.fetch);
  try {
    await client.callTool({
      name: "memory_pack",
      arguments: { run_id: "r", query: "q", min_seq: 7, scopes: ["team:a", "b"], limit: 5 },
    });
    assert.deepEqual(stub.calls[0]?.body, {
      run_id: "r",
      query: "q",
      min_seq: 7,
      scopes: ["team:a", "b"],
      limit: 5,
    });
  } finally {
    await close();
  }
});

const ERROR_CASES: Array<{ status: number; code: string }> = [
  { status: 400, code: "invalid_run_id" },
  { status: 400, code: "min_seq_out_of_range" },
  { status: 401, code: "unauthorized" },
  { status: 404, code: "not_found" },
  { status: 409, code: "model_mismatch" },
];

for (const { status, code } of ERROR_CASES) {
  test(`a ${status} ${code} response surfaces as a tool error carrying the machine code`, async () => {
    const stub = stubFetch({ "/v1/pack": { status, body: { code, message: `${code} happened` } } });
    const { client, close } = await connect(stub.fetch);
    try {
      const res = await client.callTool({ name: "memory_pack", arguments: { run_id: "r", query: "q" } });
      assert.equal(res.isError, true);
      assert.match(textOf(res), new RegExp(`^${code}: `));
    } finally {
      await close();
    }
  });
}

test("a transport failure surfaces as a connection tool error", async () => {
  const { client, close } = await connect(throwingFetch(new TypeError("connection refused")));
  try {
    const res = await client.callTool({ name: "create_run", arguments: {} });
    assert.equal(res.isError, true);
    assert.match(textOf(res), /^connection: /);
  } finally {
    await close();
  }
});

test("an unmodelled server code surfaces as an unknown tool error", async () => {
  const stub = stubFetch({ "/v1/runs": { status: 500, body: { message: "boom" } } });
  const { client, close } = await connect(stub.fetch);
  try {
    const res = await client.callTool({ name: "create_run", arguments: {} });
    assert.equal(res.isError, true);
    assert.match(textOf(res), /^unknown: /);
  } finally {
    await close();
  }
});
