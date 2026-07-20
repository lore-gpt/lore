import { type NextRequest, NextResponse } from "next/server";

import { isAllowedHost } from "@/lib/http-guards";

// Runs on every page route (the matcher below skips the BFF, Next internals, and static assets). Two jobs:
//
// 1. DNS-rebinding defense for the PAGE surface. The server components render upstream data (memory content, run
//    traces) directly into HTML — they do NOT go through the BFF (/api/*), so the BFF's identical host-check
//    cannot protect them. A request whose Host is not a loopback name is a rebinding attempt (an attacker domain
//    re-resolved to 127.0.0.1 is same-origin to the browser, so SameSite/Sec-Fetch cannot catch it); refuse it
//    here so a rebound Host can never read a rendered page. /api/health (an intentionally-open liveness probe)
//    is excluded with the rest of /api by the matcher.
// 2. Expose the requested path+query to server components as an `x-pathname` request header, so the app-shell
//    layout can build a `?next=` return target when it bounces an unconnected visitor to /connect (a shared deep
//    link then resumes after connecting).
export function proxy(request: NextRequest) {
  if (!isAllowedHost(request.headers.get("host"), process.env.LORE_INSPECTOR_ALLOWED_HOSTS)) {
    return new NextResponse("host not allowed", { status: 403 });
  }
  const headers = new Headers(request.headers);
  headers.set("x-pathname", request.nextUrl.pathname + request.nextUrl.search);
  return NextResponse.next({ request: { headers } });
}

export const config = {
  // Run on page routes only; skip the BFF, Next internals, and static assets.
  matcher: ["/((?!api|_next/static|_next/image|icon.svg|.*\\.svg$).*)"],
};
