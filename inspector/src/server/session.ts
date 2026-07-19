import { cookies } from "next/headers";

import { getServerKey } from "@/lib/api/server-key";

// Name of the httpOnly session cookie that holds a browser-supplied API key.
export const SESSION_COOKIE = "lore_inspector_key";

export type KeySource = "server" | "cookie";

// Resolve the active API key. A server-side key (operator-configured) always
// wins over a browser-supplied cookie so there is never any ambiguity.
export async function getActiveKey(): Promise<{ key: string; source: KeySource } | null> {
  const serverKey = getServerKey();
  if (serverKey) {
    return { key: serverKey, source: "server" };
  }

  const cookieStore = await cookies();
  const cookieKey = cookieStore.get(SESSION_COOKIE)?.value?.trim();
  if (cookieKey) {
    return { key: cookieKey, source: "cookie" };
  }

  return null;
}

// Show only the non-secret prefix of a key (matches the server's key_prefix
// convention — roughly `lore_sk_` plus a few characters). The full key is never
// returned to the browser.
export function maskKey(key: string): string {
  return key.length <= 12 ? key : `${key.slice(0, 12)}…`;
}

export type ConnectionState = { connected: false } | { connected: true; source: KeySource; maskedKey: string };

export async function getConnectionState(): Promise<ConnectionState> {
  const active = await getActiveKey();
  if (!active) {
    return { connected: false };
  }
  return { connected: true, source: active.source, maskedKey: maskKey(active.key) };
}
