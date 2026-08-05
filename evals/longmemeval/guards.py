"""Real-run guards. Pure predicates a real measurement run must clear before it spends API credits — kept in
the library (mypy-checked and unit-tested) rather than in the CLI, so the rules are provable. Each returns a
human-readable reason string when it blocks, or None when the run is clear to proceed. The keyless dry-run and
fixture paths are exempt by construction (the callers only consult these on the real-run path)."""

from __future__ import annotations

from .loader import PLACEHOLDER_REVISION

# A composed embedder whose model@dim identity starts with this prefix is the offline, deterministic fixture:
# fine for keyless dev, wrong for a measurement run whose whole point is a real vector space.
FIXTURE_EMBEDDER_PREFIX = "fixture-embed-"


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
