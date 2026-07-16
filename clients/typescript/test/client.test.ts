import test from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import {
  InvalidRunIdError,
  LoreApiError,
  LoreClient,
  LoreConnectionError,
  LoreParseError,
  MinSeqOutOfRangeError,
  ModelMismatchError,
  NotFoundError,
  UnauthorizedError,
  UnknownLoreError,
  version,
} from "../src/index.ts";

interface RecordedCall {
  url: string;
  method: string;
  headers: Record<string, string>;
  body: unknown;
}

// A hand-written fake fetch: records each call and returns a built-in Response. This is the ONLY test seam —
// no msw, no nock. It doubles as the SDK's real `fetch` option (custom transport).
function recorder(
  status: number,
  responseBody?: unknown,
  opts: { throwErr?: Error; raw?: string } = {},
): { fetch: typeof globalThis.fetch; calls: RecordedCall[] } {
  const calls: RecordedCall[] = [];
  const fetch: typeof globalThis.fetch = async (input, init) => {
    const i = init ?? {};
    calls.push({
      url: String(input),
      method: i.method ?? "GET",
      headers: (i.headers ?? {}) as Record<string, string>,
      body: typeof i.body === "string" && i.body.length > 0 ? JSON.parse(i.body) : undefined,
    });
    if (opts.throwErr) throw opts.throwErr;
    const text =
      opts.raw !== undefined ? opts.raw : responseBody === undefined ? "" : JSON.stringify(responseBody);
    return new Response(text, { status, headers: { "content-type": "application/json" } });
  };
  return { fetch, calls };
}

function clientWith(fetch: typeof globalThis.fetch): LoreClient {
  return new LoreClient({ apiKey: "lore_sk_test", fetch });
}

test("createRun maps snake_case wire to camelCase and hits POST /v1/runs", async () => {
  const { fetch, calls } = recorder(201, { run_id: "run-1", created_at: "2026-07-16T00:00:00Z" });
  const res = await clientWith(fetch).createRun();
  assert.deepEqual(res, { runId: "run-1", createdAt: "2026-07-16T00:00:00Z" });
  assert.equal(calls.length, 1);
  assert.equal(calls[0]?.method, "POST");
  assert.ok(calls[0]?.url.endsWith("/v1/runs"));
  assert.equal(calls[0]?.headers["authorization"], "Bearer lore_sk_test");
  assert.equal(calls[0]?.headers["content-type"], "application/json");
  assert.deepEqual(calls[0]?.body, {});
});

test("write with content wraps it into { content } payload", async () => {
  const { fetch, calls } = recorder(202, { event_id: "ev-1", seq: 1 });
  const res = await clientWith(fetch).write({ runId: "r", agentId: "researcher", content: "hello" });
  assert.deepEqual(res, { eventId: "ev-1", seq: 1 });
  assert.ok(calls[0]?.url.endsWith("/v1/events"));
  assert.deepEqual(calls[0]?.body, { run_id: "r", agent_id: "researcher", payload: { content: "hello" } });
});

test("write with payload sends it verbatim", async () => {
  const { fetch, calls } = recorder(202, { event_id: "ev-2", seq: 2 });
  await clientWith(fetch).write({ runId: "r", agentId: "a", payload: { note: "x", n: 3 } });
  assert.deepEqual(calls[0]?.body, { run_id: "r", agent_id: "a", payload: { note: "x", n: 3 } });
});

test("writeState builds the kind:state fact payload", async () => {
  const { fetch, calls } = recorder(202, { event_id: "ev-3", seq: 3 });
  const res = await clientWith(fetch).writeState({
    runId: "r",
    agentId: "a",
    entity: "auth-service",
    predicate: "status",
    value: "up",
  });
  assert.deepEqual(res, { eventId: "ev-3", seq: 3 });
  assert.deepEqual(calls[0]?.body, {
    run_id: "r",
    agent_id: "a",
    payload: { kind: "state", entity: "auth-service", predicate: "status", value: "up" },
  });
});

test("pack maps the request and response, always sends min_seq, and omits absent optionals", async () => {
  const { fetch, calls } = recorder(200, {
    text: "PACK",
    sources: [{ id: "m1", kind: "semantic", score: 0.9, section: "distilled" }],
    covered_seq: 5,
    freshness_lag_ms: 12,
    saved_tokens: 100,
    working_source: "live",
    truncated: false,
  });
  const res = await clientWith(fetch).pack({ runId: "r", query: "auth", tokenBudget: 2000 });
  // Response mapping (snake -> camel).
  assert.deepEqual(res, {
    text: "PACK",
    sources: [{ id: "m1", kind: "semantic", score: 0.9, section: "distilled" }],
    coveredSeq: 5,
    freshnessLagMs: 12,
    savedTokens: 100,
    workingSource: "live",
    truncated: false,
  });
  // Request mapping: min_seq always present (0), token_budget mapped, scopes/limit/min_seq-from-user omitted.
  assert.deepEqual(calls[0]?.body, { run_id: "r", query: "auth", min_seq: 0, token_budget: 2000 });
});

