/**
 * A Claude Agent SDK agent that coordinates through Lore's MCP tools.
 *
 * The other examples in this directory call Lore's SDK from code the author wrote: the program decides
 * when to write and when to pack. This one is different on purpose. The agent is given Lore as a set of
 * MCP tools and decides for itself — it creates a run, records what it worked out, and packs the run
 * back when it needs to know where things stand. Nothing here calls Lore directly.
 *
 * That makes the interesting surface the tool contract rather than a client library, which is why the
 * check that runs in CI starts the MCP server and compares the tools it offers against the ones this
 * file allows. See ./lore-mcp.ts for the wiring, and ../README.md for what CI does and does not prove.
 *
 * Prerequisites: a running Lore (see the repo README quickstart), its API key, and an Anthropic key for
 * the agent itself:
 *
 *     export LORE_API_KEY=...        # from ./.lore/credentials
 *     export ANTHROPIC_API_KEY=...
 *     pnpm start
 */

import { query } from "@anthropic-ai/claude-agent-sdk";

import { agentOptions } from "./lore-mcp.ts";

const TASK = `You are coordinating a small team's work on an auth service migration.

1. Create a Lore run for this task.
2. Record that the auth service moved to OAuth v2 and the legacy token path is deprecated.
3. Pack the run back and tell me, in one sentence, what the team currently knows.

Use the Lore tools for all of it — do not answer from memory.`;

async function main(): Promise<void> {
  for await (const message of query({ prompt: TASK, options: agentOptions() })) {
    // The SDK streams the whole conversation — tool calls, results, and the assistant's turns. Printing
    // the assistant's text keeps the output readable; drop this filter to watch the tool traffic.
    if (message.type === "assistant") {
      for (const block of message.message.content) {
        if (block.type === "text") {
          process.stdout.write(block.text);
        }
      }
    }
  }
  process.stdout.write("\n");
}

await main();
