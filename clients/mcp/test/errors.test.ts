import { strict as assert } from "node:assert";
import { test } from "node:test";
import { fromResponse, UnknownLoreError } from "../src/errors.ts";

// The (HTTP status, machine code) -> error-class vector. This is the SAME vector the TypeScript SDK pins in
// its own error test: keeping the two identical is the contract-parity guarantee — the MCP client and the SDK
// can never map a server error differently.
const VECTOR: Array<{ status: number; code: string; cls: string }> = [
  { status: 400, code: "invalid_body", cls: "InvalidBodyError" },
  { status: 400, code: "invalid_run_id", cls: "InvalidRunIdError" },
  { status: 400, code: "min_seq_out_of_range", cls: "MinSeqOutOfRangeError" },
  { status: 401, code: "unauthorized", cls: "UnauthorizedError" },
  { status: 404, code: "not_found", cls: "NotFoundError" },
  { status: 409, code: "model_mismatch", cls: "ModelMismatchError" },
];

test("maps each (status, code) to the typed error class", () => {
  for (const { status, code, cls } of VECTOR) {
    const err = fromResponse(status, { code, message: `${code} happened` });
    assert.equal(err.code, code);
    assert.equal(err.httpStatus, status);
    assert.equal(err.constructor.name, cls);
    assert.equal(err.message, `${code} happened`);
  }
});

test("an unmodelled code becomes UnknownLoreError carrying the raw code", () => {
  const err = fromResponse(418, { code: "future_code", message: "later" });
  assert.equal(err.code, "unknown");
  assert.equal(err.httpStatus, 418);
  assert.ok(err instanceof UnknownLoreError);
  assert.equal(err.rawCode, "future_code");
});

test("a missing code becomes UnknownLoreError with no raw code", () => {
  const err = fromResponse(500, { message: "boom" });
  assert.equal(err.code, "unknown");
  assert.equal(err.httpStatus, 500);
  assert.ok(err instanceof UnknownLoreError);
  assert.equal(err.rawCode, undefined);
});

test("a blank message falls back to an HTTP-status message", () => {
  const err = fromResponse(503, { message: "" });
  assert.equal(err.message, "HTTP 503");
});

test("a whitespace-only message is passed through (only an empty message triggers the fallback)", () => {
  const err = fromResponse(503, { message: "   " });
  assert.equal(err.message, "   ");
});
