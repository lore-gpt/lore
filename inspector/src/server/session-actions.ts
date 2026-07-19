"use server";

import { cookies, headers } from "next/headers";
import { redirect } from "next/navigation";

import { getServerKey } from "@/lib/api/server-key";
import { probeKey } from "@/lib/api/upstream";

import { SESSION_COOKIE } from "./session";

export type ConnectResult = { ok: false; message: string };

// Store a browser-supplied API key in an httpOnly session cookie after
// validating it against the upstream server. On success this redirects to the
// overview; it only returns when there is an error to surface.
export async function connect(formData: FormData): Promise<ConnectResult> {
  // A server-side key wins; connecting via the browser is disabled in that case.
  if (getServerKey()) {
    return { ok: false, message: "A server-side key is configured; browser connect is disabled." };
  }

  const raw = String(formData.get("apiKey") ?? "").trim();
  if (!raw) {
    return { ok: false, message: "Enter an API key." };
  }

  const probe = await probeKey(raw);
  if (probe === "invalid") {
    return { ok: false, message: "That API key was rejected by the server." };
  }
  // "ok" or "unreachable" → store it; the overview reflects live status. A server
  // that is momentarily down should not block connecting.

  const cookieStore = await cookies();
  const hdrs = await headers();
  // Fail safe to Secure in production, and parse the first token of a possibly
  // comma-joined x-forwarded-proto (proxy chains emit "https, http") so a real
  // HTTPS deployment never ships a non-Secure key cookie.
  const proto = hdrs.get("x-forwarded-proto")?.split(",")[0]?.trim();
  const secure = process.env.NODE_ENV === "production" || proto === "https";
  cookieStore.set(SESSION_COOKIE, raw, {
    httpOnly: true,
    sameSite: "strict",
    secure,
    path: "/",
    // Session cookie: no maxAge/expires, so it is cleared when the browser closes.
  });

  redirect("/");
}

// Clear the session cookie and return to the connect screen.
export async function disconnect(): Promise<void> {
  const cookieStore = await cookies();
  cookieStore.delete(SESSION_COOKIE);
  redirect("/connect");
}
