const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

// Validate a canonical UUID. Ids in this API (runs, memories) are UUIDs, so the
// Runs entry box can reject a malformed id locally before any request.
export function isUuid(value: string): boolean {
  return UUID_RE.test(value.trim());
}
