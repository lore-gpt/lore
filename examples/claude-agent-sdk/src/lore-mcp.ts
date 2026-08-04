/**
 * The Lore MCP server this example gives the agent, and the tools it is allowed to call.
 *
 * Everything about that wiring lives here rather than inline in the agent, because the test needs the
 * exact same values: it starts this package and asks it what tools it has. If the agent's allow-list
 * and the test's expectation were written out twice, the test would be checking a copy rather than the
 * thing the agent actually uses.
 */

/**
 * Pinned deliberately. The package is fetched from the registry — the same artifact a user installs —
 * so pinning is what turns a breaking change in the published server into a red CI run here instead of
 * a silent behaviour change at someone's desk. A tool being renamed or dropped upstream is exactly the
 * kind of break this catches.
 */
export const LORE_MCP_PACKAGE = "@loregpt/mcp@0.1.2";

/** The MCP server name. Tool ids the agent sees are `mcp__<serverName>__<tool>`. */
export const SERVER_NAME = "lore";

/**
 * The tools the agent may call, unprefixed. Naming them individually rather than using a wildcard is
 * the point of an allow-list: a future version of the server could add a tool this example has never
 * reasoned about, and it should not become callable just because it arrived.
 */
export const LORE_TOOLS = ["create_run", "memory_write", "memory_pack"] as const;

/** The same tools as the agent sees them, `mcp__lore__…`. */
export const ALLOWED_TOOLS = LORE_TOOLS.map((tool) => `mcp__${SERVER_NAME}__${tool}`);

/**
 * How to launch the server as a stdio subprocess.
 *
 * The key is passed through `env` rather than inherited: the MCP server is a separate process, and
 * being explicit about the one secret it needs is both clearer and narrower than handing it the
 * agent's whole environment. `LORE_BASE_URL` is optional and defaults to localhost inside the server.
 */
export function loreServerConfig(env: NodeJS.ProcessEnv = process.env) {
  const apiKey = env["LORE_API_KEY"];
  if (!apiKey) {
    throw new Error(
      "LORE_API_KEY is not set. Start Lore (see the repo quickstart) and export the key from ./.lore/credentials.",
    );
  }
  const serverEnv: Record<string, string> = { LORE_API_KEY: apiKey };
  const baseUrl = env["LORE_BASE_URL"];
  if (baseUrl) {
    serverEnv["LORE_BASE_URL"] = baseUrl;
  }
  return {
    command: "npx",
    args: ["-y", LORE_MCP_PACKAGE],
    env: serverEnv,
  };
}

/**
 * The options handed to the agent: which MCP servers it has, and which of their tools it may call.
 *
 * This lives here rather than next to the agent loop so that inspecting the wiring never starts an
 * agent. The entry point runs on import — that is what an entry point is for — so anything a test
 * needs to look at has to live somewhere else.
 */
export function agentOptions(env: NodeJS.ProcessEnv = process.env) {
  return {
    mcpServers: { [SERVER_NAME]: loreServerConfig(env) },
    // Without an allow-list the agent can see the tools but not call them, so this is not optional
    // decoration — it is what makes the server usable.
    allowedTools: ALLOWED_TOOLS,
  };
}
