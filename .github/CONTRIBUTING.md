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

## Releasing

Two independent release trains: the **SDKs** (npm + PyPI, on `sdk-v*` tags) and the **images** (server +
inspector, to GHCR, on `v*` tags).

### SDKs

The TypeScript (`@loregpt/sdk` → npm) and Python (`loregpt` → PyPI) SDKs release together, in lockstep, from a
`sdk-v*` tag (independent of the server image, which releases on `v*`). Publishing is **OIDC trusted
publishing** — no registry token is stored in the repo.

**One-time maintainer setup** (before the first release):

- **npm** — create the `@loregpt` org, then add a Trusted Publisher on `@loregpt/sdk`:
  - Repository: `lore-gpt/lore` · Workflow: `release-sdk.yml` · Environment: `release`
- **PyPI** — add a Trusted Publisher (a *pending* publisher works before the project's first release) on
  project `loregpt`:
  - Owner/Repo: `lore-gpt/lore` · Workflow: `release-sdk.yml` · Environment: `release`
- **GitHub** — create a `release` environment with **yourself as a required reviewer** (Settings →
  Environments), so an irreversible publish waits for an explicit approval.

**Cutting a release:**

1. Bump the version in **both** `clients/typescript/package.json` and `clients/python/pyproject.toml` to the
   same `X.Y.Z`; commit and merge to `main`.
2. Tag and push: `git tag sdk-vX.Y.Z && git push origin sdk-vX.Y.Z`.
3. The `build` job re-runs every SDK check, verifies the tag matches both versions, and writes a plan (package
   names, version, spec commit, file lists) to the run summary. The `publish` job then waits in the `release`
   environment — review the plan and approve. A post-publish job installs both packages from the registries and
   constructs the client as a smoke check.

**If something goes wrong:**

- A check fails **before** publish → delete the tag, fix, and re-tag the *same* version (nothing was published,
  so the version number is not burned).
- A **partial** publish (one registry succeeded, the other failed) → that version is now taken on the succeeded
  registry, so bump the patch (`X.Y.Z+1`), re-tag, and release again; both packages move to the new version and
  stay in lockstep.

### Images

The server image (`ghcr.io/lore-gpt/lore`) and the Inspector image (`ghcr.io/lore-gpt/lore-inspector`) publish
to GHCR on a `v*` tag, both multi-arch (linux/amd64 + linux/arm64), in lockstep — one tag builds both, so
`lore init` can pin both to a single version. Publishing uses the workflow's `GITHUB_TOKEN`, so no secret is
stored. Tag and push (`git tag vX.Y.Z && git push origin vX.Y.Z`); each job builds, pushes, and smoke-tests its
image.

**One-time, after the first publish of any NEW package:** a newly created GHCR package is **private** by
default, so anonymous `docker pull` — and `lore init` for end users — fails until it is made public. Once, in
the org's **Settings → Packages**, enable public package creation; then on the package page set its visibility
to **Public**. Do this per package (currently `lore` and `lore-inspector`); published versions then stay public.

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
