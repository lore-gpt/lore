# Examples

Runnable examples that wire Lore into agent frameworks. Each is a small, self-contained project with its
own pinned dependencies and its own CI check, and each tells one coordination story rather than touring the
API.

| Example | What it shows |
| --- | --- |
| [`langgraph/`](./langgraph/) | Two LangGraph agents handing off through Lore's shared memory, with a read-your-writes guarantee on the handoff. |

Each example installs the SDK from its published package (not a path dependency), so it runs exactly as a
user's would — and an SDK regression surfaces in the example's CI.
