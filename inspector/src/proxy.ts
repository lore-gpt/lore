import { type NextRequest, NextResponse } from "next/server";

// Expose the requested path+query to server components as an `x-pathname` request
// header. The app-shell layout reads it to build a `?next=` return target when it
// bounces an unconnected visitor to /connect, so a shared deep link resumes after
// connecting. This is a pure header pass-through — it reads no env or cookies, so
// it never gates a server-key deployment. (Next 16's proxy convention, formerly
// the middleware file.)
export function proxy(request: NextRequest) {
  const headers = new Headers(request.headers);
  headers.set("x-pathname", request.nextUrl.pathname + request.nextUrl.search);
  return NextResponse.next({ request: { headers } });
}

export const config = {
  // Run on page routes only; skip the BFF, Next internals, and static assets.
  matcher: ["/((?!api|_next/static|_next/image|icon.svg|.*\\.svg$).*)"],
};
