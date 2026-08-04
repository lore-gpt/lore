# Changelog

Notable changes to the Lore server, its CLI, and the compose stack it scaffolds. The client packages
(`@loregpt/sdk`, `@loregpt/mcp`, `loregpt`) release on their own `sdk-v*` tags and are noted here only
when a server change affects them.

This file starts at the entries below. Releases before it — `v0.0.1` through `v0.0.3` — are recorded in
the [git tags](https://github.com/lore-gpt/lore/tags); rather than reconstruct their notes after the fact,
they are left to the commit history that actually holds them.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project is
pre-1.0: while the major version is 0, a minor bump may carry a breaking change. Anything that requires
action on upgrade says so in its entry.

## [Unreleased]

### Added

- `lore keys list --project <id>` lists a project's API keys — id, name, non-secret prefix, creation
  time and revocation status. Never the keys themselves, which are unrecoverable by design. Until now
  the id printed once at creation was the only handle on a key, so losing it made `lore keys revoke`
  unusable.
- Working memory is now durable. A `kind: "state"` fact is written to the database in the same
  transaction as the event carrying it, so it survives a restart and is served from there rather than
  from an optional cache.
- Examples for [CrewAI](examples/crewai/) and the [Claude Agent SDK](examples/claude-agent-sdk/),
  joining the existing LangGraph one. Each covers a different capability: read-your-writes within a run,
  recall across runs, and tool-driven access over MCP.

### Changed

- **`/metrics` now listens on its own port, for both `serve` and `worker`.** It was previously served
  on the server's API port, which made it impossible to publish the API without also publishing an
  unauthenticated metrics endpoint. Both roles now bind `LORE_METRICS_ADDR` (default `:9090`), and the
  compose stack publishes neither.
  <br>**On upgrade:** point any collector scraping `<api-host>:8080/metrics` at port 9090 instead. A
  scraper left on the old address now gets a 404 that shows up in the server's own request metrics.
- **The build-from-source compose stack is now a separate Compose project, `lore-dev`.** Both compose
  files previously declared `name: lore`, which made them one stack: bringing up one would recreate the
  other's containers, and `docker compose down -v` in either would drop the other's database.
  <br>**On upgrade (contributors only):** an existing dev stack is still under the old `lore` name and
  is now orphaned. It was throwaway data — `docker compose -p lore down -v` clears it.
- **`lore keys revoke` is idempotent.** Revoking a key that is already revoked now reports no change and
  exits 0, so a script that runs twice is safe. An unknown key id still exits non-zero — that one means
  the id names nothing, which a caller should not ignore.
- The router's own rejections use the JSON error envelope. An unknown path and a wrong method used to
  return bare text from the router; they now return the same shape as every other error, with distinct
  codes (`unknown_route`, `method_not_allowed`) so a client can tell "no such endpoint" from "no such
  resource".
- `lore doctor` names the invocation that works when it fails to reach a dependency. A compose install
  can only satisfy every check from inside the network, which was not obvious from either failure.
- The context pack states that its bracketed numbers are citation labels rather than a ranking. They
  index into `sources` — `[n]` is `sources[n-1]` — while the text is grouped by kind, so `[1]` is the
  first section's top item rather than the highest-scoring one. Each source carries its own `score`.
- `working_source` in a pack response no longer varies: the working section always comes from the
  durable store, so the value is always `live`. The field is kept because it is part of the published
  response contract.

### Removed

- `core.WithMetricsHandler` and `httpapi.Config.MetricsHandler`. The API router no longer serves
  `/metrics`, so the seam that injected that handler had nothing left to do. Only relevant if you embed
  `core` as a library.

### Fixed

- Background jobs that fail or panic are now logged. The queue reported a failed attempt below its own
  logger's threshold, so a job could fail every attempt and then be discarded in silence — including a
  model-mismatch fail-safe that was working correctly and saying nothing.
- The Inspector's memory-detail tabs are addressable: `?tab=versions` opens the Versions tab, and the
  URL survives a refresh or a shared link.

### Documentation

- What the credentials file's permissions actually guarantee, stated per platform. The repo asserted
  `0600` in several places without qualification; that holds on POSIX hosts, but Docker Desktop writes
  the file through a bind mount that carries no POSIX modes, so on Windows it inherits the ACL of the
  directory it lands in. The README carries an optional hardening step for that case.