test("pack forwards a string[] scopes verbatim and flattens an object to k:v", async () => {
  const packResponse = {
    text: "",
    sources: [],
    covered_seq: 0,
    freshness_lag_ms: 0,
    saved_tokens: 0,
    working_source: "skipped",
    truncated: false,
  };
  const a = recorder(200, packResponse);
  await clientWith(a.fetch).pack({ runId: "r", query: "q", scopes: ["team:a", "b"], minSeq: 7 });
  assert.deepEqual(a.calls[0]?.body, { run_id: "r", query: "q", min_seq: 7, scopes: ["team:a", "b"] });

  const b = recorder(200, packResponse);
  await clientWith(b.fetch).pack({ runId: "r", query: "q", scopes: { team: "platform", tier: "gold" } });
  assert.deepEqual(b.calls[0]?.body, {
    run_id: "r",
    query: "q",
    min_seq: 0,
    scopes: ["team:platform", "tier:gold"],
  });
});

test("server error codes map to the right typed error with status", async () => {
  const cases: Array<[number, string, new (...a: never[]) => Error]> = [
    [400, "invalid_run_id", InvalidRunIdError],
    [400, "min_seq_out_of_range", MinSeqOutOfRangeError],
    [401, "unauthorized", UnauthorizedError],
    [404, "not_found", NotFoundError],
    [409, "model_mismatch", ModelMismatchError],
  ];
  for (const [status, code, Cls] of cases) {
    const { fetch } = recorder(status, { code, message: `${code} happened` });
    await assert.rejects(
      () => clientWith(fetch).createRun(),
      (err: unknown) => {
        assert.ok(err instanceof Cls, `${code} -> ${Cls.name}`);
        assert.ok(err instanceof LoreApiError);
        assert.equal(err.code, code);
        assert.equal(err.httpStatus, status);
        return true;
      },
    );
  }
});

test("a missing or unmodelled code becomes UnknownLoreError keyed by status", async () => {
  const noCode = recorder(500, { message: "boom" });
  await assert.rejects(
    () => clientWith(noCode.fetch).createRun(),
    (err: unknown) => {
      assert.ok(err instanceof UnknownLoreError);
      assert.equal(err.code, "unknown");
      assert.equal(err.httpStatus, 500);
      assert.equal(err.rawCode, undefined);
      return true;
    },
  );

  const futureCode = recorder(418, { code: "future_code", message: "later" });
  await assert.rejects(
    () => clientWith(futureCode.fetch).createRun(),
    (err: unknown) => {
      assert.ok(err instanceof UnknownLoreError);
      assert.equal(err.rawCode, "future_code");
      assert.equal(err.httpStatus, 418);
      return true;
    },
  );
});

test("a transport failure is a LoreConnectionError; a non-JSON 2xx body is a LoreParseError", async () => {
  const down = recorder(0, undefined, { throwErr: new Error("ECONNREFUSED") });
  await assert.rejects(() => clientWith(down.fetch).createRun(), LoreConnectionError);

  const html = recorder(200, undefined, { raw: "<html>not json</html>" });
  await assert.rejects(() => clientWith(html.fetch).createRun(), LoreParseError);
});

test("a POST is not retried on a 500 (called exactly once)", async () => {
  const { fetch, calls } = recorder(500, { code: "internal", message: "boom" });
  await assert.rejects(() => clientWith(fetch).write({ runId: "r", agentId: "a", content: "x" }));
  assert.equal(calls.length, 1);
});

test("the exported version matches package.json", () => {
  const pkg = JSON.parse(readFileSync(new URL("../package.json", import.meta.url), "utf8")) as {
    version: string;
  };
  assert.equal(version, pkg.version);
});

test("a base URL with a trailing slash is normalized", async () => {
  const { fetch, calls } = recorder(201, { run_id: "r", created_at: "t" });
  await new LoreClient({ apiKey: "k", baseUrl: "http://example.test:9000/", fetch }).createRun();
  assert.equal(calls[0]?.url, "http://example.test:9000/v1/runs");
});

test("an empty apiKey throws at construction", () => {
  assert.throws(() => new LoreClient({ apiKey: "" }), TypeError);
});

// A fetch that resolves after delayMs unless the request's AbortSignal fires first (then it rejects) — so a
// too-slow request is cut off by the client's timeout.
function slowFetch(delayMs: number): typeof globalThis.fetch {
  return (_input, init) =>
    new Promise<Response>((resolve, reject) => {
      const timer = setTimeout(
        () => resolve(new Response("{}", { status: 201, headers: { "content-type": "application/json" } })),
        delayMs,
      );
      const signal = init?.signal;
      if (signal) {
        signal.addEventListener("abort", () => {
          clearTimeout(timer);
          reject(signal.reason);
        });
      }
    });
}

test("a custom timeoutMs aborts a slow request (not the 30s default)", async () => {
  // The fake resolves at 200ms; a 5ms timeout must abort first. Had the client used the default timeout, the
  // request would resolve and assert.rejects would fail — so this pins that the custom value is threaded.
  const client = new LoreClient({ apiKey: "k", timeoutMs: 5, fetch: slowFetch(200) });
  await assert.rejects(() => client.createRun(), LoreConnectionError);
});
