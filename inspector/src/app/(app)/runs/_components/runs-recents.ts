import { isUuid } from "@/lib/uuid";

// Recently-viewed run ids for this browser session. There is no list-runs endpoint,
// so a diagnostician navigates by run id; this keeps the last few they opened as a
// convenience. Only UUIDs are stored, capped, and only added when a trace actually
// loaded. Client-only (reads sessionStorage).
export const RECENT_RUNS_KEY = "lore_recent_runs";
const CAP = 10;

export function getRecentRuns(): string[] {
  try {
    const raw = sessionStorage.getItem(RECENT_RUNS_KEY);
    if (!raw) {
      return [];
    }
    const parsed: unknown = JSON.parse(raw);
    return Array.isArray(parsed)
      ? parsed.filter((value): value is string => typeof value === "string" && isUuid(value))
      : [];
  } catch {
    return [];
  }
}

export function addRecentRun(id: string): void {
  if (!isUuid(id)) {
    return;
  }
  const next = [id, ...getRecentRuns().filter((value) => value !== id)].slice(0, CAP);
  try {
    sessionStorage.setItem(RECENT_RUNS_KEY, JSON.stringify(next));
  } catch {
    // sessionStorage unavailable — no-op.
  }
}

export function clearRecentRuns(): void {
  try {
    sessionStorage.removeItem(RECENT_RUNS_KEY);
  } catch {
    // no-op
  }
}
