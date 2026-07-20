import { authedFetch, parse } from "./server-fetch";
import type { Memory, MemoryListResponse, MemoryVersionListResponse } from "./types";

// Server-side data access for the memory-inspection endpoints (see server-fetch
// for the shared authed, uncached transport).

// Query parameters for the memory list. Field names mirror the API contract
// exactly (kind · run_id · trust_tier · review_status · q · cursor · limit) so
// the URL, the wire request, and the docs all read the same.
export interface MemoryListParams {
  limit?: string;
  cursor?: string;
  q?: string;
  kind?: string;
  run_id?: string;
  trust_tier?: string;
  review_status?: string;
}

const MEMORY_LIST_KEYS: (keyof MemoryListParams)[] = [
  "limit",
  "cursor",
  "q",
  "kind",
  "run_id",
  "trust_tier",
  "review_status",
];

export async function fetchMemories(params: MemoryListParams): Promise<MemoryListResponse> {
  const qs = new URLSearchParams();
  for (const key of MEMORY_LIST_KEYS) {
    const value = params[key]?.trim();
    if (value) {
      qs.set(key, value);
    }
  }
  const query = qs.toString();
  return parse<MemoryListResponse>(await authedFetch(`/v1/memories${query ? `?${query}` : ""}`));
}

export async function fetchMemory(id: string): Promise<Memory> {
  return parse<Memory>(await authedFetch(`/v1/memories/${encodeURIComponent(id)}`));
}

// Version history, oldest-first per the API (the UI renders it newest-first). A
// soft-deleted memory keeps its history, so this can still 200 when fetchMemory
// 404s — that is exactly how the detail page tells a tombstone apart from an
// unknown id.
export async function fetchMemoryVersions(id: string): Promise<MemoryVersionListResponse> {
  return parse<MemoryVersionListResponse>(await authedFetch(`/v1/memories/${encodeURIComponent(id)}/versions`));
}

// Soft-delete a memory, returning the upstream status. The delete is idempotent
// per live row, so 204 (just deleted) and 404 (already gone / superseded) both
// mean "no longer live"; the caller branches on 401 for the connect flow. Throws
// only on a network failure.
export async function deleteMemory(id: string): Promise<number> {
  const res = await authedFetch(`/v1/memories/${encodeURIComponent(id)}`, { method: "DELETE" });
  return res.status;
}
