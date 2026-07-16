"""The hero flow with the async client (type-checked). AsyncLoreClient has the same facade as LoreClient, only
with ``await`` — use it inside an async agent framework."""

from __future__ import annotations

import asyncio
import os

from loregpt import AsyncLoreClient


async def main() -> None:
    async with AsyncLoreClient(api_key=os.environ["LORE_API_KEY"]) as lore:
        run = await lore.create_run()
        result = await lore.write(run_id=run.run_id, agent_id="researcher", content="hello memory")
        pack = await lore.pack(run_id=run.run_id, query="state of work", min_seq=result.seq)
        print(pack.covered_seq, pack.saved_tokens)


if __name__ == "__main__":
    asyncio.run(main())
