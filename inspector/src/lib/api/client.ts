import { LoreApiError } from "./errors";

// Call the Inspector BFF (same-origin `/api/*`). Returns parsed JSON, or throws
// a typed LoreApiError carrying the upstream status + code. Never caches.
export async function apiFetch<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`/api/${path.replace(/^\/+/, "")}`, { ...init, cache: "no-store" });

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

  return data as T;
}
