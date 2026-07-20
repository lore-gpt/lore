import { getActiveKey } from "@/server/session";

import { LORE_API_URL } from "./config";
import { LoreApiError } from "./errors";

// Shared server-side access to the upstream Lore API. Runs only in server
// components / actions: it attaches the active API key (server-configured or the
// browser-session cookie) and never caches, so the Inspector always shows live
// state. The browser talks to the BFF (`/api/*`); this talks to the upstream
// directly (server-to-server, key never reaches the client).

export async function authedFetch(path: string, init?: RequestInit): Promise<Response> {
  const active = await getActiveKey();
  const headers = new Headers(init?.headers);
  if (active) {
    headers.set("authorization", `Bearer ${active.key}`);
  }
  return fetch(`${LORE_API_URL}${path}`, { ...init, headers, cache: "no-store" });
}

// Parse a JSON response, throwing a typed LoreApiError (carrying the upstream
// status + error `code`) on any non-2xx so callers can branch on 401 / 404 /
// invalid_cursor. A network failure rejects with the raw fetch error; a 2xx with
// no decodable JSON is surfaced as an error rather than an empty result.
export async function parse<T>(res: Response): Promise<T> {
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
    throw new LoreApiError(res.status, "The Lore server returned an unexpected non-JSON response.");
  }
  return data as T;
}
