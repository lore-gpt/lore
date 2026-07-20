import { getActiveKey } from "@/server/session";

import { LORE_API_URL } from "./config";
import { LoreApiError } from "./errors";
import type { Memory, MemoryListResponse, MemoryVersionListResponse } from "./types";

// Server-side data access for the memory-inspection endpoints. These run only in
// server components / actions: they attach the active API key (server-configured
// or the browser-session cookie) and never cache, so the Inspector always shows
// live server state. The browser talks to the BFF (`/api/*`); this module talks
// to the upstream directly (server-to-server, key never reaches the client).

async function authedFetch(path: string, init?: RequestInit): Promise<Response> {
  const active = await getActiveKey();
  const headers = new Headers(init?.headers);
  if (active) {
    headers.set("authorization", `Bearer ${active.key}`);
  }
  return fetch(`${LORE_API_URL}${path}`, { ...init, headers, cache: "no-store" });
}

// Parse a JSON response, throwing a typed LoreApiError (carrying the upstream
// status + error `code`) on any non-2xx so callers can branch on 401 / 404 /
// invalid_cursor. A network failure rejects with the raw fetch error.
async function parse<T>(res: Response): Promise<T> {
  if (res.status === 204) {
    return undefined as T;
  }
  let data: unknown = null;
  if ((res.headers.get("content-type") ?? "").includes("application/json")) {
    try {
      data = await res.json();
    } catch {
      data = null;
    }
  }
  if (!res.ok) {
    const err = (data ?? {}) as { message?: string; code?: string };
    throw new LoreApiError(res.status, err.message ?? `Request failed (${res.status})`, err.code);
  }
  if (data === null) {
    // A 2xx that carried no decodable JSON body (a proxy/gateway returned HTML, or
    // the content-type was rewritten). Surface it as an error rather than letting
    // an empty result masquerade as a legitimately empty project.
    throw new LoreApiError(res.status, "The Lore server returned an unexpected non-JSON response.");
  }
  return data as T;
}

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
