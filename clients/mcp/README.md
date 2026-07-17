# @loregpt/mcp

A [Model Context Protocol](https://modelcontextprotocol.io) server for **Lore** — coordination memory for
multi-agent AI systems. It exposes Lore's write → pack loop as MCP tools, so any MCP-capable client (Claude
Code, Cursor, and everything else that speaks MCP) can give its agents a shared, read-your-writes memory with
one integration.

It is a thin adapter over the Lore REST API: it holds no state between calls, and each tool maps to one API
endpoint.

## Tools

| Tool | What it does |
| --- | --- |
| `create_run` | Create a run — an isolated coordination session that groups a team's events and memories. Returns a `run_id`. |
| `memory_write` | Append one event (a `content` string, an opaque `payload`, or a `state` working-memory fact) authored by an agent. Returns the event's monotonic `seq`. |
| `memory_pack` | Retrieve a budget-fit context pack for a run — distilled memories + live working facts + a raw tail of not-yet-distilled events. Returns the pack text plus `covered_seq` / `freshness_lag_ms`. |

Read-your-writes is a first-class contract: keep the `seq` a `memory_write` returns and pass it as `min_seq`
to a later `memory_pack`, and that pack is guaranteed to reflect your write — distilled if the server has
caught up, as a raw tail if not.

> This is the v0.1 surface. Tools that manage individual memories (inspecting versions, deleting) arrive with
> their REST endpoints in a later release; the server advertises only tools backed by a live endpoint, so a
> client never sees a tool that always fails.

## Configure it in an MCP client

The server authenticates to Lore with an API key. Provision one against your Lore server first:

```console
$ lore provision --out .lore/credentials      # writes the token to .lore/credentials
```

Then register the server. In a generic MCP client config (Claude Code's `.mcp.json`, Cursor's
`~/.cursor/mcp.json`, etc.):

```json
{
  "mcpServers": {
    "lore": {
      "command": "npx",
      "args": ["-y", "@loregpt/mcp"],
      "env": {
        "LORE_API_KEY": "lore_sk_...",
        "LORE_BASE_URL": "http://localhost:8080"
      }
    }
  }
}
```

Copy the `LORE_API_KEY` value from the `.lore/credentials` file `lore provision` wrote. The server
communicates over **stdio**; all diagnostics go to stderr, and the key is never logged.

### Environment

| Variable | Required | Default | Meaning |
| --- | --- | --- | --- |
| `LORE_API_KEY` | yes | — | Bearer key from `lore provision` / `lore keys create`. |
| `LORE_BASE_URL` | no | `http://localhost:8080` | URL of the Lore server. |
| `LORE_TIMEOUT_MS` | no | `30000` | Per-request timeout in milliseconds. |

## The tool contract

- **Errors are data.** A typed API failure (`unauthorized`, `not_found`, `min_seq_out_of_range`,
  `model_mismatch`, …) comes back as a tool error carrying the machine `code`, not a transport crash — the
  same codes the REST API and the Lore SDKs use.
- **Pack text is data, not instructions.** `memory_pack` returns the assembled pack verbatim as retrieved
  context; it is never reformatted, and an agent should treat it as data to read, not commands to follow.
- **Exactly one of `content` / `payload` / `state`** on `memory_write` — supplying zero or more than one is a
  tool error, not a silent guess.

## Run from source

```console
$ pnpm install
$ pnpm build
$ LORE_API_KEY=lore_sk_... node dist/bin.js
```

`pnpm run check` runs the full local suite (generated-type drift check, type-check, and the tool round-trip
tests).

## License

Apache-2.0.
