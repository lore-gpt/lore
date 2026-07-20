// A liveness probe for the container HEALTHCHECK — 200 as soon as the Next server
// is serving. It does NOT check the upstream Lore server (that is the overview's
// job) so the Inspector reports healthy even while the API is starting.
export function GET() {
  return new Response("ok", { status: 200, headers: { "cache-control": "no-store" } });
}

export const dynamic = "force-dynamic";
