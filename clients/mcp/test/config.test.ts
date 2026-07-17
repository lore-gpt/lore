import { strict as assert } from "node:assert";
import { test } from "node:test";
import { clientFromConfig, configFromEnv, httpConfigFromEnv } from "../src/config.ts";

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

test("httpConfigFromEnv defaults host/port/body-cap, leaves the host allowlist off, and needs no API key", () => {
  const c = httpConfigFromEnv({});
  assert.equal(c.baseUrl, "http://localhost:8080");
  assert.equal(c.host, "127.0.0.1");
  assert.equal(c.port, 3000);
  assert.equal(c.timeoutMs, undefined);
  assert.equal(c.allowedHosts, undefined);
  assert.equal(c.maxBodyBytes, 4 * 1024 * 1024);
});

test("httpConfigFromEnv parses a comma-separated host allowlist and a custom body cap", () => {
  const c = httpConfigFromEnv({
    LORE_MCP_ALLOWED_HOSTS: "lore.example.com, 127.0.0.1:3000 ,",
    LORE_MAX_BODY_BYTES: "1048576",
  });
  assert.deepEqual(c.allowedHosts, ["lore.example.com", "127.0.0.1:3000"]);
  assert.equal(c.maxBodyBytes, 1048576);
});

test("httpConfigFromEnv rejects a non-positive body cap", () => {
  assert.throws(() => httpConfigFromEnv({ LORE_MAX_BODY_BYTES: "0" }), /LORE_MAX_BODY_BYTES/);
});

test("httpConfigFromEnv honours env, and a CLI port override beats the env", () => {
  const c = httpConfigFromEnv(
    { LORE_BASE_URL: "http://lore.test:9", LORE_MCP_HOST: "0.0.0.0", LORE_MCP_PORT: "8000" },
    { port: "9999" },
  );
  assert.equal(c.baseUrl, "http://lore.test:9");
  assert.equal(c.host, "0.0.0.0");
  assert.equal(c.port, 9999);
});

test("httpConfigFromEnv rejects an out-of-range or non-numeric port", () => {
  assert.throws(() => httpConfigFromEnv({ LORE_MCP_PORT: "70000" }), /port/);
  assert.throws(() => httpConfigFromEnv({}, { port: "abc" }), /port/);
});
