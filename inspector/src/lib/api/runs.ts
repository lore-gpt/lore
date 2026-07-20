import { authedFetch, parse } from "./server-fetch";
import type { RunTraceEntry, RunTraceResponse } from "./types";

export interface RunTraceParams {
  limit?: string;
  cursor?: string;
}

const RUN_TRACE_KEYS: (keyof RunTraceParams)[] = ["limit", "cursor"];

// A page of a run's pack trace (newest-first, keyset-paginated). An unknown run
// (or one in another project) is a 404 — the same as a memory, no existence oracle.
export async function fetchRunTrace(runId: string, params: RunTraceParams): Promise<RunTraceResponse> {
  const qs = new URLSearchParams();
  for (const key of RUN_TRACE_KEYS) {
    const value = params[key]?.trim();
    if (value) {
      qs.set(key, value);
    }
  }
  const query = qs.toString();
  return parse<RunTraceResponse>(
    await authedFetch(`/v1/runs/${encodeURIComponent(runId)}/trace${query ? `?${query}` : ""}`),
  );
}

// The pack trace is a list, not addressable per entry, so the diff locates its two
// entries by id by paging the trace up to a bound (a diagnostic run has few packs).
// Returns whichever wanted ids were found; a caller renders "not found" for the
// rest (retention or beyond the bound). Throws (404) if the run itself is unknown.
export async function findTraceEntries(runId: string, ids: string[]): Promise<Map<string, RunTraceEntry>> {
  const wanted = new Set(ids);
  const found = new Map<string, RunTraceEntry>();
  const MAX_PAGES = 10; // up to 10 * 200 = 2000 packs
  let cursor: string | undefined;

  for (let page = 0; page < MAX_PAGES && wanted.size > 0; page++) {
    const res = await fetchRunTrace(runId, { limit: "200", cursor });
    for (const entry of res.packs) {
      if (wanted.has(entry.id)) {
        found.set(entry.id, entry);
        wanted.delete(entry.id);
      }
    }
    if (!res.has_more || !res.next_cursor) {
      break;
    }
    cursor = res.next_cursor;
  }
  return found;
}
