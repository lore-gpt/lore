import { strict as assert } from "node:assert";
import { test } from "node:test";

import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StdioClientTransport } from "@modelcontextprotocol/sdk/client/stdio.js";

import {
  ALLOWED_TOOLS,
  LORE_MCP_PACKAGE,
  LORE_TOOLS,
  SERVER_NAME,
  agentOptions,
  loreServerConfig,
} from "../src/lore-mcp.ts";

// Long enough for `npx` to fetch and start the package on a cold CI runner, short enough that a hang
// fails the job instead of holding it until the workflow's own limit.
const HANDSHAKE_TIMEOUT_MS = 120_000;

test("the agent's options name the MCP server and allow its tools", () => {
  const options = agentOptions({ LORE_API_KEY: "lore_sk_test" });

  const server = options.mcpServers[SERVER_NAME];
  assert.ok(server, `no MCP server registered under ${SERVER_NAME}`);
  assert.equal(server.command, "npx");
  assert.ok(server.args.includes(LORE_MCP_PACKAGE), "the pinned package is not what gets launched");

  // Every allowed tool has to carry the mcp__<server>__ prefix, or the agent will see the tool and be
  // unable to call it — a failure that looks like the model refusing rather than a config mistake.
  for (const tool of options.allowedTools) {
    assert.match(tool, new RegExp(`^mcp__${SERVER_NAME}__`), `${tool} is not prefixed for this server`);
  }
});

test("the key is passed to the server explicitly, and only the key", () => {
  const server = loreServerConfig({ LORE_API_KEY: "lore_sk_test", SOMETHING_ELSE: "should not travel" });
  assert.deepEqual(server.env, { LORE_API_KEY: "lore_sk_test" });
});

test("an optional base URL travels, and a missing key fails loudly", () => {
  const withBase = loreServerConfig({ LORE_API_KEY: "k", LORE_BASE_URL: "http://lore.test:9000" });
  assert.equal(withBase.env["LORE_BASE_URL"], "http://lore.test:9000");
  // The failure has to name the variable — the server's own error is otherwise the first thing a user
  // sees, several seconds later, from a subprocess.
  assert.throws(() => loreServerConfig({}), /LORE_API_KEY/);
});

// This is the check worth having. Everything above compares the example against itself; this one starts
// the PUBLISHED MCP server — the artifact a user installs — and asks it what tools it has.
//
// The failure it exists to catch is a tool being renamed or dropped upstream. That break is invisible to
// a type-checker (the tool names are strings crossing a process boundary) and invisible to the agent
// until a model tries to call something that is not there. It has happened before in this project, on
// the published MCP package, which is why the pin and this test are a pair.
//
// It needs no Anthropic key and no running Lore: the server starts and lists its tools on a key it never
// validates. What it does NOT prove is that the agent loop works — see the README.
test("the published MCP server still offers every tool this example allows", { timeout: HANDSHAKE_TIMEOUT_MS }, async () => {
  const server = loreServerConfig({ LORE_API_KEY: "lore_sk_not_validated_by_a_handshake" });
  const transport = new StdioClientTransport({
    command: server.command,
    args: server.args,
    env: server.env,
  });
  const client = new Client({ name: "lore-example-tool-contract-check", version: "0.0.0" });

  try {
    await client.connect(transport);
    const offered = (await client.listTools()).tools.map((t) => t.name);

    for (const tool of LORE_TOOLS) {
      assert.ok(
        offered.includes(tool),
        `${LORE_MCP_PACKAGE} no longer offers "${tool}", which this example allows as ` +
          `mcp__${SERVER_NAME}__${tool}. It offers: ${offered.join(", ")}`,
      );
    }
    // Deliberately a subset check, not equality: the server gaining a tool is not this example's
    // problem, and asserting equality would turn every upstream addition into a false failure here.
    assert.equal(ALLOWED_TOOLS.length, LORE_TOOLS.length);
  } finally {
    // Closing the client closes the transport, which kills the subprocess. In a finally block because
    // a failed assertion above must not leave an orphaned `npx` behind on a CI runner.
    await client.close().catch(() => {});
  }
});
