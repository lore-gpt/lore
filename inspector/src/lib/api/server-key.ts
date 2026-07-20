import { readFileSync } from "node:fs";

// A server-side API key supplied by the operator. It takes priority over any
// browser-supplied cookie, and when present the connect screen is skipped.
//
// Two sources, env first:
//   1. LORE_API_KEY — the key directly.
//   2. LORE_CREDENTIALS_FILE — a path to the credentials file `lore provision`
//      writes (a `KEY=value` file with a LORE_API_KEY line). The compose stack
//      mounts this read-only so the container auto-connects with the provisioned
//      project key without the key ever being an env var.
//
// This module must only be imported from server code (route handlers, server
// actions, server components) — it reads process.env and the filesystem and is
// never bundled to the client.

// The provisioned key rarely changes; read the file once and cache it.
// `undefined` = not yet attempted.
let cachedFileKey: string | null | undefined;

function keyFromFile(): string | null {
  if (cachedFileKey !== undefined) {
    return cachedFileKey;
  }
  const path = process.env.LORE_CREDENTIALS_FILE?.trim();
  if (!path) {
    cachedFileKey = null;
    return null;
  }
  try {
    const match = readFileSync(path, "utf8").match(/^\s*LORE_API_KEY\s*=\s*(\S+)\s*$/m);
    cachedFileKey = match ? match[1].trim() : null;
  } catch {
    cachedFileKey = null;
  }
  return cachedFileKey;
}

export function getServerKey(): string | null {
  const envKey = process.env.LORE_API_KEY?.trim();
  if (envKey) {
    return envKey;
  }
  return keyFromFile();
}
