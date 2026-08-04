# Claude Agent SDK + Lore: an agent that keeps its own notes

An agent built on the [Claude Agent SDK](https://code.claude.com/docs/en/agent-sdk/overview) is given
Lore as a set of MCP tools and decides for itself when to record something and when to look it up.

## The idea

The other examples here call Lore's SDK from code the author wrote: the program decides when to write
and when to pack. This one inverts that. The agent gets `create_run`, `memory_write` and `memory_pack`
as tools, and nothing in this example calls Lore directly — the model chooses when coordination memory
is worth reaching for.

```ts
const options = {
  mcpServers: { lore: { command: "npx", args: ["-y", "@loregpt/mcp@0.1.2"], env: { LORE_API_KEY } } },
  allowedTools: ["mcp__lore__create_run", "mcp__lore__memory_write", "mcp__lore__memory_pack"],
};
```

Two details that are easy to get wrong:

- **The allow-list is not decoration.** Without it the agent can see the tools and cannot call them,
  which shows up as the model apparently refusing rather than as a configuration mistake.
- **The tools are named individually, not with a wildcard.** A future version of the server could add a
  tool this example has never reasoned about; it should not become callable just because it arrived.

The agent talks to Lore through MCP tools here. For calling Lore directly from TypeScript, see the
[TypeScript SDK](../../clients/typescript/).

## Run it

You need a running Lore and its API key, plus an Anthropic key for the agent itself. Start Lore with the
[repo quickstart](../../README.md#quickstart-self-host), then:

```bash
export LORE_API_KEY=...        # from ./.lore/credentials
export ANTHROPIC_API_KEY=...
pnpm install
pnpm start
```

## What CI proves, and what it does not

Worth being exact about, because the two layers are not the same thing:

- **CI proves the wiring.** It checks the options the agent is given — the server is registered, the
  pinned package is what gets launched, every allowed tool carries the `mcp__lore__` prefix, and only
  the API key travels into the subprocess. Then it starts the **published** MCP server over stdio and
  asks it what tools it offers, asserting that every tool this example allows is still there. No keys,
  no running Lore.
- **The live agent loop is a key-gated smoke.** The Agent SDK has no dry-run — it needs a real model
  key to run at all — so "does the agent actually use these tools well" is not something the routine
  check can answer.

The tool-contract check is the one that earns its place. A renamed or dropped tool is invisible to the
type-checker (tool names are strings crossing a process boundary) and invisible to the agent until a
model tries to call something that is not there. That break has happened before on this project's
published MCP package, which is why the version pin and that check are a pair: the pin makes the break
happen at a moment someone is watching.

## What's pinned

`@loregpt/mcp` is pinned to an exact version and fetched from the registry — the same artifact you would
install — so the check runs against what users actually get rather than against the copy in this repo.
When the MCP package releases a new version, updating the pin here is the moment its tool contract gets
re-verified.
