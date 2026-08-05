from longmemeval import dataset_pin_blocker, lore_embedder_blocker, lore_isolation_blocker
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
