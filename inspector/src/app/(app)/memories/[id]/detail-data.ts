import { headers } from "next/headers";
import { redirect } from "next/navigation";

import { LoreApiError } from "@/lib/api/errors";
import { fetchMemory, fetchMemoryVersions } from "@/lib/api/memories";
import type { Memory, MemoryVersion } from "@/lib/api/types";
import { sanitizeNextPath } from "@/lib/nav";

// The resolved detail state, shared by the full page and the slide-over modal so
// both render identically.
export type MemoryDetail =
  | { kind: "live"; memory: Memory; versions: MemoryVersion[]; versionsLoaded: boolean }
  | { kind: "tombstone"; versions: MemoryVersion[]; versionsLoaded: boolean }
  | { kind: "notfound" }
  | { kind: "error"; offline: boolean };

function isStatus(reason: unknown, status: number): boolean {
  return reason instanceof LoreApiError && reason.status === status;
}

// Fetch a memory and its version history in parallel and resolve the detail state.
// A 401 on either leg routes to the connect flow (returning to this URL). The
// live-row 404 is the authoritative tombstone signal; only when BOTH legs cleanly
// 404 is the id genuinely unknown.
export async function loadMemoryDetail(id: string): Promise<MemoryDetail> {
  const [memResult, verResult] = await Promise.allSettled([fetchMemory(id), fetchMemoryVersions(id)]);

  if (
    (memResult.status === "rejected" && isStatus(memResult.reason, 401)) ||
    (verResult.status === "rejected" && isStatus(verResult.reason, 401))
  ) {
    const here = sanitizeNextPath((await headers()).get("x-pathname")) ?? `/memories/${id}`;
    redirect(`/connect?expired=1&next=${encodeURIComponent(here)}`);
  }

  const versionsLoaded = verResult.status === "fulfilled";
  // Prior versions, reversed to newest-first for display.
  const versions: MemoryVersion[] = verResult.status === "fulfilled" ? [...verResult.value.versions].reverse() : [];

  if (memResult.status === "fulfilled") {
    return { kind: "live", memory: memResult.value, versions, versionsLoaded };
  }

  const memGone = isStatus(memResult.reason, 404);
  const verGone = verResult.status === "rejected" && isStatus(verResult.reason, 404);

  if (memGone && verGone) {
    return { kind: "notfound" };
  }
  if (memGone) {
    return { kind: "tombstone", versions, versionsLoaded };
  }
  return { kind: "error", offline: !(memResult.reason instanceof LoreApiError) };
}
