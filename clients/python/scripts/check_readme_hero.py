"""Assert the package README's `python` hero block is byte-identical to the marked region of examples/hero.py,
so the shipped snippet always type-checks against the real client. Run via `python scripts/check_readme_hero.py`."""

from __future__ import annotations

import re
import sys
from pathlib import Path

_ROOT = Path(__file__).parent.parent
_README = _ROOT / "README.md"
_HERO = _ROOT / "examples" / "hero.py"
_START = "# >>> readme-hero"
_END = "# <<< readme-hero"


def _region(text: str) -> str:
    start = text.index(_START)
    end = text.index(_END, start + len(_START))
    return text[start + len(_START) : end].strip()


def _python_blocks(md: str) -> list[str]:
    return [m.strip() for m in re.findall(r"```python\r?\n(.*?)```", md, re.DOTALL)]


def main() -> int:
    want = _region(_HERO.read_text(encoding="utf-8"))
    if want in _python_blocks(_README.read_text(encoding="utf-8")):
        print("ok: the README hero snippet matches examples/hero.py")
        return 0
    print(
        "The README hero snippet does not match examples/hero.py. Update the ```python block to:\n",
        file=sys.stderr,
    )
    print(want, file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
