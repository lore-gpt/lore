// Programmatic surface of the Lore MCP server. The executable entry point is bin.ts (`lore-mcp`); this barrel
// lets a host embed the server, or a test drive the tools, without spawning a process.
export { buildServer } from "./server.ts";
export { createHttpHandler, serveHttp } from "./http.ts";
export { LoreRestClient } from "./client.ts";
export type { LoreRestClientOptions } from "./client.ts";
export { clientFromConfig, configFromEnv, httpConfigFromEnv } from "./config.ts";
export type { HttpConfig, StdioConfig } from "./config.ts";
export { VERSION } from "./version.ts";
export {
  InvalidBodyError,
  InvalidRunIdError,
  LoreApiError,
  LoreConnectionError,
  LoreError,
  LoreParseError,
  MinSeqOutOfRangeError,
  ModelMismatchError,
  NotFoundError,
  UnauthorizedError,
  UnknownLoreError,
  fromResponse,
} from "./errors.ts";
export type { LoreApiErrorUnion, LoreErrorCode } from "./errors.ts";
