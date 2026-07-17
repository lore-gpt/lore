import { strict as assert } from "node:assert";
import { test } from "node:test";
import { clientFromConfig, configFromEnv } from "../src/config.ts";

test("missing LORE_API_KEY throws a message naming the variable, never a key", () => {
  assert.throws(() => configFromEnv({}), /LORE_API_KEY/);
});

test("defaults the base URL and leaves the timeout unset", () => {
  const config = configFromEnv({ LORE_API_KEY: "lore_sk_test" });
  assert.equal(config.apiKey, "lore_sk_test");
  assert.equal(config.baseUrl, "http://localhost:8080");
  assert.equal(config.timeoutMs, undefined);
});

test("honours LORE_BASE_URL and LORE_TIMEOUT_MS", () => {
  const config = configFromEnv({
    LORE_API_KEY: "k",
    LORE_BASE_URL: "https://lore.example.test:9000",
    LORE_TIMEOUT_MS: "5000",
  });
  assert.equal(config.baseUrl, "https://lore.example.test:9000");
  assert.equal(config.timeoutMs, 5000);
});

test("rejects a non-positive or non-numeric timeout", () => {
  assert.throws(() => configFromEnv({ LORE_API_KEY: "k", LORE_TIMEOUT_MS: "0" }), /LORE_TIMEOUT_MS/);
  assert.throws(() => configFromEnv({ LORE_API_KEY: "k", LORE_TIMEOUT_MS: "abc" }), /LORE_TIMEOUT_MS/);
});

test("clientFromConfig builds a client without throwing", () => {
  const client = clientFromConfig({ apiKey: "k", baseUrl: "http://localhost:8080", timeoutMs: undefined });
  assert.ok(client);
});
