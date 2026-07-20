// Render an ISO timestamp as a compact, deterministic UTC string (no locale
// drift) — this is a diagnostic surface, shared by the list and detail views.
export function formatUtc(iso: string): string {
  const date = new Date(iso);
  return Number.isNaN(date.getTime()) ? iso : `${date.toISOString().slice(0, 16).replace("T", " ")}Z`;
}
