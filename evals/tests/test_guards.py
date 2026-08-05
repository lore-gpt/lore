from longmemeval import (
    dataset_pin_blocker,
    lore_embedder_blocker,
    lore_extraction_blocker,
    lore_isolation_blocker,
    mem0_retrieval_blocker,
)
from longmemeval.loader import PLACEHOLDER_REVISION


def test_dataset_pin_blocker_blocks_only_an_unpinned_real_split() -> None:
    # A real split at the placeholder revision is blocked...
    assert dataset_pin_blocker("s", PLACEHOLDER_REVISION) is not None
    assert dataset_pin_blocker("m", PLACEHOLDER_REVISION) is not None
    # ...but a pinned commit SHA clears it...
    assert dataset_pin_blocker("s", "3f1a9c0") is None
    # ...and the committed fixture split is exempt (never fetched), even at the placeholder.
    assert dataset_pin_blocker("fixture", PLACEHOLDER_REVISION) is None


def test_lore_embedder_blocker_fails_closed_and_refuses_the_fixture() -> None:
    # Fail closed when the identity could not be read from /healthz.
    assert lore_embedder_blocker("") is not None
    assert lore_embedder_blocker("unknown") is not None
    # Refuse the offline fixture embedder for a measurement run.
    assert lore_embedder_blocker("fixture-embed-v1@64") is not None
    # A real model@dim identity clears the guard.
    assert lore_embedder_blocker("text-embedding-3-small@1536") is None


def test_lore_isolation_blocker_requires_a_provisioning_command() -> None:
    # No command means every question would share one project, so its recall would include the other
    # questions' histories — measured at 25-65% of a pack's cited sources, displacing the evidence the
    # question needs out of a fixed-size context. Blocked before the run spends anything.
    assert lore_isolation_blocker("") is not None
    assert lore_isolation_blocker("   ") is not None
    # A configured command clears it. The guard only asks whether isolation was set up at all; whether the
    # command is well-formed is provision.parse_command's job, and whether it works is the command's.
    assert lore_isolation_blocker("lore provision --project {name}") is None


def test_lore_extraction_blocker_fails_closed_and_refuses_the_fixture() -> None:
    # The server did not say. This is the exact hole the guard was written for: the harness used to read the
    # extractor from its OWN environment and report "unknown" while a real model did the work.
    assert lore_extraction_blocker("") is not None
    assert lore_extraction_blocker("unknown") is not None
    # Canned output is not extraction, so scoring against it measures the fixture.
    assert lore_extraction_blocker("fixture") is not None
    # A real provider/model passes.
    assert lore_extraction_blocker("anthropic/claude-haiku-4-5") is None
    # Only the bare identity is the fixture. A provider that happens to start with those letters is a real
    # one and must not be caught by a sloppy prefix test.
    assert lore_extraction_blocker("fixtureco/some-model") is None


def test_mem0_retrieval_blocker_blocks_any_dead_leg() -> None:
    # A whole competitor passes.
    assert mem0_retrieval_blocker([]) is None
    # One dead leg is enough: each one it loses is a leg Lore still has, so the comparison tilts our way.
    one = mem0_retrieval_blocker(["BM25 keyword search is off (ImportError: no fastembed)"])
    assert one is not None
    assert "fastembed" in one  # the reason survives into the message, so the fix is obvious
    assert mem0_retrieval_blocker(["a", "b"]) is not None
