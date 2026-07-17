#!/usr/bin/env node
// Entry point for the Lore MCP server over stdio. Run directly (`lore-mcp`) or configure it as an MCP server
// command in a client. stdout is the MCP protocol channel — every diagnostic goes to stderr, and the API key
// is never written to either stream.
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { clientFromConfig, configFromEnv } from "./config.ts";
import { buildServer } from "./server.ts";
import { VERSION } from "./version.ts";

async function main(): Promise<void> {
  const config = configFromEnv(process.env);
  const client = clientFromConfig(config);
  const server = buildServer(client);
  const transport = new StdioServerTransport();
  await server.connect(transport);
  process.stderr.write(`lore-mcp ${VERSION} on stdio -> ${config.baseUrl}\n`);
}

main().catch((err: unknown) => {
  const message = err instanceof Error ? err.message : String(err);
  process.stderr.write(`lore-mcp: fatal: ${message}\n`);
  process.exitCode = 1;
});
