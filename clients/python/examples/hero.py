"""A runnable, type-checked mirror of the README hero. The block between the markers is kept byte-identical to
the fenced ``python`` block in the package README (scripts/check_readme_hero.py enforces it), so the snippet we
ship always type-checks against the real client. Type-checked, not executed."""

from __future__ import annotations

import os

from loregpt import LoreClient

api_key = os.environ["LORE_API_KEY"]

# >>> readme-hero
lore = LoreClient(api_key=api_key)

run = lore.create_run()
result = lore.write(
    run_id=run.run_id,
    agent_id="researcher",
    content="Auth flow moved to v2 - PR #42 merged",
)

pack = lore.pack(
    run_id=run.run_id,
    query="current state of auth work",
    scopes={"team": "platform"},
    min_seq=result.seq,
    token_budget=2000,
)

covered_seq = pack.covered_seq  # >= result.seq -> read-your-writes, guaranteed
saved_tokens = pack.saved_tokens  # estimated tokens saved by packing
# <<< readme-hero
