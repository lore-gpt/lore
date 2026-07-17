#!/usr/bin/env node
// Entry point for the Lore MCP server. By default it speaks the MCP protocol over stdio; with `--http` it
// serves the stateless streamable-HTTP transport instead. In stdio mode, stdout is the protocol channel, so
// every diagnostic goes to stderr and the API key is never written to either stream.
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { clientFromConfig, configFromEnv, httpConfigFromEnv } from "./config.ts";
import { serveHttp } from "./http.ts";
import { buildServer } from "./server.ts";
import { VERSION } from "./version.ts";

async function main(): Promise<void> {
  const args = process.argv.slice(2);
  if (args.includes("--http")) {
    await runHttp(args);
  } else {
    await runStdio();
  }
}

async function runStdio(): Promise<void> {
  const config = configFromEnv(process.env);
  const client = clientFromConfig(config);
  const server = buildServer(client);
  const transport = new StdioServerTransport();
  await server.connect(transport);
  process.stderr.write(`lore-mcp ${VERSION} on stdio -> ${config.baseUrl}\n`);
}

async function runHttp(args: string[]): Promise<void> {
  const port = flagValue(args, "--port");
  const config = httpConfigFromEnv(process.env, port !== undefined ? { port } : undefined);
  const server = await serveHttp(config);
  const address = server.address();
  const boundPort = typeof address === "object" && address !== null ? address.port : config.port;
  process.stderr.write(`lore-mcp ${VERSION} on http://${config.host}:${boundPort}/mcp -> ${config.baseUrl}\n`);
}

// Read `--flag value` or `--flag=value` from argv.
function flagValue(args: string[], flag: string): string | undefined {
  const prefix = `${flag}=`;
  for (let i = 0; i < args.length; i++) {
    const arg = args[i];
    if (arg === flag) return args[i + 1];
    if (arg !== undefined && arg.startsWith(prefix)) return arg.slice(prefix.length);
  }
  return undefined;
}

main().catch((err: unknown) => {
  const message = err instanceof Error ? err.message : String(err);
  process.stderr.write(`lore-mcp: fatal: ${message}\n`);
  process.exitCode = 1;
});
