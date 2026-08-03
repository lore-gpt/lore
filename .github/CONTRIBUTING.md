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
- **Local stack** — `task compose:up` starts the build-from-source stack (`infra/docker-compose.yml`). It is
  a Compose project named **`lore-dev`**, deliberately different from the `lore` stack that `lore init`
  scaffolds from the published image: Compose keys containers and volumes off the project name, and that
  name is global to the machine, so one shared name means `task compose:down` wipes the database of any
  quickstart you happen to have running elsewhere.
  <br>**One-time migration:** if you have a dev stack from before this change it is still under the old
  `lore` name and is now orphaned. It was throwaway dev data — remove it with
  `docker compose -p lore down -v`, then `task compose:up` builds a fresh `lore-dev` stack.

## Releasing

Two independent release trains: the **client packages** (npm + PyPI, on `sdk-v*` tags) and the **images**
(server + inspector, to GHCR, on `v*` tags).

### Client packages

The TypeScript SDK (`@loregpt/sdk` → npm), the Python SDK (`loregpt` → PyPI), and the MCP server
(`@loregpt/mcp` → npm) release together, in lockstep, from a `sdk-v*` tag (independent of the server image,
which releases on `v*`). They are generated from one spec commit, so one version identifies the wire contract
across all three; a release with changes in only one package still moves the others. Publishing is **OIDC
trusted publishing** — no registry token is stored in the repo.

**One-time maintainer setup** (before the first release):

- **npm** — create the `@loregpt` org, then add a Trusted Publisher on **each** package (`@loregpt/sdk` and
  `@loregpt/mcp`; trusted publishers are configured per package, not per org):
  - Repository: `lore-gpt/lore` · Workflow: `release-sdk.yml` · Environment: `release`
  - ⚠️ **npm OIDC cannot perform a package's first publish.** The trusted-publisher settings only exist on a
    package that npm already knows about, so a brand-new package needs one bootstrap publish from a temporary
    granular token (publish a placeholder below the first real version, e.g. `0.0.0`), *then* add the trusted
    publisher, *then* revoke the token. Every release after that is tokenless.
- **PyPI** — add a Trusted Publisher (a *pending* publisher works before the project's first release) on
  project `loregpt`:
  - Owner/Repo: `lore-gpt/lore` · Workflow: `release-sdk.yml` · Environment: `release`
- **GitHub** — create a `release` environment with **yourself as a required reviewer** (Settings →
  Environments), so an irreversible publish waits for an explicit approval.

**Cutting a release:**

1. Bump the version to the same `X.Y.Z` in **six** places — each package declares it twice, once in its
   manifest and once as an exported constant, and a per-package test pins the pair together:

   | package | manifest | exported constant |
   |---|---|---|
   | `@loregpt/sdk` | `clients/typescript/package.json` | `clients/typescript/src/index.ts` (`version`) |
   | `loregpt` | `clients/python/pyproject.toml` | `clients/python/loregpt/__init__.py` (`__version__`) |
   | `@loregpt/mcp` | `clients/mcp/package.json` | `clients/mcp/src/version.ts` (`VERSION`) |

   Commit and merge to `main`. The release gate re-checks all six against the tag, so a missed constant fails
   the run in seconds instead of deep inside a package's test output.
2. Tag and push: `git tag sdk-vX.Y.Z && git push origin sdk-vX.Y.Z`.
3. The `build` job re-runs every client check, verifies the tag matches all six declarations, and writes a plan
   (package names, version, spec commit, file lists) to the run summary. The `publish` job then waits in the
   `release` environment — review the plan and approve. A post-publish job installs each package from its
   registry: it constructs both SDK clients, and spawns `npx -y @loregpt/mcp` to complete an MCP `initialize`
   handshake — the published server must speak the protocol, not merely install.

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
stored; each job builds, pushes, and smoke-tests its image.

**Cutting a release — order matters:**

1. **Version-bump commit first.** Update the version pins in `README.md` — the `ghcr.io/lore-gpt/lore:vX.Y.Z`
   `init` lines and the `/healthz` sample output — to the release version; commit and merge to `main`.
2. **Tag that commit, then push.** With `main` at the version-bump commit, `git tag vX.Y.Z && git push origin
   vX.Y.Z`. The tag must contain the bump so the published image, the git tag, and the README pins all agree —
   `init` pins the generated compose to the tag, and the release smoke test asserts the image reports the tag as
   its `core.Version`, so any drift fails loud instead of shipping.

**One-time, after the first publish of any NEW package:** a newly created GHCR package is **private** by
default, so anonymous `docker pull` — and `lore init` for end users — fails until it is made public. Once, in
the org's **Settings → Packages**, enable public package creation; then on the package page set its visibility
to **Public**. Do this per package (currently `lore` and `lore-inspector`); published versions then stay public.

**Release checklist:**

- [ ] Docs site bumped in the *same* release — `loreVersion` / `loreImage` set to the new version **and** the
      API spec re-synced to the tag — so the README pins and docs.loregpt.ai never drift.
- [ ] Any newly created GHCR package is public — a new package publishes **private** by default, so flip it to
      public (above) or `docker pull` / `lore init` fails for end users.

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
