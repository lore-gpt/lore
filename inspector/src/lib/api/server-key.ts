// A server-side API key supplied by the operator (env var today; a mounted
// credentials file is wired in a later slice). When present it takes priority
// over any browser-supplied key and the connect screen is skipped entirely.
//
// This module must only be imported from server code (route handlers, server
// actions, server components) — it reads process.env and is never bundled to
// the client.
export function getServerKey(): string | null {
  const key = process.env.LORE_API_KEY?.trim();
  return key ? key : null;
}
