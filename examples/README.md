# Examples

Runnable examples that wire Lore into agent frameworks. Each is a small, self-contained project with its
own pinned dependencies and its own CI check, and each tells one coordination story rather than touring the
API.

| Example | Language | What it shows |
| --- | --- | --- |
| [`langgraph/`](./langgraph/) | Python | Two LangGraph agents handing off through Lore's shared memory, with a read-your-writes guarantee on the handoff. |
| [`crewai/`](./crewai/) | Python | A CrewAI crew whose second run opens with what its first run learned — recall that outlives a single `kickoff()`. |
| [`claude-agent-sdk/`](./claude-agent-sdk/) | TypeScript | An agent given Lore as MCP tools, deciding for itself when to record and when to look up. |

They are meant to be read as a set: each one covers a capability the others do not — read-your-writes
within a run, recall across runs, and tool-driven access through MCP — and between them they cover both
ecosystems Lore ships a client for. A new example should add a surface that is not here yet.

Each example installs the SDK from its published package (not a path dependency), so it runs exactly as a
user's would — and an SDK regression surfaces in the example's CI.
