import { LORE_API_URL } from "./config";

// Result of validating a key against the upstream server.
export type ProbeResult = "ok" | "invalid" | "unreachable";

// Probe a candidate key against the upstream server. A 401 means the key was
// rejected; any other response (200, or e.g. 409 no-active-model) means it
// authenticated. Used at connect time so a wrong key never gets stored.
export async function probeKey(key: string): Promise<ProbeResult> {
  try {
    const res = await fetch(`${LORE_API_URL}/v1/memories?limit=1`, {
      headers: { authorization: `Bearer ${key}` },
      cache: "no-store",
    });
    return res.status === 401 ? "invalid" : "ok";
  } catch {
    return "unreachable";
  }
}

export type HealthResult =
  | { reachable: false }
  | { reachable: true; status: number; body: Record<string, unknown> | null };

// Server-side reachability probe of the upstream /healthz (unauthenticated).
export async function fetchHealth(): Promise<HealthResult> {
  try {
    const res = await fetch(`${LORE_API_URL}/healthz`, { cache: "no-store" });
    let body: Record<string, unknown> | null = null;
    try {
      body = (await res.json()) as Record<string, unknown>;
    } catch {
      body = null;
    }
    return { reachable: true, status: res.status, body };
  } catch {
    return { reachable: false };
  }
}
