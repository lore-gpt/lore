"""Real-run guards. Pure predicates a real measurement run must clear before it spends API credits — kept in
the library (mypy-checked and unit-tested) rather than in the CLI, so the rules are provable. Each returns a
human-readable reason string when it blocks, or None when the run is clear to proceed. The keyless dry-run and
fixture paths are exempt by construction (the callers only consult these on the real-run path)."""

from __future__ import annotations

from collections.abc import Sequence

from .loader import PLACEHOLDER_REVISION

# A composed embedder whose model@dim identity starts with this prefix is the offline, deterministic fixture:
# fine for keyless dev, wrong for a measurement run whose whole point is a real vector space.
FIXTURE_EMBEDDER_PREFIX = "fixture-embed-"

# The server reports its extractor as provider/model; this is the whole identity the offline one goes by (it
# has no model half), so an exact match is the right test rather than a prefix.
FIXTURE_EXTRACTOR = "fixture"


def dataset_pin_blocker(split: str, dataset_revision: str) -> str | None:
    """Block a real run that would fetch a real split at the unpinned placeholder revision — a score is not
    reproducible then, and a baseline reference measured under a specific revision would not apply. The fixture
    split is exempt: it is committed to the repo, not fetched."""
    if split != "fixture" and dataset_revision == PLACEHOLDER_REVISION:
        return (
            f"the dataset revision is the unpinned placeholder {PLACEHOLDER_REVISION!r} — pin it to a commit "
            "SHA before a real run (see the eval runbook); changing it later invalidates the locked baseline"
        )
    return None


def lore_isolation_blocker(provision_command: str) -> str | None:
    """A real Lore run must give every question its own project. Lore scopes distilled recall to the project
    and only the raw tail to the run, so questions sharing a project recall each other's histories: measured
    on this harness, 25-65% of a pack's cited sources came from a different question, and a fixed-size pack
    means those displace the evidence the question needs rather than merely adding to it. The system Lore is
    compared against isolates per question, so a shared-project run does not measure the same thing.

    Blocks when no provisioning command is configured — the run would otherwise silently produce a
    contaminated number, which is worse than not producing one."""
    if not provision_command.strip():
        return (
            "every question would share one Lore project, so each question's recall would include the "
            "other questions' histories — set --lore-provision-cmd to provision an isolated project per "
            "ingestion (see the eval runbook for the command for your deployment)"
        )
    return None


def lore_embedder_blocker(embedder: str) -> str | None:
    """A real Lore run must know, and not fake, its embedding model. Fail closed when the identity could not be
    read from the server's /healthz (empty or the sentinel 'unknown'), and refuse the fixture embedder — a
    measurement run against the offline fixture would report a vector space that no real deployment uses."""
    if not embedder or embedder == "unknown":
        return "the embedding model could not be read from the server's /healthz — a real run must record it"
    if embedder.startswith(FIXTURE_EMBEDDER_PREFIX):
        return (
            f"the server is using the fixture embedder ({embedder}) — set LORE_EMBEDDING_PROVIDER to a real "
            "model; a measurement run against the fixture is not representative"
        )
    return None


def lore_extraction_blocker(extraction: str) -> str | None:
    """A real Lore run must know, and not fake, the model that wrote the memories it is about to score.

    This closes the gap its sibling above left open. The embedder was read from the server and guarded; the
    extractor was read from the HARNESS process's environment, which knows nothing about the worker container
    where extraction actually runs — so a real measurement reported its extractor as 'unknown' while a real
    model did the work, and nothing objected. A score whose write path is unidentified cannot be reproduced
    or compared, which is most of what a measurement is for.

    Blocks on an identity the server did not give ('' or 'unknown', including a provider name the server
    would refuse), and on the offline fixture — which distils canned output, so a run against it measures the
    fixture rather than extraction."""
    if not extraction or extraction == "unknown":
        return (
            "the extraction model could not be read from the server's /healthz — a real run must record "
            "which model wrote the memories it scores; check that the server carries LORE_EXTRACTION_* and "
            "is recent enough to report the field"
        )
    if extraction == FIXTURE_EXTRACTOR:
        return (
            f"the deployment is using the fixture extractor ({extraction}) — set LORE_EXTRACTION_PROVIDER "
            "with its API key on BOTH the server and the worker; the fixture returns canned output, so a "
            "run against it measures the fixture and not extraction"
        )
    return None


def mem0_retrieval_blocker(degraded_legs: Sequence[str]) -> str | None:
    """A competitor must be measured with the retrieval it ships, not with whatever a bare install left
    running.

    Mem0's search is a hybrid: semantic, BM25 keyword, and an entity boost. The two non-semantic legs are
    optional dependencies that degrade to a no-op with a log line and no error, so a bare install scores it
    on one leg of three while Lore runs its full hybrid. Nothing about the resulting number looks wrong — it
    is simply lower — and a reference locked from it would make the gate trivially passable. The sanity floor
    cannot see this: it catches a low number, but a plausibly-low one from a quietly halved competitor is
    exactly the shape it lets through.

    Blocks when any leg is reported degraded. `degraded_legs` carries one human-readable reason per dead
    leg; an empty sequence means the competitor is whole."""
    if not degraded_legs:
        return None
    reasons = "; ".join(degraded_legs)
    return (
        f"Mem0 is not running its full retrieval stack ({reasons}) — measuring it this way understates the "
        "competitor and flatters Lore, so the comparison would not be a parity comparison. Install the "
        "competitor extras (`uv sync --extra competitors`) and re-run"
    )
