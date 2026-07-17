import { strict as assert } from "node:assert";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import { VERSION } from "../src/version.ts";

test("the exported VERSION matches package.json", () => {
  const pkgUrl = new URL("../package.json", import.meta.url);
  const pkg = JSON.parse(readFileSync(pkgUrl, "utf8")) as { version: string };
  assert.equal(VERSION, pkg.version);
});
