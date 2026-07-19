import { LORE_API_URL } from "@/lib/api/config";
import { getActiveKey } from "@/server/session";

// The Inspector's BFF: a thin, 1:1 passthrough to the upstream Lore server.
// It attaches the active API key server-side (the browser never holds it), never
// caches (a diagnostic tool must show live state), and forwards status + body
// verbatim so the API's typed error codes surface unchanged. No response caching.
export const dynamic = "force-dynamic";

function json(status: number, body: Record<string, unknown>): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json", "cache-control": "no-store" },
  });
}

// CSRF defense-in-depth for state-changing requests: a cross-site request
// carrying our cookie is rejected. SameSite=Strict already blocks this in modern
// browsers; the header check is a cheap second belt.
function isSameOrigin(req: Request): boolean {
  const site = req.headers.get("sec-fetch-site");
  if (site) {
    // "same-origin" = our own page; "none" = user-initiated (address bar).
    return site === "same-origin" || site === "none";
  }
  const origin = req.headers.get("origin");
  if (!origin) {
    return true;
  }
  try {
    return new URL(origin).host === req.headers.get("host");
  } catch {
    return false;
  }
}

async function proxy(req: Request, ctx: { params: Promise<{ path: string[] }> }): Promise<Response> {
  const { path } = await ctx.params;
  const segments = path;
  // Reject dot-segments and injected slashes. A decoded `%2f` lands INSIDE a
  // segment, so without this guard the isV1 gate (decided on segments[0]) could
  // diverge from the URL-normalized upstream target and climb out of /v1 with the
  // key attached (e.g. `/api/v1/..%2f..%2fadmin` -> `${LORE_API_URL}/admin`).
  if (segments.some((s) => s === "" || s === "." || s === ".." || s.includes("/") || s.includes("\\"))) {
    return json(400, { message: "invalid path", code: "invalid_path" });
  }
  const upstreamPath = segments.join("/");
  const isV1 = segments[0] === "v1";
  const isMutation = req.method !== "GET" && req.method !== "HEAD";

  if (isMutation && !isSameOrigin(req)) {
    return json(403, { message: "cross-site request blocked", code: "forbidden" });
  }

  // Only /v1 requires a key (mirrors the upstream; /healthz and /metrics are open).
  const active = await getActiveKey();
  if (isV1 && !active) {
    return json(401, { message: "not connected", code: "not_connected" });
  }

  const search = new URL(req.url).search;
  const target = `${LORE_API_URL}/${upstreamPath}${search}`;

  const headers = new Headers();
  const contentType = req.headers.get("content-type");
  if (contentType) {
    headers.set("content-type", contentType);
  }
  const accept = req.headers.get("accept");
  if (accept) {
    headers.set("accept", accept);
  }
  if (active) {
    headers.set("authorization", `Bearer ${active.key}`);
  }

  const init: RequestInit = { method: req.method, headers, cache: "no-store", redirect: "manual" };
  if (isMutation) {
    const body = await req.arrayBuffer();
    if (body.byteLength > 0) {
      init.body = body;
    }
  }

  let upstream: Response;
  try {
    upstream = await fetch(target, init);
  } catch {
    return json(502, { message: "cannot reach the Lore server", code: "upstream_unreachable" });
  }

  // Forward status + body verbatim. Strip set-cookie / hop-by-hop headers; the
  // upstream never sets cookies for the browser and its key must not leak back.
  const respHeaders = new Headers();
  // A diagnostic tool streams live stored-memory content; never let it be cached.
  respHeaders.set("cache-control", "no-store");
  const upstreamContentType = upstream.headers.get("content-type");
  if (upstreamContentType) {
    respHeaders.set("content-type", upstreamContentType);
  }
  return new Response(upstream.body, { status: upstream.status, headers: respHeaders });
}

export const GET = proxy;
export const POST = proxy;
export const PUT = proxy;
export const PATCH = proxy;
export const DELETE = proxy;
export const HEAD = proxy;
