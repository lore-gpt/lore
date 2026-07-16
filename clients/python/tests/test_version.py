from __future__ import annotations

import re
from pathlib import Path

import loregpt


def test_version_matches_pyproject() -> None:
    # Read pyproject with a regex (not tomllib, which is stdlib only on 3.11+) so this passes on the 3.10 floor.
    pyproject = (Path(__file__).parent.parent / "pyproject.toml").read_text(encoding="utf-8")
    match = re.search(r'^version = "([^"]+)"', pyproject, re.MULTILINE)
    assert match is not None
    assert loregpt.__version__ == match.group(1)
