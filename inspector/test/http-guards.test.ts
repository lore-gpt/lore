import { isAllowedHost, isSameOrigin } from "../src/lib/http-guards.ts";
import assert from "node:assert/strict";
import test from "node:test";

test("isAllowedHost: allows loopback names by default (with or without a port)", () => {
  for (const host of ["localhost", "localhost:3000", "127.0.0.1", "127.0.0.1:3000", "[::1]", "[::1]:3000"]) {
    assert.equal(isAllowedHost(host, undefined), true, host);
  }
});

test("isAllowedHost: refuses non-loopback hosts by default (the DNS-rebinding case)", () => {
  for (const host of ["evil.com", "evil.com:3000", "example.org", "127.0.0.1.evil.com", "0.0.0.0", "10.0.0.1"]) {
    assert.equal(isAllowedHost(host, undefined), false, host);
  }
});

test("isAllowedHost: a missing or empty Host is refused", () => {
  assert.equal(isAllowedHost(null, undefined), false);
  assert.equal(isAllowedHost("", undefined), false);
});

test("isAllowedHost: matching is case-insensitive", () => {
  assert.equal(isAllowedHost("LOCALHOST:3000", undefined), true);
  assert.equal(isAllowedHost("Evil.COM", "TRUSTED.example"), false);
});

test("isAllowedHost: the allowlist ADDS hosts and loopback is still allowed", () => {
  // Loopback stays allowed even when an allowlist is set — a same-machine caller is never a rebinding target.
  assert.equal(isAllowedHost("127.0.0.1:3000", "trusted.example"), true);
  // A listed host matches by full host, by host:port, or by bare hostname ignoring the port.
  assert.equal(isAllowedHost("trusted.example", "trusted.example"), true);
  assert.equal(isAllowedHost("trusted.example:8080", "trusted.example"), true);
  assert.equal(isAllowedHost("trusted.example:8080", "trusted.example:8080"), true);
  // An unlisted non-loopback host is still refused.
  assert.equal(isAllowedHost("evil.com", "trusted.example"), false);
});

test("isAllowedHost: '*' allows any host, and blanks/whitespace in the list are ignored", () => {
  assert.equal(isAllowedHost("anything.example", "*"), true);
  assert.equal(isAllowedHost("a.example", " a.example , , b.example "), true);
  assert.equal(isAllowedHost("b.example", " a.example , , b.example "), true);
  assert.equal(isAllowedHost("c.example", " a.example , , b.example "), false);
});

test("isSameOrigin: Sec-Fetch-Site decides when present", () => {
  assert.equal(isSameOrigin("same-origin", null, "localhost:3000"), true);
  assert.equal(isSameOrigin("none", null, "localhost:3000"), true); // user-initiated (address bar)
  assert.equal(isSameOrigin("cross-site", "https://evil.com", "localhost:3000"), false);
  assert.equal(isSameOrigin("same-site", null, "localhost:3000"), false); // a sibling subdomain is not same-origin
});

test("isSameOrigin: falls back to Origin vs Host when Sec-Fetch-Site is absent", () => {
  assert.equal(isSameOrigin(null, null, "localhost:3000"), true); // no Origin ⇒ non-CORS ⇒ allowed
  assert.equal(isSameOrigin(null, "http://localhost:3000", "localhost:3000"), true);
  assert.equal(isSameOrigin(null, "http://evil.com", "localhost:3000"), false);
  assert.equal(isSameOrigin(null, "not-a-url", "localhost:3000"), false); // a malformed Origin is refused
});
