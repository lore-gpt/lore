// Pure request-guard predicates for the Inspector, kept free of any Next/Node request object so they are
// directly unit-testable (header values in, verdict out). The route handler and the proxy middleware are thin
// shells that read the headers and call these.

// DNS-rebinding defense. The Inspector is an unauthenticated, localhost-bound tool, so a request whose Host
// header is not a loopback name is a rebinding attempt: an attacker's domain re-resolved to 127.0.0.1 is
// same-origin to the browser, which SameSite / Sec-Fetch cannot catch. Applied to BOTH data surfaces — the
// page requests (proxy middleware) and the BFF (/api/[...path]) — since the pages render upstream data
// server-side WITHOUT going through the BFF. Loopback is ALWAYS allowed (a same-machine caller is never a
// rebinding target); a trusted reverse proxy can ADD hosts via LORE_INSPECTOR_ALLOWED_HOSTS (comma-separated
// host or host:port; "*" allows any). `configured` is the raw env value, read by the caller at request time.
export function isAllowedHost(host: string | null, configured: string | undefined): boolean {
  if (!host) {
    return false; // HTTP/1.1 mandates Host; its absence is anomalous.
  }
  const lower = host.toLowerCase();
  const hostname = lower.replace(/:\d+$/, ""); // drop the port
  if (hostname === "localhost" || hostname === "127.0.0.1" || hostname === "[::1]") {
    return true;
  }
  if (configured) {
    const list = configured
      .split(",")
      .map((h) => h.trim().toLowerCase())
      .filter(Boolean);
    return list.includes("*") || list.includes(lower) || list.includes(hostname);
  }
  return false;
}

// CSRF defense-in-depth for state-changing requests. Prefers the Fetch-Metadata Sec-Fetch-Site header
// (same-origin, or a user-initiated "none" such as the address bar, is allowed); a cross-site or same-site
// value is refused. When the header is absent (older clients), falls back to comparing the Origin's host to the
// request's Host; a missing Origin is a non-CORS request and is allowed, a malformed Origin is refused.
export function isSameOrigin(secFetchSite: string | null, origin: string | null, host: string | null): boolean {
  if (secFetchSite) {
    return secFetchSite === "same-origin" || secFetchSite === "none";
  }
  if (!origin) {
    return true;
  }
  try {
    return new URL(origin).host === host;
  } catch {
    return false;
  }
}
