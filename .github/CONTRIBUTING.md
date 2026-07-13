# Contributing to Lore

Lore is being **built in the open**, and early contributors shape it more than late ones. Whether you
open an issue, comment on an RFC, or send a patch — thank you.

> **Status:** pre-release. The most valuable contributions right now are **design feedback on the
> [RFCs](docs/rfcs)** and real-world use cases, not large code PRs against a moving target.

## Ways to contribute

- **RFC feedback** — the core design lives in [`docs/rfcs/`](docs/rfcs). Comment via
  [Discussions](../../discussions) or open a PR against an RFC.
- **Issues** — bugs, papercuts, and feature ideas. Look for
  [`good first issue`](../../issues?q=label%3A%22good+first+issue%22) once code lands.
- **Examples & integrations** — "add shared memory to your {LangGraph, CrewAI, AutoGen, Claude Agent
  SDK} agents" — real, runnable examples are gold.

## Development workflow

- **Conventional Commits** — `feat:`, `fix:`, `docs:`, `refactor:`, `test:`, `chore:` … This drives the
  changelog and release train.
- **Small PRs** — keep diffs focused (< ~400 lines where possible); one logical change per PR.
- **Green CI** — lint + tests must pass. Any architectural change should explain its design
  rationale in the PR description.
- **DCO / CLA** — contributions are accepted under the project's Contributor License Agreement (a CLA bot
  will guide you on your first PR). This keeps future licensing flexibility open and protects the project
  and its contributors.

## The OSS and paid boundary

Lore is **open-core**. The principle: **the engine is open; operations, governance depth, and
coordination analytics are commercial.** Concretely:

| Open source (Apache-2.0, this repo) | Commercial (hosted cloud / enterprise) |
|---|---|
| Write/read pipeline, extraction, consolidation | Usage metering & billing |
| Scope model (run/agent/team/org), MCP server, TS/Py SDKs | Advanced ACL (policy engine), curation workflow |
| pgvector + hybrid retrieval, context pack | Coordination-health & savings analytics |
| Basic inspector, basic conflict policies (LWW/merge) | Managed hosting, SSO/SCIM, audit, BYOK, VPC/on-prem |

**Why some PRs to the boundary may be declined:** features that belong to the commercial layer can't be
merged here — not because they're unwelcome, but because that's what funds full-time work on the open
core. If you send a boundary PR, we'll say thank you, explain why, and suggest an OSS-appropriate framing
if one exists. The boundary is public precisely so there are **no surprises** — the surprise is what
erodes trust, not the boundary itself.

## Code of conduct

By participating you agree to uphold our [Code of Conduct](CODE_OF_CONDUCT.md).

## Security

Please **do not** open public issues for vulnerabilities. See [SECURITY.md](SECURITY.md).
