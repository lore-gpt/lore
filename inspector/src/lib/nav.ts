// Validate a `next` redirect target so a crafted `?next=` can never bounce a
// visitor to an off-site or protocol-relative URL after connecting. Only a plain
// root-relative in-app path is allowed.
export function sanitizeNextPath(raw: string | undefined | null): string | null {
  if (!raw) {
    return null;
  }
  // Must be root-relative ("/foo"), never protocol-relative ("//evil.com") or a
  // backslash-smuggled variant ("/\evil.com").
  if (!raw.startsWith("/") || raw.startsWith("//") || raw.startsWith("/\\")) {
    return null;
  }
  // Reject any control character (redirect/header smuggling).
  for (let i = 0; i < raw.length; i++) {
    if (raw.charCodeAt(i) < 0x20) {
      return null;
    }
  }
  return raw;
}
