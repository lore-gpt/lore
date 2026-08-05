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
- `LORE_EXTRACTION_MAX_TOKENS` and `LORE_EXTRACTION_MAX_WINDOW` size one extraction pass: how much room
  its answer has, and how many events it distils. The right pair depends on how much text your events
  carry, which is why they are yours to set — but a mismatch costs extra model calls rather than lost
  memories, because a pass that overruns the ceiling shrinks and retries. The ceiling is a cap, not a
  spend: you are billed for tokens actually generated.
- `lore_extract_window_shrink_total` counts passes that overran the output ceiling, by outcome —
  `retried` (halved and distilled) or `exhausted` (still too dense at the floor, so that run has stopped
  distilling). A steady `retried` rate is the signal to re-size the pair above; any `exhausted` needs
  attention.

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

- **A burst of writes no longer stops a run from ever distilling again.** One extraction pass used to
  read every event past the run's checkpoint, however many that was. A client writing faster than
  extraction runs — a bulk import, a backfill, an agent replaying a transcript — could therefore build a
  window whose extraction output exceeded the model's response ceiling. That failed the pass, every retry
  rebuilt the identical window and failed identically, and the job was discarded with the checkpoint
  frozen: the run stopped distilling permanently, and its `covered_seq` never advanced again. Passes are
  now capped (200 events by default), and a pass whose answer still overruns the ceiling halves its window
  and retries until it fits — committing what fits and leaving the rest for the next pass, so a dense run
  makes progress instead of stopping. The remainder is drained by the pass that follows.
  <br>The cap alone was not enough, and measuring said so: a real conversational workload overran the
  ceiling at exactly the 200-event cap, on all three attempts, and froze the run — the same permanent stall
  one size down. A cap is a guess about how much text an event carries, and only shrinking in response to
  the actual answer removes the guess. The two bounds are now adjustable
  (`LORE_EXTRACTION_MAX_WINDOW`, `LORE_EXTRACTION_MAX_TOKENS`) and the default ceiling has been raised to
  fit the default window. Content dense enough to overrun the ceiling a handful of events at a time still
  stops that run, now loudly and naming the variable to raise, rather than in silence.
- **An extraction pass that needs more than a minute is no longer cancelled for it.** Extraction inherited
  the job queue's one-minute default deadline, which a real pass does not fit inside: a 200-event window of
  conversational content was measured overrunning it, and because the window is rebuilt from the same events,
  the retry overran it identically — three cancellations discarded the job and left the run's checkpoint
  frozen, the same permanent stall as a truncated response, arrived at by a different road. It also read as
  an intermittent provider fault rather than a deadline. Extraction now gets its own deadline, generous
  enough for a pass that shrinks its window and retries several times within one attempt, and still bounded
  so a stuck job cannot hold a worker slot indefinitely.
- **One impossible date from the model no longer discards an entire extraction pass.** A claim's optional
  `event_time` was parsed strictly, so a value like `2023-02-29` — a date that does not exist — failed the
  whole decode and threw away every memory, claim and entity extracted alongside it, deterministically, on
  every retry. An unusable timestamp is now dropped and the rest of the pass is kept. The field is
  advisory and never drove ordering, so nothing downstream changes.
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
